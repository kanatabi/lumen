// Package identity is the seam between the control plane (which OWNS the
// users lineage) and the app planes (which may only reference people by id).
//
// The role split (2026-11-13) and the tasks app-plane move (2026-11-29) make
// this boundary load-bearing: a cms deployment has no users table, so the
// tasks module resolves usernames, display names, and invitee searches
// through this package. The LOCAL implementation backs the legacy role=all
// dev plane; the REMOTE implementation speaks the admin plane's token-gated
// identity door.
//
// Privacy rule: email is a SEARCH KEY inside the control plane. It never
// crosses this interface — Person carries the id, the immutable username and
// the display name only.
package identity

import (
	"context"
	"errors"
)

var (
	// ErrNotFound means the identity is known to nobody (a deleted account,
	// a mistyped username). It maps to the caller's 400/404, never to 500.
	ErrNotFound = errors.New("identity: no such user")
	// ErrUnavailable means the directory could not be reached or answered
	// outside its contract. App reads fail loudly; writes keep their commit.
	ErrUnavailable = errors.New("identity: directory unavailable")
)

// Person is the only user projection an app plane may see.
type Person struct {
	ID       string
	Username string
	Name     string
}

// Directory is the identity read surface. The tasks module depends on this
// interface, never on the users table.
type Directory interface {
	// IDByUsername resolves the immutable login handle to the identity id.
	IDByUsername(ctx context.Context, username string) (string, error)
	// PeopleByIDs batch-hydrates display names. Missing ids (deleted
	// accounts) are simply absent from the map — that absence is the signal
	// the tasks app uses to self-heal orphaned memberships.
	PeopleByIDs(ctx context.Context, ids []string) (map[string]Person, error)
	// SearchPeople is the invite typeahead: username prefix (ranked first),
	// display-name prefix, or an EXACT case-insensitive email. Disabled
	// accounts are not suggested.
	SearchPeople(ctx context.Context, query string, limit int) ([]Person, error)
}
