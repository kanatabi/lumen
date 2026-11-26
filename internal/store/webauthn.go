package store

// webauthn.go - the passkey credential CRUD (the auth-method seam, C1
// round 2026-11-25). Admin-plane lineage (0012): rows key on users.id,
// the credential id is the browser-addressed primary key, and the
// public key is the only half lumen holds. The flow layer
// (internal/adminsvc) converts these rows into the webauthn library's
// types at verification time - the storage stays protocol-shallow.

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrCredentialExists is the duplicate-registration race: the browser's
// exclude list is a hint, so a second finish for the same authenticator
// lands here. Handlers answer 409, never 500.
var ErrCredentialExists = errors.New("credential already registered")

// WebAuthnCredential is one registered authenticator bound to a user.
type WebAuthnCredential struct {
	ID         string // base64url credential id
	UserID     string // the users.id surrogate (UUID string)
	PublicKey  []byte // COSE key bytes
	SignCount  uint32
	Transports []string
	Label      string
	// The library's stored flags (0013): BE is invariant and VALIDATED on
	// every login (a synced passkey is BE=1); BS may change and is
	// refreshed on login. UP/UV are per-assertion, not credential facts.
	BackupEligible bool
	BackupState    bool
	CreatedAt      string
	LastUsedAt     string
}

const webauthnCols = `id, user_id, public_key, sign_count, transports, label,
	backup_eligible, backup_state,
	COALESCE(to_char(created_at, 'YYYY-MM-DD HH24:MI'), ''),
	COALESCE(to_char(last_used_at, 'YYYY-MM-DD HH24:MI'), '')`

func scanWebAuthn(row pgx.Row) (WebAuthnCredential, error) {
	var c WebAuthnCredential
	err := row.Scan(&c.ID, &c.UserID, &c.PublicKey, &c.SignCount, &c.Transports, &c.Label,
		&c.BackupEligible, &c.BackupState, &c.CreatedAt, &c.LastUsedAt)
	return c, err
}

// WebAuthnCreateCredential persists a freshly verified registration and
// answers the row's created_at (the response contract prints it; the
// roster's format — minutes, no zone — so both surfaces read one way).
// A duplicate credential id is the ONE race the caller maps to 409.
func (g *PG) WebAuthnCreateCredential(ctx context.Context, c WebAuthnCredential) (string, error) {
	var createdAt string
	err := g.pool.QueryRow(ctx,
		`INSERT INTO webauthn_credentials (id, user_id, public_key, sign_count, transports, label,
		                                   backup_eligible, backup_state)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING COALESCE(to_char(created_at, 'YYYY-MM-DD HH24:MI'), '')`,
		c.ID, c.UserID, c.PublicKey, c.SignCount, c.Transports, c.Label,
		c.BackupEligible, c.BackupState).Scan(&createdAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return "", ErrCredentialExists
		}
		return "", err
	}
	return createdAt, nil
}

// WebAuthnCredentialsFor lists a user's registered authenticators -
// the settings page's roster AND the registration's exclude list.
func (g *PG) WebAuthnCredentialsFor(ctx context.Context, userID string) ([]WebAuthnCredential, error) {
	rows, err := g.pool.Query(ctx,
		`SELECT `+webauthnCols+` FROM webauthn_credentials WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebAuthnCredential
	for rows.Next() {
		c, err := scanWebAuthn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// WebAuthnCredentialByID resolves ONE credential by its browser id -
// the discoverable-login handler's lookup (the assertion names the
// credential; the user comes with it).
func (g *PG) WebAuthnCredentialByID(ctx context.Context, id string) (WebAuthnCredential, error) {
	c, err := scanWebAuthn(g.pool.QueryRow(ctx,
		`SELECT `+webauthnCols+` FROM webauthn_credentials WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return WebAuthnCredential{}, ErrWrongPassword // the ONE indistinguishable denial
	}
	return c, err
}

// WebAuthnTouchSignCount persists the replay tripwire after a verified
// assertion (the counter must move; a static counter = a cloned key).
func (g *PG) WebAuthnTouchSignCount(ctx context.Context, id string, count uint32, backupState bool) error {
	// BS can legitimately change (the key is synced/backed up later); BE
	// must NEVER change, so it is not written here.
	_, err := g.pool.Exec(ctx,
		`UPDATE webauthn_credentials
		    SET sign_count = $2, backup_state = $3, last_used_at = now()
		  WHERE id = $1`,
		id, count, backupState)
	return err
}

// WebAuthnDelete removes one of the user's own credentials. The WHERE
// carries user_id: you delete YOURS, not an id you know.
func (g *PG) WebAuthnDelete(ctx context.Context, userID, id string) (bool, error) {
	tag, err := g.pool.Exec(ctx,
		`DELETE FROM webauthn_credentials WHERE id = $1 AND user_id = $2`, id, userID)
	return tag.RowsAffected() > 0, err
}
