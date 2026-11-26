// store.go — the notifications persistence seam (the estate-generic
// facility, owner decisions 2026-11-25/27). One notification is one row
// in the admin-plane lineage (0015): identity is users.id (FK, cascade),
// and the row carries everything the surfaces render — app/type slugs for
// provenance, a plain-text title, a SANITIZED-HTML body, a JSONB payload
// for click-through, and the TTL as DATA.
//
// Three rules are load-bearing here:
//
//  1. TTL is data, not infrastructure. There is NO purge job (owner:
//     hold) and the admin plane runs no worker (no River on this plane),
//     so EVERY read filters `expires_at > now()` — an expired row is
//     invisible even though it still sits in the table. The expires index
//     is ready for the day a janitor lands.
//  2. Tokens are never rows. The send door authenticates ONE shared
//     service token from the environment (a MANUAL k8s Secret, §5.6
//     family); the `app` slug is the sender's self-declared display/audit
//     identity, not an authentication boundary. Nothing stored here
//     grants access.
//  3. The store does NOT sanitize. The body allowlist (bluemonday, the
//     inline subset) and the payload `url` check belong to the service —
//     persistence stores what the validated caller decided, exactly as
//     the schema comment says.
package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is the ONE denial voice for mark-read: an id that is
// unknown, someone else's, expired, or not even a UUID names no row the
// caller may touch. Handlers map it to 404 — never 403 (existence is not
// a signal).
var ErrNotFound = errors.New("notification not found")

// Store is the Postgres-backed notification persistence. The pool is the
// only dependency: every statement is parameterized, and every user
// boundary is a WHERE clause (the caller's id comes from the verified
// session, never from the body).
type Store struct{ pool *pgxpool.Pool }

// NewStore binds the store to the process pool (composition happens in
// cmd/lumen; this type never opens its own connections).
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const (
	// defaultTTL is the owner's 30-day window. It is applied in code
	// (Send) rather than left to the column default so one batch shares
	// one clock and the rule is testable without reading DDL.
	defaultTTL = 30 * 24 * time.Hour

	// the panel asks for 20 rows at a time; the clamp keeps a wire limit
	// from turning into a full-table read.
	defaultListLimit = 20
	maxListLimit     = 100
)

// Notification is one persisted notification, timestamps normalized to
// UTC at the store edge (the wire prints RFC 3339 with Z).
type Notification struct {
	ID        string
	UserID    string
	App       string
	Type      string
	Title     string
	Body      string
	Payload   json.RawMessage
	ReadAt    *time.Time
	ExpiresAt time.Time
	CreatedAt time.Time
}

// SendParams is one recipient's notification as the service decided it.
// A nil ExpiresAt means now()+30 days; a nil/empty Payload means `{}`.
type SendParams struct {
	UserID    string
	App       string
	Type      string
	Title     string
	Body      string
	Payload   json.RawMessage
	ExpiresAt *time.Time
}

// ListOpts is the panel's page request. Before is the cursor: strictly
// older rows only, zero means now. Limit is clamped to 1..100 (zero picks
// the default of 20).
type ListOpts struct {
	UnreadOnly bool
	Before     time.Time
	Limit      int
}

const notificationColumns = `id, user_id, app, type, title, body, payload,
	read_at, expires_at, created_at`

// Send persists one row per recipient in ONE transaction: a batch that
// fails halfway leaves none of it behind (the service's best-effort
// contract is about the CALLER's mutation, never about half a fan-out).
// The INSERTs are one per recipient, values always parameterized; the
// user FK is the last word on whether the recipient exists.
func (s *Store) Send(ctx context.Context, batch []SendParams) (int, error) {
	if len(batch) == 0 {
		return 0, nil
	}
	// One clock for the whole batch: two recipients of the same event
	// must not disagree about when it expires.
	now := time.Now().UTC()
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for index, params := range batch {
			payload := params.Payload
			if len(payload) == 0 {
				// JSONB is NOT NULL; the column default only fires when
				// the column is omitted. A bare notification gets `{}`.
				payload = json.RawMessage(`{}`)
			}
			expiresAt := now.Add(defaultTTL)
			if params.ExpiresAt != nil {
				expiresAt = params.ExpiresAt.UTC()
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO notifications (user_id, app, type, title, body, payload, expires_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				params.UserID, params.App, params.Type, params.Title, params.Body,
				payload, expiresAt); err != nil {
				return fmt.Errorf("notification %d for user %s: %w", index, params.UserID, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(batch), nil
}

// List returns the caller's live notifications, newest first. The SQL
// protects THREE boundaries at once: `user_id = $1` (a caller can only
// page their own rows), `expires_at > now()` (the TTL filter that stands
// in for the deferred purge), and the strict `created_at < $before`
// cursor (paging never repeats the boundary row). A zero Before means
// now, so the first page needs no clock from the handler.
func (s *Store) List(ctx context.Context, userID string, opts ListOpts) ([]Notification, error) {
	before := opts.Before
	if before.IsZero() {
		before = time.Now().UTC()
	} else {
		before = before.UTC()
	}
	limit := opts.Limit
	switch {
	case limit <= 0:
		limit = defaultListLimit
	case limit > maxListLimit:
		limit = maxListLimit
	}

	query := `SELECT ` + notificationColumns + `
		   FROM notifications
		  WHERE user_id = $1 AND expires_at > now()`
	if opts.UnreadOnly {
		query += ` AND read_at IS NULL`
	}
	query += ` AND created_at < $2 ORDER BY created_at DESC LIMIT $3`

	rows, err := s.pool.Query(ctx, query, userID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []Notification{}
	for rows.Next() {
		notification, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, notification)
	}
	return items, rows.Err()
}

// UnreadCount counts exactly the rows List would show: this user,
// unexpired, unread. The bell must never advertise a notification the
// panel cannot open (an expired row is gone, not pending).
func (s *Store) UnreadCount(ctx context.Context, userID string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications
		  WHERE user_id = $1 AND read_at IS NULL AND expires_at > now()`, userID).Scan(&count)
	return count, err
}

// MarkRead stamps one of the caller's live notifications as read. The
// WHERE carries BOTH `user_id = $1` (you mark YOUR rows, not an id you
// know) and the TTL filter (an expired row is already invisible, so
// marking it read would be a lie about a row the panel cannot show). Zero
// rows — foreign, expired, unknown, or not a UUID — is ErrNotFound. The
// update is idempotent: it does not filter on read_at, so a second call
// still matches and succeeds.
func (s *Store) MarkRead(ctx context.Context, userID, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE notifications
		    SET read_at = now()
		  WHERE id = $2 AND user_id = $1 AND expires_at > now()`,
		userID, id)
	if err != nil {
		// 22P02 = the id is not UUID-shaped. It names no row either, so
		// it gets the same denial as an id that merely does not exist —
		// a malformed path segment must not become a 500.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
			return ErrNotFound
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkAllRead clears the caller's unread set in one statement and returns
// how many rows moved. The same filters as MarkRead apply: another user's
// rows and expired rows stay untouched (and an expired row keeps
// read_at NULL — the future janitor deletes it, it was never read). A
// second call is naturally 0.
func (s *Store) MarkAllRead(ctx context.Context, userID string) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE notifications
		    SET read_at = now()
		  WHERE user_id = $1 AND read_at IS NULL AND expires_at > now()`, userID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func scanNotification(row pgx.Row) (Notification, error) {
	var notification Notification
	var payload []byte
	var readAt *time.Time
	var expiresAt time.Time
	var createdAt time.Time
	if err := row.Scan(&notification.ID, &notification.UserID, &notification.App,
		&notification.Type, &notification.Title, &notification.Body, &payload,
		&readAt, &expiresAt, &createdAt); err != nil {
		return Notification{}, err
	}
	notification.Payload = json.RawMessage(payload)
	notification.ReadAt = utcTimePtr(readAt)
	notification.ExpiresAt = expiresAt.UTC()
	notification.CreatedAt = createdAt.UTC()
	return notification, nil
}

// utcTimePtr keeps the nullable read stamp in the same zone as every
// other timestamp this store emits (pgx decodes timestamptz in the
// process's local zone).
func utcTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
