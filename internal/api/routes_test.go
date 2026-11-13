// routes_test.go — THE anti-drift guard (hotfix lesson, made structural).
// For each role the router is built and WALKED (chi.Walk), and the method
// + pattern set must equal the Matrix rows tagged for that plane —
// exactly. If a mount and the table ever disagree (a forgotten role tag,
// a route added on one plane only), the battery fails before a
// deployment can. This test needs no DB, no engine, no network.
package api_test

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/api"
	"github.com/kanatabi/lumen/internal/api/admin"
	"github.com/kanatabi/lumen/internal/api/public"
	"github.com/kanatabi/lumen/internal/bao"
	"github.com/kanatabi/lumen/internal/role"
	"github.com/kanatabi/lumen/internal/service"
	"github.com/kanatabi/lumen/internal/throttle"
)

// builtRoutes walks a router built for the given planes and returns
// "METHOD /pattern" lines. The routers are built with EMPTY services —
// registration touches no state, only serving would, and we never serve.
func builtRoutes(t *testing.T, planes role.Planes, withBao bool) []string {
	t.Helper()
	svc := adminsvc.New(nil, nil, nil, "test-secret-32-bytes-aaaaaaaaaaaaaa", throttle.New(0))
	if withBao {
		// honest matrix entry needs the relay ENABLED; a fake client is
		// indistinguishable at registration time (mount only reads
		// SecretsEnabled) — wire it through the bao env contract.
		t.Setenv("LUMEN_BAO_ADDR", "http://127.0.0.1:1") // never dialed: registration only
		t.Setenv("LUMEN_BAO_ROLE_ID", "r")
		t.Setenv("LUMEN_BAO_SECRET_ID", "s")
		baoOn(t, svc)
	}
	pub := public.New(service.New(nil, nil), nil)
	h := api.NewRouter(pub, admin.New(svc), planes)
	mux, ok := h.(*chi.Mux)
	if !ok {
		t.Fatal("NewRouter must return *chi.Mux for the walk (single-table promise)")
	}
	var got []string
	if err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/") {
			return nil // middleware pseudo-entries are not routes
		}
		got = append(got, method+" "+route)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	return got
}

func baoOn(t *testing.T, svc *adminsvc.Service) {
	t.Helper()
	// bao.New() validates ENV ONLY (the AppRole login is LAZY — nothing is
	// dialed at build), so the client flips SecretsEnabled and the mount
	// table grows its §9 rows exactly as a configured admin plane would.
	c, err := bao.New()
	if err != nil {
		t.Fatalf("bao wiring for the matrix walk: %v", err)
	}
	svc.EnableBao(c)
}

func want(planes role.Planes, withBao bool) []string {
	out := []string{}
	for _, m := range api.Matrix {
		if !m.Serves(planes) {
			continue
		}
		if !withBao && strings.HasPrefix(m.Pattern, "/admin/bao") {
			continue
		}
		out = append(out, m.Method+" "+m.Pattern)
	}
	sort.Strings(out)
	return out
}

func TestRouteMatrix_AdminPlane(t *testing.T) {
	got := builtRoutes(t, role.Admin.Planes(), true)
	wantRows := want(role.Admin.Planes(), true)
	assertSame(t, "admin plane", got, wantRows)
}

func TestRouteMatrix_CMSPlane(t *testing.T) {
	got := builtRoutes(t, role.CMS.Planes(), false)
	// the cms plane never registers bao — the matrix must say so AND the
	// router must not grow it even if the secret happened to be configured
	assertSame(t, "cms plane", got, want(role.CMS.Planes(), false))
	for _, r := range got {
		if strings.Contains(r, "/admin/bao") {
			t.Fatalf("cms plane grew the relay: %s", r)
		}
		if strings.Contains(r, " /admin/session") && !strings.Contains(r, "verify") {
			t.Fatalf("cms plane grew the mint/whoami surface: %s", r)
		}
		if strings.HasPrefix(r, "POST /admin/session") && !strings.Contains(r, "verify") {
			t.Fatalf("cms plane can MINT sessions — ruling violation: %s", r)
		}
	}
}

func TestRouteMatrix_LegacyAllPlane(t *testing.T) {
	got := builtRoutes(t, role.All.Planes(), true)
	// union = every row
	assertSame(t, "all plane", got, want(role.PlanesAll, true))
}

// TestRouteMatrix_CMSServesNoContentOnAdmin — spot-checks the two ruling
// violations a union-only compare could still miss only if the matrix
// itself drifted; belt AND suspenders on the hotfix lesson.
func TestRouteMatrix_AdminPlaneHasNoContentSurface(t *testing.T) {
	for _, r := range builtRoutes(t, role.Admin.Planes(), true) {
		for _, forbidden := range []string{"/api/", "/admin/posts", "/admin/photos", "/admin/tags", "/admin/collections", "/admin/search"} {
			if strings.Contains(r, forbidden) {
				t.Fatalf("admin plane serves a content route: %s", r)
			}
		}
	}
}

func assertSame(t *testing.T, label string, got, wantRows []string) {
	t.Helper()
	if len(got) != len(wantRows) {
		t.Fatalf("%s: %d built routes vs %d matrix rows\ngot=%v\nwant=%v", label, len(got), len(wantRows), got, wantRows)
	}
	for i := range got {
		if got[i] != wantRows[i] {
			t.Fatalf("%s: drift at %d: built %q, matrix %q\ngot=%v\nwant=%v", label, i, got[i], wantRows[i], got, wantRows)
		}
	}
}
