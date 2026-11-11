// Package admin serves the §8/§9 HTTP surface: sessions, desk writes,
// photos, vocabulary, the palette door, the secrets relay, the setup wizard.
// Handlers DECODE the request, call adminsvc, MAP sentinels to statuses,
// ENCODE — no store, no queue, no bao imports (owner layering ruling:
// controller -> service -> datastore). The auth middleware asks the SERVICE
// whether a bearer lives AND who it belongs to; revocation, forced-change and
// role gates are decisions with exactly one place to live.
package admin

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/httpx"
)

type Handler struct{ svc *adminsvc.Service }

func New(svc *adminsvc.Service) *Handler { return &Handler{svc: svc} }

// ctxKey is package-private on purpose: the Principal only travels through
// THIS package's middleware -> handlers path.
type ctxKey int

const principalKey ctxKey = 1

// Mount registers /admin/*: the session mint and the setup wizard OUTSIDE
// auth, everything else behind it, the secrets relay only when the plane is
// configured (honest absence, never 404 theater).
func (h *Handler) Mount(r chi.Router) {
	r.Route("/admin", func(a chi.Router) {
		// the wizard pair answers before there are credentials — public by
		// design; register is atomically self-disabling (TOFU)
		a.Get("/setup", h.setupStatus)
		a.Post("/setup/register", h.setupRegister)

		a.Post("/session", h.login)
		a.Group(func(auth chi.Router) {
			auth.Use(h.authenticator)
			auth.Get("/session", h.whoami)
			auth.Delete("/session", h.logout)
			auth.Post("/session/password", h.changePassword)
			h.mountUsers(auth)
			h.mountPosts(auth)
			h.mountPhotos(auth)
			h.mountVocab(auth)
			h.mountSearch(auth)
			if h.svc.SecretsEnabled() {
				h.mountSecrets(auth)
			}
		})
	})
}

// authenticator is the single gate with three verdicts: dead bearer -> 401;
// must-change pending -> 428 Precondition Required (except the escape
// triad: whoami, change-password, logout); non-admin on user management ->
// 403. Roles/flags come fresh from the row read inside VerifyBearer — the
// JWT carries identity, not privileges.
func (h *Handler) authenticator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := h.svc.VerifyBearer(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if p.MustChange && !passwordEscape(r) {
			httpx.WriteErr(w, http.StatusPreconditionRequired, "password change required")
			return
		}
		if p.Role != "admin" && strings.HasPrefix(r.URL.Path, "/admin/users") {
			httpx.WriteErr(w, http.StatusForbidden, "admin required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

// passwordEscape: the only three doors a locked-out (must_change) user may
// pass through. Everything else 428s — enforced here, not by UI courtesy.
func passwordEscape(r *http.Request) bool {
	switch r.URL.Path {
	case "/admin/session": // whoami (GET) and logout (DELETE) — locking
		return true // someone INTO a temp they cannot abandon is worse
		// than the risk of their own logout
	case "/admin/session/password":
		return r.Method == http.MethodPost
	}
	return false
}

func principalFrom(ctx context.Context) (adminsvc.Principal, bool) {
	p, ok := ctx.Value(principalKey).(adminsvc.Principal)
	return p, ok
}

// ---- shared handler kit ----

// envelope is the §8 list wire shape (mellon's Paged contract): total,
// items, 1-based clamped page, and the pageSize actually served.
type envelope[T any] struct {
	Total    int `json:"total"`
	Items    []T `json:"items"`
	Page     int `json:"page"`
	PageSize int `json:"pageSize"`
}

func clampPage(raw string) int {
	n, _ := strconv.Atoi(raw)
	return max(n, 1)
}

func clientIP(r *http.Request) string {

	// chi's ClientIPFromXFFTrustedProxies published the real IP at the
	// router level; RemoteAddr is the honest fallback for direct cluster
	// callers with no proxy hop.
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
