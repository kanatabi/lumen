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
	token, expiresIn, p, err := h.svc.Login(r.Context(), in.Username, in.Password, clientIP(r))
	var throttled *adminsvc.ThrottledError
	switch {
	case err == nil:
		slog.Info("session.grant", "user", in.Username)
		// camelCase per mellon's AdminApi — the executable spec. role +
		// mustChange route the portal: 428-gated users land on /setup.
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"token": token, "expiresIn": expiresIn,
			"role": p.Role, "mustChange": p.MustChange,
		})
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

// verifySession is POST /admin/session/verify — the introspection door of
// the role split (the admin plane's answer to "is this bearer alive, and
// who is it"). WIRE CONTRACT (mellon/CI agents match verbatim):
//
//	POST /admin/session/verify        Authorization: Bearer <token> (as-is)
//	                                  (empty body; nothing else consulted)
//	200 {"username": string, "role": "admin"|"editor", "mustChange": bool}
//	401 {"error": "invalid token"}   (bad signature/expiry/revoked/unknown user)
//	500 {"error": "internal error"}  (authority unreadable — never "valid")
//
// Registered OUTSIDE the auth group on purpose: the bearer being asked
// about IS the request's only credential. Trust: ClusterIP-only path
// (INFRA §2 — lumen has no public ingress); the admin plane holds the
// real auth decision.
func (h *Handler) verifySession(w http.ResponseWriter, r *http.Request) {
	p, err := h.svc.Verify(r.Context(), r.Header.Get("Authorization"))
	switch {
	case err == nil:
		slog.Info("session.verify", "user", p.Username)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"username": p.Username, "role": p.Role, "mustChange": p.MustChange,
		})
	case errors.Is(err, adminsvc.ErrInvalidCredentials):
		httpx.WriteErr(w, http.StatusUnauthorized, "invalid token")
	default:
		slog.Error("session.verify.error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}
