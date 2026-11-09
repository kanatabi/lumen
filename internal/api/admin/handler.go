// Package admin serves the §8/§9 HTTP surface: sessions, desk writes,
// photos, vocabulary, the palette door, the secrets relay. Handlers DECODE
// the request, call adminsvc, MAP sentinels to statuses, ENCODE — no store,
// no queue, no bao imports (owner layering ruling: controller -> service ->
// datastore). The auth middleware asks the SERVICE whether a bearer lives;
// revocation logic is not transport's to know.
package admin

import (
	"net"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/httpx"
)

type Handler struct{ svc *adminsvc.Service }

func New(svc *adminsvc.Service) *Handler { return &Handler{svc: svc} }

// Mount registers /admin/*: the session mint OUTSIDE auth, everything else
// behind it, the secrets relay only when the plane is configured (honest
// absence, never 404 theater).
func (h *Handler) Mount(r chi.Router) {
	r.Route("/admin", func(a chi.Router) {
		a.Post("/session", h.login)
		a.Group(func(auth chi.Router) {
			auth.Use(h.authenticator)
			auth.Delete("/session", h.logout)
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

func (h *Handler) authenticator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h.svc.VerifyBearer(r.Context(), r.Header.Get("Authorization")); err != nil {
			httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
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
