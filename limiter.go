package main

import (
	"context"
	"errors"
	"time"
)

// ErrQueueTimeout se devuelve cuando una peticion espera mas de QueueTimeout
// sin conseguir un hueco libre.
var ErrQueueTimeout = errors.New("cola de espera agotada")

// Limiter acota cuantas peticiones a Gemini pueden estar EN VUELO a la vez.
//
// Ojo con la distincion, porque es la clave de tu pregunta:
//
//   - CONEXIONES concurrentes: Go atiende miles sin problema (cada goroutine
//     son ~4 KB). Eso no lo limita nadie.
//   - PETICIONES A GEMINI en vuelo: esto es lo que se limita aqui. Cada
//     llamada consume cuota y memoria (buffer de la respuesta). Poner un techo
//     evita que 200 usuarios simultaneos te revienten la cuota y la latencia.
//
// Con MaxConcurrency=3, el cuarto usuario NO es rechazado: espera en cola
// hasta QueueTimeout. Si expira, recibe un 503 con Retry-After.
type Limiter struct {
	slots chan struct{}
	wait  time.Duration
}

// NewLimiter crea el limitador. maxConcurrency<=0 se normaliza a 1.
func NewLimiter(maxConcurrency int, wait time.Duration) *Limiter {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	if wait <= 0 {
		wait = 30 * time.Second
	}
	return &Limiter{
		slots: make(chan struct{}, maxConcurrency),
		wait:  wait,
	}
}

// Acquire reserva un hueco. Bloquea como maximo `wait`.
func (l *Limiter) Acquire(ctx context.Context) error {
	// Camino rapido: si hay hueco libre, entra sin crear timer.
	select {
	case l.slots <- struct{}{}:
		return nil
	default:
	}

	timer := time.NewTimer(l.wait)
	defer timer.Stop()

	select {
	case l.slots <- struct{}{}:
		return nil
	case <-timer.C:
		return ErrQueueTimeout
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release libera el hueco. Siempre en defer, justo despues de un Acquire exitoso.
func (l *Limiter) Release() { <-l.slots }

// InFlight devuelve cuantas peticiones estan en vuelo ahora mismo.
func (l *Limiter) InFlight() int { return len(l.slots) }

// Capacity devuelve el maximo configurado.
func (l *Limiter) Capacity() int { return cap(l.slots) }
