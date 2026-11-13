// authz_test.go — the introspection client's contract, offline and
// deterministic: verdict mapping (valid→identity, revoked→ErrInvalid,
// admin-down→ErrAdminUnreachable), bearer-forwarded-as-is, cache
// hit/expiry on a FAKE CLOCK, and the no-negative-cache honesty.
package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAdmin is the admin plane as the client sees it: a canned verdict
// (flippable), with a hit counter and a capture of the last request.
type fakeAdmin struct {
	srv      *httptest.Server
	hits     atomic.Int64
	lastAuth atomic.Value // string — the Authorization header received
	// verdicts: status to answer (200 with identity, or 401)
	status   atomic.Int32
	identity Identity
}

func newFakeAdmin(t *testing.T, id Identity) *fakeAdmin {
	f := &fakeAdmin{identity: id}
	f.status.Store(int32(http.StatusOK))
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.lastAuth.Store(r.Header.Get("Authorization"))
		if r.Method != http.MethodPost || r.URL.Path != "/admin/session/verify" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch f.status.Load() {
		case http.StatusOK:
			if err := json.NewEncoder(w).Encode(f.identity); err != nil {
				t.Errorf("encode: %v", err)
			}
		case http.StatusUnauthorized:
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
		default:
			http.Error(w, `{"error":"boom"}`, int(f.status.Load()))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// clock is the fake-time seam.
func newClock() (*Remote, *time.Time) {
	now := time.Unix(1700000000, 0)
	r := &Remote{ttl: CacheTTL, cache: map[string]cacheEntry{}, Now: func() time.Time { return now }}
	return r, &now
}

func TestVerifyTable(t *testing.T) {
	who := Identity{Username: "fzr", Role: "admin", MustChange: false}

	cases := []struct {
		name    string
		status  int
		authz   string
		wantErr error
		want    Identity
	}{
		{"valid token → identity claims", http.StatusOK, "Bearer alive-token", nil, who},
		{"revoked/expired token → ErrInvalid", http.StatusUnauthorized, "Bearer dead-token", ErrInvalid, Identity{}},
		{"editor keeps its role verbatim", http.StatusOK, "Bearer ed", nil, Identity{Username: "ed", Role: "editor", MustChange: true}},
		{"no bearer prefix → ErrInvalid (cheap local reject)", http.StatusOK, "", ErrInvalid, Identity{}},
		{"empty bearer → ErrInvalid", http.StatusOK, "Bearer ", ErrInvalid, Identity{}},
		{"500 from admin → fail CLOSED as unreachable", http.StatusInternalServerError, "Bearer x", ErrAdminUnreachable, Identity{}},
		{"404 from admin (misconfig) → unreachable", http.StatusNotFound, "Bearer x", ErrAdminUnreachable, Identity{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want.Username != "" {
				who = tc.want // the editor row carries its own identity
			}
			f := newFakeAdmin(t, who)
			f.status.Store(int32(tc.status))
			r := NewRemote(f.srv.URL)
			if tc.authz == "" {
				f.srv.Close() // nothing must be reached: the reject is local
				r2 := NewRemote("http://127.0.0.1:1/never")
				_, err := r2.Verify(context.Background(), tc.authz)
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err=%v want %v", err, tc.wantErr)
				}
				return
			}
			got, err := r.Verify(context.Background(), tc.authz)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err=%v want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("identity=%+v want %+v", got, tc.want)
			}
			if f.hits.Load() > 0 && f.lastAuth.Load().(string) != tc.authz {
				t.Fatalf("bearer must be forwarded AS-IS: got %q want %q", f.lastAuth.Load(), tc.authz)
			}
		})
	}
}

func TestAdminDownFailsClosed(t *testing.T) {
	f := newFakeAdmin(t, Identity{Username: "x", Role: "admin"})
	base := f.srv.URL
	f.srv.Close() // the plane is GONE: transport error, not a verdict
	r := NewRemote(base)
	_, err := r.Verify(context.Background(), "Bearer whatever")
	if !errors.Is(err, ErrAdminUnreachable) {
		t.Fatalf("want ErrAdminUnreachable (closed), got %v", err)
	}
}

func TestTTLCacheFakeClock(t *testing.T) {
	f := newFakeAdmin(t, Identity{Username: "fzr", Role: "admin"})
	r, now := newClock()
	r.endpoint = f.srv.URL + "/admin/session/verify"
	r.client = f.srv.Client()

	authzHdr := "Bearer cached-token"
	if _, err := r.Verify(context.Background(), authzHdr); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 1 {
		t.Fatalf("first verify must hit the plane, hits=%d", f.hits.Load())
	}
	// within TTL: cache hit, ZERO new calls
	*now = now.Add(CacheTTL - time.Millisecond)
	if _, err := r.Verify(context.Background(), authzHdr); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 1 {
		t.Fatalf("TTL window must be served from cache, hits=%d", f.hits.Load())
	}
	// the documented staleness bound in action: the plane flips to REVOKED
	// while the entry lives — the client still answers the cached verdict…
	f.status.Store(http.StatusUnauthorized)
	if _, err := r.Verify(context.Background(), authzHdr); err != nil {
		t.Fatalf("in-window: cached OK expected, got %v", err)
	}
	// …until the TTL expires, and not one moment after: revocation
	// propagation is bounded at CacheTTL.
	*now = now.Add(2 * time.Millisecond)
	if _, err := r.Verify(context.Background(), authzHdr); !errors.Is(err, ErrInvalid) {
		t.Fatalf("post-expiry: want ErrInvalid, got %v", err)
	}
	if f.hits.Load() != 2 {
		t.Fatalf("expiry must re-ask the plane, hits=%d", f.hits.Load())
	}
}

func TestDenialsAreNeverCached(t *testing.T) {
	f := newFakeAdmin(t, Identity{Username: "fzr", Role: "admin"})
	f.status.Store(http.StatusUnauthorized)
	r, _ := newClock()
	r.endpoint = f.srv.URL + "/admin/session/verify"
	r.client = f.srv.Client()
	for i := 1; i <= 3; i++ {
		if _, err := r.Verify(context.Background(), "Bearer dead"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("iter %d: %v", i, err)
		}
		if int64(i) != f.hits.Load() {
			t.Fatalf("a 401 must never be cached: hits=%d want %d", f.hits.Load(), i)
		}
	}
}

func TestCacheKeyedByTokenHash(t *testing.T) {
	f := newFakeAdmin(t, Identity{Username: "fzr", Role: "admin"})
	r, _ := newClock()
	r.endpoint = f.srv.URL + "/admin/session/verify"
	r.client = f.srv.Client()
	if _, err := r.Verify(context.Background(), "Bearer one"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Verify(context.Background(), "Bearer two"); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 2 {
		t.Fatalf("distinct tokens must not share a cache entry: hits=%d", f.hits.Load())
	}
	if _, err := r.Verify(context.Background(), "Bearer one"); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 2 {
		t.Fatalf("known token should hit cache: hits=%d", f.hits.Load())
	}
}

func TestNewRemoteEndpointShape(t *testing.T) {
	for _, base := range []string{"http://lumen-admin:8080", "http://lumen-admin:8080/"} {
		r := NewRemote(base)
		if want := "http://lumen-admin:8080/admin/session/verify"; r.endpoint != want {
			t.Fatalf("base %q → %q want %q", base, r.endpoint, want)
		}
	}
	// the wire-shape doc: the request is POST with the bearer header and
	// NO body — assert the client sends exactly that.
	var bodySeen string
	var methodSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methodSeen = r.Method
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		bodySeen = strings.TrimSpace(string(buf[:n]))
		_ = json.NewEncoder(w).Encode(Identity{Username: "u", Role: "editor"})
	}))
	defer srv.Close()
	r := NewRemote(srv.URL)
	if _, err := r.Verify(context.Background(), "Bearer tok"); err != nil {
		t.Fatal(err)
	}
	if methodSeen != http.MethodPost || bodySeen != "" {
		t.Fatalf("verify wire shape: method=%q body=%q", methodSeen, bodySeen)
	}
}
