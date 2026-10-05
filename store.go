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

-- Claves de tester. Solo se guarda el SHA-256 del token, nunca el token en claro.
create table if not exists api_keys (
    id           bigserial primary key,
    token_hash   text not null unique,
    label        text not null,
    daily_quota  integer not null default 100,
    active       boolean not null default true,
    created_at   timestamptz not null default now(),
    last_used_at timestamptz,
    expires_at   timestamptz
);

create index if not exists api_keys_hash_idx on api_keys (token_hash);

-- Consumo por clave y dia. La clave primaria compuesta hace que el incremento
-- sea un UPSERT atomico: no hay condicion de carrera entre peticiones.
create table if not exists usage_daily (
    api_key_id bigint  not null references api_keys (id) on delete cascade,
    day        date    not null,
    requests   integer not null default 0,
    primary key (api_key_id, day)
);

-- Tope global del dia. Es la red de seguridad: aunque la autenticacion falle
-- o alguien abuse, el gasto tiene un techo.
create table if not exists usage_global (
    day      date primary key,
    requests integer not null default 0
);
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

// --------------------------------------------------------------------------- //
// Claves de tester y cuotas
// --------------------------------------------------------------------------- //

// FindAPIKey busca una clave ACTIVA por el hash de su token.
// Devuelve (nil, nil) si no existe, esta desactivada o ha caducado.
func (s *Store) FindAPIKey(ctx context.Context, hash string) (*APIKey, error) {
	c, cancel := s.ctx(ctx)
	defer cancel()

	var (
		key       APIKey
		createdAt time.Time
	)
	err := s.pool.QueryRow(c,
		`select id, label, daily_quota, active, created_at
		 from api_keys
		 where token_hash = $1
		   and active
		   and (expires_at is null or expires_at > now())`,
		hash,
	).Scan(&key.ID, &key.Label, &key.DailyQuota, &key.Active, &createdAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	key.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	return &key, nil
}

// RegisterKeyUsage incrementa el contador del dia para una clave y el global.
// Todo en una sola sentencia: el UPSERT con clave primaria compuesta es atomico,
// asi que no hay condicion de carrera entre peticiones simultaneas.
func (s *Store) RegisterKeyUsage(ctx context.Context, keyID int64) (usedToday, globalToday int, err error) {
	c, cancel := s.ctx(ctx)
	defer cancel()

	err = s.pool.QueryRow(c,
		`with d as (
		     insert into usage_daily (api_key_id, day, requests)
		     values ($1, current_date, 1)
		     on conflict (api_key_id, day)
		     do update set requests = usage_daily.requests + 1
		     returning requests
		 ), g as (
		     insert into usage_global (day, requests)
		     values (current_date, 1)
		     on conflict (day)
		     do update set requests = usage_global.requests + 1
		     returning requests
		 ), t as (
		     -- last_used_at solo se toca cada 5 min: es un dato informativo y
		     -- asi no pagamos una escritura en cada peticion.
		     update api_keys set last_used_at = now()
		     where id = $1
		       and (last_used_at is null or last_used_at < now() - interval '5 minutes')
		     returning id
		 )
		 select (select requests from d), (select requests from g)`,
		keyID,
	).Scan(&usedToday, &globalToday)
	return usedToday, globalToday, err
}

// RegisterServiceUsage cuenta la peticion solo en el contador global.
// La clave de servicio no tiene cuota propia.
func (s *Store) RegisterServiceUsage(ctx context.Context) (globalToday int, err error) {
	c, cancel := s.ctx(ctx)
	defer cancel()

	err = s.pool.QueryRow(c,
		`insert into usage_global (day, requests)
		 values (current_date, 1)
		 on conflict (day)
		 do update set requests = usage_global.requests + 1
		 returning requests`,
	).Scan(&globalToday)
	return globalToday, err
}

// CreateAPIKey guarda una clave nueva y devuelve su id.
func (s *Store) CreateAPIKey(ctx context.Context, hash, label string, quota int,
	expiresAt *time.Time) (int64, error) {
	c, cancel := s.ctx(ctx)
	defer cancel()

	var id int64
	err := s.pool.QueryRow(c,
		`insert into api_keys (token_hash, label, daily_quota, expires_at)
		 values ($1, $2, $3, $4)
		 returning id`,
		hash, label, quota, expiresAt,
	).Scan(&id)
	return id, err
}

// ListAPIKeys devuelve todas las claves con su consumo de hoy.
func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	c, cancel := s.ctx(ctx)
	defer cancel()

	rows, err := s.pool.Query(c,
		`select k.id, k.label, k.daily_quota, k.active, k.created_at,
		        k.last_used_at, k.expires_at, coalesce(u.requests, 0)
		 from api_keys k
		 left join usage_daily u
		        on u.api_key_id = k.id and u.day = current_date
		 order by k.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]APIKey, 0, 8)
	for rows.Next() {
		var (
			key       APIKey
			createdAt time.Time
			lastUsed  *time.Time
			expiresAt *time.Time
		)
		if err := rows.Scan(&key.ID, &key.Label, &key.DailyQuota, &key.Active,
			&createdAt, &lastUsed, &expiresAt, &key.UsedToday); err != nil {
			return nil, err
		}
		key.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		if lastUsed != nil {
			s := lastUsed.UTC().Format(time.RFC3339)
			key.LastUsedAt = &s
		}
		if expiresAt != nil {
			s := expiresAt.UTC().Format(time.RFC3339)
			key.ExpiresAt = &s
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

// DeactivateAPIKey desactiva una clave (borrado logico: se conserva el historico).
func (s *Store) DeactivateAPIKey(ctx context.Context, id int64) (bool, error) {
	c, cancel := s.ctx(ctx)
	defer cancel()

	tag, err := s.pool.Exec(c,
		"update api_keys set active = false where id = $1 and active", id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// GlobalUsageToday devuelve el consumo global de hoy.
func (s *Store) GlobalUsageToday(ctx context.Context) (int, error) {
	c, cancel := s.ctx(ctx)
	defer cancel()

	var total int
	err := s.pool.QueryRow(c,
		"select coalesce((select requests from usage_global where day = current_date), 0)",
	).Scan(&total)
	return total, err
}
