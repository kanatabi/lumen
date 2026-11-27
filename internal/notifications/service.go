// service.go — the notifications module's decision layer (M2.A): the
// Notifier SEAM every lumen module calls in-process (tasks is the first
// caller), the validation that makes a row safe to render, and the send
// door's env-only shared token.
//
// The store deliberately persists what a validated caller decided, so
// three owner rulings live HERE:
//
//  1. Bodies are SANITIZED HTML from a narrow inline allowlist (b, strong,
//     i, em, u, s, code, br, same-origin links, and a colour/weight span
//     style). sanitize-on-write is the first of the two gates; mellon
//     renders through DOMPurify again.
//  2. Payloads are JSON objects; a `url`, when present, is a same-origin
//     path (`/…`, never `//…`) so a click can never leave the estate's
//     origin.
//  3. The send door authenticates ONE shared token read from the
//     environment (the manual `lumen-notification-token` Secret, §5.6
//     family). It is NOT a config binding and NOT a Bao value: declaring
//     it as config would demand an ES render and re-open the epoch story.
//     Unset means the route is not mounted at all.
package notifications

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"
)

// ErrInvalid is the ONE validation voice. Every rule below wraps it, and
// the HTTP door maps it to 400: a well-formed request with an unknown
// username, a bad slug, an oversized title, or a foreign-scheme payload
// URL all mean "the caller's copy is wrong", not "the estate is broken".
var ErrInvalid = errors.New("invalid notification")

// ErrRateLimited is deliberately separate from ErrInvalid: the request is
// fine, the sender is merely too loud. The door maps it to 429.
var ErrRateLimited = errors.New("notification send rate limited")

const (
	// notificationTokenEnv is read ONCE in NewService (the LUMEN_BAO_*
	// pattern): environment only, never a config-file value.
	notificationTokenEnv = "LUMEN_NOTIFICATION_TOKEN"

	// maxRecipients caps one logical send (one event, many people). It is
	// the same number the HTTP door's `to` list is validated against.
	maxRecipients = 100

	// maxTitleRunes is the panel's one-line title budget. Runes, not
	// bytes: emoji and accents are the point of the field.
	maxTitleRunes = 200

	// maxBodyBytes caps the HTML BEFORE sanitization; the stored string is
	// what bluemonday decided to keep.
	maxBodyBytes = 8192

	// maxPayloadBytes caps the structured click-through payload.
	maxPayloadBytes = 8 * 1024

	// defaultSendLimit is the per-app fixed-window allowance: 120 sends a
	// minute is far above any estate module's real cadence and low enough
	// that a runaway loop cannot flood every inbox.
	defaultSendLimit = 120
)

// slugPattern is the app/type grammar, defined locally because the
// notifications module is self-contained (it does NOT import adminsvc).
// It is slightly wider than the Bao relay's app/env grammar: event types
// are dotted (`project.invite`), so dots are legal after the first
// character.
var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,31}$`)

// sameOriginPathPattern is the one URL rule, shared by payload values and
// sanitized anchors: a leading slash, and a second character that is
// neither another slash (protocol-relative `//evil`) nor a backslash or
// control character (browsers normalize `\` to `/`, so `/\evil` would
// also leave the origin). RE2 has no lookahead, hence the alternation for
// the bare "/" case.
var sameOriginPathPattern = regexp.MustCompile(`^/($|[^/\\\r\n\t])[^\\\r\n\t]*$`)

// notificationPolicy is the inline-HTML allowlist, built once: a body may
// keep emphasis, code, line breaks, same-origin links, and a span whose
// ONLY styles are colour and weight. Anything else — scripts, frames,
// images, event handlers, external links, positioning — is stripped.
var notificationPolicy = newNotificationPolicy()

func newNotificationPolicy() *bluemonday.Policy {
	policy := bluemonday.NewPolicy()
	policy.AllowElements("b", "strong", "i", "em", "u", "s", "code", "br", "span")
	policy.AllowAttrs("href").Matching(sameOriginPathPattern).OnElements("a")
	policy.AllowStyles("color", "background-color", "font-weight").OnElements("span")
	// Links are validated (and normalized) by net/url after the regex;
	// relative URLs are the ONLY kind this allowlist admits.
	policy.RequireParseableURLs(true)
	policy.AllowRelativeURLs(true)
	return policy
}

// Service is the module's facade: persistence plus the rules a caller
// cannot skip. The token is a value read at construction, not a lookup on
// every request — the Secret changes only on pod restart.
type Service struct {
	store   *Store
	token   string
	limiter *sendLimiter
}

// NewService binds the persistence store and reads the shared send token
// ONCE. Everything else is per-process state (the limiter included).
func NewService(store *Store) *Service {
	return &Service{
		store:   store,
		token:   os.Getenv(notificationTokenEnv),
		limiter: newSendLimiter(defaultSendLimit, time.Minute),
	}
}

// SendEnabled reports whether the HTTP SEND door should be mounted.
func (s *Service) SendEnabled() bool { return s.token != "" }

// AuthorizeSend compares the presented bearer token in constant time. An
// unconfigured service authorizes nobody, not even an empty token.
func (s *Service) AuthorizeSend(token string) bool {
	if s.token == "" || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(s.token), []byte(token)) == 1
}

// Send is the Notifier SEAM: validate the whole batch, sanitize the
// bodies, rate-limit the app, then hand the store one transaction. It
// returns the number of notifications persisted.
//
// Best-effort semantics belong to the CALLER (owner decision C10): tasks
// logs a Send failure and never rolls back its mutation — the invite or
// assignment row is the truth.
func (s *Service) Send(ctx context.Context, batch []SendParams) (int, error) {
	validated, err := validateBatch(batch)
	if err != nil {
		return 0, err
	}
	for _, app := range distinctApps(validated) {
		if !s.limiter.Allow(app) {
			return 0, fmt.Errorf("%w: %s", ErrRateLimited, app)
		}
	}
	return s.store.Send(ctx, validated)
}

// UserIDsForUsernames resolves the HTTP door's `to` list to identity ids,
// preserving the caller's order. Any unknown name rejects the whole
// request (ErrInvalid): a partial fan-out would silently drop a recipient.
func (s *Service) UserIDsForUsernames(ctx context.Context, names []string) ([]string, error) {
	if len(names) == 0 || len(names) > maxRecipients {
		return nil, fmt.Errorf("%w: to must name 1..%d usernames", ErrInvalid, maxRecipients)
	}
	rows, err := s.store.pool.Query(ctx,
		`SELECT username, id FROM users WHERE username = ANY($1)`, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	found := make(map[string]string, len(names))
	for rows.Next() {
		var username, id string
		if err := rows.Scan(&username, &id); err != nil {
			return nil, err
		}
		found[username] = id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		// A repeated name is ONE recipient: the wire's `to` is a set of
		// people, and a typo must not fan out duplicate rows.
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		id, ok := found[name]
		if !ok {
			return nil, fmt.Errorf("%w: unknown username %q", ErrInvalid, name)
		}
		seen[name] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// List delegates the panel's page read (the store owns the TTL filter,
// the user boundary, and the limit clamp).
func (s *Service) List(ctx context.Context, userID string, opts ListOpts) ([]Notification, error) {
	return s.store.List(ctx, userID, opts)
}

// UnreadCount delegates the bell's count.
func (s *Service) UnreadCount(ctx context.Context, userID string) (int, error) {
	return s.store.UnreadCount(ctx, userID)
}

// MarkRead delegates the one-row mark (ErrNotFound covers foreign,
// expired, unknown, and malformed ids alike).
func (s *Service) MarkRead(ctx context.Context, userID, id string) error {
	return s.store.MarkRead(ctx, userID, id)
}

// MarkAllRead delegates the bulk mark and returns the rows touched.
func (s *Service) MarkAllRead(ctx context.Context, userID string) (int, error) {
	return s.store.MarkAllRead(ctx, userID)
}

// validateBatch applies every rule and returns a normalized copy: the
// caller's slice is never mutated (the in-process seam may reuse it), the
// body is sanitized, and timestamps are normalized to UTC here so the
// store's one-clock rule sees clean values.
func validateBatch(batch []SendParams) ([]SendParams, error) {
	if len(batch) == 0 {
		return nil, fmt.Errorf("%w: at least one recipient is required", ErrInvalid)
	}
	if len(batch) > maxRecipients {
		return nil, fmt.Errorf("%w: at most %d recipients per send", ErrInvalid, maxRecipients)
	}

	validated := make([]SendParams, len(batch))
	for index, params := range batch {
		if params.UserID == "" {
			return nil, fmt.Errorf("%w: recipient user id is required", ErrInvalid)
		}
		if !slugPattern.MatchString(params.App) {
			return nil, fmt.Errorf("%w: app must match %s", ErrInvalid, slugPattern)
		}
		if !slugPattern.MatchString(params.Type) {
			return nil, fmt.Errorf("%w: type must match %s", ErrInvalid, slugPattern)
		}
		if !utf8.ValidString(params.Title) || strings.TrimSpace(params.Title) == "" {
			return nil, fmt.Errorf("%w: title must be non-empty text", ErrInvalid)
		}
		if runes := utf8.RuneCountInString(params.Title); runes > maxTitleRunes {
			return nil, fmt.Errorf("%w: title must be at most %d characters", ErrInvalid, maxTitleRunes)
		}
		if !utf8.ValidString(params.Body) {
			return nil, fmt.Errorf("%w: body must be valid UTF-8", ErrInvalid)
		}
		if len(params.Body) > maxBodyBytes {
			return nil, fmt.Errorf("%w: body must be at most %d bytes", ErrInvalid, maxBodyBytes)
		}
		payload, err := validatePayload(params.Payload)
		if err != nil {
			return nil, err
		}

		params.Body = notificationPolicy.Sanitize(params.Body)
		params.Payload = payload
		if params.ExpiresAt != nil {
			expiresAt := params.ExpiresAt.UTC()
			params.ExpiresAt = &expiresAt
		}
		validated[index] = params
	}
	return validated, nil
}

// validatePayload enforces the object shape and the same-origin `url`
// rule. An empty payload stays empty: the store writes the column's `{}`.
func validatePayload(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxPayloadBytes {
		return nil, fmt.Errorf("%w: payload must be at most %d bytes", ErrInvalid, maxPayloadBytes)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("%w: payload must be a JSON object", ErrInvalid)
	}
	var object map[string]any
	if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%w: payload must be a JSON object", ErrInvalid)
	}
	if rawURL, present := object["url"]; present {
		url, ok := rawURL.(string)
		if !ok || !sameOriginPathPattern.MatchString(url) {
			return nil, fmt.Errorf("%w: payload.url must be a same-origin path starting with %q", ErrInvalid, "/")
		}
	}
	return json.RawMessage(trimmed), nil
}

// distinctApps lists each app slug once per batch: the limiter counts
// SEND EVENTS, not recipients — one invite to twenty people is one send.
func distinctApps(batch []SendParams) []string {
	seen := make(map[string]struct{}, 1)
	apps := make([]string, 0, 1)
	for _, params := range batch {
		if _, ok := seen[params.App]; ok {
			continue
		}
		seen[params.App] = struct{}{}
		apps = append(apps, params.App)
	}
	return apps
}

// sendLimiter is a per-app fixed-window counter. In-process and
// per-replica on purpose (the admin plane runs no worker and needs no
// shared store for an in-cluster door); 120/min is the production window,
// and the constructor takes it so tests can prove the third call with a
// 2/min window fails.
type sendLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	now     func() time.Time
	windows map[string]sendWindow
}

type sendWindow struct {
	start time.Time
	count int
}

func newSendLimiter(limit int, window time.Duration) *sendLimiter {
	return &sendLimiter{
		limit:   limit,
		window:  window,
		now:     time.Now,
		windows: make(map[string]sendWindow),
	}
}

// Allow consumes one slot for the app in the current window. A new window
// starts lazily on the first call past its predecessor; the window's start
// is never slid, so a sender cannot ride a rolling allowance.
func (l *sendLimiter) Allow(app string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	current, seen := l.windows[app]
	if !seen || now.Sub(current.start) >= l.window {
		l.windows[app] = sendWindow{start: now, count: 1}
		return true
	}
	if current.count >= l.limit {
		return false
	}
	current.count++
	l.windows[app] = current
	return true
}
