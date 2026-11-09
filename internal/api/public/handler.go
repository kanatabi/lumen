// Package public serves the §1 read API — the surface earendil consumes.
// One scope per process role (owner structure ruling): this package holds
// NO admin routes, NO store imports, NO policy; it parses requests, asks
// the service, writes responses.
package public

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/httpx"
	"github.com/kanatabi/lumen/internal/search"
	"github.com/kanatabi/lumen/internal/service"
)

type Handler struct {
	svc    *service.Service
	engine *search.Service // nil only in offline test wiring; readyz says so
}

func New(svc *service.Service, engine *search.Service) *Handler {
	return &Handler{svc: svc, engine: engine}
}

// Mount registers the probes and the /api contract. The mount table READS
// like the contract itself — that is the documentation property, kept.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	r.Get("/readyz", h.readyz)

	r.Route("/api", func(api chi.Router) {
		api.Get("/posts", h.listPosts)
		api.Get("/posts/{slug}", h.getPost)
		api.Get("/search", h.search)
		api.Get("/search/suggest", h.suggest)
		api.Get("/photos", h.photos)
		api.Get("/photos/featured", h.featured)
		api.Get("/tags", h.tags)
		// /api/feed.xml: D2 open question + FE consumer land together —
		// deliberately absent until its path is decided.
	})
}

// readyz: Postgres (truth) AND the engine (read path) must ANSWER, not
// merely accept TCP. Never fake readiness.
func (h *Handler) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if h.engine == nil {
		httpx.WriteErr(w, http.StatusServiceUnavailable, "engine not configured (offline test wiring)")
		return
	}
	if err := h.engine.Ready(ctx); err != nil {
		httpx.WriteErr(w, http.StatusServiceUnavailable, "engine unreachable")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
