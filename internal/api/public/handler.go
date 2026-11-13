// Package public serves the §1 read API — the surface earendil consumes.
// One scope per process role (owner structure ruling): this package holds
// NO admin routes, NO store imports, NO policy; it parses requests, asks
// the service, writes responses. The role split (2026-11-13) adds the
// PLANE rule: §1 reads are the CMS plane's surface — the admin plane
// serves probes only (no content tables in its DB to read from).
package public

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/httpx"
	"github.com/kanatabi/lumen/internal/role"
	"github.com/kanatabi/lumen/internal/search"
	"github.com/kanatabi/lumen/internal/service"
)

type Handler struct {
	svc    *service.Service
	engine *search.Service // nil only in offline test wiring; readyz says so

	// dbReady, when wired, powers the admin plane's readiness: truth IS
	// the only dependency there (engine absence is the honest shape, not
	// an outage). The composition root injects the pool ping.
	dbReady func(context.Context) error
}

func New(svc *service.Service, engine *search.Service) *Handler {
	return &Handler{svc: svc, engine: engine}
}

// EnableDBReadiness wires the truth-probe (composition roots only; tests
// wire it when they care about the admin-plane readyz shape).
func (h *Handler) EnableDBReadiness(ping func(context.Context) error) { h.dbReady = ping }

// Mount registers the probes and, on planes carrying the cms surface, the
// §1 contract. The mount table READS like the contract itself — that is
// the documentation property, kept.
func (h *Handler) Mount(r chi.Router, planes role.Planes) {
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		h.readyz(w, req, planes)
	})

	if !planes.Has(role.PlaneCMS) {
		return // admin plane: no content surface exists here, period
	}
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

// readyz: on the cms plane Postgres (truth) AND the engine (read path)
// must ANSWER, not merely accept TCP; on the admin plane truth is the
// whole dependency set. Never fake readiness.
func (h *Handler) readyz(w http.ResponseWriter, r *http.Request, planes role.Planes) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if planes.Has(role.PlaneCMS) {
		if h.engine == nil {
			httpx.WriteErr(w, http.StatusServiceUnavailable, "engine not configured (offline test wiring)")
			return
		}
		if err := h.engine.Ready(ctx); err != nil {
			httpx.WriteErr(w, http.StatusServiceUnavailable, "engine unreachable")
			return
		}
	}
	if h.dbReady != nil {
		if err := h.dbReady(ctx); err != nil {
			httpx.WriteErr(w, http.StatusServiceUnavailable, "database unreachable")
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
