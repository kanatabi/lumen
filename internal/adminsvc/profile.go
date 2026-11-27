package adminsvc

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"unicode/utf8"

	"github.com/kanatabi/lumen/internal/store"
)

// The self-service profile (owner round 2026-11-29): display name + private
// email. Username stays the immutable login handle; email is account contact
// only — never public, never a byline. Validation lives here (the transport
// maps statuses); the store owns uniqueness.
var (
	ErrBadDisplayName = errors.New("display name must be 1-80 characters")
	ErrBadEmail       = errors.New("enter a valid email address")
)

const (
	maxDisplayName = 80
	maxEmail       = 254
)

func validDisplayName(name string) bool {
	n := utf8.RuneCountInString(strings.TrimSpace(name))
	return n >= 1 && n <= maxDisplayName
}

// normalizeEmail trims, lowercases and validates a PLAIN address. A display
// form ("Name <a@b>") is refused — the field is an address, not a header.
func normalizeEmail(email string) (string, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", true // optional: empty is stored as NULL
	}
	if len(email) > maxEmail {
		return "", false
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return "", false
	}
	return email, true
}

// Profile is the self read behind /admin/profile and whoami's name/email.
func (s *Service) Profile(ctx context.Context, id string) (store.UserRow, error) {
	return s.store.GetUserByID(ctx, id)
}

// UpdateProfile validates and writes the self-service fields. The caller is
// the authenticated principal itself — no users:manage scope is involved.
func (s *Service) UpdateProfile(ctx context.Context, id, name, email string) (store.UserRow, error) {
	name = strings.TrimSpace(name)
	if !validDisplayName(name) {
		return store.UserRow{}, ErrBadDisplayName
	}
	email, ok := normalizeEmail(email)
	if !ok {
		return store.UserRow{}, ErrBadEmail
	}
	return s.store.UpdateProfile(ctx, id, name, email)
}
