package adminsvc

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"math/big"
	"regexp"
	"strings"

	"github.com/kanatabi/lumen/internal/scopes"
	"github.com/kanatabi/lumen/internal/session"
	"github.com/kanatabi/lumen/internal/store"
)

// Multi-user + setup-wizard service layer (owner ruling 2026-11-11): the
// portal bootstraps itself. An EMPTY users table means the estate is unclaimed
// — /setup registers the first admin (TOFU, race-proof via the store's
// advisory-lock insert) and mints their session. Admins add users with a
// SERVER-GENERATED temp password (shown exactly once, never logged, never
// stored in plaintext); the new user's first stop is the guided change
// (must_change -> 428 until done). Reset = fresh temp + must_change + session
// revocation. Roles are per-request facts (see session.go), never JWT claims.

var (
	ErrBadUsername  = errors.New("username must be 2-32 chars: lowercase letters, digits, dot, dash, underscore")
	ErrShortPass    = errors.New("password must be at least 10 characters")
	ErrSamePassword = errors.New("the new password must differ from the current one")
	// validUsername is deliberate: no spaces, no lookalike of the throttle keys.
	validUsername = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,31}$`)
)

// SetupStatus is the wizard probe: one bit of information, nothing enumerable.
func (s *Service) SetupStatus(ctx context.Context) (bool, error) {
	n, err := s.store.CountUsers(ctx)
	return n == 0, err
}

// RegisterFirstAdmin claims the estate: only while the table is empty,
// atomically; returns the login-shaped session so the wizard IS the door.
func (s *Service) RegisterFirstAdmin(ctx context.Context, username, password string) (string, int, Principal, error) {
	username = strings.TrimSpace(username)
	if !validUsername.MatchString(username) {
		return "", 0, Principal{}, ErrBadUsername
	}
	if len(password) < 10 {
		return "", 0, Principal{}, ErrShortPass
	}
	hash, err := session.HashPassword(password)
	if err != nil {
		return "", 0, Principal{}, err
	}
	created, err := s.store.InsertFirstAdmin(ctx, username, hash)
	if err != nil {
		return "", 0, Principal{}, err
	}
	if !created {
		return "", 0, Principal{}, store.ErrUsersExist
	}
	// audit line: the single most consequential event in a deployment — who
	// claimed the estate — logged WITHOUT credentials, as always
	slog.Info("setup.register", "user", username)
	auth, exists, err := s.store.AuthFor(ctx, username)
	if err != nil || !exists {
		return "", 0, Principal{}, store.ErrUsersExist // vanished mid-flight: treat as claimed
	}
	token, expiresIn, err := session.MintSession(s.jwtSecret, username, auth.TokenVersion, auth.Scopes)
	if err != nil {
		return "", 0, Principal{}, err
	}
	return token, expiresIn, Principal{Username: username, ID: auth.ID, Scopes: auth.Scopes, MustChange: auth.MustChange, Disabled: auth.Disabled}, nil
}

// ListUsers is admin-eyes-only (the transport enforces the gate; the service
// stays policy-free by convention).
func (s *Service) ListUsers(ctx context.Context) ([]store.UserRow, error) {
	return s.store.ListUsers(ctx)
}

// CreateUserWithTemp provisions an account with a server-generated temp
// password (must_change armed), granted EXACT scopes — roles are gone
// (the spine ruling): the admin's picker IS the grant. The temp is
// returned EXACTLY ONCE — the caller's response, nothing else; it is not
// logged and not recoverable.
func (s *Service) CreateUserWithTemp(ctx context.Context, username string, granted []string) (string, error) {
	username = strings.TrimSpace(username)
	if !validUsername.MatchString(username) {
		return "", ErrBadUsername
	}
	for _, sc := range granted {
		if !scopes.Valid(sc) {
			return "", ErrBadUsername
		}
	}
	temp := generateTempPassword()
	hash, err := session.HashPassword(temp)
	if err != nil {
		return "", err
	}
	if err := s.store.InsertUser(ctx, username, hash, granted); err != nil {
		return "", err
	}
	slog.Info("users.create", "user", username, "scopes", granted)
	return temp, nil
}

// ResetPassword issues a NEW temp, arms must_change and bumps token_version
// (the account's live sessions die on the spot). Once-only reveal as create.
func (s *Service) ResetPassword(ctx context.Context, username string) (string, error) {
	temp := generateTempPassword()
	hash, err := session.HashPassword(temp)
	if err != nil {
		return "", err
	}
	if err := s.store.SetPassword(ctx, username, hash, true, true); err != nil {
		return "", err
	}
	slog.Info("users.reset", "user", username)
	return temp, nil
}

// ChangeOwnPassword is the forced flow's destination: verify the current
// credential, write the new one, CLEAR must_change, bump token_version (the
// session that just changed it dies too — the portal re-logs in through the
// fresh password; changing a password must not inherit old sessions).
func (s *Service) ChangeOwnPassword(ctx context.Context, username, current, next string) error {
	if len(next) < 10 {
		return ErrShortPass
	}
	if current == next {
		return ErrSamePassword
	}
	if _, err := s.store.Authenticate(ctx, username, current); err != nil {
		return err // ErrWrongPassword maps to 401 at the edge
	}
	hash, err := session.HashPassword(next)
	if err != nil {
		return err
	}
	if err := s.store.SetPassword(ctx, username, hash, false, true); err != nil {
		return err
	}
	slog.Info("users.password_changed", "user", username)
	return nil
}

// temp syllable pool: 24 CVC units -> a 4-unit word-pair + 4 digits,
// human-readable, ~55 bits of entropy per temp. Long enough to survive the
// handoff, disposable by design (must_change on first login).
var tempSyllables = []string{
	"ba", "ko", "mi", "re", "sa", "ne", "lu", "va",
	"de", "gi", "bo", "ku", "pa", "le", "ti", "ro",
	"nu", "ma", "se", "ki", "du", "fa", "lo", "ta",
}

func generateTempPassword() string {
	pick := func(n int) *big.Int { v, _ := rand.Int(rand.Reader, big.NewInt(int64(n))); return v }
	var b strings.Builder
	for i := range 4 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(tempSyllables[int(pick(len(tempSyllables)).Int64())])
		b.WriteString(tempSyllables[int(pick(len(tempSyllables)).Int64())])
	}
	b.WriteString("-")
	for range 4 {
		b.WriteByte(byte('0' + int(pick(10).Int64())))
	}
	return b.String()
}

// ErrBadScope is the grammar refusal for scope assignment and catalog
// onboarding (existence is the catalog's own question — the store's FK).
var ErrBadScope = errors.New("scope must be <domain>:<verb>, lowercase")

// ErrDisabled re-exports the store's disabled-account sentinel — the
// handler maps it to 403 (distinct from invalid credentials: the undo is
// the admin's re-enable, not a retype).
var ErrDisabled = store.ErrDisabled

// ErrSelfDelete / ErrLastManager re-export the delete guards (400 /
// 409 at the edge — the counts ride the 409 copy).
var ErrSelfDelete = store.ErrSelfDelete
var ErrNoSuchUser = store.ErrNoSuchUser
var ErrLastManager = store.ErrLastManager

// UpdateScopes replaces the user's scope set and BUMPS token_version (the
// spine ruling: a demotion is immediate, the user's other sessions die).
// Grammar is checked here; catalog existence is the store's FK.
func (s *Service) UpdateScopes(ctx context.Context, username string, granted []string) (int, error) {
	for _, sc := range granted {
		if !scopes.Valid(sc) {
			return 0, ErrBadScope
		}
	}
	tv, err := s.store.SetScopes(ctx, username, granted)
	if err != nil {
		return 0, err
	}
	return tv, nil
}

// ListCatalog reads the portal-managed scope registry.
func (s *Service) ListCatalog(ctx context.Context) ([]store.CatalogEntry, error) {
	return s.store.ListCatalog(ctx)
}

// AddScope onboards a scope (the spine ruling: the portal, never manual
// SQL). Grammar is checked here; duplicate adds are idempotent at the store.
func (s *Service) AddScope(ctx context.Context, scope, description string) (bool, error) {
	if !scopes.Valid(scope) {
		return false, ErrBadScope
	}
	if strings.TrimSpace(description) == "" {
		return false, ErrBadScope
	}
	return s.store.AddScope(ctx, scope, description)
}

// DeleteScope removes a catalog scope; the store refuses while any user
// holds it (the handler speaks the refusal with the holder counts).
func (s *Service) DeleteScope(ctx context.Context, scope string) (bool, int, error) {
	if !scopes.Valid(scope) {
		return false, 0, ErrBadScope
	}
	return s.store.DeleteScope(ctx, scope)
}

// RevokeSessions kicks the user's live sessions WITHOUT touching their
// password (the admin's "sign out everywhere"). The revocation lever is
// token_version — the same one logout and password changes use.
func (s *Service) RevokeSessions(ctx context.Context, username string) (int, error) {
	tv, err := s.store.BumpTokenVersion(ctx, username)
	if err != nil {
		return 0, err
	}
	slog.Info("users.revoke_sessions", "user", username)
	return tv, nil
}

// ForceChange arms must_change WITHOUT touching the password — the user's
// NEXT request meets the 428 gate (their live session is the one that
// completes it, exactly like wizard temps).
func (s *Service) ForceChange(ctx context.Context, username string) error {
	found, err := s.store.ArmMustChange(ctx, username)
	if err != nil {
		return err
	}
	if !found {
		return store.ErrNoSuchUser
	}
	slog.Info("users.force_change", "user", username)
	return nil
}

// SetUserDisabled flips the account. Disabling BUMPS token_version — the
// login door AND the live sessions die together; enabling re-admits a
// fresh login.
func (s *Service) SetUserDisabled(ctx context.Context, username string, disabled bool) (int, error) {
	tv, err := s.store.SetDisabled(ctx, username, disabled)
	if err != nil {
		return 0, err
	}
	slog.Info("users.disabled", "user", username, "disabled", disabled)
	return tv, nil
}

// DeleteUser removes the account — the store carries the TWO guards
// (self-deletion, the last users:manage holder). The audit line names the
// ACTOR: an account deletion is the most consequential admin verb.
func (s *Service) DeleteUser(ctx context.Context, actor, username string) error {
	if err := s.store.DeleteUser(ctx, actor, username); err != nil {
		return err
	}
	slog.Info("users.delete", "actor", actor, "user", username)
	return nil
}
