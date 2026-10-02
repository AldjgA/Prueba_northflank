package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config agrupa toda la configuracion, leida del entorno.
//
// Los secretos mantienen exactamente los mismos nombres que ya tienes en
// Northflank: clave1, clave2 y clave3.
type Config struct {
	// --- Secretos ---------------------------------------------------------
	APIToken     string // clave1: antes token del bot de Telegram, ahora auth del backend
	GeminiAPIKey string // clave2: API key de Gemini
	DatabaseURL  string // clave3: DSN de Postgres/Supabase (opcional)

	// --- Gemini -----------------------------------------------------------
	Model           string
	SystemPrompt    string
	MaxInputChars   int
	MaxOutputTokens int32
	MaxHistoryTurns int
	Temperature     float32
	MaxRetries      int
	RequestTimeout  time.Duration

	// --- Concurrencia -----------------------------------------------------
	// MaxConcurrency es el numero de peticiones a Gemini que pueden estar
	// EN VUELO a la vez. No es un limite de conexiones HTTP: Go atiende
	// miles de conexiones concurrentes sin despeinarse.
	MaxConcurrency int
	// QueueTimeout es cuanto espera una peticion en cola antes de recibir
	// un 503 con Retry-After. Evita que el cliente espere indefinidamente.
	QueueTimeout time.Duration

	// --- Rate limit -------------------------------------------------------
	RateLimitPerMin int

	// --- Base de datos ----------------------------------------------------
	DBPoolMin        int32
	DBPoolMax        int32
	DBIdleLifetime   time.Duration
	DBQueryTimeout   time.Duration
	DBConnectTimeout time.Duration
	DBConnectRetries int
	AutoMigrate      bool
	HistoryLimit     int

	// --- Servidor ---------------------------------------------------------
	Addr string
}

func LoadConfig() Config {
	return Config{
		APIToken:     envStr("clave1", ""),
		GeminiAPIKey: envStr("clave2", ""),
		DatabaseURL:  envStr("clave3", ""),

		Model:           envStr("MODEL_ID", "gemini-3.5-flash-lite"),
		SystemPrompt:    envStr("SYSTEM_PROMPT", ""),
		MaxInputChars:   envInt("MAX_INPUT_CHARS", 8000),
		MaxOutputTokens: int32(envInt("MAX_OUTPUT_TOKENS", 1024)),
		MaxHistoryTurns: envInt("MAX_HISTORY_TURNS", 20),
		Temperature:     float32(envFloat("TEMPERATURE", 0.7)),
		MaxRetries:      envInt("MAX_RETRIES", 2),
		RequestTimeout:  envDuration("REQUEST_TIMEOUT_S", 60*time.Second),

		MaxConcurrency: envInt("MAX_CONCURRENCY", 3),
		QueueTimeout:   envDuration("QUEUE_TIMEOUT_S", 30*time.Second),

		RateLimitPerMin: envInt("RATE_LIMIT_PER_MIN", 60),

		DBPoolMin:        int32(envInt("DB_POOL_MIN", 1)),
		DBPoolMax:        int32(envInt("DB_POOL_MAX", 5)),
		DBIdleLifetime:   envDuration("DB_IDLE_LIFETIME_S", 30*time.Second),
		DBQueryTimeout:   envDuration("DB_QUERY_TIMEOUT_S", 10*time.Second),
		DBConnectTimeout: envDuration("DB_CONNECT_TIMEOUT_S", 10*time.Second),
		DBConnectRetries: envInt("DB_CONNECT_RETRIES", 3),
		AutoMigrate:      envBool("AUTO_MIGRATE", true),
		HistoryLimit:     envInt("HISTORY_LIMIT", 50),

		Addr: envStr("ADDR", ":8080"),
	}
}

// PersistenceEnabled indica si hay DSN configurado.
func (c Config) PersistenceEnabled() bool { return strings.TrimSpace(c.DatabaseURL) != "" }

// AuthEnabled indica si hay token de acceso configurado.
func (c Config) AuthEnabled() bool { return strings.TrimSpace(c.APIToken) != "" }

// --- Helpers de entorno ----------------------------------------------------

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}

// envDuration lee segundos y devuelve una duracion.
func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return time.Duration(f * float64(time.Second))
		}
	}
	return def
}
