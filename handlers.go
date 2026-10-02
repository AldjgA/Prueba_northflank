package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const maxBodyBytes = 512 << 10 // 512 KB

var uuidRe = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Server agrupa las dependencias de los handlers.
type Server struct {
	cfg     Config
	gemini  Generator
	store   *Store
	limiter *Limiter
	rate    *rateLimiter
	log     *slog.Logger
}

// httpError permite devolver un codigo concreto desde capas internas.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

// --------------------------------------------------------------------------- //
// Rutas
// --------------------------------------------------------------------------- //

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	// Sin autenticacion: son sondas del orquestador.
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleReady)

	// Con autenticacion (`clave1`) y rate limit.
	mux.HandleFunc("POST /chat", s.authMiddleware(s.handleChat))
	mux.HandleFunc("POST /chat/stream", s.authMiddleware(s.handleChatStream))
	mux.HandleFunc("GET /chat/{id}/history", s.authMiddleware(s.handleHistory))
	mux.HandleFunc("DELETE /chat/{id}", s.authMiddleware(s.handleDelete))

	return mux
}

// --------------------------------------------------------------------------- //
// Sondas
// --------------------------------------------------------------------------- //

// handleHealth es liveness: barato, no toca Gemini ni la base de datos.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"inflight": s.limiter.InFlight(),
		"capacity": s.limiter.Capacity(),
	})
}

// handleReady es readiness: valida configuracion y, si aplica, la base de datos.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.gemini == nil {
		writeError(w, http.StatusServiceUnavailable, "Sin configuracion.", "")
		return
	}

	dbStatus := "disabled"
	if s.store != nil {
		if err := s.store.Ping(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "Base de datos no disponible.", "")
			return
		}
		dbStatus = "ok"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ready",
		"model":       s.cfg.Model,
		"database":    dbStatus,
		"concurrency": s.limiter.Capacity(),
	})
}

// --------------------------------------------------------------------------- //
// POST /chat
// --------------------------------------------------------------------------- //

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	var req ChatRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Cuerpo JSON invalido.", err.Error())
		return
	}
	if err := s.validate(&req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error(), "")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout+s.cfg.QueueTimeout)
	defer cancel()

	history, convID, err := s.resolveHistory(ctx, &req)
	if err != nil {
		s.writeResolveError(w, err)
		return
	}

	s.persistUser(ctx, convID, req.Message)

	// --- Limite de concurrencia -------------------------------------------
	// Si hay MAX_CONCURRENCY peticiones en vuelo, esta espera en cola hasta
	// QueueTimeout. Si expira, se devuelve 503 en vez de dejar al cliente
	// colgado indefinidamente.
	queuedAt := time.Now()
	if err := s.limiter.Acquire(ctx); err != nil {
		if errors.Is(err, ErrQueueTimeout) {
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable,
				"Servidor ocupado. Intentalo de nuevo en unos segundos.", "")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "Servicio no disponible.", "")
		return
	}
	queuedMs := time.Since(queuedAt).Milliseconds()
	defer s.limiter.Release()

	reply, err := s.gemini.Generate(ctx,
		BuildContents(history, req.Message),
		s.gemini.BuildConfig(req.System, req.Temperature))
	if err != nil {
		s.log.Error("fallo al llamar a Gemini", "error", err)
		writeError(w, http.StatusBadGateway,
			"El servicio de IA no esta disponible en este momento.", "")
		return
	}

	latencyMs := time.Since(started).Milliseconds()
	s.persistModel(ctx, convID, reply, latencyMs)

	writeJSON(w, http.StatusOK, ChatResponse{
		Reply:          reply,
		Model:          s.cfg.Model,
		LatencyMs:      latencyMs,
		ConversationID: convID,
		QueuedMs:       queuedMs,
	})
}

// --------------------------------------------------------------------------- //
// POST /chat/stream
// --------------------------------------------------------------------------- //

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req ChatRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Cuerpo JSON invalido.", err.Error())
		return
	}
	if err := s.validate(&req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error(), "")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "Streaming no soportado.", "")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout+s.cfg.QueueTimeout)
	defer cancel()

	history, convID, err := s.resolveHistory(ctx, &req)
	if err != nil {
		s.writeResolveError(w, err)
		return
	}

	// El limite se aplica ANTES de escribir la cabecera, para poder responder 503.
	if err := s.limiter.Acquire(ctx); err != nil {
		if errors.Is(err, ErrQueueTimeout) {
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable,
				"Servidor ocupado. Intentalo de nuevo en unos segundos.", "")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "Servicio no disponible.", "")
		return
	}
	defer s.limiter.Release()

	s.persistUser(ctx, convID, req.Message)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if convID != nil {
		fmt.Fprintf(w, "event: meta\ndata: %s\n\n", *convID)
		flusher.Flush()
	}

	var full strings.Builder
	contents := BuildContents(history, req.Message)
	config := s.gemini.BuildConfig(req.System, req.Temperature)

	for resp, err := range s.gemini.GenerateStream(ctx, contents, config) {
		if err != nil {
			s.log.Error("error en streaming", "error", err)
			fmt.Fprint(w, "data: [ERROR]\n\n")
			flusher.Flush()
			break
		}
		if text := resp.Text(); text != "" {
			full.WriteString(text)
			writeSSE(w, text)
			flusher.Flush()
		}
	}

	if full.Len() > 0 {
		s.persistModel(ctx, convID, full.String(), 0)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// --------------------------------------------------------------------------- //
// Historial y borrado
// --------------------------------------------------------------------------- //

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusConflict, "Persistencia desactivada.", "")
		return
	}
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		writeError(w, http.StatusBadRequest, "'id' no es un UUID valido.", "")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.DBQueryTimeout)
	defer cancel()

	exists, err := s.store.ConversationExists(ctx, id)
	if err != nil {
		s.log.Error("error comprobando conversacion", "error", err)
		writeError(w, http.StatusInternalServerError, "Error interno.", "")
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "Conversacion no encontrada.", "")
		return
	}

	messages, err := s.store.History(ctx, id, s.cfg.HistoryLimit)
	if err != nil {
		s.log.Error("error leyendo historial", "error", err)
		writeError(w, http.StatusInternalServerError, "Error interno.", "")
		return
	}
	writeJSON(w, http.StatusOK, HistoryResponse{ConversationID: id, Messages: messages})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusConflict, "Persistencia desactivada.", "")
		return
	}
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		writeError(w, http.StatusBadRequest, "'id' no es un UUID valido.", "")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.DBQueryTimeout)
	defer cancel()

	deleted, err := s.store.DeleteConversation(ctx, id)
	if err != nil {
		s.log.Error("error borrando conversacion", "error", err)
		writeError(w, http.StatusInternalServerError, "Error interno.", "")
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "Conversacion no encontrada.", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "conversation_id": id})
}

// --------------------------------------------------------------------------- //
// Helpers
// --------------------------------------------------------------------------- //

// resolveHistory decide de donde sale el historial y cual es la conversacion.
//
//   - Con persistencia: el historial vive en Postgres (el cliente no lo manda).
//   - Sin persistencia: se usa el `history` que envia el cliente.
func (s *Server) resolveHistory(ctx context.Context, req *ChatRequest) ([]Turn, *string, error) {
	if s.store == nil || !req.ShouldPersist() {
		return req.History, nil, nil
	}

	var convID string
	if req.ConversationID != nil {
		convID = *req.ConversationID
		exists, err := s.store.ConversationExists(ctx, convID)
		if err != nil {
			return nil, nil, err
		}
		if !exists {
			return nil, nil, &httpError{http.StatusNotFound, "Conversacion no encontrada."}
		}
	} else {
		id, err := s.store.CreateConversation(ctx)
		if err != nil {
			return nil, nil, err
		}
		convID = id
	}

	rows, err := s.store.History(ctx, convID, s.cfg.MaxHistoryTurns)
	if err != nil {
		return nil, nil, err
	}

	history := make([]Turn, 0, len(rows))
	for _, m := range rows {
		if m.Role == "user" || m.Role == "model" {
			history = append(history, Turn{Role: m.Role, Content: m.Content})
		}
	}
	return history, &convID, nil
}

func (s *Server) writeResolveError(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		writeError(w, he.code, he.msg, "")
		return
	}
	s.log.Error("error resolviendo el historial", "error", err)
	writeError(w, http.StatusInternalServerError, "Error interno.", "")
}

// persistCtx crea un contexto desligado del cliente: si el cliente se
// desconecta a mitad, queremos guardar igual lo que ya se genero.
func (s *Server) persistCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
}

func (s *Server) persistUser(ctx context.Context, convID *string, content string) {
	if s.store == nil || convID == nil {
		return
	}
	c, cancel := s.persistCtx(ctx)
	defer cancel()
	if err := s.store.AddMessage(c, *convID, "user", content, nil, nil); err != nil {
		s.log.Warn("no se pudo guardar el mensaje del usuario", "error", err)
	}
}

func (s *Server) persistModel(ctx context.Context, convID *string, content string, latencyMs int64) {
	if s.store == nil || convID == nil {
		return
	}
	c, cancel := s.persistCtx(ctx)
	defer cancel()

	model := s.cfg.Model
	var latency *int32
	if latencyMs > 0 {
		v := int32(latencyMs)
		latency = &v
	}
	if err := s.store.AddMessage(c, *convID, "model", content, &model, latency); err != nil {
		s.log.Warn("no se pudo guardar la respuesta", "error", err)
	}
}

func (s *Server) validate(req *ChatRequest) error {
	if strings.TrimSpace(req.Message) == "" {
		return errors.New("El campo 'message' es obligatorio.")
	}
	if n := len([]rune(req.Message)); n > s.cfg.MaxInputChars {
		return fmt.Errorf("'message' tiene %d caracteres y el maximo es %d.", n, s.cfg.MaxInputChars)
	}
	if len(req.History) > s.cfg.MaxHistoryTurns {
		return fmt.Errorf("'history' tiene %d turnos y el maximo es %d.",
			len(req.History), s.cfg.MaxHistoryTurns)
	}
	if req.ConversationID != nil && !uuidRe.MatchString(*req.ConversationID) {
		return errors.New("'conversation_id' no es un UUID valido.")
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return errors.New("'temperature' debe estar entre 0 y 2.")
	}
	return nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	return json.NewDecoder(r.Body).Decode(dst)
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, code int, msg, detail string) {
	writeJSON(w, code, ErrorResponse{Error: msg, Detail: detail})
}

// writeSSE emite un evento SSE correctamente.
//
// Detalle que se suele pasar por alto: si el fragmento contiene saltos de
// linea (muy comun en markdown), hay que emitir una linea `data:` por cada
// una. Un `data: <texto con \n>` rompe el protocolo SSE.
func writeSSE(w io.Writer, text string) {
	for _, line := range strings.Split(text, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}
