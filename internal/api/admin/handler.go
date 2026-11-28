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
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/authz"
	"github.com/kanatabi/lumen/internal/httpx"
	"github.com/kanatabi/lumen/internal/role"
	"github.com/kanatabi/lumen/internal/scopes"
)

type Handler struct {
	svc *adminsvc.Service

	// intro is the cms plane's introspection seam (role split). nil means
	// LOCAL bearer verification — the admin plane owns the mint and the
	// users rows, and the legacy "all" dev plane keeps today's behavior.
	intro Introspector

	// notifications is the estate-generic module (M2.A). nil until the
	// composition root enables it: a process without the module mounts no
	// notification route at all (honest absence, like EnableBao).
	notifications notificationsService

	// tasks is the first lumen-native application (M2.B). It mounts on the
	// cms plane (staging/production), where its tables live; the admin
	// plane never enables it (2026-11-29 app-plane ruling).
	tasks tasksService

	// identityToken enables the internal identity door the app planes use
	// to resolve usernames/display names (and exact-email invite searches).
	// Empty = the door does not exist on this router.
	identityToken string
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

// EnableIdentity mounts the token-gated internal identity door. The app
// planes (cms) resolve people through it; the admin plane owns the users
// lineage and never exposes email through the response.
func (h *Handler) EnableIdentity(token string) { h.identityToken = token }

// EnableNotifications mounts the notifications module's routes (the
// composition-root opt-in, mirroring EnableBao on adminsvc). The read
// routes are any authenticated user; the SEND door is registered only
// when the shared token is configured — no token, no route.
func (h *Handler) EnableNotifications(svc notificationsService) {
	h.notifications = svc
}

// EnableTasks mounts the tasks module's routes (M2.B). The module owns all
// business rules; the handler side adds only the scope matrix and the wire.
func (h *Handler) EnableTasks(svc tasksService) {
	h.tasks = svc
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
				auth.Get("/profile", h.getProfile)
				auth.Patch("/profile", h.updateProfile)
				auth.Post("/session/password", h.changePassword)
			})

			// the AUTH-METHOD SEAM (the passkey round, C1): login begin/
			// finish is PUBLIC (the mint happens inside finish — the
			// second half of the login door); register/list/delete are
			// the setup surfaces (authenticated). The methods endpoint
			// is the login UI's data source (a button per enabled
			// method — oauth joins later without a UI rewrite).
			a.Get("/session/auth/methods", h.authMethods)
			a.Post("/session/webauthn/login/begin", h.webauthnLoginBegin)
			a.Post("/session/webauthn/login/finish", h.webauthnLoginFinish)
			a.Group(func(wa chi.Router) {
				wa.Use(h.localAuthenticator)
				wa.Get("/session/webauthn", h.webauthnList)
				wa.Post("/session/webauthn/register/begin", h.webauthnRegisterBegin)
				wa.Post("/session/webauthn/register/finish", h.webauthnRegisterFinish)
				wa.Delete("/session/webauthn/{id}", h.webauthnDelete)
			})
			// the management surfaces are the SPINE's scope gates: each
			// group declares its requirement in code, middleware checks the
			// principal's scopes (verified fresh per request)
			a.Group(func(mgmt chi.Router) {
				mgmt.Use(h.localAuthenticator, h.requireScope(scopes.UsersManage))
				h.mountUsers(mgmt)
				h.mountScopesCatalog(mgmt)
			})
			if h.svc.SecretsEnabled() {
				a.Group(func(bao chi.Router) {
					bao.Use(h.localAuthenticator, h.requireScope(scopes.SecretsAdmin))
					h.mountSecrets(bao) // the §9 relay is a control-plane surface
				})
			}

			// the notifications module (M2.A): the bell/panel routes are
			// ANY authenticated user (no scope — every account owns its
			// inbox), while the SEND door is a PUBLIC mount gated by ONE
			// shared token and registered ONLY when that token exists.
			// Unset = the path does not exist on this router at all.
			if h.notifications != nil {
				a.Group(func(inbox chi.Router) {
					inbox.Use(h.localAuthenticator)
					inbox.Get("/notifications", h.notificationList)
					inbox.Get("/notifications/unread-count", h.notificationUnreadCount)
					inbox.Post("/notifications/{id}/read", h.notificationMarkRead)
					inbox.Post("/notifications/read-all", h.notificationMarkAllRead)
				})
				if h.notifications.SendEnabled() {
					a.Post("/notifications", h.notificationSend)
				}
			}

			// the internal identity door (2026-11-29): token-gated, in-cluster,
			// read-only. The app planes (cms) hydrate names and resolve invitees
			// here; email is a match key and never a response field.
			if h.identityToken != "" {
				a.Route("/identity", func(id chi.Router) {
					id.Use(h.requireIdentityToken)
					id.Get("/people", h.identityPeople)
					id.Post("/users", h.identityUsers)
					id.Get("/by-username/{username}", h.identityByUsername)
				})
			}
		}
		if planes.Has(role.PlaneCMS) {
			// ---- CONTENT + APP PLANE (lumen-staging / lumen-production) ----
			// The authenticator only proves WHO the caller is; each surface
			// declares its own scope below. The tasks app lives here (its
			// tables are in this env's DB) and never demands content:manage.
			a.Group(func(desk chi.Router) {
				desk.Use(h.deskAuthenticator) // introspection here; local only in legacy "all"
				desk.Group(func(content chi.Router) {
					content.Use(h.requireScope(scopes.ContentManage))
					h.mountPosts(content)
					h.mountPhotos(content)
					h.mountVocab(content)
					h.mountSearch(content)
				})
				if h.tasks != nil {
					desk.Group(func(taskRoutes chi.Router) {
						taskRoutes.Use(h.requireAnyScope(
							scopes.TasksCreate, scopes.TasksUpdate, scopes.TasksManage))
						h.mountTasks(taskRoutes)
					})
				}
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
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

// requireScope is the spine's per-surface gate: the principal arrives in
// ctx from the authenticator; a missing scope is 403 with the scope's own
// name (the UI renders lumen's copy verbatim — ghost-field).
func (h *Handler) requireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := principalFrom(r.Context())
			if !ok || !slices.Contains(p.Scopes, scope) {
				httpx.WriteErr(w, http.StatusForbidden, scope+" required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireAnyScope is the tasks workspace's front door: a caller holding ANY
// of the listed scopes passes (reads and invite accept/decline need the
// workspace, not one specific verb). The refusal names every acceptable
// scope so the portal can render the requirement verbatim.
func (h *Handler) requireAnyScope(required ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := principalFrom(r.Context())
			if ok {
				for _, scope := range required {
					if slices.Contains(p.Scopes, scope) {
						next.ServeHTTP(w, r)
						return
					}
				}
			}
			httpx.WriteErr(w, http.StatusForbidden, strings.Join(required, " or ")+" required")
		})
	}
}

// deskAuthenticator guards the cms plane's surfaces. It authenticates only
// (must-change, token validity); each surface's scope (content:manage, the
// tasks trio) is applied by its group. In a real cms deployment the bearer is
// INTROSPECTED against the admin plane — fail closed: an unreachable admin answers 503, a dead token
// answers 401, and the cache's 30 s staleness bound is the documented
// revocation-propagation window (internal/authz). In the legacy "all"
// dev plane (no intro) the desk shares the local gate — dev behavior is
// unchanged, which is the point of the "all" default.
func (h *Handler) deskAuthenticator(next http.Handler) http.Handler {
	if h.intro == nil {
		// legacy "all": the desk gate rides the local authenticator; the
		// per-surface scope is applied by the caller's group.
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
			p := adminsvc.Principal{Username: id.Username, ID: id.ID, Scopes: id.Scopes, MustChange: id.MustChange}
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
