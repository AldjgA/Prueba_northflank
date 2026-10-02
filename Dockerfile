# syntax=docker/dockerfile:1.7
# ---------------------------------------------------------------------------
# Stage 1: compilacion. CGO_ENABLED=0 -> binario estatico, sin libc.
# ---------------------------------------------------------------------------
FROM golang:1.27-alpine AS builder

WORKDIR /src

# Primero solo los manifiestos: aprovecha la cache de capas de Docker.
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

# -s -w quitan tabla de simbolos y debug info (~30% menos de binario).
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/server .

# ---------------------------------------------------------------------------
# Stage 2: imagen final. Solo el binario: sin shell, sin package manager.
# ---------------------------------------------------------------------------
FROM scratch

# Certificados TLS: imprescindibles para hablar con la API de Gemini y con
# Supabase por HTTPS/TLS.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Usuario no root (scratch no trae /etc/passwd, se declara por UID).
USER 10001:10001

COPY --from=builder /out/server /server

ENV PORT=8080 \
    ADDR=:8080

EXPOSE 8080

# scratch no tiene shell ni curl: el propio binario hace de healthcheck.
HEALTHCHECK --interval=30s --timeout=3s --start-period=3s --retries=3 \
  CMD ["/server", "-healthcheck"]

ENTRYPOINT ["/server"]
