package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/kanatabi/lumen/internal/session"
)

// ErrWrongPassword is deliberately indistinguishable from unknown user at
// the handler edge (one message, one status — no account enumeration).
var ErrWrongPassword = errors.New("invalid credentials")

// Authenticate verifies the argon2id passhash and returns the CURRENT
// token_version — the mint stamps it, so the token is born knowing truth.
func (g *PG) Authenticate(ctx context.Context, username, password string) (int, error) {
	var hash string
	var tokenVersion int
	err := g.pool.QueryRow(ctx,
		`SELECT passhash, token_version FROM users WHERE username = $1`, username).
		Scan(&hash, &tokenVersion)
	if err == pgx.ErrNoRows {
		return 0, ErrWrongPassword // same error: existence is not a signal
	}
	if err != nil {
		return 0, err
	}
	if !session.VerifyPassword(hash, password) {
		return 0, ErrWrongPassword
	}
	return tokenVersion, nil
}

func (g *PG) TokenVersion(ctx context.Context, username string) (int, bool, error) {
	var tokenVersion int
	err := g.pool.QueryRow(ctx, `SELECT token_version FROM users WHERE username = $1`, username).Scan(&tokenVersion)
	if err == pgx.ErrNoRows {
		return 0, false, nil
	}
	return tokenVersion, err == nil, err
}

// BumpTokenVersion is REVOCATION: every outstanding JWT carries the old tokenVersion;
// after this they are all dead. Returns the new version.
func (g *PG) BumpTokenVersion(ctx context.Context, username string) (int, error) {
	var tokenVersion int
	err := g.pool.QueryRow(ctx,
		`UPDATE users SET token_version = token_version + 1 WHERE username = $1 RETURNING token_version`,
		username).Scan(&tokenVersion)
	return tokenVersion, err
}

// UpsertUser backs cmd/userctl (bootstrap one-time credential creation).
func (g *PG) UpsertUser(ctx context.Context, username, passhash string) error {
	_, err := g.pool.Exec(ctx,
		`INSERT INTO users(username, passhash) VALUES ($1,$2)
		 ON CONFLICT (username) DO UPDATE SET passhash = EXCLUDED.passhash`,
		username, passhash)
	return err
}
