package main

import (
	"log/slog"
	"net/http"
	"time"
)

// estadoRecorder guarda el codigo de estado para poder registrarlo en el log.
type estadoRecorder struct {
	http.ResponseWriter
	estado int
}

func (r *estadoRecorder) WriteHeader(estado int) {
	r.estado = estado
	r.ResponseWriter.WriteHeader(estado)
}

// conLog registra una linea JSON por peticion (Northflank las indexa por campo).
func conLog(log *slog.Logger, sig http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inicio := time.Now()
		rec := &estadoRecorder{ResponseWriter: w, estado: http.StatusOK}

		sig.ServeHTTP(rec, r)

		log.Info("peticion",
			"metodo", r.Method,
			"ruta", r.URL.Path,
			"estado", rec.estado,
			"ms", time.Since(inicio).Milliseconds(),
		)
	})
}

// conRecuperacion evita que un panic tumbe el proceso entero.
func conRecuperacion(log *slog.Logger, sig http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Error("panic recuperado", "valor", v, "ruta", r.URL.Path)
				escribirError(w, http.StatusInternalServerError, "error_interno",
					"ocurrio un error inesperado")
			}
		}()
		sig.ServeHTTP(w, r)
	})
}

// conCORS permite probar la API desde un navegador o desde otra app.
func conCORS(sig http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		sig.ServeHTTP(w, r)
	})
}
