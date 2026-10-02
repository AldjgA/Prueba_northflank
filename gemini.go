package main

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/genai"
)

// Generator abstrae el cliente de IA. Existe para poder sustituirlo por un
// doble en los tests sin llamar de verdad a la API.
type Generator interface {
	Generate(ctx context.Context, contents []*genai.Content,
		config *genai.GenerateContentConfig) (string, error)
	GenerateStream(ctx context.Context, contents []*genai.Content,
		config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error]
	BuildConfig(system string, temperature *float32) *genai.GenerateContentConfig
}

// Gemini envuelve el SDK oficial google.golang.org/genai.
type Gemini struct {
	client *genai.Client
	cfg    Config
	log    *slog.Logger
}

// NewGemini crea el cliente una sola vez, para toda la vida del proceso.
// Reutilizarlo mantiene el pool de conexiones HTTP vivo (keep-alive) y evita
// un handshake TLS nuevo en cada peticion.
func NewGemini(ctx context.Context, cfg Config, log *slog.Logger) (*Gemini, error) {
	if strings.TrimSpace(cfg.GeminiAPIKey) == "" {
		return nil, errors.New("falta el secreto clave2 (API key de Gemini)")
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  cfg.GeminiAPIKey,
		Backend: genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{
			Timeout: genai.Ptr(cfg.RequestTimeout),
		},
	})
	if err != nil {
		return nil, err
	}
	return &Gemini{client: client, cfg: cfg, log: log}, nil
}

// --------------------------------------------------------------------------- //
// Construccion de peticiones
// --------------------------------------------------------------------------- //

// BuildContents convierte el historial + el mensaje nuevo en el formato del SDK.
func BuildContents(history []Turn, message string) []*genai.Content {
	contents := make([]*genai.Content, 0, len(history)+1)
	for _, turn := range history {
		if strings.TrimSpace(turn.Content) == "" {
			continue
		}
		// Ojo: genai.RoleUser/RoleModel son constantes sin tipo, asi que hay
		// que anotar la variable para que sea genai.Role y no string.
		var role genai.Role = genai.RoleUser
		if turn.Role == "model" {
			role = genai.RoleModel
		}
		contents = append(contents, genai.NewContentFromText(turn.Content, role))
	}
	return append(contents, genai.NewContentFromText(message, genai.RoleUser))
}

// BuildConfig arma la configuracion de generacion.
func (g *Gemini) BuildConfig(system string, temperature *float32) *genai.GenerateContentConfig {
	temp := g.cfg.Temperature
	if temperature != nil {
		temp = *temperature
	}

	cfg := &genai.GenerateContentConfig{
		MaxOutputTokens: g.cfg.MaxOutputTokens, // int32, no puntero
		Temperature:     genai.Ptr(temp),       // *float32
	}

	sys := strings.TrimSpace(system)
	if sys == "" {
		sys = strings.TrimSpace(g.cfg.SystemPrompt)
	}
	if sys != "" {
		cfg.SystemInstruction = genai.NewContentFromText(sys, genai.RoleUser)
	}
	return cfg
}

// --------------------------------------------------------------------------- //
// Llamadas
// --------------------------------------------------------------------------- //

// Generate hace la llamada no-streaming, con reintentos y backoff exponencial
// solo ante errores transitorios.
func (g *Gemini) Generate(ctx context.Context, contents []*genai.Content,
	config *genai.GenerateContentConfig) (string, error) {

	var lastErr error
	for attempt := 0; attempt <= g.cfg.MaxRetries; attempt++ {
		resp, err := g.client.Models.GenerateContent(ctx, g.cfg.Model, contents, config)
		if err == nil {
			return strings.TrimSpace(resp.Text()), nil
		}
		lastErr = err

		if attempt < g.cfg.MaxRetries && isTransient(err) {
			backoff := time.Duration(1<<attempt) * 500 * time.Millisecond
			g.log.Warn("reintentando llamada a Gemini",
				"intento", attempt+1, "espera", backoff, "error", err)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			continue
		}
		break
	}
	return "", lastErr
}

// GenerateStream devuelve la secuencia de fragmentos. El SDK usa range-over-func
// (iter.Seq2), asi que se consume con `for resp, err := range ...`.
func (g *Gemini) GenerateStream(ctx context.Context, contents []*genai.Content,
	config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	return g.client.Models.GenerateContentStream(ctx, g.cfg.Model, contents, config)
}

// isTransient indica si merece la pena reintentar.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	// El SDK devuelve genai.APIError (valor o puntero segun la ruta de codigo).
	var ptrErr *genai.APIError
	if errors.As(err, &ptrErr) {
		return transientCode(ptrErr.Code)
	}
	var valErr genai.APIError
	if errors.As(err, &valErr) {
		return transientCode(valErr.Code)
	}

	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"timeout", "deadline exceeded", "connection reset",
		"unavailable", "resource_exhausted", "try again",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func transientCode(code int) bool {
	switch code {
	case 429, 500, 502, 503, 504:
		return true
	}
	return false
}
