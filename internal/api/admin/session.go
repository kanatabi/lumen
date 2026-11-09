package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/httpx"
)

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "json body required")
		return
	}
	token, expiresIn, err := h.svc.Login(r.Context(), in.Username, in.Password, clientIP(r))
	var throttled *adminsvc.ThrottledError
	switch {
	case err == nil:
		slog.Info("session.grant", "user", in.Username)
		// camelCase per mellon's AdminApi — the executable spec
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"token": token, "expiresIn": expiresIn})
	case errors.Is(err, adminsvc.ErrEmptyCredentials):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.As(err, &throttled):
		w.Header().Set("Retry-After", strconv.Itoa(int(throttled.RetryAfter.Seconds())+1))
		httpx.WriteErr(w, http.StatusTooManyRequests, "too many attempts")
	case errors.Is(err, adminsvc.ErrInvalidCredentials):
		slog.Info("session.deny", "user", in.Username, "ip", clientIP(r))
		httpx.WriteErr(w, http.StatusUnauthorized, "invalid credentials")
	default:
		slog.Error("session.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	subject, err := h.svc.Revoke(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	slog.Info("session.revoke", "user", subject)
	w.WriteHeader(http.StatusNoContent)
}
