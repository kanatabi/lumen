package adminsvc

import (
	"context"
	"errors"
	"strings"

	"github.com/kanatabi/lumen/internal/session"
	"github.com/kanatabi/lumen/internal/store"
)

// Principal is the authenticated identity handed to the transport layer:
// who the bearer belongs to, what they may do (role), and whether they are
// in forced-password state. Role/mustChange come from the SAME row read that
// checks token_version — one query, no stale-claim surface, revocation-
// consistent by construction.
type Principal struct {
	Username   string
	Role       string
	MustChange bool
}

// Login is the credential decision: throttle keys (per-IP AND per-account,
// failures counted, success cleared), argon verification via store, and the
// MINT of a session carrying the live token_version. Handlers hand it the
// request facts and map the returned sentinels. The returned Principal is
// the login-RESPONSE payload (role + mustChange drive the portal's routing).
func (s *Service) Login(ctx context.Context, username, password, ip string) (string, int, Principal, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return "", 0, Principal{}, ErrEmptyCredentials // NOT a credential event: no throttle cost
	}
	ipKey := "ip:" + ip
	accountKey := "acc:" + strings.ToLower(username)
	for _, key := range []string{ipKey, accountKey} {
		if ok, wait := s.limiter.Allow(key); !ok {
			return "", 0, Principal{}, &ThrottledError{RetryAfter: wait}
		}
	}
	auth, err := s.store.Authenticate(ctx, username, password)
	if err != nil {
		if errors.Is(err, store.ErrWrongPassword) {
			s.limiter.Fail(ipKey)
			s.limiter.Fail(accountKey)
			return "", 0, Principal{}, ErrInvalidCredentials // ONE indistinguishable message: existence is not a signal
		}
		return "", 0, Principal{}, err
	}
	token, expiresIn, err := session.MintSession(s.jwtSecret, username, auth.TokenVersion)
	if err != nil {
		return "", 0, Principal{}, err
	}
	s.limiter.Success(ipKey)
	s.limiter.Success(accountKey)
	return token, expiresIn, Principal{Username: username, Role: auth.Role, MustChange: auth.MustChange}, nil
}

// VerifyBearer is the two-half check: signature (pinned HS256 — session.Parse)
// AND the live token row (which also answers role/must_change — fresh per
// request by design). Revocation lives here, not in middleware.
func (s *Service) VerifyBearer(ctx context.Context, bearer string) (Principal, error) {
	token, ok := strings.CutPrefix(bearer, "Bearer ")
	if !ok || token == "" {
		return Principal{}, ErrInvalidCredentials
	}
	claims, err := session.Parse(s.jwtSecret, token)
	if err != nil {
		return Principal{}, ErrInvalidCredentials
	}
	auth, exists, err := s.store.AuthFor(ctx, claims.Subject)
	if err != nil || !exists || auth.TokenVersion != claims.TV {
		return Principal{}, ErrInvalidCredentials // bumped = dead
	}
	return Principal{Username: claims.Subject, Role: auth.Role, MustChange: auth.MustChange}, nil
}

// Verify is the introspection verdict the admin plane owes the other
// planes (role-split ruling 2026-11-13): the SAME two-half check as every
// local request — pinned signature AND the live token_version row — but
// spoken as its own endpoint so a plane without users/JWT knowledge can
// ask. The cms plane caches the answer ~30 s (internal/authz); revocation
// propagates within that window plus the 12 h TTL semantics of §8.
// Errors are the transport's to map: invalid → 401, DB failure → the
// caller must NOT answer "valid".
func (s *Service) Verify(ctx context.Context, bearer string) (Principal, error) {
	return s.VerifyBearer(ctx, bearer)
}

// Revoke is logout: bump the version so every outstanding bearer dies.
func (s *Service) Revoke(ctx context.Context, bearer string) (string, error) {
	token, _ := strings.CutPrefix(bearer, "Bearer ")
	claims, err := session.Parse(s.jwtSecret, token)
	if err != nil {
		return "", ErrInvalidCredentials
	}
	if _, err := s.store.BumpTokenVersion(ctx, claims.Subject); err != nil {
		return "", err
	}
	return claims.Subject, nil
}

// BearerToken extracts the raw token from a header value (used by Revoke's
// callers who keep the header string).
func BearerToken(header string) string {
	tok, _ := strings.CutPrefix(header, "Bearer ")
	return tok
}
