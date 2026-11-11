// Setup-wizard + user-administration endpoints. Transport only — decisions
// live in adminsvc; statuses are mapped, never invented here.
package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/httpx"
	"github.com/kanatabi/lumen/internal/store"
)

// mountUsers sits inside the auth group; the authenticator already enforced
// role=admin for every /admin/users path.
func (h *Handler) mountUsers(a chi.Router) {
	a.Get("/users", h.listUsers)
	a.Post("/users", h.createUser)
	a.Post("/users/{username}/reset-password", h.resetUserPassword)
}

func (h *Handler) setupStatus(w http.ResponseWriter, r *http.Request) {
	empty, err := h.svc.SetupStatus(r.Context())
	if err != nil {
		slog.Error("setup.status.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"unclaimed": empty})
}

func (h *Handler) setupRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "json body required")
		return
	}
	token, expiresIn, p, err := h.svc.RegisterFirstAdmin(r.Context(), in.Username, in.Password)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{
			"token": token, "expiresIn": expiresIn,
			"role": p.Role, "mustChange": p.MustChange,
		})
	case errors.Is(err, adminsvc.ErrBadUsername), errors.Is(err, adminsvc.ErrShortPass):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrUsersExist):
		// terminal, not retryable — the estate is claimed; login instead
		httpx.WriteErr(w, http.StatusGone, "setup already completed")
	default:
		slog.Error("setup.register.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.svc.ListUsers(r.Context())
	if err != nil {
		slog.Error("users.list.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, users)
}

func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "json body required")
		return
	}
	temp, err := h.svc.CreateUserWithTemp(r.Context(), in.Username, in.Role)
	switch {
	case err == nil:
		// the temp appears in THIS response and nowhere else — not logs,
		// not the DB (argon2id hash only)
		httpx.WriteJSON(w, http.StatusCreated, map[string]string{
			"username": in.Username, "role": in.Role, "tempPassword": temp,
		})
	case errors.Is(err, adminsvc.ErrBadUsername):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	default:
		slog.Error("users.create.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

func (h *Handler) resetUserPassword(w http.ResponseWriter, r *http.Request) {
	temp, err := h.svc.ResetPassword(r.Context(), chi.URLParam(r, "username"))
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"tempPassword": temp})
	case errors.Is(err, store.ErrNoSuchUser):
		httpx.WriteErr(w, http.StatusNotFound, "no such user")
	default:
		slog.Error("users.reset.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "json body required")
		return
	}
	err := h.svc.ChangeOwnPassword(r.Context(), p.Username, in.Current, in.Next)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case errors.Is(err, adminsvc.ErrShortPass), errors.Is(err, adminsvc.ErrSamePassword):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrWrongPassword):
		httpx.WriteErr(w, http.StatusUnauthorized, "invalid credentials")
	default:
		slog.Error("users.password.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

func (h *Handler) whoami(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"username": p.Username, "role": p.Role, "mustChange": p.MustChange,
	})
}
