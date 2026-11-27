package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/httpx"
	"github.com/kanatabi/lumen/internal/store"
)

// getProfile is the self read: the authenticated principal's display name,
// email, scopes and lineage. Auth-only (no users:manage — it is yourself).
func (h *Handler) getProfile(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	prof, err := h.svc.Profile(r.Context(), p.ID)
	if err != nil {
		slog.Error("users.profile.read", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, prof)
}

// updateProfile writes the self-service fields. The transport maps the
// service/store sentinels; values are validated in adminsvc (the email is
// never logged).
func (h *Handler) updateProfile(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "json body required")
		return
	}
	prof, err := h.svc.UpdateProfile(r.Context(), p.ID, in.Name, in.Email)
	switch {
	case err == nil:
		slog.Info("users.profile.set", "user", p.Username)
		httpx.WriteJSON(w, http.StatusOK, prof)
	case errors.Is(err, adminsvc.ErrBadDisplayName), errors.Is(err, adminsvc.ErrBadEmail):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrEmailTaken):
		httpx.WriteErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrNoSuchUser):
		httpx.WriteErr(w, http.StatusNotFound, err.Error())
	default:
		slog.Error("users.profile.write", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}
