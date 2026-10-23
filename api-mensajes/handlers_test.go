package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// servidorPrueba levanta la API completa con un almacen mock limpio.
func servidorPrueba(t *testing.T) *httptest.Server {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(nuevoRouter(StoreNuevo(), log))
	t.Cleanup(srv.Close)
	return srv
}

func TestEnviarMensaje(t *testing.T) {
	srv := servidorPrueba(t)

	cuerpo := `{"de":"ana","para":"luis","texto":"hola desde la prueba"}`
	resp, err := http.Post(srv.URL+"/enviar_mensaje", "application/json", strings.NewReader(cuerpo))
	if err != nil {
		t.Fatalf("POST fallo: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("estado = %d, queria 201", resp.StatusCode)
	}

	var m Mensaje
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("respuesta no es JSON valido: %v", err)
	}
	if m.ID <= 3 {
		t.Errorf("ID = %d, queria > 3 (los mock ocupan 1..3)", m.ID)
	}
	if m.De != "ana" || m.Para != "luis" || m.Texto != "hola desde la prueba" {
		t.Errorf("mensaje inesperado: %+v", m)
	}
	if m.Estado != "entregado" || m.Leido {
		t.Errorf("estado/leido inesperados: %+v", m)
	}
	if m.EnviadoEn.IsZero() {
		t.Error("enviado_en no deberia estar vacio")
	}
}

func TestEnviarMensajeValidacion(t *testing.T) {
	srv := servidorPrueba(t)

	casos := []struct {
		nombre string
		cuerpo string
		codigo string
	}{
		{"falta de", `{"para":"luis","texto":"hola"}`, "datos_invalidos"},
		{"falta para", `{"de":"ana","texto":"hola"}`, "datos_invalidos"},
		{"texto vacio", `{"de":"ana","para":"luis","texto":"   "}`, "datos_invalidos"},
		{"campo desconocido", `{"de":"ana","para":"luis","texto":"hola","extra":1}`, "json_invalido"},
		{"json roto", `{"de":`, "json_invalido"},
	}

	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			resp, err := http.Post(srv.URL+"/enviar_mensaje", "application/json", strings.NewReader(c.cuerpo))
			if err != nil {
				t.Fatalf("POST fallo: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("estado = %d, queria 400", resp.StatusCode)
			}

			var e respuestaError
			if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
				t.Fatalf("respuesta no es JSON valido: %v", err)
			}
			if e.Error.Codigo != c.codigo {
				t.Errorf("codigo = %q, queria %q", e.Error.Codigo, c.codigo)
			}
		})
	}
}

func TestRecibirMensaje(t *testing.T) {
	srv := servidorPrueba(t)

	// Luis tiene un mensaje mock; enviamos otro para comprobar que aparecen los dos.
	cuerpo := `{"de":"ana","para":"luis","texto":"segundo mensaje"}`
	resp, err := http.Post(srv.URL+"/enviar_mensaje", "application/json", strings.NewReader(cuerpo))
	if err != nil {
		t.Fatalf("POST fallo: %v", err)
	}
	resp.Body.Close()

	resp, err = http.Get(srv.URL + "/recibir_mensaje?para=luis")
	if err != nil {
		t.Fatalf("GET fallo: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("estado = %d, queria 200", resp.StatusCode)
	}

	var r struct {
		Para     string    `json:"para"`
		Total    int       `json:"total"`
		Mensajes []Mensaje `json:"mensajes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("respuesta no es JSON valido: %v", err)
	}
	if r.Para != "luis" {
		t.Errorf("para = %q, queria luis", r.Para)
	}
	if r.Total != len(r.Mensajes) {
		t.Errorf("total = %d pero llegaron %d mensajes", r.Total, len(r.Mensajes))
	}
	if r.Total < 2 {
		t.Fatalf("total = %d, queria al menos 2", r.Total)
	}
	for _, m := range r.Mensajes {
		if m.Para != "luis" {
			t.Errorf("llego un mensaje de otro destinatario: %+v", m)
		}
		if !m.Leido {
			t.Errorf("el mensaje %d deberia marcarse como leido", m.ID)
		}
	}
}

func TestRecibirMensajeDesdeID(t *testing.T) {
	srv := servidorPrueba(t)

	resp, err := http.Get(srv.URL + "/recibir_mensaje?para=luis&desde_id=3")
	if err != nil {
		t.Fatalf("GET fallo: %v", err)
	}
	defer resp.Body.Close()

	var r struct {
		Total    int       `json:"total"`
		Mensajes []Mensaje `json:"mensajes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("respuesta no es JSON valido: %v", err)
	}
	if r.Total != 0 {
		t.Errorf("total = %d, queria 0 (no hay mensajes con ID > 3)", r.Total)
	}
}

func TestRecibirMensajeSinPara(t *testing.T) {
	srv := servidorPrueba(t)

	resp, err := http.Get(srv.URL + "/recibir_mensaje")
	if err != nil {
		t.Fatalf("GET fallo: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("estado = %d, queria 400", resp.StatusCode)
	}
}

func TestRecibirMensajeDesdeIDInvalido(t *testing.T) {
	srv := servidorPrueba(t)

	resp, err := http.Get(srv.URL + "/recibir_mensaje?para=luis&desde_id=abc")
	if err != nil {
		t.Fatalf("GET fallo: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("estado = %d, queria 400", resp.StatusCode)
	}
}

func TestDescargarArchivoPorNombre(t *testing.T) {
	srv := servidorPrueba(t)

	resp, err := http.Get(srv.URL + "/descargar_archivo?nombre=datos.csv")
	if err != nil {
		t.Fatalf("GET fallo: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("estado = %d, queria 200", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="datos.csv"`) {
		t.Errorf("Content-Disposition = %q, queria incluir filename=\"datos.csv\"", cd)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q, queria text/csv", ct)
	}

	cuerpo, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("no se pudo leer el cuerpo: %v", err)
	}
	if !strings.Contains(string(cuerpo), "alfa") {
		t.Errorf("contenido inesperado: %q", cuerpo)
	}
}

func TestDescargarArchivoPorID(t *testing.T) {
	srv := servidorPrueba(t)

	resp, err := http.Get(srv.URL + "/descargar_archivo?id=arch-2")
	if err != nil {
		t.Fatalf("GET fallo: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("estado = %d, queria 200", resp.StatusCode)
	}
	if id := resp.Header.Get("X-Archivo-Id"); id != "arch-2" {
		t.Errorf("X-Archivo-Id = %q, queria arch-2", id)
	}
}

func TestDescargarArchivoInexistente(t *testing.T) {
	srv := servidorPrueba(t)

	resp, err := http.Get(srv.URL + "/descargar_archivo?nombre=no-existe.txt")
	if err != nil {
		t.Fatalf("GET fallo: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("estado = %d, queria 404", resp.StatusCode)
	}
}

func TestDescargarArchivoSinParametros(t *testing.T) {
	srv := servidorPrueba(t)

	resp, err := http.Get(srv.URL + "/descargar_archivo")
	if err != nil {
		t.Fatalf("GET fallo: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("estado = %d, queria 400", resp.StatusCode)
	}
}

func TestListarArchivos(t *testing.T) {
	srv := servidorPrueba(t)

	resp, err := http.Get(srv.URL + "/archivos")
	if err != nil {
		t.Fatalf("GET fallo: %v", err)
	}
	defer resp.Body.Close()

	var r struct {
		Total    int       `json:"total"`
		Archivos []Archivo `json:"archivos"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("respuesta no es JSON valido: %v", err)
	}
	if r.Total != 3 || len(r.Archivos) != 3 {
		t.Fatalf("total = %d, queria 3", r.Total)
	}
	for _, a := range r.Archivos {
		if a.Bytes == 0 || a.Nombre == "" || a.ID == "" {
			t.Errorf("metadatos incompletos: %+v", a)
		}
	}
}

func TestRutaNoEncontrada(t *testing.T) {
	srv := servidorPrueba(t)

	resp, err := http.Get(srv.URL + "/no-existe")
	if err != nil {
		t.Fatalf("GET fallo: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("estado = %d, queria 404", resp.StatusCode)
	}

	var e respuestaError
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("la respuesta 404 deberia ser JSON: %v", err)
	}
	if e.Error.Codigo != "ruta_no_encontrada" {
		t.Errorf("codigo = %q, queria ruta_no_encontrada", e.Error.Codigo)
	}
}

func TestHealth(t *testing.T) {
	srv := servidorPrueba(t)

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET fallo: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("estado = %d, queria 200", resp.StatusCode)
	}
}

func TestMetodoNoPermitido(t *testing.T) {
	srv := servidorPrueba(t)

	// /enviar_mensaje solo acepta POST: un GET debe caer en el 404 JSON.
	resp, err := http.Get(srv.URL + "/enviar_mensaje")
	if err != nil {
		t.Fatalf("GET fallo: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("estado = %d, queria 404", resp.StatusCode)
	}
}
