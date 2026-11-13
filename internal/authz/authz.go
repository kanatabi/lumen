// Package authz is the cms plane's session-introspection client (role-split
// ruling, 2026-11-13). The cms plane holds NO users table and NO signing
// key — an auth decision is therefore asked of the admin plane:
//
//	POST {LUMEN_ADMIN_URL}/admin/session/verify
//	Authorization: Bearer <token>      (forwarded AS-IS; empty body)
//	→ 200 {"username","role","mustChange"}  |  401 {"error":"invalid token"}
//
// TRUST (documented ruling): no extra shared secret on the call. The path
// is ClusterIP-only — lumen has no public ingress by design (INFRA §2) —
// and the admin plane holds the REAL auth check (signature + live
// token_version row). Anyone who can reach verify already rides the
// internal network mellon's traffic rides.
//
// STALENESS BOUND: verdicts are cached ~30 s (CacheTTL) keyed by the
// token's SHA-256, so a revocation (logout / reset / demotion) propagates
// to a cms desk request at most 30 s late; session-TTL semantics (12 h)
// live in DESIGN §8. Only OK verdicts are cached — a 401 is re-asked
// immediately (denials are cheap to be fresh about; positives are the
// latency-sensitive path).
//
// FAILURE POSTURE: FAIL CLOSED. An unreachable admin plane (transport
// error, timeout, or any status outside 200/401) answers
// ErrAdminUnreachable, which the cms middleware maps to 503 "admin plane
// unreachable" — a down admin plane must never degrade into "probably
// still valid".
package authz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CacheTTL is the documented staleness bound; one knob, greppable.
const CacheTTL = 30 * time.Second

// RequestTimeout bounds ONE verify call: the middleware's whole budget is
// the client request, so a hung admin plane must stall a desk write at
// most this long before the fail-closed verdict.
const RequestTimeout = 5 * time.Second

// Identity is the verdict the admin plane returns about a bearer — the
// same facts whoami carries (roles are row facts, never JWT claims).
type Identity struct {
	Username   string `json:"username"`
	Role       string `json:"role"`
	MustChange bool   `json:"mustChange"`
}

var (
	// ErrInvalid: the token is dead (signature/expiry/token_version).
	ErrInvalid = errors.New("invalid token")
	// ErrAdminUnreachable: the admin plane could not ANSWER — the caller
	// must fail closed (503), never fall open.
	ErrAdminUnreachable = errors.New("admin plane unreachable")
)

// Remote is the introspecting client with its TTL cache. The zero-value
// clock/transport seams exist for table tests only (fake clock, hit-count
// transport) — production gets the defaults.
type Remote struct {
	endpoint string
	client   *http.Client
	ttl      time.Duration

	Now func() time.Time // fake-clock seam (tests); nil = time.Now

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	id      Identity
	expires time.Time
}

// NewRemote builds the client for one admin plane. base is LUMEN_ADMIN_URL
// (scheme://host[:port], trailing slash tolerated); the verify path is
// appended exactly once.
func NewRemote(base string) *Remote {
	return &Remote{
		endpoint: strings.TrimRight(base, "/") + "/admin/session/verify",
		client:   &http.Client{Timeout: RequestTimeout},
		ttl:      CacheTTL,
		cache:    map[string]cacheEntry{},
	}
}

// WithTTL overrides the cache window (tests); not for production use.
func (r *Remote) WithTTL(d time.Duration) *Remote {
	r.ttl = d
	return r
}

// WithHTTPClient injects the transport seam (tests count/short-circuit
// calls without a clock fight).
func (r *Remote) WithHTTPClient(c *http.Client) *Remote {
	r.client = c
	return r
}

// Verify asks the admin plane about an Authorization header value,
// forwarded AS-IS. Cached positive verdicts short-circuit within the TTL.
func (r *Remote) Verify(ctx context.Context, authorization string) (Identity, error) {
	token, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok || token == "" {
		return Identity{}, ErrInvalid // nothing to ask about — local cheap reject
	}
	key := hashToken(token)

	r.mu.Lock()
	if e, hit := r.cache[key]; hit && r.now().Before(e.expires) {
		r.mu.Unlock()
		return e.id, nil
	}
	r.mu.Unlock()

	id, err := r.ask(ctx, authorization)
	if err != nil {
		return Identity{}, err
	}
	r.mu.Lock()
	r.pruneLocked()
	r.cache[key] = cacheEntry{id: id, expires: r.now().Add(r.ttl)}
	r.mu.Unlock()
	return id, nil
}

func (r *Remote) ask(ctx context.Context, authorization string) (Identity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, strings.NewReader(""))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrAdminUnreachable, err)
	}
	req.Header.Set("Authorization", authorization) // AS-IS — the bearer IS the credential
	resp, err := r.client.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrAdminUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		var id Identity
		if err := json.NewDecoder(resp.Body).Decode(&id); err != nil {
			return Identity{}, fmt.Errorf("%w: malformed verify response: %v", ErrAdminUnreachable, err)
		}
		if id.Username == "" {
			return Identity{}, fmt.Errorf("%w: verify response without username", ErrAdminUnreachable)
		}
		return id, nil
	case http.StatusUnauthorized:
		return Identity{}, ErrInvalid
	default: // 404/500/whatever — an ANSWERING admin that we cannot read is
		// still an authority we cannot consult: closed, not guessed
		return Identity{}, fmt.Errorf("%w: verify answered %d", ErrAdminUnreachable, resp.StatusCode)
	}
}

func (r *Remote) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// pruneLocked drops expired entries (the cache is tiny — one entry per
// live session token — but unbounded-by-lifetime is still unbounded).
func (r *Remote) pruneLocked() {
	now := r.now()
	for k, e := range r.cache {
		if !now.Before(e.expires) {
			delete(r.cache, k)
		}
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
