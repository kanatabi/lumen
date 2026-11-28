// desk_auth_test.go — the cms plane's middleware wiring, offline: the
// introspection verdicts map EXACTLY to statuses — valid → passes with
// the principal in ctx; revoked → 401; admin down → 503 CLOSED (never
// "probably valid"); mustChange → 428 with no escape (the escape doors
// live at the mint). A fake Introspector isolates the mapping; the
// client itself (cache, TTL, wire shape) is proven in internal/authz.
package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/authz"
	"github.com/kanatabi/lumen/internal/scopes"
)

type fakeIntro struct {
	id     authz.Identity
	err    error
	calls  int
	lastAH string
}

func (f *fakeIntro) Verify(_ context.Context, authorization string) (authz.Identity, error) {
	f.calls++
	f.lastAH = authorization
	if f.err != nil {
		return authz.Identity{}, f.err
	}
	return f.id, nil
}

func deskRouter(t *testing.T, intro *fakeIntro, captured *adminsvc.Principal) http.Handler {
	t.Helper()
	// the desk group's handlers touch the store; here we only prove the
	// GATE, so the terminal handler is a probe that echoes the principal
	h := NewCMS(nil, intro)
	r := chi.NewRouter()
	r.Route("/admin", func(a chi.Router) {
		a.Group(func(desk chi.Router) {
			desk.Use(h.deskAuthenticator)
			// the REAL content group applies its own scope after auth (the
			// tasks group applies the tasks trio instead)
			desk.Use(h.requireScope(scopes.ContentManage))
			desk.HandleFunc("/posts", func(w http.ResponseWriter, req *http.Request) {
				if p, ok := principalFrom(req.Context()); ok {
					*captured = p
				}
				w.WriteHeader(http.StatusTeapot) // reached-the-desk marker
			})
		})
	})
	return r
}

func TestDeskAuthenticatorTable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		intro  *fakeIntro
		wantSt int
		wantP  adminsvc.Principal
	}{
		{
			name:   "valid → claims travel to the desk",
			intro:  &fakeIntro{id: authz.Identity{Username: "ed", Scopes: []string{scopes.ContentManage}}},
			wantSt: http.StatusTeapot,
			wantP:  adminsvc.Principal{Username: "ed", Scopes: []string{scopes.ContentManage}},
		},
		{
			name:   "revoked → 401, desk never sees the request",
			intro:  &fakeIntro{err: authz.ErrInvalid},
			wantSt: http.StatusUnauthorized,
		},
		{
			name:   "admin plane down → 503 FAIL CLOSED",
			intro:  &fakeIntro{err: authz.ErrAdminUnreachable},
			wantSt: http.StatusServiceUnavailable,
		},
		{
			name:   "unmapped error → closed too (default branch is 503)",
			intro:  &fakeIntro{err: errors.New("something strange")},
			wantSt: http.StatusServiceUnavailable,
		},
		{
			name:   "mustChange → 428 (the lock lives at the mint, not here)",
			intro:  &fakeIntro{id: authz.Identity{Username: "ed", Scopes: []string{scopes.ContentManage}, MustChange: true}},
			wantSt: http.StatusPreconditionRequired,
		},
		{
			// the desks ARE the content:manage surface — a verified bearer
			// WITHOUT the scope is 403 (the spine's per-surface gate)
			name:   "verified bearer without content:manage → 403",
			intro:  &fakeIntro{id: authz.Identity{Username: "ed", Scopes: []string{scopes.UsersManage}}},
			wantSt: http.StatusForbidden,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got adminsvc.Principal
			h := deskRouter(t, tc.intro, &got)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/posts", nil))
			if rec.Code != tc.wantSt {
				t.Fatalf("status %d want %d (body %s)", rec.Code, tc.wantSt, rec.Body.String())
			}
			if tc.wantSt == http.StatusServiceUnavailable {
				if body := rec.Body.String(); !strings.Contains(body, "admin plane unreachable") {
					t.Fatalf("closed response must name the cause: %q", body)
				}
			}
			if !reflect.DeepEqual(got, tc.wantP) {
				t.Fatalf("principal %+v want %+v", got, tc.wantP)
			}
		})
	}
}

func TestDeskForwardsBearerAsIs(t *testing.T) {
	var got adminsvc.Principal
	intro := &fakeIntro{id: authz.Identity{Username: "u", Scopes: []string{scopes.UsersManage}}}
	h := deskRouter(t, intro, &got)
	req := httptest.NewRequest(http.MethodGet, "/admin/posts", nil)
	req.Header.Set("Authorization", "Bearer forward-me-verbatim")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if intro.lastAH != "Bearer forward-me-verbatim" {
		t.Fatalf("header mutated in flight: %q", intro.lastAH)
	}
}
