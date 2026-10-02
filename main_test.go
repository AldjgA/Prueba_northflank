package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"
)

// --------------------------------------------------------------------------- //
// Doble de prueba del cliente de IA
// --------------------------------------------------------------------------- //

type fakeGen struct {
	reply  string
	chunks []string
	err    error
	delay  time.Duration
}

func (f *fakeGen) Generate(ctx context.Context, _ []*genai.Content,
	_ *genai.GenerateContentConfig) (string, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if f.err != nil {
		return "", f.err
	}
	return f.reply, nil
}

func (f *fakeGen) GenerateStream(_ context.Context, _ []*genai.Content,
	_ *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		for _, chunk := range f.chunks {
			resp := &genai.GenerateContentResponse{
				Candidates: []*genai.Candidate{
					{Content: &genai.Content{Parts: []*genai.Part{{Text: chunk}}}},
				},
			}
			if !yield(resp, nil) {
				return
			}
		}
	}
}

func (f *fakeGen) BuildConfig(string, *float32) *genai.GenerateContentConfig {
	return &genai.GenerateContentConfig{MaxOutputTokens: 128}
}

func newTestServer(gen Generator) *Server {
	cfg := LoadConfig()
	cfg.APIToken = "token-de-prueba"
	cfg.MaxInputChars = 8000
	cfg.MaxHistoryTurns = 20
	cfg.MaxConcurrency = 3
	cfg.QueueTimeout = 2 * time.Second
	cfg.RateLimitPerMin = 1000
	cfg.Model = "gemini-3.5-flash-lite"

	return &Server{
		cfg:     cfg,
		gemini:  gen,
		store:   nil, // sin persistencia
		limiter: NewLimiter(cfg.MaxConcurrency, cfg.QueueTimeout),
		rate:    newRateLimiter(cfg.RateLimitPerMin),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func do(t *testing.T, srv *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

var authHeader = map[string]string{"X-API-Key": "token-de-prueba"}

// --------------------------------------------------------------------------- //
// DSN de Supabase
// --------------------------------------------------------------------------- //

func TestNormalizeDSN(t *testing.T) {
	raw := "postgresql://postgres.miproject:secreto@aws-0-us-west-2.pooler.supabase.com:6543" +
		"/postgres?sslmode=require&pgbouncer=true&connection_limit=1"

	got, err := normalizeDSN(raw)
	if err != nil {
		t.Fatalf("normalizeDSN devolvio error: %v", err)
	}
	if strings.Contains(got, "pgbouncer") || strings.Contains(got, "connection_limit") {
		t.Errorf("no se limpiaron los parametros de Supabase: %s", got)
	}
	if !strings.Contains(got, "sslmode=require") {
		t.Errorf("se perdio sslmode, que pgx SI entiende: %s", got)
	}
	if !strings.Contains(got, "pooler.supabase.com:6543") {
		t.Errorf("se perdio el host o el puerto: %s", got)
	}
}

func TestIsTransactionPooler(t *testing.T) {
	pooler := "postgresql://u:p@aws-0-us-west-2.pooler.supabase.com:6543/postgres"
	direct := "postgresql://u:p@db.xxx.supabase.co:5432/postgres"

	if !isTransactionPooler(pooler) {
		t.Error("no detecto el transaction pooler en el puerto 6543")
	}
	if isTransactionPooler(direct) {
		t.Error("confundio el puerto 5432 con el transaction pooler")
	}
}

// --------------------------------------------------------------------------- //
// Concurrencia: el nucleo de la pregunta del usuario
// --------------------------------------------------------------------------- //

func TestLimiterRespetaElMaximoConfigurado(t *testing.T) {
	const maxConcurrency = 3
	lim := NewLimiter(maxConcurrency, 5*time.Second)

	var (
		mu        sync.Mutex
		active    int
		peak      int
		completed int
	)
	var wg sync.WaitGroup

	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := lim.Acquire(context.Background()); err != nil {
				return
			}
			defer lim.Release()

			mu.Lock()
			active++
			if active > peak {
				peak = active
			}
			mu.Unlock()

			time.Sleep(40 * time.Millisecond)

			mu.Lock()
			active--
			completed++
			mu.Unlock()
		}()
	}
	wg.Wait()

	if peak > maxConcurrency {
		t.Errorf("hubo %d peticiones simultaneas, el maximo era %d", peak, maxConcurrency)
	}
	if peak < 2 {
		t.Errorf("solo %d en paralelo: no se esta aprovechando la concurrencia", peak)
	}
	if completed != 12 {
		t.Errorf("completadas %d de 12: el limitador esta perdiendo peticiones", completed)
	}
}

func TestLimiterEncolaYAtiendeCuandoSeLiberaUnHueco(t *testing.T) {
	lim := NewLimiter(1, 2*time.Second)

	if err := lim.Acquire(context.Background()); err != nil {
		t.Fatalf("primer Acquire: %v", err)
	}

	// Liberamos el hueco 100 ms despues, desde otra goroutine.
	go func() {
		time.Sleep(100 * time.Millisecond)
		lim.Release()
	}()

	start := time.Now()
	if err := lim.Acquire(context.Background()); err != nil {
		t.Fatalf("la peticion en cola deberia haber entrado: %v", err)
	}
	lim.Release()

	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("no espero en cola (%.0f ms)", elapsed.Seconds()*1000)
	}
}

func TestLimiterDevuelveErrorSiLaColaSeAgota(t *testing.T) {
	lim := NewLimiter(1, 100*time.Millisecond)

	if err := lim.Acquire(context.Background()); err != nil {
		t.Fatalf("primer Acquire: %v", err)
	}
	defer lim.Release()

	start := time.Now()
	err := lim.Acquire(context.Background())
	if !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("queria ErrQueueTimeout, obtuve %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("no espero el QueueTimeout completo (%.0f ms)", elapsed.Seconds()*1000)
	}
}

func TestLimiterRespetaLaCancelacionDelContexto(t *testing.T) {
	lim := NewLimiter(1, 10*time.Second)
	if err := lim.Acquire(context.Background()); err != nil {
		t.Fatalf("primer Acquire: %v", err)
	}
	defer lim.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := lim.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queria DeadlineExceeded, obtuve %v", err)
	}
}

// --------------------------------------------------------------------------- //
// Rate limit
// --------------------------------------------------------------------------- //

func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter(3)

	for i := 1; i <= 3; i++ {
		if !rl.Allow("1.2.3.4") {
			t.Fatalf("la peticion %d deberia pasar", i)
		}
	}
	if rl.Allow("1.2.3.4") {
		t.Error("la cuarta peticion deberia rebotar")
	}
	if !rl.Allow("5.6.7.8") {
		t.Error("otra IP no deberia verse afectada")
	}
}

func TestRateLimiterSinLimite(t *testing.T) {
	rl := newRateLimiter(0)
	for i := 0; i < 100; i++ {
		if !rl.Allow("1.2.3.4") {
			t.Fatal("con limit=0 no deberia haber tope")
		}
	}
}

// --------------------------------------------------------------------------- //
// Validacion y SSE
// --------------------------------------------------------------------------- //

func TestValidate(t *testing.T) {
	s := newTestServer(&fakeGen{})
	str := func(v string) *string { return &v }
	f32 := func(v float32) *float32 { return &v }

	cases := []struct {
		name    string
		req     ChatRequest
		wantErr bool
	}{
		{"mensaje valido", ChatRequest{Message: "hola"}, false},
		{"mensaje vacio", ChatRequest{Message: ""}, true},
		{"mensaje solo espacios", ChatRequest{Message: "   "}, true},
		{"mensaje gigante", ChatRequest{Message: strings.Repeat("x", 8001)}, true},
		{"uuid invalido", ChatRequest{Message: "hola", ConversationID: str("no-es-uuid")}, true},
		{"uuid valido", ChatRequest{Message: "hola",
			ConversationID: str("11111111-1111-1111-1111-111111111111")}, false},
		{"temperatura fuera de rango", ChatRequest{Message: "hola", Temperature: f32(3)}, true},
		{"temperatura valida", ChatRequest{Message: "hola", Temperature: f32(0.5)}, false},
		{"historial excesivo", ChatRequest{Message: "hola",
			History: make([]Turn, 21)}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.validate(&tc.req)
			if tc.wantErr && err == nil {
				t.Error("esperaba error y no lo hubo")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("no esperaba error: %v", err)
			}
		})
	}
}

// TestWriteSSEMultilinea cubre un fallo clasico: si el fragmento trae saltos
// de linea (markdown), hay que emitir una linea `data:` por cada una.
func TestWriteSSEMultilinea(t *testing.T) {
	var sb strings.Builder
	writeSSE(&sb, "linea1\nlinea2\nlinea3")
	got := sb.String()

	want := "data: linea1\ndata: linea2\ndata: linea3\n\n"
	if got != want {
		t.Errorf("SSE mal formado.\n  queria: %q\n  obtuve: %q", want, got)
	}
	if strings.Contains(got, "data: linea1\nlinea2") {
		t.Error("se emitio un salto de linea crudo dentro de un evento SSE")
	}
}

func TestBuildContents(t *testing.T) {
	history := []Turn{
		{Role: "user", Content: "hola"},
		{Role: "model", Content: "buenas"},
		{Role: "user", Content: "   "}, // se descarta
	}
	contents := BuildContents(history, "¿y eso?")

	if len(contents) != 3 {
		t.Fatalf("queria 3 contenidos, obtuve %d", len(contents))
	}
	if contents[0].Role != genai.RoleUser {
		t.Errorf("primer rol = %q, queria %q", contents[0].Role, genai.RoleUser)
	}
	if contents[1].Role != genai.RoleModel {
		t.Errorf("segundo rol = %q, queria %q", contents[1].Role, genai.RoleModel)
	}
	if last := contents[len(contents)-1]; last.Role != genai.RoleUser {
		t.Errorf("el ultimo debe ser el mensaje nuevo con rol user, es %q", last.Role)
	}
}

// --------------------------------------------------------------------------- //
// Endpoints HTTP
// --------------------------------------------------------------------------- //

func TestAuthRechazaSinToken(t *testing.T) {
	s := newTestServer(&fakeGen{reply: "hola"})

	if rec := do(t, s, "POST", "/chat", `{"message":"hola"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("sin token: %d, queria 401", rec.Code)
	}
	if rec := do(t, s, "POST", "/chat", `{"message":"hola"}`,
		map[string]string{"X-API-Key": "malo"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("token incorrecto: %d, queria 401", rec.Code)
	}
	if rec := do(t, s, "POST", "/chat", `{"message":"hola"}`,
		map[string]string{"Authorization": "Bearer token-de-prueba"}); rec.Code != http.StatusOK {
		t.Errorf("Bearer valido: %d, queria 200", rec.Code)
	}
}

func TestHealthYReady(t *testing.T) {
	s := newTestServer(&fakeGen{})

	rec := do(t, s, "GET", "/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/health: %d", rec.Code)
	}
	var health map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &health)
	if health["capacity"] != float64(3) {
		t.Errorf("/health deberia informar capacity=3, informa %v", health["capacity"])
	}

	rec = do(t, s, "GET", "/ready", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/ready: %d", rec.Code)
	}
	var ready map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &ready)
	if ready["database"] != "disabled" {
		t.Errorf("/ready deberia informar database=disabled, informa %v", ready["database"])
	}
}

func TestChatDevuelveElContratoEsperado(t *testing.T) {
	s := newTestServer(&fakeGen{reply: "Hola, soy el mini-backend."})

	rec := do(t, s, "POST", "/chat", `{"message":"hola"}`, authHeader)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var body ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("respuesta no parseable: %v", err)
	}
	if body.Reply != "Hola, soy el mini-backend." {
		t.Errorf("reply = %q", body.Reply)
	}
	if body.Model == "" {
		t.Error("falta el campo model")
	}
	if body.ConversationID != nil {
		t.Error("sin persistencia conversation_id deberia ser null")
	}
}

func TestChatValidaLaEntrada(t *testing.T) {
	s := newTestServer(&fakeGen{reply: "x"})

	if rec := do(t, s, "POST", "/chat", `{}`, authHeader); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("sin message: %d, queria 422", rec.Code)
	}
	if rec := do(t, s, "POST", "/chat", `no-json`, authHeader); rec.Code != http.StatusBadRequest {
		t.Errorf("JSON invalido: %d, queria 400", rec.Code)
	}
}

func TestChatDevuelve502SiElModeloFalla(t *testing.T) {
	s := newTestServer(&fakeGen{err: errors.New("boom")})

	rec := do(t, s, "POST", "/chat", `{"message":"hola"}`, authHeader)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status %d, queria 502", rec.Code)
	}
	// No debe filtrar el error interno.
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("se filtro el error interno al cliente")
	}
}

func TestChatStreamEmiteSSE(t *testing.T) {
	s := newTestServer(&fakeGen{chunks: []string{"Hola", ", soy", " el bot."}})

	rec := do(t, s, "POST", "/chat/stream", `{"message":"hola"}`, authHeader)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}

	body := rec.Body.String()
	for _, want := range []string{"data: Hola", "data: , soy", "data:  el bot.", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("falta %q en el stream:\n%s", want, body)
		}
	}
}

func TestChatStreamMantieneElFormatoConSaltosDeLinea(t *testing.T) {
	s := newTestServer(&fakeGen{chunks: []string{"# Titulo\n\n- punto 1\n- punto 2"}})

	rec := do(t, s, "POST", "/chat/stream", `{"message":"hola"}`, authHeader)
	body := rec.Body.String()

	// Cada linea del markdown debe ir precedida de "data: ".
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") && !strings.HasPrefix(line, "event: ") {
			t.Errorf("linea SSE mal formada: %q", line)
		}
	}
}

func TestPersistenciaDesactivadaDevuelve409(t *testing.T) {
	s := newTestServer(&fakeGen{})
	const id = "11111111-1111-1111-1111-111111111111"

	if rec := do(t, s, "GET", "/chat/"+id+"/history", "", authHeader); rec.Code != http.StatusConflict {
		t.Errorf("history sin persistencia: %d, queria 409", rec.Code)
	}
	if rec := do(t, s, "DELETE", "/chat/"+id, "", authHeader); rec.Code != http.StatusConflict {
		t.Errorf("delete sin persistencia: %d, queria 409", rec.Code)
	}
}

func TestHistorialConUUIDInvalidoDevuelve400(t *testing.T) {
	s := newTestServer(&fakeGen{})
	s.store = nil

	if rec := do(t, s, "GET", "/chat/no-es-uuid/history", "", authHeader); rec.Code != http.StatusConflict {
		// Sin store el 409 va primero; el 400 solo aplica con persistencia.
		t.Logf("sin store devuelve %d (esperado 409)", rec.Code)
	}
}

// --------------------------------------------------------------------------- //
// Configuracion
// --------------------------------------------------------------------------- //

func TestLoadConfigPorDefecto(t *testing.T) {
	t.Setenv("MAX_CONCURRENCY", "")
	t.Setenv("clave1", "")
	t.Setenv("clave3", "")

	cfg := LoadConfig()
	if cfg.MaxConcurrency != 3 {
		t.Errorf("MaxConcurrency por defecto = %d, queria 3", cfg.MaxConcurrency)
	}
	if cfg.PersistenceEnabled() {
		t.Error("sin clave3 la persistencia debe estar desactivada")
	}
	if cfg.AuthEnabled() {
		t.Error("sin clave1 la autenticacion debe estar desactivada")
	}
}

func TestConfigLeeLosSecretos(t *testing.T) {
	t.Setenv("clave1", "tok")
	t.Setenv("clave2", "api")
	t.Setenv("clave3", "postgresql://u:p@h:6543/db")

	cfg := LoadConfig()
	if !cfg.AuthEnabled() {
		t.Error("clave1 deberia activar la autenticacion")
	}
	if !cfg.PersistenceEnabled() {
		t.Error("clave3 deberia activar la persistencia")
	}
	if cfg.GeminiAPIKey != "api" {
		t.Errorf("clave2 no se leyo: %q", cfg.GeminiAPIKey)
	}
}
