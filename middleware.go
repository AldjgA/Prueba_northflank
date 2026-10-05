package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --------------------------------------------------------------------------- //
// Identidad de la peticion
// --------------------------------------------------------------------------- //

type ctxKey string

const identityCtxKey ctxKey = "identidad"

// IdentityFrom recupera quien hizo la peticion. Nunca es nil en un handler
// protegido: el middleware garantiza que existe.
func IdentityFrom(ctx context.Context) *Identity {
	if ident, ok := ctx.Value(identityCtxKey).(*Identity); ok && ident != nil {
		return ident
	}
	return &Identity{Kind: KindNone}
}

// authError lleva el codigo HTTP y el mensaje que se devuelve al cliente.
type authError struct {
	code       int
	message    string
	retryAfter string
}

func (e *authError) Error() string { return e.message }

// --------------------------------------------------------------------------- //
// Middleware de autenticacion
// --------------------------------------------------------------------------- //

// authMiddleware valida la credencial y aplica las cuotas.
//
// Acepta DOS tipos de credencial:
//   - `clave1`, la clave de servicio: tus scripts y llamadas entre servidores.
//     No tiene cuota propia, solo cuenta para el tope global.
//   - Un token de tester (`mb_...`) de la tabla `api_keys`, con su cuota diaria.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// El rate limit por IP va primero: es barato y corta el abuso antes
		// de tocar la base de datos.
		if !s.rate.Allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "Demasiadas peticiones.", "")
			return
		}

		ident, err := s.authenticate(r.Context(), r)
		if err != nil {
			var ae *authError
			if errors.As(err, &ae) {
				if ae.retryAfter != "" {
					w.Header().Set("Retry-After", ae.retryAfter)
				}
				writeError(w, ae.code, ae.message, "")
				return
			}
			s.log.Error("error autenticando la peticion", "error", err)
			writeError(w, http.StatusInternalServerError, "Error interno.", "")
			return
		}

		// Cabeceras informativas de cuota (convencion tipo GitHub).
		setQuotaHeaders(w, ident, s.cfg.GlobalDailyCap)

		next(w, r.WithContext(context.WithValue(r.Context(), identityCtxKey, ident)))
	}
}

// adminMiddleware protege los endpoints de administracion.
//
// A diferencia del resto, aqui NO existe modo abierto: si no hay `clave1`
// configurada, la administracion queda deshabilitada. Es deliberado, porque
// crear claves no puede estar nunca al alcance de cualquiera.
func (s *Server) adminMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.AuthEnabled() {
			writeError(w, http.StatusServiceUnavailable,
				"Administracion deshabilitada: define clave1 para poder usarla.", "")
			return
		}
		if s.store == nil {
			writeError(w, http.StatusServiceUnavailable,
				"Administracion deshabilitada: hace falta clave3 (base de datos).", "")
			return
		}
		if subtle.ConstantTimeCompare([]byte(extractToken(r)), []byte(s.cfg.APIToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "Credenciales invalidas.", "")
			return
		}
		next(w, r)
	}
}

// authenticate resuelve la identidad de la peticion.
func (s *Server) authenticate(ctx context.Context, r *http.Request) (*Identity, error) {
	token := extractToken(r)

	// --- 1. Clave de servicio (`clave1`) ---------------------------------
	if s.cfg.AuthEnabled() {
		if token == "" {
			return nil, &authError{http.StatusUnauthorized, "Falta la credencial.", ""}
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.APIToken)) == 1 {
			ident := &Identity{Kind: KindService, Label: "service"}
			if s.store != nil {
				if global, err := s.store.RegisterServiceUsage(ctx); err == nil {
					ident.GlobalToday = global
				} else {
					s.log.Warn("no se pudo contar el uso del servicio", "error", err)
				}
			}
			return s.checkGlobalCap(ident)
		}
	}

	// --- 2. Token de tester (tabla api_keys) -----------------------------
	if token != "" && s.store != nil {
		key, err := s.store.FindAPIKey(ctx, HashToken(token))
		if err != nil {
			return nil, err
		}
		if key != nil {
			used, global, err := s.store.RegisterKeyUsage(ctx, key.ID)
			if err != nil {
				return nil, err
			}
			ident := &Identity{
				Kind:        KindKey,
				KeyID:       key.ID,
				Label:       key.Label,
				DailyQuota:  key.DailyQuota,
				UsedToday:   used,
				GlobalToday: global,
			}
			return s.checkQuotas(ident)
		}
	}

	// --- 3. Modo abierto --------------------------------------------------
	// Solo si NO hay `clave1` configurada y NO se envio credencial alguna.
	// Si alguien manda un token invalido, se le dice: mejor que se entere.
	if !s.cfg.AuthEnabled() && token == "" {
		return &Identity{Kind: KindNone, Label: "open"}, nil
	}

	return nil, &authError{http.StatusUnauthorized, "Credenciales invalidas.", ""}
}

// checkQuotas aplica la cuota del tester y, despues, el tope global.
func (s *Server) checkQuotas(ident *Identity) (*Identity, error) {
	if _, err := s.checkGlobalCap(ident); err != nil {
		return nil, err
	}
	if ident.DailyQuota > 0 && ident.UsedToday > ident.DailyQuota {
		return nil, &authError{
			code: http.StatusTooManyRequests,
			message: fmt.Sprintf("Cuota diaria agotada (%d peticiones al dia). "+
				"Se reinicia a medianoche UTC.", ident.DailyQuota),
			retryAfter: "3600",
		}
	}
	return ident, nil
}

// checkGlobalCap aplica el techo diario de todo el servicio. Es la red de
// seguridad: aunque la autenticacion fallase, el gasto tiene un limite.
func (s *Server) checkGlobalCap(ident *Identity) (*Identity, error) {
	if s.cfg.GlobalDailyCap > 0 && ident.GlobalToday > s.cfg.GlobalDailyCap {
		return nil, &authError{
			code:       http.StatusServiceUnavailable,
			message:    "Tope diario global alcanzado. El servicio se reanuda a medianoche UTC.",
			retryAfter: "3600",
		}
	}
	return ident, nil
}

// setQuotaHeaders informa al cliente de cuanto le queda.
func setQuotaHeaders(w http.ResponseWriter, ident *Identity, globalCap int) {
	if ident.Kind == KindKey && ident.DailyQuota > 0 {
		w.Header().Set("X-Quota-Limit", strconv.Itoa(ident.DailyQuota))
		w.Header().Set("X-Quota-Used", strconv.Itoa(ident.UsedToday))
		w.Header().Set("X-Quota-Remaining", strconv.Itoa(max(0, ident.DailyQuota-ident.UsedToday)))
	}
	if globalCap > 0 && ident.GlobalToday > 0 {
		w.Header().Set("X-Global-Remaining", strconv.Itoa(max(0, globalCap-ident.GlobalToday)))
	}
}

// extractToken lee la credencial de X-API-Key o de Authorization: Bearer.
func extractToken(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("X-API-Key")); value != "" {
		return value
	}
	if auth := r.Header.Get("Authorization"); len(auth) > 7 &&
		strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

// --------------------------------------------------------------------------- //
// Rate limit por IP
// --------------------------------------------------------------------------- //

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
