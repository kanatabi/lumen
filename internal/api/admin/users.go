// Setup-wizard + user-administration endpoints. Transport only — decisions
// live in adminsvc; statuses are mapped, never invented here.
package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/httpx"
	"github.com/kanatabi/lumen/internal/store"
)

// mountUsers + mountScopesCatalog sit inside the mgmt group: the
// authenticator verified the bearer, requireScope verified users:manage.
func (h *Handler) mountUsers(a chi.Router) {
	a.Get("/users", h.listUsers)
	a.Post("/users", h.createUser)
	a.Post("/users/{username}/reset-password", h.resetUserPassword)
	a.Patch("/users/{username}/scopes", h.updateUserScopes)
	a.Post("/users/{username}/revoke-sessions", h.revokeSessions)
	a.Post("/users/{username}/force-change", h.forceChange)
	a.Post("/users/{username}/disable", h.disableUser)
	a.Post("/users/{username}/enable", h.enableUser)
	a.Delete("/users/{username}", h.deleteUser)
}

// mountScopesCatalog is the PORTAL-MANAGED scope registry (the spine
// ruling: onboarding a scope never means manual SQL) — the assignable
// vocabulary, users:manage-gated like the users it feeds.
func (h *Handler) mountScopesCatalog(a chi.Router) {
	a.Get("/scopes", h.listScopes)
	a.Post("/scopes", h.addScope)
	a.Delete("/scopes/{scope}", h.deleteScope)
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
			"scopes": p.Scopes, "mustChange": p.MustChange,
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
		Username string   `json:"username"`
		Scopes   []string `json:"scopes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "json body required")
		return
	}
	temp, err := h.svc.CreateUserWithTemp(r.Context(), in.Username, in.Scopes)
	switch {
	case err == nil:
		// the temp appears in THIS response and nowhere else — not logs,
		// not the DB (argon2id hash only)
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{
			"username": in.Username, "scopes": in.Scopes, "tempPassword": temp,
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
	case errors.Is(err, adminsvc.ErrNoSuchUser):
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
		"username": p.Username, "id": p.ID, "scopes": p.Scopes, "mustChange": p.MustChange,
	})
}

// updateUserScopes REPLACES the user's scope set (the admin's assignment
// is the whole truth) and bumps token_version — the spine ruling: a
// demotion is immediate; the user's other sessions die (re-login).
func (h *Handler) updateUserScopes(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	var in struct {
		Scopes []string `json:"scopes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "json body required")
		return
	}
	tv, err := h.svc.UpdateScopes(r.Context(), username, in.Scopes)
	switch {
	case err == nil:
		slog.Info("users.scopes.set", "user", username, "scopes", in.Scopes)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"username": username, "scopes": in.Scopes, "tokenVersion": tv,
		})
	case errors.Is(err, adminsvc.ErrBadScope):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, adminsvc.ErrNoSuchUser):
		httpx.WriteErr(w, http.StatusNotFound, err.Error())
	default:
		slog.Error("users.scopes.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

// listScopes reads the catalog — the users page's picker and the catalog
// surface both read this.
func (h *Handler) listScopes(w http.ResponseWriter, r *http.Request) {
	catalog, err := h.svc.ListCatalog(r.Context())
	if err != nil {
		slog.Error("scopes.list.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, catalog)
}

// addScope onboards a scope into the catalog (the spine ruling: never
// manual SQL). Grammar-checked here; existence is the DB's.
func (h *Handler) addScope(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scope       string `json:"scope"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "json body required")
		return
	}
	created, err := h.svc.AddScope(r.Context(), in.Scope, in.Description)
	switch {
	case err == nil:
		if created {
			slog.Info("scopes.add", "scope", in.Scope)
			httpx.WriteJSON(w, http.StatusCreated, map[string]any{
				"scope": in.Scope, "description": in.Description,
			})
			return
		}
		// re-adding an existing scope: the catalog is idempotent, the
		// answer carries the row either way
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"scope": in.Scope, "description": in.Description,
		})
	case errors.Is(err, adminsvc.ErrBadScope):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	default:
		slog.Error("scopes.add.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

// deleteScope removes a scope from the catalog. lumen REFUSES while any
// user holds it — the counts ride the refusal, the UI renders them.
func (h *Handler) deleteScope(w http.ResponseWriter, r *http.Request) {
	scope := chi.URLParam(r, "scope")
	deleted, holders, err := h.svc.DeleteScope(r.Context(), scope)
	switch {
	case err == nil && deleted:
		slog.Info("scopes.delete", "scope", scope)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	case err == nil && holders > 0:
		httpx.WriteErr(w, http.StatusConflict,
			"scope still assigned to "+strconv.Itoa(holders)+" user(s) — unassign first")
	case err == nil:
		httpx.WriteErr(w, http.StatusNotFound, "no such scope")
	default:
		slog.Error("scopes.delete.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

// The user-LIFECYCLE verbs (the RBAC round's second cut): every one rides
// the users:manage group, maps its own sentinels, and audits in the
// service. Disabled-vs-deleted are different answers on purpose: 403 names
// the undo (re-enable), 404 is terminal.
func (h *Handler) revokeSessions(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	tv, err := h.svc.RevokeSessions(r.Context(), username)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"username": username, "tokenVersion": tv,
		})
	case errors.Is(err, adminsvc.ErrNoSuchUser):
		httpx.WriteErr(w, http.StatusNotFound, err.Error())
	default:
		slog.Error("users.revoke.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

func (h *Handler) forceChange(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	err := h.svc.ForceChange(r.Context(), username)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"username": username, "mustChange": true,
		})
	case errors.Is(err, adminsvc.ErrNoSuchUser):
		httpx.WriteErr(w, http.StatusNotFound, err.Error())
	default:
		slog.Error("users.force.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

func (h *Handler) disableUser(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	tv, err := h.svc.SetUserDisabled(r.Context(), username, true)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"username": username, "disabled": true, "tokenVersion": tv,
		})
	case errors.Is(err, adminsvc.ErrNoSuchUser):
		httpx.WriteErr(w, http.StatusNotFound, err.Error())
	default:
		slog.Error("users.disable.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

func (h *Handler) enableUser(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	tv, err := h.svc.SetUserDisabled(r.Context(), username, false)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"username": username, "disabled": false, "tokenVersion": tv,
		})
	case errors.Is(err, adminsvc.ErrNoSuchUser):
		httpx.WriteErr(w, http.StatusNotFound, err.Error())
	default:
		slog.Error("users.enable.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

func (h *Handler) deleteUser(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	actorName := ""
	if p, ok := principalFrom(r.Context()); ok {
		actorName = p.Username
	}
	err := h.svc.DeleteUser(r.Context(), actorName, username)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	case errors.Is(err, adminsvc.ErrSelfDelete):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, adminsvc.ErrLastManager):
		httpx.WriteErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, adminsvc.ErrNoSuchUser):
		httpx.WriteErr(w, http.StatusNotFound, err.Error())
	default:
		slog.Error("users.delete.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}
