package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Prefijo de los tokens, para que se reconozcan de un vistazo ("mb_" de
// mini-backend). Util para soporte: si alguien pega un token, sabes de donde es.
const tokenPrefix = "mb_"

// Kind indica COMO se autentico la peticion.
type Kind string

const (
	// KindService es la clave de servicio (`clave1`): tus scripts y las
	// llamadas servidor-a-servidor. No tiene cuota propia.
	KindService Kind = "service"
	// KindKey es un token de tester guardado en la base de datos.
	KindKey Kind = "key"
	// KindNone es una peticion sin autenticacion (solo posible si no hay
	// `clave1` configurada).
	KindNone Kind = "none"
)

// Identity es quien hace la peticion, ya autenticado.
type Identity struct {
	Kind        Kind   `json:"kind"`
	KeyID       int64  `json:"key_id,omitempty"`
	Label       string `json:"label,omitempty"`
	DailyQuota  int    `json:"daily_quota,omitempty"`
	UsedToday   int    `json:"used_today,omitempty"`
	GlobalToday int    `json:"global_today,omitempty"`
}

// APIKey es una clave de tester tal como se expone por la API de administracion.
type APIKey struct {
	ID         int64   `json:"id"`
	Label      string  `json:"label"`
	DailyQuota int     `json:"daily_quota"`
	Active     bool    `json:"active"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at,omitempty"`
	ExpiresAt  *string `json:"expires_at,omitempty"`
	UsedToday  int     `json:"used_today"`
}

// GenerateToken crea un token nuevo y devuelve tambien su hash.
//
// El token en claro se muestra UNA sola vez (al crearlo). En la base de datos
// solo se guarda el hash: si alguien volcase la tabla, no podria usar las claves.
func GenerateToken() (token, hash string, err error) {
	buf := make([]byte, 32) // 256 bits de entropia
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	token = tokenPrefix + hex.EncodeToString(buf)
	return token, HashToken(token), nil
}

// HashToken usa SHA-256 a proposito.
//
// Para contrasenas habria que usar bcrypt o argon2 (son cortas y adivinables).
// Aqui NO hace falta: el token tiene 256 bits aleatorios, asi que la fuerza
// bruta es inviable y solo queremos una busqueda rapida por indice.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// LooksLikeToken indica si la cadena tiene pinta de token nuestro.
func LooksLikeToken(value string) bool {
	return strings.HasPrefix(value, tokenPrefix)
}
