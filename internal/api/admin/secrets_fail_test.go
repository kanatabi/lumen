package admin

// The sentinel→status TABLE, proven offline through the pure mapper (the
// HTTP harness behind it wants the integration plane). The 403 body matters
// beyond the others: it is config-gate WIRE CONTRACT COPY — mellon's UI and
// CI scripts match `{"error":"pinned at v<N> — unpin first"}` verbatim, so
// the RAW bytes, not just the parsed field, get pinned here.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kanatabi/lumen/internal/adminsvc"
)

func TestSecretFailMapping(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		handled  bool
		wantCode int
		wantMsg  string
	}{
		{"write lock: 403 with the contract copy", adminsvc.ErrPinned{Version: 2}, true,
			http.StatusForbidden, "pinned at v2 — unpin first"},
		{"stale cas stays a 409", adminsvc.ErrStaleCAS, true,
			http.StatusConflict, "stale cas — someone wrote first"},
		{"bad slug", adminsvc.ErrInvalidSlug, true,
			http.StatusBadRequest, "app/env must match ^[a-z][a-z0-9-]{0,31}$"},
		{"unknown path", adminsvc.ErrSecretNotFound, true,
			http.StatusNotFound, "secret path not found"},
		{"an unmapped error is NOT swallowed", errors.New("plane down"), false, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/admin/bao/app/dev", nil)
			if got := secretFail(rec, req, c.err); got != c.handled {
				t.Fatalf("handled = %v, want %v", got, c.handled)
			}
			if !c.handled {
				return
			}
			if rec.Code != c.wantCode {
				t.Fatalf("status %d, want %d", rec.Code, c.wantCode)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q: %v", rec.Body.String(), err)
			}
			if body["error"] != c.wantMsg {
				t.Fatalf("error %q, want %q", body["error"], c.wantMsg)
			}
		})
	}
}

func TestPinned403BodyIsRawContract(t *testing.T) {
	rec := httptest.NewRecorder()
	if !secretFail(rec, httptest.NewRequest(http.MethodPut, "/admin/bao/app/dev", nil), adminsvc.ErrPinned{Version: 7}) {
		t.Fatal("ErrPinned must be handled")
	}
	// the sibling agents' matcher sees EXACTLY this object (WriteJSON adds
	// the encoder's trailing newline): {"error":"pinned at v7 — unpin first"}
	want := `{"error":"pinned at v7 — unpin first"}` + "\n"
	if rec.Body.String() != want {
		t.Fatalf("raw 403 body %q, want %q", rec.Body.String(), want)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content-type %q", rec.Header().Get("Content-Type"))
	}
	if strings.Contains(rec.Body.String(), "Version") {
		t.Fatal("body must carry the copy, not a struct dump")
	}
}
