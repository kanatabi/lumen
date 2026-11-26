package admin

// webauthn.go - the passkey method's transport (the auth-method seam,
// C1 round 2026-11-25). Handlers DECODE and ENCODE; the decisions live
// in internal/adminsvc. The normalized shape: login = begin/finish
// (public - the mint happens inside finish), register = begin/finish
// (authenticated - the credential binds to the session's user), the
// roster + delete are the settings page's verbs. The methods endpoint
// is the login UI's data source (a button per enabled method).

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/httpx"
)

// principal pulls the local authenticator's Principal out of the ctx.
func (h *Handler) principal(r *http.Request) adminsvc.Principal {
	p, _ := principalFrom(r.Context())
	return p
}

// authMethods is GET /admin/session/auth/methods - the login UI's data
// source (Round C1's normalization: a button per enabled method; oauth
// joins later without a UI rewrite). Public: it leaks nothing - method
// NAMES are the login page's own copy.
func (h *Handler) authMethods(w http.ResponseWriter, r *http.Request) {
	methods := []string{"password"}
	if h.svc.WebAuthnEnabled() {
		methods = append(methods, "passkey")
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"methods": methods})
}

// webauthnLoginBegin is POST /admin/session/webauthn/login/begin -
// PUBLIC (the login door's first half): the challenge options + the
// signed state the client carries back.
func (h *Handler) webauthnLoginBegin(w http.ResponseWriter, r *http.Request) {
	state, options, err := h.svc.PasskeyLoginBegin(r.Context())
	if err != nil {
		h.webauthnError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"state": state, "options": options})
}

// webauthnLoginFinish is POST /admin/session/webauthn/login/finish -
// PUBLIC (the second half): the assertion comes in, the session mint
// comes out. THE SAME response shape as the password login (mellon's
// cookie path is identical).
func (h *Handler) webauthnLoginFinish(w http.ResponseWriter, r *http.Request) {
	state, body, ok := h.readPasskeyBody(w, r)
	if !ok {
		return
	}
	token, expiresIn, p, err := h.svc.PasskeyLoginFinish(r.Context(), state, body, clientIP(r))
	var throttled *adminsvc.ThrottledError
	switch {
	case err == nil:
		slog.Info("session.grant.passkey", "user", p.Username, "ip", clientIP(r))
		// THE SAME payload as the password login - mellon's cookie path
		// is identical by construction
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"token": token, "expiresIn": expiresIn,
			"scopes": p.Scopes, "mustChange": p.MustChange, "disabled": p.Disabled,
		})
	case errors.As(err, &throttled):
		w.Header().Set("Retry-After", strconv.Itoa(int(throttled.RetryAfter.Seconds())+1))
		httpx.WriteErr(w, http.StatusTooManyRequests, "too many attempts")
	case errors.Is(err, adminsvc.ErrDisabled):
		slog.Info("session.deny.passkey.disabled", "ip", clientIP(r))
		httpx.WriteErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, adminsvc.ErrPasskeyState):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, adminsvc.ErrInvalidCredentials):
		// the LOG carries the reason (the wire stays indistinguishable):
		// the deny voice is one message, the audit is a diagnostic
		slog.Info("session.deny.passkey", "ip", clientIP(r), "err", err)
		httpx.WriteErr(w, http.StatusUnauthorized, "invalid credentials")
	default:
		slog.Error("session.passkey.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

// webauthnList is GET /admin/session/webauthn - the settings roster.
func (h *Handler) webauthnList(w http.ResponseWriter, r *http.Request) {
	p := h.principal(r)
	creds, err := h.svc.Passkeys(r.Context(), p.Username)
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	type passkey struct {
		ID         string `json:"id"`
		Label      string `json:"label"`
		CreatedAt  string `json:"createdAt"`
		LastUsedAt string `json:"lastUsedAt"`
	}
	out := make([]passkey, 0, len(creds))
	for _, c := range creds {
		out = append(out, passkey{ID: c.ID, Label: c.Label, CreatedAt: c.CreatedAt, LastUsedAt: c.LastUsedAt})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"passkeys": out})
}

// webauthnRegisterBegin is POST /admin/session/webauthn/register/begin -
// AUTHENTICATED (the credential binds to the session's user).
func (h *Handler) webauthnRegisterBegin(w http.ResponseWriter, r *http.Request) {
	p := h.principal(r)
	state, options, err := h.svc.PasskeyRegisterBegin(r.Context(), p)
	if err != nil {
		h.webauthnError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"state": state, "options": options})
}

// webauthnRegisterFinish is POST /admin/session/webauthn/register/finish -
// AUTHENTICATED: the attestation comes in, the credential is stored
// (labelled with the body's label). The response carries the roster row.
func (h *Handler) webauthnRegisterFinish(w http.ResponseWriter, r *http.Request) {
	p := h.principal(r)
	var in struct {
		Label    string          `json:"label"`
		Response json.RawMessage `json:"response"`
		State    string          `json:"state"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || in.State == "" || len(in.Response) == 0 {
		httpx.WriteErr(w, http.StatusBadRequest, "state and response required")
		return
	}
	row, err := h.svc.PasskeyRegisterFinish(r.Context(), p, in.State, in.Label, in.Response)
	if err != nil {
		h.webauthnError(w, r, err)
		return
	}
	slog.Info("passkey.registered", "user", p.Username, "id", row.ID)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"passkey": map[string]any{"id": row.ID, "label": row.Label, "createdAt": row.CreatedAt},
	})
}

// webauthnDelete is DELETE /admin/session/webauthn/{id} - the settings
// roster's remove. Never a lockout: every user holds a password.
func (h *Handler) webauthnDelete(w http.ResponseWriter, r *http.Request) {
	p := h.principal(r)
	id := chi.URLParam(r, "id")
	ok, err := h.svc.PasskeyDelete(r.Context(), p.Username, id)
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		httpx.WriteErr(w, http.StatusNotFound, "no such passkey")
		return
	}
	slog.Info("passkey.deleted", "user", p.Username, "id", id)
	w.WriteHeader(http.StatusNoContent)
}

// readPasskeyBody pulls the state + the raw assertion body (the
// library parses the browser's PublicKeyCredential JSON verbatim).
func (h *Handler) readPasskeyBody(w http.ResponseWriter, r *http.Request) (string, []byte, bool) {
	var in struct {
		State    string          `json:"state"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || in.State == "" || len(in.Response) == 0 {
		httpx.WriteErr(w, http.StatusBadRequest, "state and response required")
		return "", nil, false
	}
	return in.State, in.Response, true
}

// webauthnError maps the service's sentinels (one table, every passkey
// route - the same voice the session handlers speak).
func (h *Handler) webauthnError(w http.ResponseWriter, r *http.Request, err error) {
	var throttled *adminsvc.ThrottledError
	switch {
	case errors.As(err, &throttled):
		w.Header().Set("Retry-After", strconv.Itoa(int(throttled.RetryAfter.Seconds())+1))
		httpx.WriteErr(w, http.StatusTooManyRequests, "too many attempts")
	case errors.Is(err, adminsvc.ErrDisabled):
		httpx.WriteErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, adminsvc.ErrPasskeyState):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, adminsvc.ErrPasskeyExists):
		httpx.WriteErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, adminsvc.ErrPasskeyLabel):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, adminsvc.ErrPasskeyForbidden):
		httpx.WriteErr(w, http.StatusNotFound, "no such method") // an unconfigured feature is ABSENT
	case errors.Is(err, adminsvc.ErrInvalidCredentials):
		httpx.WriteErr(w, http.StatusUnauthorized, "invalid credentials")
	case errors.Is(err, adminsvc.ErrPasskeyNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "no such passkey")
	default:
		slog.Error("passkey.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}
