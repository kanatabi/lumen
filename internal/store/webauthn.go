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
)

// WebAuthnCredential is one registered authenticator bound to a user.
type WebAuthnCredential struct {
	ID         string   // base64url credential id
	UserID     string   // the users.id surrogate (UUID string)
	PublicKey  []byte   // COSE key bytes
	SignCount  uint32
	Transports []string
	Label      string
	CreatedAt  string
	LastUsedAt string
}

const webauthnCols = `id, user_id, public_key, sign_count, transports, label,
	COALESCE(to_char(created_at, 'YYYY-MM-DD HH24:MI'), ''),
	COALESCE(to_char(last_used_at, 'YYYY-MM-DD HH24:MI'), '')`

func scanWebAuthn(row pgx.Row) (WebAuthnCredential, error) {
	var c WebAuthnCredential
	err := row.Scan(&c.ID, &c.UserID, &c.PublicKey, &c.SignCount, &c.Transports, &c.Label, &c.CreatedAt, &c.LastUsedAt)
	return c, err
}

// WebAuthnCreateCredential persists a freshly verified registration.
func (g *PG) WebAuthnCreateCredential(ctx context.Context, c WebAuthnCredential) error {
	_, err := g.pool.Exec(ctx,
		`INSERT INTO webauthn_credentials (id, user_id, public_key, sign_count, transports, label)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		c.ID, c.UserID, c.PublicKey, c.SignCount, c.Transports, c.Label)
	return err
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
func (g *PG) WebAuthnTouchSignCount(ctx context.Context, id string, count uint32) error {
	_, err := g.pool.Exec(ctx,
		`UPDATE webauthn_credentials SET sign_count = $2, last_used_at = now() WHERE id = $1`,
		id, count)
	return err
}

// WebAuthnDelete removes one of the user's own credentials. The WHERE
// carries user_id: you delete YOURS, not an id you know.
func (g *PG) WebAuthnDelete(ctx context.Context, userID, id string) (bool, error) {
	tag, err := g.pool.Exec(ctx,
		`DELETE FROM webauthn_credentials WHERE id = $1 AND user_id = $2`, id, userID)
	return tag.RowsAffected() > 0, err
}
