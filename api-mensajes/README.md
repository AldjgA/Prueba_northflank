# api-mensajes

API HTTP en Go con **datos mock en memoria** (sin base de datos, sin servicios
externos). Vive en `api-mensajes/` como modulo independiente, asi que no toca la
API que ya existe en la raiz del repositorio.

- Go 1.24+ (solo biblioteca estandar, cero dependencias)
- Escucha en `PORT` (por defecto `8080`)

## Endpoints

| Metodo | Ruta | Que hace |
| --- | --- | --- |
| `POST` | `/enviar_mensaje` | Crea un mensaje y lo guarda |
| `GET` | `/recibir_mensaje?para=<usuario>` | Devuelve los mensajes de ese destinatario y los marca como leidos |
| `GET` | `/descargar_archivo?id=<id>` o `?nombre=<archivo>` | Descarga el archivo como adjunto |
| `GET` | `/archivos` | Lista los archivos disponibles (util para saber que descargar) |
| `GET` | `/health` | Estado del servicio (lo usa Northflank / Docker) |
| `GET` | `/` | Indice con la descripcion de la API |

### Enviar un mensaje

```bash
curl -X POST http://localhost:8080/enviar_mensaje \
  -H "Content-Type: application/json" \
  -d '{"de":"ana","para":"luis","texto":"hola, esto es una prueba"}'
```

```json
{
  "id": 4,
  "de": "ana",
  "para": "luis",
  "texto": "hola, esto es una prueba",
  "enviado_en": "2026-10-23T04:10:00Z",
  "estado": "entregado",
  "leido": false
}
```

### Recibir mensajes

```bash
curl "http://localhost:8080/recibir_mensaje?para=luis"
curl "http://localhost:8080/recibir_mensaje?para=luis&desde_id=3"   # solo los nuevos
```

`desde_id` es opcional: devuelve unicamente mensajes con `id` mayor a ese valor
(sondeo incremental). Los mensajes devueltos pasan a `leido: true`.

### Descargar un archivo

```bash
curl -OJ "http://localhost:8080/descargar_archivo?nombre=datos.csv"
curl -OJ "http://localhost:8080/descargar_archivo?id=arch-2"
```

Archivos mock incluidos: `nota.txt` (`arch-1`), `reporte.json` (`arch-2`),
`datos.csv` (`arch-3`).

## Datos mock

Al arrancar se cargan 3 mensajes (para `luis` y `ana`) y 3 archivos. Todo vive en
memoria: **se reinicia con el proceso**. La logica esta en `store.go`, lista para
cambiarse por Postgres despues.

## Errores

Formato unico:

```json
{ "error": { "codigo": "falta_para", "mensaje": "indica el destinatario con ?para=<usuario>" } }
```

| Codigo | HTTP | Cuando |
| --- | --- | --- |
| `json_invalido` | 400 | El cuerpo no es JSON o trae campos desconocidos |
| `datos_invalidos` | 400 | Falta `de`, `para` o `texto`, o el texto supera 4000 caracteres |
| `falta_para` | 400 | `recibir_mensaje` sin `?para=` |
| `desde_id_invalido` | 400 | `desde_id` no es entero >= 0 |
| `falta_archivo` | 400 | `descargar_archivo` sin `?id=` ni `?nombre=` |
| `archivo_no_encontrado` | 404 | No existe ese archivo |
| `ruta_no_encontrada` | 404 | Ruta o metodo no registrado |

## Local

```bash
cd api-mensajes
go run .
go test ./...
```

## Northflank

Servicio tipo **Build from repo** con:

- Build type: `Dockerfile`
- Build context: `/api-mensajes`
- Dockerfile: `/api-mensajes/Dockerfile`
- Puerto: `8080` (HTTP)
- Health check: `/health`
