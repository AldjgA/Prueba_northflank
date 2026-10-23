// Command api-mensajes es una API HTTP de mensajeria que trabaja con datos mock
// en memoria: enviar_mensaje, recibir_mensaje y descargar_archivo.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "consulta /health y termina (lo usa el HEALTHCHECK de Docker)")
	flag.Parse()

	if *healthcheck {
		os.Exit(ejecutarHealthcheck())
	}

	if err := ejecutar(); err != nil {
		slog.Error("el servidor termino con error", "err", err)
		os.Exit(1)
	}
}

// puerto devuelve PORT (Northflank lo inyecta) o 8080 por defecto.
func puerto() string {
	if p := strings.TrimSpace(os.Getenv("PORT")); p != "" {
		return p
	}
	return "8080"
}

func nuevoLogger() *slog.Logger {
	nivel := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		nivel = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: nivel}))
}

// nuevoRouter arma las rutas y la cadena de middlewares.
func nuevoRouter(st *Store, log *slog.Logger) http.Handler {
	h := &Handler{store: st, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.index)
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("POST /enviar_mensaje", h.enviarMensaje)
	mux.HandleFunc("GET /recibir_mensaje", h.recibirMensaje)
	mux.HandleFunc("GET /archivos", h.listarArchivos)
	mux.HandleFunc("GET /descargar_archivo", h.descargarArchivo)
	mux.HandleFunc("/", h.noEncontrado)

	return conRecuperacion(log, conLog(log, conCORS(mux)))
}

func ejecutar() error {
	log := nuevoLogger()
	slog.SetDefault(log)

	srv := &http.Server{
		Addr:              ":" + puerto(),
		Handler:           nuevoRouter(StoreNuevo(), log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errores := make(chan error, 1)
	go func() {
		log.Info("api-mensajes escuchando", "addr", srv.Addr, "datos", "mock en memoria")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errores <- err
		}
	}()

	select {
	case err := <-errores:
		return err
	case <-ctx.Done():
		log.Info("apagando el servidor")
	}

	apagado, cancelar := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelar()

	if err := srv.Shutdown(apagado); err != nil {
		return fmt.Errorf("apagado forzado: %w", err)
	}
	log.Info("servidor detenido correctamente")
	return nil
}

// ejecutarHealthcheck consulta /health en localhost; devuelve 0 si responde 200.
func ejecutarHealthcheck() int {
	cliente := &http.Client{Timeout: 2 * time.Second}
	url := "http://127.0.0.1:" + puerto() + "/health"

	resp, err := cliente.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck fallido: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck fallido: estado %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
