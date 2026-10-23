package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Limite de longitud del texto de un mensaje (en caracteres, no bytes).
const maxTexto = 4000

var (
	errDeVacio         = errors.New("el campo de es obligatorio")
	errParaVacio       = errors.New("el campo para es obligatorio")
	errTextoVacio      = errors.New("el campo texto es obligatorio")
	errTextoMuyLargo   = fmt.Errorf("el texto supera el maximo de %d caracteres", maxTexto)
	errArchivoNoExiste = errors.New("archivo no encontrado")
)

// Store es un almacen en memoria (datos mock, se pierde al reiniciar).
// Es seguro para uso concurrente.
type Store struct {
	mu          sync.RWMutex
	mensajes    []Mensaje
	siguienteID int64
	archivos    map[string]*Archivo
}

// StoreNuevo crea el almacen y lo rellena con datos de ejemplo.
func StoreNuevo() *Store {
	s := &Store{archivos: make(map[string]*Archivo, 4)}
	s.sembrar()
	return s
}

// sembrar carga mensajes y archivos de ejemplo para poder probar la API
// sin depender de ningun servicio externo.
func (s *Store) sembrar() {
	ahora := time.Now().UTC()

	ejemplos := []struct{ de, para, texto string }{
		{"ana", "luis", "Hola Luis, ya subi el reporte del sprint."},
		{"luis", "ana", "Perfecto, lo reviso y te digo algo hoy."},
		{"carla", "luis", "Te pase el archivo de ejemplo en /descargar_archivo?nombre=datos.csv"},
	}
	for _, e := range ejemplos {
		s.siguienteID++
		s.mensajes = append(s.mensajes, Mensaje{
			ID:        s.siguienteID,
			De:        e.de,
			Para:      e.para,
			Texto:     e.texto,
			EnviadoEn: ahora.Add(-time.Duration(s.siguienteID) * time.Minute),
			Estado:    "entregado",
			Leido:     false,
		})
	}

	s.archivos["arch-1"] = &Archivo{
		ID:        "arch-1",
		Nombre:    "nota.txt",
		MimeType:  "text/plain; charset=utf-8",
		CreadoEn:  ahora.Add(-72 * time.Hour),
		Contenido: []byte("Nota de ejemplo generada por la API de mensajes.\n"),
	}
	s.archivos["arch-2"] = &Archivo{
		ID:        "arch-2",
		Nombre:    "reporte.json",
		MimeType:  "application/json; charset=utf-8",
		CreadoEn:  ahora.Add(-48 * time.Hour),
		Contenido: []byte("{\n  \"ejemplo\": true,\n  \"origen\": \"datos mock\"\n}\n"),
	}
	s.archivos["arch-3"] = &Archivo{
		ID:        "arch-3",
		Nombre:    "datos.csv",
		MimeType:  "text/csv; charset=utf-8",
		CreadoEn:  ahora.Add(-24 * time.Hour),
		Contenido: []byte("id,nombre,valor\n1,alfa,10\n2,beta,20\n3,gamma,30\n"),
	}
	for _, a := range s.archivos {
		a.Bytes = len(a.Contenido)
	}
}

// EnviarMensaje valida y guarda un mensaje nuevo.
func (s *Store) EnviarMensaje(de, para, texto string) (Mensaje, error) {
	de = strings.TrimSpace(de)
	para = strings.TrimSpace(para)
	texto = strings.TrimSpace(texto)

	switch {
	case de == "":
		return Mensaje{}, errDeVacio
	case para == "":
		return Mensaje{}, errParaVacio
	case texto == "":
		return Mensaje{}, errTextoVacio
	case len([]rune(texto)) > maxTexto:
		return Mensaje{}, errTextoMuyLargo
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.siguienteID++
	m := Mensaje{
		ID:        s.siguienteID,
		De:        de,
		Para:      para,
		Texto:     texto,
		EnviadoEn: time.Now().UTC(),
		Estado:    "entregado",
		Leido:     false,
	}
	s.mensajes = append(s.mensajes, m)
	return m, nil
}

// RecibirMensaje devuelve los mensajes dirigidos a "para" con ID mayor que
// desdeID. Los mensajes devueltos se marcan como leidos.
func (s *Store) RecibirMensaje(para string, desdeID int64) []Mensaje {
	s.mu.Lock()
	defer s.mu.Unlock()

	resultado := make([]Mensaje, 0, len(s.mensajes))
	for i := range s.mensajes {
		m := &s.mensajes[i]
		if m.Para != para || m.ID <= desdeID {
			continue
		}
		m.Leido = true
		resultado = append(resultado, *m)
	}
	return resultado
}

// ListarArchivos devuelve los metadatos de los archivos disponibles.
func (s *Store) ListarArchivos() []Archivo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	lista := make([]Archivo, 0, len(s.archivos))
	for _, a := range s.archivos {
		lista = append(lista, *a)
	}
	return lista
}

// BuscarArchivo busca por ID o por nombre exacto (sin distinguir mayusculas).
func (s *Store) BuscarArchivo(clave string) (Archivo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if a, ok := s.archivos[clave]; ok {
		return *a, nil
	}
	for _, a := range s.archivos {
		if strings.EqualFold(a.Nombre, clave) {
			return *a, nil
		}
	}
	return Archivo{}, errArchivoNoExiste
}
