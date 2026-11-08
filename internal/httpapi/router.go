// Package httpapi is the §1 wire contract: chi routes (owner ruling,
// HUMAN-GATE #2 — chi), JSON shapes with keys always present, errors as
// {"error": msg} at the right status. No business logic here — parsing hands
// the validated query to the service and encodes its result.
//
// Layout doctrine (owner call 2026-11-08): handlers live ONE FILE PER
// RESOURCE (posts.go, search.go, photos.go, tags.go), shared plumbing in
// router.go/respond.go — the package must not accrete into a single file.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kanatabi/lumen/internal/service"
)

type Server struct{ svc *service.Service }

func New(svc *service.Service) *Server { return &Server{svc: svc} }

// jsonOnly declares intent, not protection — the header is set per response by
// writeJSON anyway; healthz stays plain.
func jsonOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "text/html" && r.URL.Path != "/healthz" {
			writeErr(w, http.StatusNotAcceptable, "json only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Router is the §1 surface — the mount table reads like the contract itself.
// M3 mounts /admin/* beside it (auth middleware chain, same file-per-resource
// discipline).
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID) // identity for lumen's op-level logs
	// Client IP for M3's login throttle, via chi's MODERN extraction (the
	// deprecated RealIP rewrote RemoteAddr; ClientIPFromXFFTrustedProxies
	// publishes GetClientIP(ctx) without touching the connection).
	// numTrustedProxies=1 states the topology assumption honestly: lumen is
	// cluster-internal and its DIRECT callers (earendil/mellon SSR pods) are
	// the one trusted hop, appending the real client IP behind Traefik.
	// Nothing reads it in M1; M3's throttle does — revisit the depth THEN.
	r.Use(middleware.ClientIPFromXFFTrustedProxies(1))
	r.Use(middleware.Recoverer) // a panic 500s, never kills a connection
	r.Use(jsonOnly)             // we answer JSON; nothing here serves HTML

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	r.Route("/api", func(api chi.Router) {
		api.Get("/posts", s.listPosts)
		api.Get("/posts/{slug}", s.getPost)
		api.Get("/search", s.search)
		api.Get("/search/suggest", s.suggest)
		api.Get("/photos", s.photos)
		api.Get("/photos/featured", s.featured)
		api.Get("/tags", s.tags)
		// /api/feed.xml: D2 open question + FE consumer lands at earendil M8 —
		// deliberately absent until its path is decided.
	})
	return r
}
