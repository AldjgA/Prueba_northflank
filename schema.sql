-- Esquema del mini-backend. Idempotente: se puede ejecutar varias veces.
-- Se aplica automaticamente al arrancar si AUTO_MIGRATE=true (por defecto).

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

-- Opcional: limpieza automatica de conversaciones viejas.
-- Supabase no trae pg_cron por defecto; si lo activas, esto basta:
--
-- select cron.schedule(
--     'purge-old-conversations', '0 4 * * *',
--     $$delete from conversations where created_at < now() - interval '30 days'$$
-- );
