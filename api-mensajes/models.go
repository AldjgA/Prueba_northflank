package main

import "time"

// Mensaje es una unidad de conversacion almacenada en el buzon mock.
type Mensaje struct {
	ID        int64     `json:"id"`
	De        string    `json:"de"`
	Para      string    `json:"para"`
	Texto     string    `json:"texto"`
	EnviadoEn time.Time `json:"enviado_en"`
	Estado    string    `json:"estado"`
	Leido     bool      `json:"leido"`
}

// Archivo describe un archivo mock disponible para descarga.
// Contenido no se serializa en JSON: solo viaja en el endpoint de descarga.
type Archivo struct {
	ID        string    `json:"id"`
	Nombre    string    `json:"nombre"`
	MimeType  string    `json:"mime_type"`
	Bytes     int       `json:"bytes"`
	CreadoEn  time.Time `json:"creado_en"`
	Contenido []byte    `json:"-"`
}
