package api_test

// identity_integration_test.go — the internal identity door the cms app
// planes call (2026-11-29). Token-gated, read-only, and deliberately blind
// to email in its responses: email is a MATCH KEY, never a field.

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestIdentityDoor(t *testing.T) {
	harness := newAdminHarness(t)

	var userID string
	if err := harness.pool.QueryRow(t.Context(),
		`UPDATE users SET display_name = 'Faiz Test', email = 'fzr-test@example.com'
		  WHERE username = 'fzr-test' RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	// the door is token-gated, independent of a user session
	if code, _ := harness.do(http.MethodGet, "/admin/identity/people?q=faiz", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("identity without token = %d, want 401", code)
	}

	var people struct {
		People []struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Name     string `json:"name"`
			Email    string `json:"email"`
		} `json:"people"`
	}
	code, raw := harness.do(http.MethodGet, "/admin/identity/people?q=faiz", "", "itest-identity")
	if code != http.StatusOK {
		t.Fatalf("people %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &people); err != nil {
		t.Fatal(err)
	}
	if len(people.People) != 1 || people.People[0].ID != userID ||
		people.People[0].Username != "fzr-test" || people.People[0].Name != "Faiz Test" {
		t.Fatalf("people wire = %s", raw)
	}
	if people.People[0].Email != "" {
		t.Fatalf("email leaked through the identity door: %s", raw)
	}

	// exact email (case-insensitive) works; a partial address never does
	for query, want := range map[string]int{
		"fzr-test@example.com": 1,
		"FZR-TEST@example.com": 1,
		"fzr-test@example":     0,
	} {
		var hits struct {
			People []json.RawMessage `json:"people"`
		}
		code, raw = harness.do(http.MethodGet,
			"/admin/identity/people?q="+query, "", "itest-identity")
		if code != http.StatusOK {
			t.Fatalf("email search %q: %d %s", query, code, raw)
		}
		if err := json.Unmarshal(raw, &hits); err != nil {
			t.Fatal(err)
		}
		if len(hits.People) != want {
			t.Fatalf("email search %q = %s, want %d rows", query, raw, want)
		}
	}

	// batch hydration
	var users struct {
		Users []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"users"`
	}
	code, raw = harness.do(http.MethodPost, "/admin/identity/users",
		`{"ids":["`+userID+`","00000000-0000-0000-0000-000000000000"]}`, "itest-identity")
	if code != http.StatusOK {
		t.Fatalf("users %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &users); err != nil {
		t.Fatal(err)
	}
	if len(users.Users) != 1 || users.Users[0].Name != "Faiz Test" {
		t.Fatalf("batch users = %s (unknown ids must be absent)", raw)
	}

	// username -> id resolution
	var resolved struct {
		ID string `json:"id"`
	}
	code, raw = harness.do(http.MethodGet, "/admin/identity/by-username/fzr-test", "", "itest-identity")
	if code != http.StatusOK {
		t.Fatalf("by-username %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.ID != userID {
		t.Fatalf("by-username id = %q, want %q", resolved.ID, userID)
	}
	if code, _ := harness.do(http.MethodGet, "/admin/identity/by-username/ghost-user-zz", "", "itest-identity"); code != http.StatusNotFound {
		t.Fatalf("unknown username = %d, want 404", code)
	}
}
