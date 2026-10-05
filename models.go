package main

// --- Contratos HTTP --------------------------------------------------------

// Turn es un mensaje del historial enviado por el cliente.
type Turn struct {
	Role    string `json:"role"` // "user" | "model"
	Content string `json:"content"`
}

// ChatRequest es el cuerpo de POST /chat y POST /chat/stream.
type ChatRequest struct {
	Message        string   `json:"message"`
	ConversationID *string  `json:"conversation_id,omitempty"`
	History        []Turn   `json:"history,omitempty"`
	System         string   `json:"system,omitempty"`
	Temperature    *float32 `json:"temperature,omitempty"`
	Persist        *bool    `json:"persist,omitempty"`
}

// ShouldPersist respeta el flag `persist` (por defecto true).
func (r ChatRequest) ShouldPersist() bool {
	return r.Persist == nil || *r.Persist
}

// ChatResponse es la respuesta de POST /chat.
type ChatResponse struct {
	Reply          string  `json:"reply"`
	Model          string  `json:"model"`
	LatencyMs      int64   `json:"latency_ms"`
	ConversationID *string `json:"conversation_id"`
	QueuedMs       int64   `json:"queued_ms"`
}

// Message es un mensaje almacenado.
type Message struct {
	Role      string  `json:"role"`
	Content   string  `json:"content"`
	Model     *string `json:"model,omitempty"`
	LatencyMs *int32  `json:"latency_ms,omitempty"`
	CreatedAt string  `json:"created_at"`
}

// HistoryResponse es la respuesta de GET /chat/{id}/history.
type HistoryResponse struct {
	ConversationID string    `json:"conversation_id"`
	Messages       []Message `json:"messages"`
}

// ErrorResponse es el formato uniforme de error.
type ErrorResponse struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

// --- Administracion de claves ---------------------------------------------

// CreateKeyRequest es el cuerpo de POST /admin/keys.
type CreateKeyRequest struct {
	Label         string `json:"label"`
	DailyQuota    *int   `json:"daily_quota,omitempty"`
	ExpiresInDays *int   `json:"expires_in_days,omitempty"`
}

// CreateKeyResponse devuelve el token EN CLARO. Es la unica vez que se muestra:
// en la base de datos solo queda su hash.
type CreateKeyResponse struct {
	ID         int64   `json:"id"`
	Label      string  `json:"label"`
	Token      string  `json:"token"`
	DailyQuota int     `json:"daily_quota"`
	ExpiresAt  *string `json:"expires_at,omitempty"`
	Aviso      string  `json:"aviso"`
}

// ListKeysResponse es la respuesta de GET /admin/keys.
type ListKeysResponse struct {
	Keys        []APIKey `json:"keys"`
	GlobalToday int      `json:"global_today"`
	GlobalCap   int      `json:"global_daily_cap"`
}
