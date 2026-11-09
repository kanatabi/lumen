package adminsvc

import (
	"context"
	"errors"
	"strings"

	"github.com/kanatabi/lumen/internal/session"
	"github.com/kanatabi/lumen/internal/store"
)

// Login is the credential decision: throttle keys (per-IP AND per-account,
// failures counted, success cleared), argon verification via store, and the
// MINT of a session carrying the live token_version. Handlers hand it the
// request facts and map the returned sentinels.
func (s *Service) Login(ctx context.Context, username, password, ip string) (string, int, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return "", 0, ErrEmptyCredentials // NOT a credential event: no throttle cost
	}
	ipKey := "ip:" + ip
	accountKey := "acc:" + strings.ToLower(username)
	for _, key := range []string{ipKey, accountKey} {
		if ok, wait := s.limiter.Allow(key); !ok {
			return "", 0, &ThrottledError{RetryAfter: wait}
		}
	}
	tokenVersion, err := s.store.Authenticate(ctx, username, password)
	if err != nil {
		if errors.Is(err, store.ErrWrongPassword) {
			s.limiter.Fail(ipKey)
			s.limiter.Fail(accountKey)
			return "", 0, ErrInvalidCredentials // ONE indistinguishable message: existence is not a signal
		}
		return "", 0, err
	}
	token, expiresIn, err := session.MintSession(s.jwtSecret, username, tokenVersion)
	if err != nil {
		return "", 0, err
	}
	s.limiter.Success(ipKey)
	s.limiter.Success(accountKey)
	return token, expiresIn, nil
}

// VerifyBearer is the two-half check: signature (pinned HS256 — session.Parse)
// AND the live token_version row. Revocation lives here, not in middleware.
func (s *Service) VerifyBearer(ctx context.Context, bearer string) error {
	token, ok := strings.CutPrefix(bearer, "Bearer ")
	if !ok || token == "" {
		return ErrInvalidCredentials
	}
	claims, err := session.Parse(s.jwtSecret, token)
	if err != nil {
		return ErrInvalidCredentials
	}
	tokenVersion, exists, err := s.store.TokenVersion(ctx, claims.Subject)
	if err != nil || !exists || tokenVersion != claims.TV {
		return ErrInvalidCredentials // bumped = dead
	}
	return nil
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
