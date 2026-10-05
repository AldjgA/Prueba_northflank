package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false,
		"consulta /health y sale con 0 o 1 (para el HEALTHCHECK de Docker)")
	flag.Parse()

	cfg := LoadConfig()
	log := newLogger()

	// Modo healthcheck: scratch no tiene shell ni curl, asi que el propio
	// binario hace de sonda.
	if *healthcheck {
		os.Exit(runHealthcheck(cfg))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var gen Generator
	gemini, err := NewGemini(ctx, cfg, log)
	if err != nil {
		log.Error("no se pudo crear el cliente de Gemini", "error", err)
		os.Exit(1)
	}
	gen = gemini

	// Si la base de datos no responde, se arranca igualmente sin persistencia.
	store, err := NewStore(ctx, cfg, log)
	if err != nil {
		log.Error("Postgres no disponible: se arranca SIN persistencia", "error", err)
		store = nil
	}

	srv := &Server{
		cfg:     cfg,
		gemini:  gen,
		store:   store,
		limiter: NewLimiter(cfg.MaxConcurrency, cfg.QueueTimeout),
		rate:    newRateLimiter(cfg.RateLimitPerMin),
		log:     log,
	}

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WriteTimeout DEBE ser 0: con un valor fijo, las respuestas SSE
		// de larga duracion se cortarian a mitad del stream.
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	if !cfg.AuthEnabled() {
		log.Warn("clave1 no esta definida: el mini-backend queda SIN autenticacion")
	}
	log.Info("arranque OK",
		"addr", cfg.Addr,
		"model", cfg.Model,
		"max_concurrencia", cfg.MaxConcurrency,
		"cola", cfg.QueueTimeout.String(),
		"rate_limit", cfg.RateLimitPerMin,
		"persistencia", store != nil,
	)

	serverErr := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		log.Error("el servidor se detuvo inesperadamente", "error", err)
		store.Close()
		os.Exit(1)
	case <-ctx.Done():
		log.Info("apagando de forma ordenada...")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("apagado forzado", "error", err)
	}
	store.Close()
	log.Info("servidor detenido")
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	// Logs en JSON: Northflank los indexa y los puedes filtrar por campo.
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

// runHealthcheck consulta /health en el propio proceso.
func runHealthcheck(cfg Config) int {
	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		host, port = "127.0.0.1", "8080"
	}
	// 0.0.0.0 y "" no son destinos validos para un cliente HTTP.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/health", net.JoinHostPort(host, port)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}
