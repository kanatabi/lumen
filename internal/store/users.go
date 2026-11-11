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

// ErrUsersExist is the TOFU boundary for the setup wizard: registration is
// only possible while the users table is EMPTY, atomically.
var ErrUsersExist = errors.New("users already exist")

// ErrNoSuchUser backs 404 mapping for user administration verbs.
var ErrNoSuchUser = errors.New("no such user")

// Auth is everything the request path needs to know about an account: the
// revocation handle plus the multi-user facts (0007). Roles and must_change
// are read per request — NEVER stamped into JWTs — so a demotion, a forced
// reset or a completed password change takes effect on the NEXT call, not on
// the next login.
type Auth struct {
	TokenVersion int
	Role         string // admin | editor
	MustChange   bool
}

// Authenticate verifies the argon2id passhash and returns the CURRENT auth
// facts — the mint stamps token_version, so tokens assert facts, not hopes.
func (g *PG) Authenticate(ctx context.Context, username, password string) (Auth, error) {
	var a Auth
	var hash string
	err := g.pool.QueryRow(ctx,
		`SELECT passhash, token_version, role, must_change FROM users WHERE username = $1`, username).
		Scan(&hash, &a.TokenVersion, &a.Role, &a.MustChange)
	if err == pgx.ErrNoRows {
		return Auth{}, ErrWrongPassword // same error: existence is not a signal
	}
	if err != nil {
		return Auth{}, err
	}
	if !session.VerifyPassword(hash, password) {
		return Auth{}, ErrWrongPassword
	}
	return a, nil
}

// AuthFor is the per-request identity read behind VerifyBearer.
func (g *PG) AuthFor(ctx context.Context, username string) (Auth, bool, error) {
	var a Auth
	err := g.pool.QueryRow(ctx,
		`SELECT token_version, role, must_change FROM users WHERE username = $1`, username).
		Scan(&a.TokenVersion, &a.Role, &a.MustChange)
	if err == pgx.ErrNoRows {
		return Auth{}, false, nil
	}
	return a, err == nil, err
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

// UpsertUser backs `lumen user` (headless recovery). Bootstrap accounts are
// admin with must_change=false — the CLI is the owner speaking directly; the
// forced-change story belongs to wizard-issued temps, not to them.
func (g *PG) UpsertUser(ctx context.Context, username, passhash string) error {
	_, err := g.pool.Exec(ctx,
		`INSERT INTO users(username, passhash, role) VALUES ($1,$2,'admin')
		 ON CONFLICT (username) DO UPDATE SET passhash = EXCLUDED.passhash`,
		username, passhash)
	return err
}

// CountUsers is the wizard probe: only emptiness is observable.
func (g *PG) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := g.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// InsertFirstAdmin is the TOFU registration made race-proof: an advisory
// TRANSACTION lock spans count+insert, so exactly one concurrent registration
// can win an empty table. Returns false (no error) when the estate is claimed.
func (g *PG) InsertFirstAdmin(ctx context.Context, username, passhash string) (bool, error) {
	tx, err := g.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// a stable arbitrary constant: the "setup wizard" lock namespace
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(72042611)`); err != nil {
		return false, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil // claimed: the wizard is gone forever, not "try later"
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO users(username, passhash, role, must_change) VALUES ($1,$2,'admin',false)`,
		username, passhash); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// InsertUser backs admin-created accounts (temp password, must_change armed).
func (g *PG) InsertUser(ctx context.Context, username, passhash, role string) error {
	_, err := g.pool.Exec(ctx,
		`INSERT INTO users(username, passhash, role, must_change) VALUES ($1,$2,$3,true)`,
		username, passhash, role)
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
		return errors.New("username taken")
	}
	return err
}

// UserRow is one line of the admin user list.
type UserRow struct {
	Username   string `json:"username"`
	Role       string `json:"role"`
	MustChange bool   `json:"mustChange"`
	CreatedAt  string `json:"createdAt"`
}

func (g *PG) ListUsers(ctx context.Context) ([]UserRow, error) {
	rows, err := g.pool.Query(ctx,
		`SELECT username, role, must_change, to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD')
		   FROM users ORDER BY created_at, username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserRow{}
	for rows.Next() {
		var u UserRow
		if err := rows.Scan(&u.Username, &u.Role, &u.MustChange, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetPassword writes a new hash. mustChange arms (temp issued) or clears
// (self-service change) the forced flag; bump=true kills old sessions — a
// password change (or admin reset) must never leave the previous session alive.
func (g *PG) SetPassword(ctx context.Context, username, passhash string, mustChange, bump bool) error {
	var one int
	err := g.pool.QueryRow(ctx, `SELECT 1 FROM users WHERE username = $1`, username).Scan(&one)
	if err == pgx.ErrNoRows {
		return ErrNoSuchUser
	}
	if err != nil {
		return err
	}
	if _, err := g.pool.Exec(ctx,
		`UPDATE users SET passhash = $2, must_change = $3 WHERE username = $1`,
		username, passhash, mustChange); err != nil {
		return err
	}
	if bump {
		if _, err := g.BumpTokenVersion(ctx, username); err != nil {
			return err
		}
	}
	return nil
}
