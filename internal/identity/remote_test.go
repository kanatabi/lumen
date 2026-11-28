package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRemoteDirectory pins the wire contract the cms planes depend on: the
// same token on every call, the exact paths, and the sentinel mapping that
// lets the tasks service tell "deleted account" from "directory down".
func TestRemoteDirectory(t *testing.T) {
	const token = "identity-token"
	people := []personJSON{
		{ID: "u-1", Username: "sarah", Name: "Sarah Lim"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/admin/identity/people":
			if r.URL.Query().Get("q") == "ghost" {
				_, _ = w.Write([]byte(`{"people":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"people":[{"id":"u-1","username":"sarah","name":"Sarah Lim"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/admin/identity/users":
			_, _ = w.Write([]byte(`{"users":[{"id":"u-1","username":"sarah","name":"Sarah Lim"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/admin/identity/by-username/sarah":
			_, _ = w.Write([]byte(`{"id":"u-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/admin/identity/by-username/ghost":
			http.Error(w, "no such user", http.StatusNotFound)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	directory := NewRemote(server.URL, token)
	ctx := context.Background()

	id, err := directory.IDByUsername(ctx, "sarah")
	if err != nil || id != "u-1" {
		t.Fatalf("IDByUsername = %q, %v", id, err)
	}
	if _, err := directory.IDByUsername(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown username err = %v, want ErrNotFound", err)
	}

	byID, err := directory.PeopleByIDs(ctx, []string{"u-1"})
	if err != nil || byID["u-1"].Name != "Sarah Lim" {
		t.Fatalf("PeopleByIDs = %+v, %v", byID, err)
	}

	found, err := directory.SearchPeople(ctx, "sar", 8)
	if err != nil || len(found) != 1 || found[0].Username != "sarah" {
		t.Fatalf("SearchPeople = %+v, %v", found, err)
	}
	if len(people) != 1 {
		t.Fatal("test fixture drift")
	}

	// a wrong token is an outage, not an empty directory
	if _, err := NewRemote(server.URL, "wrong").SearchPeople(ctx, "sar", 8); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unauthorized err = %v, want ErrUnavailable", err)
	}

	// an unreachable admin fails closed as unavailable
	dead := NewRemote("http://127.0.0.1:1", token)
	if _, err := dead.PeopleByIDs(ctx, []string{"u-1"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("dead directory err = %v, want ErrUnavailable", err)
	}
}
