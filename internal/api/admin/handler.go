// Package admin serves the §8/§9 HTTP surface. The role split (owner
// ruling 2026-11-13) made Mount ONE TABLE with PLANE TAGS: the admin
// plane registers the control surface (setup, the session mint, verify,
// whoami/logout/password, users, the Bao relay) and the cms plane
// registers the content desks (posts/photos/vocab/search/media) — a route
// that exists on exactly one table row exists on exactly the tagged
// planes, and routes_test walks the built router against the matrix so
// deployments cannot drift (the hotfix lesson, structurally retired).
// Handlers DECODE the request, call adminsvc, MAP sentinels to statuses,
// ENCODE — no store, no queue, no bao imports (owner layering ruling:
// controller -> service -> datastore).
package admin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/authz"
	"github.com/kanatabi/lumen/internal/httpx"
	"github.com/kanatabi/lumen/internal/role"
)

type Handler struct {
	svc *adminsvc.Service

	// intro is the cms plane's introspection seam (role split). nil means
	// LOCAL bearer verification — the admin plane owns the mint and the
	// users rows, and the legacy "all" dev plane keeps today's behavior.
	intro Introspector
}

// Introspector is the authz.Remote seam: one method, fakeable in the
// middleware's table test without a plane standing up beside it.
type Introspector interface {
	Verify(ctx context.Context, authorization string) (authz.Identity, error)
}

func New(svc *adminsvc.Service) *Handler { return &Handler{svc: svc} }

// NewCMS wires the content-desk handler with remote introspection: the
// cms plane holds no users table and no signing key, so every desk
// request's bearer is verified against the admin plane (cached, fail
// closed — internal/authz documents the exact staleness bound).
func NewCMS(svc *adminsvc.Service, intro Introspector) *Handler {
	return &Handler{svc: svc, intro: intro}
}

// ctxKey is package-private on purpose: the Principal only travels through
// THIS package's middleware -> handlers path.
type ctxKey int

const principalKey ctxKey = 1

// Mount registers /admin/* BY PLANE. Honest absence, never 404 theater:
// a route the role does not serve does not exist on that router at all.
func (h *Handler) Mount(r chi.Router, planes role.Planes) {
	r.Route("/admin", func(a chi.Router) {
		if planes.Has(role.PlaneAdmin) {
			// ---- CONTROL PLANE (lumen-admin) ----
			// the wizard pair answers before there are credentials — public by
			// design; register is atomically self-disabling (TOFU). The wizard
			// lives ENTIRELY here: the users table is admin's.
			a.Get("/setup", h.setupStatus)
			a.Post("/setup/register", h.setupRegister)

			a.Post("/session", h.login)                // THE mint — the only place sessions are born
			a.Post("/session/verify", h.verifySession) // the introspection door (see session.go)

			a.Group(func(auth chi.Router) {
				auth.Use(h.localAuthenticator) // signature + live users row: this plane OWNS both
				auth.Get("/session", h.whoami)
				auth.Delete("/session", h.logout)
				auth.Post("/session/password", h.changePassword)
				h.mountUsers(auth)
				if h.svc.SecretsEnabled() {
					h.mountSecrets(auth) // the §9 relay is a control-plane surface
				}
			})
		}
		if planes.Has(role.PlaneCMS) {
			// ---- CONTENT PLANE (lumen-staging / lumen-production) ----
			a.Group(func(desk chi.Router) {
				desk.Use(h.deskAuthenticator) // introspection here; local only in legacy "all"
				h.mountPosts(desk)
				h.mountPhotos(desk)
				h.mountVocab(desk)
				h.mountSearch(desk)
			})
		}
	})
}

// localAuthenticator is the single gate with three verdicts: dead bearer
// -> 401; must-change pending -> 428 Precondition Required (except the
// escape triad: whoami, change-password, logout); non-admin on user
// management -> 403. Roles/flags come fresh from the row read inside
// VerifyBearer — the JWT carries identity, not privileges.
func (h *Handler) localAuthenticator(next http.Handler) http.Handler {
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

// deskAuthenticator guards the cms plane's content routes. In a real cms
// deployment (intro wired) the bearer is INTROSPECTED against the admin
// plane — fail closed: an unreachable admin answers 503, a dead token
// answers 401, and the cache's 30 s staleness bound is the documented
// revocation-propagation window (internal/authz). In the legacy "all"
// dev plane (no intro) the desk shares the local gate — dev behavior is
// unchanged, which is the point of the "all" default.
func (h *Handler) deskAuthenticator(next http.Handler) http.Handler {
	if h.intro == nil {
		return h.localAuthenticator(next)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := h.intro.Verify(r.Context(), r.Header.Get("Authorization"))
		switch {
		case err == nil:
			// no passwordEscape here: the escape triad lives on the admin
			// plane — a must_change user has NOTHING to do on a desk until
			// the guided change is completed at the mint that issued them
			if id.MustChange {
				httpx.WriteErr(w, http.StatusPreconditionRequired, "password change required")
				return
			}
			p := adminsvc.Principal{Username: id.Username, Role: id.Role, MustChange: id.MustChange}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
		case errors.Is(err, authz.ErrInvalid):
			httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		default: // ErrAdminUnreachable (or anything unmapped): CLOSED
			httpx.WriteErr(w, http.StatusServiceUnavailable, "admin plane unreachable")
		}
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
