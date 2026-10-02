# Revisión y recomendaciones — `Prueba_northflank`

**Antes:** bot de Telegram (`aiogram`) que recibe webhooks y responde llamando a Gemini.
**Ahora:** mini-backend HTTP (FastAPI) que expone el chatbot directamente, con persistencia opcional en Postgres.

> Los números de RAM de la sección 3 son **estimaciones** basadas en el peso de las librerías importadas, no mediciones del contenedor en Northflank. Mídelas en tu plan real antes de redimensionar.

---

## 1. Bugs y problemas del código original

| # | Problema | Impacto |
|---|----------|---------|
| 1 | `MODEL_ID = "gemma-4-31b-it"` **no existe** en la API de Gemini | Todas las llamadas fallaban. IDs válidos: `gemini-3.5-flash-lite`, `gemini-3.8-flash`… |
| 2 | `except Exception as e: ...{str(e)}` | Filtra el error interno al usuario final. |
| 3 | Sin `timeout` en la llamada a Gemini | Una petición colgada retiene memoria y worker indefinidamente. |
| 4 | Sin validación de entrada | Cualquiera manda 5 MB de texto → coste y RAM disparados. |
| 5 | Cliente Gemini sin `lifespan` ni `aclose()` | No se cierra el pool de conexiones; fugas de sockets. |
| 6 | Sin reintentos | Un 429/503 transitorio se convertía en error visible. |
| 7 | Sin logs | Imposible diagnosticar en producción. |
| 8 | `aiogram` completo para parsear un `Update` | Decenas de dependencias innecesarias y RAM extra. |
| 9 | Sin autenticación | Telegram actuaba de control de acceso; al quitar Telegram el endpoint queda público. |
| 10 | Sin rate limit | Un cliente agota tu cuota de Gemini en segundos. |

---

## 2. Qué cambió en el refactor

- **Fuera Telegram:** se eliminan `aiogram` y `BackgroundTasks`.
- **Los mismos secretos, todos reutilizados:**
  - `clave1` → antes token del bot, **ahora token de autenticación del mini-backend**.
  - `clave2` → API key de Gemini, sin cambios.
  - `clave3` → **nuevo**, DSN de Postgres/Supabase. Si no lo defines, el backend funciona igual pero sin guardar nada.
- **Endpoints:**

| Método | Ruta | Qué hace |
|--------|------|----------|
| GET | `/health` | Liveness. No toca Gemini ni la DB. |
| GET | `/ready` | Readiness: valida config y ping a la DB. |
| POST | `/chat` | Chat. Persiste si hay `clave3`. |
| POST | `/chat/stream` | Chat en streaming SSE. |
| GET | `/chat/{id}/history` | Historial guardado. |
| DELETE | `/chat/{id}` | Borra una conversación (cascade). |

- **Un solo cliente de Gemini** creado en `lifespan` y reutilizado → pool de conexiones HTTP compartido.
- **Un solo pool de Postgres** acotado (`DB_POOL_MIN`/`DB_POOL_MAX`).

---

## 3. Cómo bajar el consumo de RAM

1. **Un solo worker de Uvicorn.** Cada worker es un proceso Python completo. Con `--workers 1` + `asyncio` + semáforo cubres cientos de peticiones concurrentes. **Es el ahorro más grande.**
2. **Eliminar `aiogram`.** La dependencia más pesada y no aporta nada en un backend HTTP. Estimación: 30–50 MB.
3. **Imagen multi-stage.** Las herramientas de compilación y los `.whl` no llegan a la imagen final.
4. **`python:3.12-slim`, no `alpine`.** Alpine usa musl y a menudo obliga a compilar desde fuente. `slim` es el punto medio correcto.
5. **`PYTHONDONTWRITEBYTECODE=1`.** No genera `.pyc` dentro del contenedor.
6. **Semáforo de concurrencia (`MAX_CONCURRENCY`).** Cada llamada a Gemini en vuelo consume memoria para el buffer. El semáforo pone un techo duro: si llegan 200 peticiones, solo 8 se procesan a la vez.
7. **`--limit-concurrency 64` en Uvicorn.** Corta conexiones excesivas antes de materializarlas en memoria.
8. **`MAX_OUTPUT_TOKENS` acotado (1024).** Respuestas cortas = menos buffer = menos RAM y menos latencia.
9. **`MAX_HISTORY_TURNS` (20).** El historial es lo que más crece por petición.
10. **`DB_POOL_MAX=5`.** El pooler de Supabase ya multiplexa; un pool grande solo gasta RAM.
11. **`DB_IDLE_LIFETIME=30`.** Cierra conexiones inactivas rápido → el servidor Postgres libera esos backends.
12. **No añadas `orjson`/`ujson`.** FastAPI 0.142 ya serializa a bytes con Pydantic. Añadirlos suma dependencia sin ganancia.
13. **NO uses `uvloop` si priorizas RAM.** Da más throughput pero suma ~10–15 MB de RSS. Está comentado en `requirements.txt`.
14. **Limita la memoria en Northflank** (256 MiB con `MAX_CONCURRENCY=8` sobra) para que el techo sea explícito.

---

## 4. Cómo mejorar el rendimiento

1. **Streaming SSE (`/chat/stream`).** El *time-to-first-token* baja de ~2 s a ~300 ms y no hay que bufferizar la respuesta completa.
2. **Reutilizar el cliente de Gemini.** Un `genai.Client` por petición implica un handshake TLS nuevo cada vez.
3. **Reintentos con backoff exponencial** solo ante errores transitorios (429/5xx), nunca ante errores de validación.
4. **Modelo adecuado.** `gemini-3.5-flash-lite` es el más rápido y barato para chat. Está en `MODEL_ID`, así que lo cambias sin recompilar.
5. **`GZipMiddleware`** comprime respuestas > 512 B.
6. **`--timeout-keep-alive 5`** libera conexiones idle rápido en servicios de bajo tráfico.
7. **`--timeout-graceful-shutdown 10`** para que el rolling deploy no corte peticiones en vuelo.
8. **`/health` sin dependencias externas.** El orquestador puede sondear cada 30 s sin coste.
9. **`X-Accel-Buffering: no`** evita que un proxy acumule el stream.
10. **Pool de Postgres caliente** (`DB_POOL_MIN=1`): la primera consulta no paga el handshake TLS con Supabase.

---

## 5. Persistencia con `clave3` (Supabase)

### El detalle que rompe el 90% de las integraciones

Tu DSN apunta al **puerto 6543**, que es el *transaction pooler* de Supabase (PgBouncer en modo transacción). En ese modo **no hay prepared statements persistentes**: la conexión del servidor puede cambiar entre consultas.

Síntomas si no lo tienes en cuenta:
```
prepared statement "__asyncpg_stmt_1__" does not exist
```

La solución está implementada en `db.py`:
- **`statement_cache_size=0`** (por defecto). Se fuerza a 0 automáticamente si detecta el puerto 6543, aunque lo subas por env.
- Si algún día usas el **puerto 5432** (session pooler o conexión directa), puedes subir `DB_STATEMENT_CACHE` para ganar rendimiento.

### Otros detalles del DSN

- **`?sslmode=require` y `?pgbouncer=true`** son sintaxis de libpq; **asyncpg los rechaza** con `unexpected keyword argument`. `db.normalize_dsn()` los limpia y traduce `sslmode` al parámetro `ssl` de asyncpg. (Verificado con tests.)
- **El pooler es la ruta correcta** si tu contenedor es solo IPv4: la conexión directa de Supabase ya es IPv6-only.
- **Usa la contraseña real en el secreto de Northflank, nunca en el repo.** Si el valor que pegaste en el chat era el real, **rótalo** en el panel de Supabase.

### Esquema

`conversations` (id, created_at) y `messages` (id, conversation_id, role, content, model, latency_ms, created_at) con FK `on delete cascade` e índice `(conversation_id, created_at)`. Se aplica solo al arrancar si `AUTO_MIGRATE=true` (idempotente). También está suelto en `schema.sql`.

### Comportamiento si la DB cae

El backend **no se cae**. `db.init_pool()` reintenta 3 veces con backoff; si no hay conexión, arranca en modo sin persistencia y lo registra en el log. `/ready` sí devuelve 503 para que Northflank no te mande tráfico a una instancia degradada.

### Latencia añadida

Dos `INSERT` por petición (~2–5 ms cada uno contra el pooler). Si te molesta, hay una vía rápida: envolver los `_persist()` en `asyncio.create_task()` para que no bloqueen la respuesta. No lo dejé así por defecto porque pierdes visibilidad de los errores de escritura.

---

## 6. Seguridad (crítico al quitar Telegram)

Telegram actuaba como capa de autenticación. Al pasar a HTTP, **el endpoint queda expuesto a internet**. Por eso:

- **`clave1` ahora autentica** (`X-API-Key` o `Authorization: Bearer`), comparada con `secrets.compare_digest` (tiempo constante, resistente a timing attacks).
- **Rate limit por IP** para proteger tu cuota de Gemini.
- **Los errores internos no se devuelven al cliente**; se registran en el servidor.
- **`max_length` en todos los campos** + validación de UUID en el path.
- **Usuario no-root** en el contenedor.
- **`.dockerignore`** evita meter `.env` y `.git` en la imagen.

> Si expones el mini-backend a un frontend público, **no pongas `clave1` en el navegador**. Monta un proxy o usa tokens por usuario.

---

## 7. ¿Merece la pena reescribirlo en Go o Rust?

**Respuesta corta: para un chatbot personal, no.** Con 256 MiB en Northflank, Python va sobrado. La reescritura solo compensa si (a) tienes un tope duro de ~128 MiB o menos, (b) necesitas *scale-to-zero* con arranques de milisegundos, o (c) quieres aprender el lenguaje.

Contexto: el arranque en frío de Python + FastAPI es ~0.7–1.2 s. Eso hace que el scale-to-zero duela. Go y Rust arrancan en milisegundos, y ahí sí hay una diferencia funcional, no solo de RAM.

### Go — viabilidad ALTA

| Aspecto | Detalle |
|---------|---------|
| HTTP | `net/http` de la stdlib (Go 1.22+ ya trae routing con patrones). Cero dependencias. |
| Gemini | **SDK oficial**: `google.golang.org/genai`, con `GenerateContent` y `GenerateContentStream`. |
| Postgres | `github.com/jackc/pgx/v5` + `pgxpool`. El mejor driver Postgres del ecosistema. |
| PgBouncer | Resuelto con `QueryExecModeExec` (evita prepared statements), o `default_query_exec_mode=exec` en el DSN. |
| Auth / rate limit | `crypto/subtle` y `golang.org/x/time/rate`. |
| Binario | ~12–18 MB estático. Imagen con distroless: ~20–25 MB. |
| RSS en reposo | ~15–25 MB (estimación). |
| Arranque | ~5–10 ms. |
| Esfuerzo | ~1–2 días para este alcance; ~300 líneas. |

**Ventaja clave:** mantienes SDKs oficiales para todo y el código es de tamaño comparable al de Python.

### Rust — viabilidad MEDIA-ALTA

| Aspecto | Detalle |
|---------|---------|
| HTTP | `axum` (sobre tokio) o `actix-web`. |
| Gemini | **No hay SDK oficial de Rust.** Llamas a la REST API con `reqwest` + `serde` y **mantienes tú los modelos** de request/response. El streaming SSE hay que parsearlo a mano (`reqwest-eventsource` ayuda). |
| Postgres | `sqlx` (consultas verificadas en compilación, pero necesita DB viva o caché offline) o `tokio-postgres` + `deadpool-postgres`. |
| PgBouncer | Igual que en Go: hay que desactivar prepared statements explícitamente. |
| Binario | ~4–8 MB con musl. Imagen con `scratch`: ~10 MB. |
| RSS en reposo | ~5–12 MB (estimación). |
| Arranque | ~1–3 ms. |
| Esfuerzo | ~3–5 días si ya dominas Rust; bastante más si no (lifetimes en async, `Pin`, bounds de `Send`). |

**Fricción principal:** la ausencia de SDK oficial de Gemini. Todo lo que Google añada o cambie en la API lo adaptas tú.

### Veredicto

| | Python (actual) | Go | Rust |
|---|---|---|---|
| RAM en reposo | ~80–120 MB (est.) | ~15–25 MB | ~5–12 MB |
| Arranque | ~0.7–1.2 s | ~5–10 ms | ~1–3 ms |
| SDK oficial Gemini | Sí | **Sí** | **No** |
| Esfuerzo de migración | — | ~1–2 días | ~3–5 días |
| Mantenibilidad para ti | Alta | Alta | Media |

**Si vas a reescribir, Go es el punto dulce:** obtienes ~75% del ahorro de Rust con la mitad del esfuerzo, SDKs oficiales para Gemini y Postgres, y `pgx` maneja el pooler de Supabase sin sorpresas. Rust solo gana si la RAM se mide en decenas de MB o si quieres Rust en el CV.

> **Implementado:** la versión en Go está en `backend-go/`. Ver la sección 12 para el detalle, la configuración de concurrencia y las diferencias respecto a esta versión de Python.

**Ruta recomendada:** quédate con este Python, despliégalo, mide el RSS real durante una semana con `MAX_CONCURRENCY=8`. Si nunca pasa de 100 MB, no toques nada. Si te topas con el límite, migra a Go.

---

## 8. Ejemplos de uso

```bash
BASE=https://<tu-servicio>.northflank.app

# Chat nuevo (crea conversación si hay persistencia)
curl -s -X POST $BASE/chat \
  -H "Content-Type: application/json" -H "X-API-Key: $clave1" \
  -d '{"message": "Hola, ¿quién eres?"}'

# Continuar la conversación (usa el conversation_id que devolvió)
curl -s -X POST $BASE/chat \
  -H "Content-Type: application/json" -H "X-API-Key: $clave1" \
  -d '{"message": "¿Y en Madrid?", "conversation_id": "<uuid>"}'

# Streaming
curl -N -X POST $BASE/chat/stream \
  -H "Content-Type: application/json" -H "X-API-Key: $clave1" \
  -d '{"message": "Cuéntame un chiste corto"}'

# Historial y borrado
curl -s $BASE/chat/<uuid>/history -H "X-API-Key: $clave1"
curl -s -X DELETE $BASE/chat/<uuid> -H "X-API-Key: $clave1"
```

---

## 9. Despliegue en Northflank

1. Sube `main.py`, `db.py`, `requirements.txt`, `Dockerfile`, `.dockerignore` a la rama que despliega Northflank. (`schema.sql`, `test_*.py` y `requirements-dev.txt` son opcionales.)
2. **Secretos:** mantén `clave1` y `clave2`; añade `clave3` con el DSN completo (con la contraseña real).
3. Puerto `8080`. Healthcheck: `/health`. Readiness: `/ready`.
4. Memoria: **256 MiB** de límite con `MAX_CONCURRENCY=8`.
5. Escala horizontalmente antes que subir workers.

---

## 10. Verificación realizada

Todo lo siguiente se ejecutó y pasa:

- `test_smoke.py` — **30 comprobaciones**: endpoints, auth (401), contrato de `/chat`, validación de entrada (422), streaming SSE, degradación sin persistencia (409) y normalización del DSN de Supabase.
- `test_db_layer.py` — repositorio con pool falso + **las 7 consultas SQL parseadas con el parser real de PostgreSQL** (`pglast`).
- `schema.sql` — validado con el parser real de PostgreSQL.
- Superficie del SDK verificada contra `google-genai 2.27.0`: `aio.aclose()`, `HttpOptions(timeout=)`, `generate_content`, `generate_content_stream`.

---

## 11. Siguientes pasos opcionales

- **Persistir sin bloquear:** mover `_persist()` a `asyncio.create_task()` si los ~5 ms te importan.
- **Cache de respuestas:** un LRU en memoria para prompts idénticos repetidos.
- **Observabilidad:** `prometheus-fastapi-instrumentator` para métricas de latencia.
- **Limpieza automática:** `pg_cron` en Supabase para purgar conversaciones viejas (plantilla comentada en `schema.sql`).
- **Tests en CI:** convertir los `test_*.py` a `pytest` (ya está en `requirements-dev.txt`).

---

## 12. Versión en Go (`backend-go/`)

Reescritura completa en Go 1.27 del mismo mini-backend. Mismos secretos, mismos endpoints, misma lógica.

### Estructura

| Archivo | Contenido |
|---------|-----------|
| `main.go` | Arranque, rutas, apagado ordenado, modo `-healthcheck`. |
| `config.go` | Toda la configuración desde el entorno. |
| `gemini.go` | Cliente del SDK oficial + interfaz `Generator` (testeable). |
| `store.go` | Pool `pgx` y repositorio. |
| `handlers.go` | Los 6 endpoints, validación, SSE. |
| `middleware.go` | Auth con `crypto/subtle` + rate limit por IP. |
| `limiter.go` | **Limitador de concurrencia** (ver abajo). |
| `main_test.go` | 30 casos de test. |

### Concurrencia: cómo se limita a 2–3 usuarios

Aquí está la distinción importante, porque son dos cosas distintas:

- **Conexiones HTTP concurrentes:** Go atiende miles sin problema (cada goroutine son ~4 KB). Esto **no** lo limita nadie.
- **Peticiones a Gemini en vuelo:** esto es lo que se acota con `MAX_CONCURRENCY`. Cada llamada consume cuota y memoria para el buffer de la respuesta.

Con `MAX_CONCURRENCY=3`, el cuarto usuario **no es rechazado**: espera en cola hasta `QUEUE_TIMEOUT_S` (30 s por defecto). Si el hueco se libera antes, entra. Si expira, recibe un `503` con `Retry-After` en lugar de quedarse colgado.

```
MAX_CONCURRENCY=3     # peticiones simultaneas a Gemini
QUEUE_TIMEOUT_S=30    # cuanto espera la 4ª antes de recibir 503
```

El semáforo es un `chan struct{}` con buffer. `/health` expone `inflight` y `capacity` para que veas la ocupación en vivo:

```json
{"status":"ok","inflight":2,"capacity":3}
```

> Puedes subirlo sin miedo: con `MAX_CONCURRENCY=50` Go ni se despeina. El límite es una decisión de política (proteger cuota y latencia), no una limitación técnica.

### Diferencias respecto a la versión de Python

| | Python | Go |
|---|---|---|
| RSS en reposo | ~80–120 MB (est.) | **18.4 MB (medido)** |
| Arranque | ~0.7–1.2 s | ~5–10 ms |
| Binario / imagen | imagen ~130 MB | binario 30.8 MB → imagen ~35 MB |
| Dependencias | 5 paquetes | 3 directas (`genai`, `pgx`, stdlib) |
| PgBouncer | `statement_cache_size=0` | `QueryExecModeExec` |
| Validación de entrada | Pydantic | manual + `regexp` |
| Logs | texto | JSON estructurado (`log/slog`) |

### Dos mejoras que no estaban en la versión de Python

1. **SSE con saltos de línea.** La versión de Python emitía `data: <texto>`, y si el fragmento traía un `\n` (muy común en markdown), **rompía el protocolo SSE**. En Go, `writeSSE()` emite una línea `data:` por cada línea del fragmento. Hay un test que lo cubre.
2. **Persistencia desligada del cliente.** Si el cliente se desconecta a mitad de la generación, se sigue guardando lo ya generado (`context.WithoutCancel`).

### Nota honesta sobre el tamaño

El SDK oficial `google.golang.org/genai` arrastra `grpc` y `cloud.google.com/go` (los necesita para el backend de Vertex AI). Por eso el binario es de **30.8 MB** en Windows sin optimizar, no los 12–18 MB que estimé en la sección 7. Con `-ldflags="-s -w"` y `GOOS=linux` baja a ~22–24 MB, y la imagen final (scratch) ronda los 35 MB. El **RSS sí cumple**: 18.4 MB medidos.

### Despliegue

1. Copia `backend-go/` a tu repo. El `Dockerfile` compila con `CGO_ENABLED=0` y la imagen final es `scratch` (sin shell, sin package manager, usuario no root).
2. **Secretos:** los mismos tres (`clave1`, `clave2`, `clave3`).
3. Puerto `8080`. Healthcheck: el propio binario con `/server -healthcheck` (scratch no tiene `curl`).
4. Memoria: **64 MiB** de límite sobran. Es un 75% menos que los 256 MiB que necesita Python.

### Verificación realizada

- `go vet ./...` — limpio.
- `go test ./...` — **30 casos, todos OK**, incluyendo:
  - el limitador respeta el máximo configurado (mide el pico real de concurrencia),
  - la petición en cola entra cuando se libera un hueco,
  - se devuelve `ErrQueueTimeout` si la cola se agota,
  - se respeta la cancelación del contexto,
  - SSE correcto con fragmentos multilínea,
  - el error interno no se filtra al cliente.
- Arranque real medido: `/health` → `{"capacity":3,"inflight":0}`, `/ready` → `database=disabled`, RSS 18.4 MB.
