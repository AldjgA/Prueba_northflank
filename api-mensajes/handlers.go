package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// maxCuerpo limita el tamano del JSON aceptado (64 KiB).
const maxCuerpo = 64 << 10

// Handler agrupa las dependencias de los endpoints.
type Handler struct {
	store *Store
	log   *slog.Logger
}

type detalleError struct {
	Codigo  string `json:"codigo"`
	Mensaje string `json:"mensaje"`
}

type respuestaError struct {
	Error detalleError `json:"error"`
}

// escribirJSON serializa v con el codigo de estado indicado.
func escribirJSON(w http.ResponseWriter, estado int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(estado)

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // las rutas se leen mejor sin \u003c ni \u0026
	if err := enc.Encode(v); err != nil {
		slog.Error("no se pudo escribir la respuesta JSON", "err", err)
	}
}

// escribirError responde con el formato de error unico de la API.
func escribirError(w http.ResponseWriter, estado int, codigo, mensaje string) {
	escribirJSON(w, estado, respuestaError{Error: detalleError{Codigo: codigo, Mensaje: mensaje}})
}

// index describe la API en la raiz.
func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	escribirJSON(w, http.StatusOK, map[string]any{
		"api":     "api-mensajes",
		"version": "1.0.0",
		"datos":   "mock en memoria",
		"endpoints": []string{
			"POST /enviar_mensaje",
			"GET  /recibir_mensaje?para=<usuario>&desde_id=<id>",
			"GET  /descargar_archivo?id=<id>|nombre=<archivo>",
			"GET  /archivos",
			"GET  /health",
		},
	})
}

// health es el endpoint de salud (lo usa Northflank y el HEALTHCHECK de Docker).
func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	escribirJSON(w, http.StatusOK, map[string]string{"estado": "ok"})
}

type peticionEnviar struct {
	De    string `json:"de"`
	Para  string `json:"para"`
	Texto string `json:"texto"`
}

// enviarMensaje atiende POST /enviar_mensaje.
func (h *Handler) enviarMensaje(w http.ResponseWriter, r *http.Request) {
	var p peticionEnviar

	cuerpo := http.MaxBytesReader(w, r.Body, maxCuerpo)
	dec := json.NewDecoder(cuerpo)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		escribirError(w, http.StatusBadRequest, "json_invalido",
			"el cuerpo debe ser JSON con los campos de, para y texto")
		return
	}

	msg, err := h.store.EnviarMensaje(p.De, p.Para, p.Texto)
	if err != nil {
		escribirError(w, http.StatusBadRequest, "datos_invalidos", err.Error())
		return
	}

	h.log.Info("mensaje enviado", "id", msg.ID, "de", msg.De, "para", msg.Para)
	escribirJSON(w, http.StatusCreated, msg)
}

// recibirMensaje atiende GET /recibir_mensaje.
func (h *Handler) recibirMensaje(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	para := strings.TrimSpace(q.Get("para"))
	if para == "" {
		escribirError(w, http.StatusBadRequest, "falta_para",
			"indica el destinatario con ?para=<usuario>")
		return
	}

	var desdeID int64
	if v := strings.TrimSpace(q.Get("desde_id")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			escribirError(w, http.StatusBadRequest, "desde_id_invalido",
				"desde_id debe ser un entero mayor o igual a 0")
			return
		}
		desdeID = n
	}

	mensajes := h.store.RecibirMensaje(para, desdeID)
	escribirJSON(w, http.StatusOK, map[string]any{
		"para":     para,
		"desde_id": desdeID,
		"total":    len(mensajes),
		"mensajes": mensajes,
	})
}

// listarArchivos atiende GET /archivos (util para saber que se puede descargar).
func (h *Handler) listarArchivos(w http.ResponseWriter, r *http.Request) {
	archivos := h.store.ListarArchivos()
	escribirJSON(w, http.StatusOK, map[string]any{
		"total":    len(archivos),
		"archivos": archivos,
	})
}

// descargarArchivo atiende GET /descargar_archivo y entrega el archivo como adjunto.
func (h *Handler) descargarArchivo(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	clave := strings.TrimSpace(q.Get("id"))
	if clave == "" {
		clave = strings.TrimSpace(q.Get("nombre"))
	}
	if clave == "" {
		escribirError(w, http.StatusBadRequest, "falta_archivo",
			"indica el archivo con ?id=<id> o ?nombre=<archivo>")
		return
	}

	archivo, err := h.store.BuscarArchivo(clave)
	if err != nil {
		if errors.Is(err, errArchivoNoExiste) {
			escribirError(w, http.StatusNotFound, "archivo_no_encontrado",
				fmt.Sprintf("no existe un archivo con identificador %q", clave))
			return
		}
		escribirError(w, http.StatusInternalServerError, "error_interno", "no se pudo leer el archivo")
		return
	}

	w.Header().Set("Content-Type", archivo.MimeType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", archivo.Nombre))
	w.Header().Set("Content-Length", strconv.Itoa(len(archivo.Contenido)))
	w.Header().Set("X-Archivo-Id", archivo.ID)
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(archivo.Contenido); err != nil {
		h.log.Error("no se pudo enviar el archivo", "id", archivo.ID, "err", err)
		return
	}
	h.log.Info("archivo descargado", "id", archivo.ID, "nombre", archivo.Nombre)
}

// noEncontrado responde 404 en JSON para cualquier ruta no registrada.
func (h *Handler) noEncontrado(w http.ResponseWriter, r *http.Request) {
	escribirError(w, http.StatusNotFound, "ruta_no_encontrada",
		fmt.Sprintf("no existe la ruta %s %s", r.Method, r.URL.Path))
}
