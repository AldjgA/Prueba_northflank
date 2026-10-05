package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// --------------------------------------------------------------------------- //
// GET /me — el tester consulta su propia cuota
// --------------------------------------------------------------------------- //

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	ident := IdentityFrom(r.Context())

	resp := map[string]any{
		"kind":  ident.Kind,
		"label": ident.Label,
	}
	if ident.Kind == KindKey {
		resp["key_id"] = ident.KeyID
		resp["daily_quota"] = ident.DailyQuota
		resp["used_today"] = ident.UsedToday
		resp["remaining"] = max(0, ident.DailyQuota-ident.UsedToday)
	}
	if s.cfg.GlobalDailyCap > 0 {
		resp["global_daily_cap"] = s.cfg.GlobalDailyCap
		resp["global_today"] = ident.GlobalToday
	}
	resp["quota_resets"] = "medianoche UTC"
	writeJSON(w, http.StatusOK, resp)
}

// --------------------------------------------------------------------------- //
// POST /admin/keys — crear una clave de tester
// --------------------------------------------------------------------------- //

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req CreateKeyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Cuerpo JSON invalido.", err.Error())
		return
	}

	label := strings.TrimSpace(req.Label)
	if label == "" {
		writeError(w, http.StatusUnprocessableEntity, "El campo 'label' es obligatorio.", "")
		return
	}
	if len([]rune(label)) > 100 {
		writeError(w, http.StatusUnprocessableEntity,
			"'label' no puede superar los 100 caracteres.", "")
		return
	}

	quota := s.cfg.DefaultDailyQuota
	if req.DailyQuota != nil {
		if *req.DailyQuota < 0 {
			writeError(w, http.StatusUnprocessableEntity,
				"'daily_quota' no puede ser negativo.", "")
			return
		}
		quota = *req.DailyQuota
	}

	var expiresAt *time.Time
	if req.ExpiresInDays != nil {
		if *req.ExpiresInDays <= 0 {
			writeError(w, http.StatusUnprocessableEntity,
				"'expires_in_days' debe ser mayor que 0.", "")
			return
		}
		t := time.Now().UTC().AddDate(0, 0, *req.ExpiresInDays)
		expiresAt = &t
	}

	token, hash, err := GenerateToken()
	if err != nil {
		s.log.Error("no se pudo generar el token", "error", err)
		writeError(w, http.StatusInternalServerError, "Error interno.", "")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.DBQueryTimeout)
	defer cancel()

	id, err := s.store.CreateAPIKey(ctx, hash, label, quota, expiresAt)
	if err != nil {
		s.log.Error("no se pudo guardar la clave", "error", err)
		writeError(w, http.StatusInternalServerError, "Error interno.", "")
		return
	}

	resp := CreateKeyResponse{
		ID:         id,
		Label:      label,
		Token:      token,
		DailyQuota: quota,
		Aviso:      "Guarda este token ahora: no se puede recuperar despues.",
	}
	if expiresAt != nil {
		formatted := expiresAt.Format(time.RFC3339)
		resp.ExpiresAt = &formatted
	}

	// El token NO se registra en el log: solo el id y la etiqueta.
	s.log.Info("clave creada", "id", id, "label", label, "cuota", quota)
	writeJSON(w, http.StatusCreated, resp)
}

// --------------------------------------------------------------------------- //
// GET /admin/keys — listar claves y consumo
// --------------------------------------------------------------------------- //

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.DBQueryTimeout)
	defer cancel()

	keys, err := s.store.ListAPIKeys(ctx)
	if err != nil {
		s.log.Error("no se pudieron listar las claves", "error", err)
		writeError(w, http.StatusInternalServerError, "Error interno.", "")
		return
	}

	global, err := s.store.GlobalUsageToday(ctx)
	if err != nil {
		s.log.Warn("no se pudo leer el consumo global", "error", err)
	}

	writeJSON(w, http.StatusOK, ListKeysResponse{
		Keys:        keys,
		GlobalToday: global,
		GlobalCap:   s.cfg.GlobalDailyCap,
	})
}

// --------------------------------------------------------------------------- //
// DELETE /admin/keys/{id} — revocar una clave
// --------------------------------------------------------------------------- //

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "'id' no es un numero valido.", "")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.DBQueryTimeout)
	defer cancel()

	revoked, err := s.store.DeactivateAPIKey(ctx, id)
	if err != nil {
		s.log.Error("no se pudo revocar la clave", "error", err)
		writeError(w, http.StatusInternalServerError, "Error interno.", "")
		return
	}
	if !revoked {
		writeError(w, http.StatusNotFound, "Clave no encontrada o ya revocada.", "")
		return
	}

	s.log.Info("clave revocada", "id", id)
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true, "id": id})
}
