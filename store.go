package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaSQL se ejecuta con el protocolo simple (permite varias sentencias).
const schemaSQL = `
create table if not exists conversations (
    id          uuid primary key default gen_random_uuid(),
    created_at  timestamptz not null default now()
);

create table if not exists messages (
    id              bigserial primary key,
    conversation_id uuid not null references conversations (id) on delete cascade,
    role            text not null check (role in ('user', 'model', 'system')),
    content         text not null,
    model           text,
    latency_ms      integer,
    created_at      timestamptz not null default now()
);

create index if not exists messages_conversation_idx
    on messages (conversation_id, created_at);
`

// allowedDSNParams son los parametros de query que pgx entiende.
// Todo lo que no este aqui se descarta antes de conectar.
var allowedDSNParams = map[string]bool{
	"sslmode":              true,
	"sslcert":              true,
	"sslkey":               true,
	"sslrootcert":          true,
	"sslpassword":          true,
	"application_name":     true,
	"connect_timeout":      true,
	"search_path":          true,
	"target_session_attrs": true,
	"options":              true,
}

// Store encapsula el pool de Postgres.
type Store struct {
	pool         *pgxpool.Pool
	queryTimeout time.Duration
	log          *slog.Logger
}

// normalizeDSN limpia el DSN para pgx.
//
// pgx SI entiende `sslmode` (a diferencia de asyncpg en Python), pero NO
// entiende los parametros que anade Supabase como `pgbouncer=true` o
// `connection_limit`. Si se los pasas, la conexion falla con
// "unrecognized configuration parameter". Aqui se descartan.
func normalizeDSN(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("DSN invalido: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("DSN invalido: falta el esquema o el host")
	}

	q := u.Query()
	for key := range q {
		if !allowedDSNParams[strings.ToLower(key)] {
			q.Del(key)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// isTransactionPooler detecta el puerto 6543 de Supabase (PgBouncer en modo
// transaccion), donde no se pueden usar prepared statements.
func isTransactionPooler(dsn string) bool {
	u, err := url.Parse(dsn)
	if err != nil {
		return false
	}
	return u.Port() == "6543"
}

// NewStore crea el pool con reintentos. Devuelve nil (sin error) si no hay DSN.
func NewStore(ctx context.Context, cfg Config, log *slog.Logger) (*Store, error) {
	if !cfg.PersistenceEnabled() {
		log.Info("persistencia desactivada (no hay clave3)")
		return nil, nil
	}

	dsn, err := normalizeDSN(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("no se pudo parsear el DSN: %w", err)
	}

	// CLAVE para el pooler de Supabase: sin esto veras
	// "prepared statement \"stmtcache_...\" already exists" o
	// "prepared statement ... does not exist".
	//   QueryExecModeExec           -> protocolo extendido, SIN cache de statements
	//   QueryExecModeSimpleProtocol -> todo por protocolo simple
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec

	poolCfg.MaxConns = cfg.DBPoolMax
	poolCfg.MinConns = cfg.DBPoolMin
	poolCfg.MaxConnIdleTime = cfg.DBIdleLifetime
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute
	poolCfg.ConnConfig.ConnectTimeout = cfg.DBConnectTimeout

	if isTransactionPooler(dsn) {
		log.Info("transaction pooler detectado (puerto 6543): prepared statements desactivados")
	}

	var pool *pgxpool.Pool
	for attempt := 1; attempt <= cfg.DBConnectRetries; attempt++ {
		pool, err = pgxpool.NewWithConfig(ctx, poolCfg)
		if err == nil {
			if pingErr := pool.Ping(ctx); pingErr == nil {
				break
			} else {
				err = pingErr
				pool.Close()
				pool = nil
			}
		}
		log.Warn("no se pudo conectar a Postgres", "intento", attempt,
			"de", cfg.DBConnectRetries, "error", err)
		if attempt < cfg.DBConnectRetries {
			select {
			case <-time.After(time.Duration(attempt*2) * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	if pool == nil {
		return nil, fmt.Errorf("Postgres no respondio tras %d intentos: %w",
			cfg.DBConnectRetries, err)
	}

	store := &Store{pool: pool, queryTimeout: cfg.DBQueryTimeout, log: log}
	if cfg.AutoMigrate {
		if err := store.ensureSchema(ctx); err != nil {
			pool.Close()
			return nil, fmt.Errorf("no se pudo crear el esquema: %w", err)
		}
	}
	log.Info("pool Postgres listo", "min", cfg.DBPoolMin, "max", cfg.DBPoolMax)
	return store, nil
}

func (s *Store) ensureSchema(ctx context.Context) error {
	// Protocolo simple: imprescindible para ejecutar varias sentencias de golpe.
	if _, err := s.pool.Exec(ctx, schemaSQL, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	s.log.Info("esquema verificado/creado")
	return nil
}

func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
		s.log.Info("pool Postgres cerrado")
	}
}

func (s *Store) ctx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, s.queryTimeout)
}

func (s *Store) Ping(ctx context.Context) error {
	c, cancel := s.ctx(ctx)
	defer cancel()
	return s.pool.Ping(c)
}

// --------------------------------------------------------------------------- //
// Repositorio. El uuid va casteado a texto en SQL para no depender del mapeo
// de tipos de pgx y evitar sorpresas.
// --------------------------------------------------------------------------- //

func (s *Store) CreateConversation(ctx context.Context) (string, error) {
	c, cancel := s.ctx(ctx)
	defer cancel()

	var id string
	err := s.pool.QueryRow(c,
		"insert into conversations default values returning id::text",
	).Scan(&id)
	return id, err
}

func (s *Store) ConversationExists(ctx context.Context, id string) (bool, error) {
	c, cancel := s.ctx(ctx)
	defer cancel()

	var exists bool
	err := s.pool.QueryRow(c,
		"select exists (select 1 from conversations where id = $1::uuid)", id,
	).Scan(&exists)
	return exists, err
}

func (s *Store) AddMessage(ctx context.Context, conversationID, role, content string,
	model *string, latencyMs *int32) error {
	c, cancel := s.ctx(ctx)
	defer cancel()

	_, err := s.pool.Exec(c,
		`insert into messages (conversation_id, role, content, model, latency_ms)
		 values ($1::uuid, $2, $3, $4, $5)`,
		conversationID, role, content, model, latencyMs,
	)
	return err
}

func (s *Store) History(ctx context.Context, conversationID string, limit int) ([]Message, error) {
	c, cancel := s.ctx(ctx)
	defer cancel()

	rows, err := s.pool.Query(c,
		`select role, content, model, latency_ms, created_at
		 from (
		     select id, role, content, model, latency_ms, created_at
		     from messages
		     where conversation_id = $1::uuid
		     order by id desc
		     limit $2
		 ) as recientes
		 order by id asc`,
		conversationID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Message, 0, limit)
	for rows.Next() {
		var (
			m         Message
			createdAt time.Time
		)
		if err := rows.Scan(&m.Role, &m.Content, &m.Model, &m.LatencyMs, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) DeleteConversation(ctx context.Context, id string) (bool, error) {
	c, cancel := s.ctx(ctx)
	defer cancel()

	tag, err := s.pool.Exec(c, "delete from conversations where id = $1::uuid", id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
