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

	// --- Administracion de claves de tester --------------------------------
	// Estos comandos hablan directamente con la base de datos, asi que se
	// pueden ejecutar en local (go run . -newkey "Ana") sin exponer nada.
	newKey := flag.String("newkey", "",
		"crea una clave de tester con esa etiqueta, imprime el token y sale")
	listKeys := flag.Bool("listkeys", false,
		"lista las claves de tester con su consumo de hoy y sale")
	revoke := flag.Int64("revoke", 0,
		"revoca la clave con ese id y sale")
	quota := flag.Int("quota", 0,
		"cuota diaria para -newkey (0 = usar DEFAULT_DAILY_QUOTA)")
	expires := flag.Int("expires", 0,
		"caducidad en dias para -newkey (0 = sin caducidad)")

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

	// Comandos de administracion: no arrancan el servidor.
	if *newKey != "" || *listKeys || *revoke > 0 {
		os.Exit(runKeyCommand(ctx, cfg, log, *newKey, *listKeys, *revoke, *quota, *expires))
	}

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
		"tope_global_diario", cfg.GlobalDailyCap,
		"cuota_por_defecto", cfg.DefaultDailyQuota,
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

// --------------------------------------------------------------------------- //
// Administracion de claves de tester por linea de comandos
// --------------------------------------------------------------------------- //

// runKeyCommand gestiona las claves sin arrancar el servidor.
//
// Sirve para crear la primera clave (o recuperarte si te quedas fuera) sin
// depender de la API de administracion.
func runKeyCommand(ctx context.Context, cfg Config, log *slog.Logger,
	label string, list bool, revoke int64, quota, expiresDays int) int {

	if !cfg.PersistenceEnabled() {
		fmt.Fprintln(os.Stderr,
			"Hace falta clave3 (el DSN de Postgres) para administrar claves.")
		return 1
	}

	store, err := NewStore(ctx, cfg, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "No se pudo conectar a Postgres:", err)
		return 1
	}
	defer store.Close()

	switch {
	case list:
		return listKeysCommand(ctx, store, cfg)

	case revoke > 0:
		ok, err := store.DeactivateAPIKey(ctx, revoke)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error revocando la clave:", err)
			return 1
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "La clave %d no existe o ya estaba revocada.\n", revoke)
			return 1
		}
		fmt.Printf("Clave %d revocada.\n", revoke)
		return 0

	default:
		return newKeyCommand(ctx, store, cfg, label, quota, expiresDays)
	}
}

func newKeyCommand(ctx context.Context, store *Store, cfg Config,
	label string, quota, expiresDays int) int {

	token, hash, err := GenerateToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, "No se pudo generar el token:", err)
		return 1
	}

	if quota <= 0 {
		quota = cfg.DefaultDailyQuota
	}
	var expiresAt *time.Time
	if expiresDays > 0 {
		t := time.Now().UTC().AddDate(0, 0, expiresDays)
		expiresAt = &t
	}

	id, err := store.CreateAPIKey(ctx, hash, label, quota, expiresAt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "No se pudo guardar la clave:", err)
		return 1
	}

	fmt.Println()
	fmt.Printf("  Clave creada (id=%d)\n", id)
	fmt.Printf("  Etiqueta     : %s\n", label)
	fmt.Printf("  Cuota diaria : %d peticiones\n", quota)
	if expiresAt != nil {
		fmt.Printf("  Caduca       : %s\n", expiresAt.Format("2006-01-02"))
	} else {
		fmt.Println("  Caduca       : nunca")
	}
	fmt.Println()
	fmt.Printf("  TOKEN: %s\n", token)
	fmt.Println()
	fmt.Println("  Guardalo ahora: en la base de datos solo queda su hash,")
	fmt.Println("  asi que no se puede recuperar despues.")
	fmt.Println()
	fmt.Println("  El tester lo usa asi:")
	fmt.Println("    curl -X POST https://TU-URL/chat \\")
	fmt.Println("      -H 'Content-Type: application/json' \\")
	fmt.Printf("      -H 'X-API-Key: %s' \\\n", token)
	fmt.Println("      -d '{\"message\":\"hola\"}'")
	fmt.Println()
	return 0
}

func listKeysCommand(ctx context.Context, store *Store, cfg Config) int {
	keys, err := store.ListAPIKeys(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "No se pudieron listar las claves:", err)
		return 1
	}

	if len(keys) == 0 {
		fmt.Println("No hay claves creadas todavia.")
		fmt.Println("Crea la primera con:  ./server -newkey \"Nombre del tester\"")
		return 0
	}

	fmt.Printf("\n  %-4s %-24s %-7s %-7s %-7s %s\n",
		"ID", "ETIQUETA", "CUOTA", "USADO", "ACTIVA", "ULTIMO USO")
	fmt.Println("  " + strings.Repeat("-", 74))
	for _, k := range keys {
		activa := "si"
		if !k.Active {
			activa = "NO"
		}
		ultimo := "nunca"
		if k.LastUsedAt != nil {
			ultimo = *k.LastUsedAt
			if len(ultimo) >= 16 {
				ultimo = strings.Replace(ultimo[:16], "T", " ", 1)
			}
		}
		label := k.Label
		if runas := []rune(label); len(runas) > 24 {
			label = string(runas[:21]) + "..."
		}
		fmt.Printf("  %-4d %-24s %-7d %-7d %-7s %s\n",
			k.ID, label, k.DailyQuota, k.UsedToday, activa, ultimo)
	}

	global, err := store.GlobalUsageToday(ctx)
	if err == nil {
		if cfg.GlobalDailyCap > 0 {
			fmt.Printf("\n  Consumo global hoy: %d de %d peticiones\n", global, cfg.GlobalDailyCap)
		} else {
			fmt.Printf("\n  Consumo global hoy: %d peticiones (sin tope)\n", global)
		}
	}
	fmt.Println()
	return 0
}
