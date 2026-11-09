// Package api composes the two scoped HTTP surfaces (public §1, admin §8/§9)
// behind the one root router: identity + client-IP extraction for the
// throttle, panic containment, and the JSON-only declaration. Which mounts
// exist is decided by the caller (nil admin = public-only wiring, the
// honest absence pattern).
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kanatabi/lumen/internal/api/admin"
	"github.com/kanatabi/lumen/internal/api/public"
	"github.com/kanatabi/lumen/internal/httpx"
)

func NewRouter(pub *public.Handler, adm *admin.Handler) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID) // identity for lumen's op-level logs
	// Client IP for the login throttle, via chi's MODERN extraction (the
	// deprecated RealIP rewrote RemoteAddr; this publishes GetClientIP(ctx)
	// without touching the connection). numTrustedProxies=1 states the
	// topology honestly: lumen is cluster-internal and its DIRECT callers
	// (earendil/mellon SSR pods) are the one trusted hop behind Traefik.
	r.Use(middleware.ClientIPFromXFFTrustedProxies(1))
	r.Use(middleware.Recoverer) // a panic 500s, never kills a connection
	r.Use(jsonOnly)             // we answer JSON; nothing here serves HTML

	pub.Mount(r)
	if adm != nil {
		adm.Mount(r)
	}
	return r
}

func jsonOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "text/html" && r.URL.Path != "/healthz" {
			httpx.WriteErr(w, http.StatusNotAcceptable, "json only")
			return
		}
		next.ServeHTTP(w, r)
	})
}
