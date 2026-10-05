package main

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// --------------------------------------------------------------------------- //
// Autenticacion + rate limit
// --------------------------------------------------------------------------- //

// authMiddleware valida `clave1` (cabecera X-API-Key o Authorization: Bearer)
// y aplica el rate limit por IP.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthEnabled() {
			provided := r.Header.Get("X-API-Key")
			if provided == "" {
				if auth := r.Header.Get("Authorization"); len(auth) > 7 &&
					strings.EqualFold(auth[:7], "bearer ") {
					provided = strings.TrimSpace(auth[7:])
				}
			}
			// ConstantTimeCompare: resistente a ataques de temporizacion.
			if subtle.ConstantTimeCompare([]byte(provided), []byte(s.cfg.APIToken)) != 1 {
				writeError(w, http.StatusUnauthorized, "Credenciales invalidas.", "")
				return
			}
		}

		if !s.rate.Allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "Demasiadas peticiones.", "")
			return
		}

		next(w, r)
	}
}

// clientIP extrae la IP del cliente, respetando X-Forwarded-For si el servicio
// corre detras del proxy de Northflank.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first, _, found := strings.Cut(fwd, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(fwd)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// --------------------------------------------------------------------------- //
// Rate limiter en memoria (ventana fija de 60 s, sin dependencias)
// --------------------------------------------------------------------------- //

type rateWindow struct {
	count int
	start time.Time
}

type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string]*rateWindow
	limit  int
	window time.Duration
}

func newRateLimiter(limit int) *rateLimiter {
	return &rateLimiter{
		hits:   make(map[string]*rateWindow),
		limit:  limit,
		window: time.Minute,
	}
}

// Allow indica si la IP puede pasar. Sin limite si limit<=0.
func (rl *rateLimiter) Allow(ip string) bool {
	if rl.limit <= 0 {
		return true
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	w, ok := rl.hits[ip]
	if !ok || now.Sub(w.start) >= rl.window {
		rl.hits[ip] = &rateWindow{count: 1, start: now}
		rl.maybeSweep(now)
		return true
	}

	w.count++
	return w.count <= rl.limit
}

// maybeSweep limpia ventanas caducadas para que el mapa no crezca sin limite.
// Se llama solo cuando se inserta una IP nueva, asi que el coste es bajo.
func (rl *rateLimiter) maybeSweep(now time.Time) {
	if len(rl.hits) < 10_000 {
		return
	}
	for ip, w := range rl.hits {
		if now.Sub(w.start) >= rl.window {
			delete(rl.hits, ip)
		}
	}
}
