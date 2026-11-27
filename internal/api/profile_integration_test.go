package api_test

// The profile round (2026-11-29): the self-service display name + private
// email, end to end over the real HTTP surface. Auth-only (no users:manage);
// username is immutable; email is normalized and never lands in whoami's
// public shape.

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestProfileSelfService(t *testing.T) {
	harness := newAdminHarness(t)
	token := harness.login("fzr-test", "integration-pw")

	// unauth is a 401, never an empty profile
	if code, _ := harness.do(http.MethodGet, "/admin/profile", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("profile without bearer = %d, want 401", code)
	}

	var prof struct {
		Username string `json:"username"`
		Name     string `json:"name"`
		Email    string `json:"email"`
	}
	code, raw := harness.do(http.MethodGet, "/admin/profile", "", token)
	if code != http.StatusOK {
		t.Fatalf("profile read %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &prof); err != nil {
		t.Fatal(err)
	}
	if prof.Username != "fzr-test" || prof.Name != "fzr-test" {
		t.Fatalf("profile seed = %+v (name must backfill to username)", prof)
	}

	// write: trim + lowercase the email; the name is the byline candidate
	code, raw = harness.do(http.MethodPatch, "/admin/profile",
		`{"name":"  Faiz Test  ","email":"Fzr-Test@Example.COM"}`, token)
	if code != http.StatusOK {
		t.Fatalf("profile write %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &prof); err != nil {
		t.Fatal(err)
	}
	if prof.Name != "Faiz Test" || prof.Email != "fzr-test@example.com" {
		t.Fatalf("profile write = %+v", prof)
	}

	// whoami carries the same identity for the shell/menu
	var who struct {
		Username string `json:"username"`
		Name     string `json:"name"`
		Email    string `json:"email"`
	}
	code, raw = harness.do(http.MethodGet, "/admin/session", "", token)
	if code != http.StatusOK {
		t.Fatalf("whoami %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &who); err != nil {
		t.Fatal(err)
	}
	if who.Name != "Faiz Test" || who.Email != "fzr-test@example.com" {
		t.Fatalf("whoami identity = %+v", who)
	}

	// validation: blank name, malformed email, display-form email
	for _, body := range []string{
		`{"name":"","email":""}`,
		`{"name":"X","email":"not-an-email"}`,
		`{"name":"X","email":"Faiz <fzr-test@example.com>"}`,
	} {
		if code, _ := harness.do(http.MethodPatch, "/admin/profile", body, token); code != http.StatusBadRequest {
			t.Fatalf("invalid profile %s = %d, want 400", body, code)
		}
	}

	// empty email unsets it (stored NULL; uniqueness ignores NULL)
	code, raw = harness.do(http.MethodPatch, "/admin/profile", `{"name":"Faiz Test","email":""}`, token)
	if code != http.StatusOK {
		t.Fatalf("unset email %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &prof); err != nil {
		t.Fatal(err)
	}
	if prof.Email != "" {
		t.Fatalf("email after unset = %q, want empty", prof.Email)
	}
}
