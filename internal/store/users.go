package store

import (
	"context"
	"errors"
	"slices"

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

// ErrEmailTaken maps the profile email's unique index to a 409 at the
// transport; the value itself is never logged.
var ErrEmailTaken = errors.New("email already in use")

// ErrUnknownScope backs 400 mapping for scope assignment: the scope failed
// the catalog join (RBAC spine, 0010) — the CATALOG is the assignable
// vocabulary, and the FK is the validator.
var ErrUnknownScope = errors.New("unknown scope")

// ErrDisabled backs 403 mapping at login: the account is disabled (0011) —
// distinct from invalid credentials (the admin can re-enable; "wrong
// password" would hide that).
var ErrDisabled = errors.New("account disabled")

// ErrSelfDelete is the delete guard: an admin deleting their OWN account
// would strand the session they are using (the 403 copy says so).
var ErrSelfDelete = errors.New("you cannot delete your own account")

// ErrLastManager is the lockout guard: the delete/demote would leave ZERO
// users holding users:manage — `lumen user` recovery exists, but it must
// never be the plan (409, counts ride the copy).
var ErrLastManager = errors.New("the last users:manage holder cannot be removed")

// Auth is everything the request path needs to know about an account: the
// revocation handle plus the RBAC facts (0010). Scopes and must_change are
// read per request — NEVER stamped into JWTs beyond the mint — so a
// demotion, a forced reset or a completed password change takes effect on
// the NEXT call, not on the next login. (The MINT stamps scopes per the
// spine ruling: token claims assert facts as of mint, and every scope
// mutation bumps token_version, so a claim can never outlive its truth.)
type Auth struct {
	ID           string // the surrogate UUID — every relational reference keys on it
	Username     string // the human login handle (the discoverable-login path resolves BY id)
	TokenVersion int
	Scopes       []string
	MustChange   bool
	Disabled     bool // 0011: denies login; disable BUMPS token_version (live sessions die)
}

// Authenticate verifies the argon2id passhash and returns the CURRENT auth
// facts — the mint stamps token_version + scopes, so tokens assert facts,
// not hopes.
func (g *PG) Authenticate(ctx context.Context, username, password string) (Auth, error) {
	var a Auth
	var hash string
	err := g.pool.QueryRow(ctx,
		`SELECT u.id, u.passhash, u.token_version, u.must_change, u.disabled,
		        COALESCE(array_agg(us.scope ORDER BY us.scope) FILTER (WHERE us.scope IS NOT NULL), '{}')
		   FROM users u
		   LEFT JOIN user_scopes us ON us.user_id = u.id
		  WHERE u.username = $1
		  GROUP BY u.id, u.passhash, u.token_version, u.must_change, u.disabled`, username).
		Scan(&a.ID, &hash, &a.TokenVersion, &a.MustChange, &a.Disabled, &a.Scopes)
	if err == pgx.ErrNoRows {
		return Auth{}, ErrWrongPassword // same error: existence is not a signal
	}
	if err != nil {
		return Auth{}, err
	}
	if a.Disabled {
		return Auth{}, ErrDisabled // re-enable is the admin's undo, not a retype
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
		`SELECT u.id, u.token_version, u.must_change, u.disabled,
		        COALESCE(array_agg(us.scope ORDER BY us.scope) FILTER (WHERE us.scope IS NOT NULL), '{}')
		   FROM users u
		   LEFT JOIN user_scopes us ON us.user_id = u.id
		  WHERE u.username = $1
		  GROUP BY u.id, u.token_version, u.must_change, u.disabled`, username).
		Scan(&a.ID, &a.TokenVersion, &a.MustChange, &a.Disabled, &a.Scopes)
	if err == pgx.ErrNoRows {
		return Auth{}, false, nil
	}
	return a, err == nil, err
}

// AuthForID resolves the auth row by the SURROGATE id (the passkey
// discoverable-login path: the assertion's userHandle IS users.id).
func (g *PG) AuthForID(ctx context.Context, id string) (Auth, bool, error) {
	var a Auth
	err := g.pool.QueryRow(ctx,
		`SELECT u.id, u.username, u.token_version, u.must_change, u.disabled,
		        COALESCE(array_agg(us.scope ORDER BY us.scope) FILTER (WHERE us.scope IS NOT NULL), '{}')
		   FROM users u
		   LEFT JOIN user_scopes us ON us.user_id = u.id
		  WHERE u.id = $1::uuid
		  GROUP BY u.id, u.username, u.token_version, u.must_change, u.disabled`, id).
		Scan(&a.ID, &a.Username, &a.TokenVersion, &a.MustChange, &a.Disabled, &a.Scopes)
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

// UpsertUser backs `lumen user` (headless recovery). Bootstrap accounts
// hold the FULL catalog — the CLI is the owner speaking directly; the
// forced-change story belongs to wizard-issued temps, not to them.
func (g *PG) UpsertUser(ctx context.Context, username, passhash string) error {
	return pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO users(username, display_name, passhash) VALUES ($1,$1,$2)
			 ON CONFLICT (username) DO UPDATE SET passhash = EXCLUDED.passhash`,
			username, passhash); err != nil {
			return err
		}
		// the full catalog, whatever it holds TODAY (portal-managed — the
		// recovery grant is "everything that exists", not a frozen list)
		if _, err := tx.Exec(ctx,
			`INSERT INTO user_scopes (user_id, scope)
			 SELECT u.id, s.scope FROM users u CROSS JOIN scopes s
			  WHERE u.username = $1
			  ON CONFLICT (user_id, scope) DO NOTHING`, username); err != nil {
			return err
		}
		return nil
	})
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
// The first admin holds the FULL catalog — the owner IS the authority.
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
		`INSERT INTO users(username, display_name, passhash, must_change) VALUES ($1,$1,$2,false)`,
		username, passhash); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO user_scopes (user_id, scope)
		 SELECT u.id, s.scope FROM users u CROSS JOIN scopes s
		  WHERE u.username = $1
		  ON CONFLICT (user_id, scope) DO NOTHING`, username); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// InsertUser backs admin-created accounts (temp password, must_change armed).
// The caller grants EXACT scopes (roles are gone); each scope must exist in
// the catalog — the FK is the validator, mapped to ErrUnknownScope.
func (g *PG) InsertUser(ctx context.Context, username, passhash string, scopes []string) error {
	err := pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO users(username, display_name, passhash, must_change) VALUES ($1,$1,$2,true)`,
			username, passhash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO user_scopes (user_id, scope)
			 SELECT u.id, s.scope FROM users u JOIN scopes s ON s.scope = ANY($2)
			  WHERE u.username = $1
			  ON CONFLICT (user_id, scope) DO NOTHING`, username, scopes); err != nil {
			return err
		}
		// completeness: every requested scope must have landed (a scope
		// missing from the catalog would silently grant fewer)
		var granted, want int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM user_scopes us JOIN users u ON u.id = us.user_id
			  WHERE u.username = $1`, username).Scan(&granted); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM unnest($1::text[])`, scopes).Scan(&want); err != nil {
			return err
		}
		if granted != want {
			return ErrUnknownScope
		}
		return nil
	})
	var pgErr interface{ SQLState() string }
	if err != nil && errors.As(err, &pgErr) {
		switch pgErr.SQLState() {
		case "23505":
			return errors.New("username taken")
		case "23503":
			return ErrUnknownScope
		}
	}
	return err
}

// UserRow is one line of the admin user list.
type UserRow struct {
	ID         string   `json:"id"`
	Username   string   `json:"username"`
	Name       string   `json:"name"`
	Email      string   `json:"email"`
	Scopes     []string `json:"scopes"`
	MustChange bool     `json:"mustChange"`
	Disabled   bool     `json:"disabled"`
	CreatedAt  string   `json:"createdAt"`
}

func (g *PG) ListUsers(ctx context.Context) ([]UserRow, error) {
	rows, err := g.pool.Query(ctx,
		`SELECT u.id, u.username, u.display_name, COALESCE(u.email, ''), u.must_change, u.disabled,
		        to_char(u.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD'),
		        COALESCE(array_agg(us.scope ORDER BY us.scope) FILTER (WHERE us.scope IS NOT NULL), '{}')
		   FROM users u
		   LEFT JOIN user_scopes us ON us.user_id = u.id
		  GROUP BY u.id, u.username, u.display_name, u.email, u.must_change, u.disabled, u.created_at
		  ORDER BY u.created_at, u.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserRow{}
	for rows.Next() {
		var u UserRow
		if err := rows.Scan(&u.ID, &u.Username, &u.Name, &u.Email, &u.MustChange, &u.Disabled, &u.CreatedAt, &u.Scopes); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUserByID is the profile read (the self-service page and whoami's
// name/email). One row + the scope aggregate.
func (g *PG) GetUserByID(ctx context.Context, id string) (UserRow, error) {
	var u UserRow
	err := g.pool.QueryRow(ctx,
		`SELECT u.id, u.username, u.display_name, COALESCE(u.email, ''), u.must_change, u.disabled,
		        to_char(u.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD'),
		        COALESCE(array_agg(us.scope ORDER BY us.scope) FILTER (WHERE us.scope IS NOT NULL), '{}')
		   FROM users u
		   LEFT JOIN user_scopes us ON us.user_id = u.id
		  WHERE u.id = $1
		  GROUP BY u.id, u.username, u.display_name, u.email, u.must_change, u.disabled, u.created_at`,
		id).
		Scan(&u.ID, &u.Username, &u.Name, &u.Email, &u.MustChange, &u.Disabled, &u.CreatedAt, &u.Scopes)
	if err == pgx.ErrNoRows {
		return UserRow{}, ErrNoSuchUser
	}
	if err != nil {
		return UserRow{}, err
	}
	return u, nil
}

// UpdateProfile writes the self-service fields (display name + email) and
// returns the updated row. The email unique index maps to ErrEmailTaken;
// an empty email is stored as NULL (the unique index ignores NULL).
func (g *PG) UpdateProfile(ctx context.Context, id, name, email string) (UserRow, error) {
	var emailArg any
	if email != "" {
		emailArg = email
	}
	tag, err := g.pool.Exec(ctx,
		`UPDATE users SET display_name = $2, email = $3 WHERE id = $1`, id, name, emailArg)
	if err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
			return UserRow{}, ErrEmailTaken
		}
		return UserRow{}, err
	}
	if tag.RowsAffected() == 0 {
		return UserRow{}, ErrNoSuchUser
	}
	return g.GetUserByID(ctx, id)
}

// SetScopes REPLACES a user's scope set (the admin's assignment is the
// whole truth, not a diff) and BUMPS token_version — the spine ruling: a
// demotion is immediate, outstanding tokens die on the spot. Returns the
// new version. Every requested scope must exist in the catalog.
func (g *PG) SetScopes(ctx context.Context, username string, scopes []string) (int, error) {
	slices.Sort(scopes)
	scopes = slices.Compact(scopes)
	var tv int
	err := pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		var userID string
		err := tx.QueryRow(ctx, `SELECT id FROM users WHERE username = $1`, username).Scan(&userID)
		if err == pgx.ErrNoRows {
			return ErrNoSuchUser
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM user_scopes WHERE user_id = $1`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO user_scopes (user_id, scope)
			 SELECT $1, s.scope FROM scopes s WHERE s.scope = ANY($2)
			  ON CONFLICT (user_id, scope) DO NOTHING`, userID, scopes); err != nil {
			return err
		}
		var held int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM user_scopes WHERE user_id = $1`, userID).Scan(&held); err != nil {
			return err
		}
		if held != len(scopes) {
			return ErrUnknownScope
		}
		return tx.QueryRow(ctx,
			`UPDATE users SET token_version = token_version + 1 WHERE id = $1 RETURNING token_version`,
			userID).Scan(&tv)
	})
	return tv, err
}

// ArmMustChange FORCES a password change on next login WITHOUT touching
// the password — the admin's "be suspicious of this account" verb. No
// token_version bump: the live session is exactly what should meet the
// 428 gate (that is how wizard temps already behave). Returns whether the
// user existed.
func (g *PG) ArmMustChange(ctx context.Context, username string) (bool, error) {
	tag, err := g.pool.Exec(ctx,
		`UPDATE users SET must_change = true WHERE username = $1`, username)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetDisabled flips the account's flag. DISABLING also BUMPS
// token_version — the user's live sessions die with the login door
// (enable re-admits a fresh login; the old token stays dead). Returns the
// (possibly new) token_version, or ErrNoSuchUser.
func (g *PG) SetDisabled(ctx context.Context, username string, disabled bool) (int, error) {
	var tv int
	err := g.pool.QueryRow(ctx,
		`UPDATE users
		    SET disabled = $2,
		        token_version = CASE WHEN $2 THEN token_version + 1 ELSE token_version END
		  WHERE username = $1
		  RETURNING token_version`, username, disabled).Scan(&tv)
	if err == pgx.ErrNoRows {
		return 0, ErrNoSuchUser
	}
	return tv, err
}

// DeleteUser removes the account (user_scopes cascade) and RETURNS the
// deleted user's id: the caller's cross-module repair (the tasks admin
// promotion) needs the identity after the row is gone. TWO GUARDS,
// evaluated in ONE transaction: no self-deletion (the acting admin would
// strand the session in their own hand), and no removal that leaves ZERO
// users:manage holders (lumen user recovery exists, but must never be the
// plan). Content is estate-owned — no author FKs — so the delete is just
// the row.
func (g *PG) DeleteUser(ctx context.Context, actor, username string) (string, error) {
	var deletedID string
	err := pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		if actor == username {
			return ErrSelfDelete
		}
		var targetID string
		err := tx.QueryRow(ctx, `SELECT id FROM users WHERE username = $1`, username).Scan(&targetID)
		if err == pgx.ErrNoRows {
			return ErrNoSuchUser
		}
		if err != nil {
			return err
		}
		// the lockout guard: if the TARGET holds users:manage, someone
		// ELSE must hold it too — count holders EXCLUDING the target
		var managers int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM user_scopes us
			   JOIN users u ON u.id = us.user_id
			  WHERE us.scope = $2 AND u.username <> $1`, username, "users:manage").Scan(&managers); err != nil {
			return err
		}
		var targetManages bool
		if err := tx.QueryRow(ctx,
			`SELECT count(*) > 0 FROM user_scopes us
			   JOIN users u ON u.id = us.user_id
			  WHERE us.scope = $2 AND u.username = $1`, username, "users:manage").Scan(&targetManages); err != nil {
			return err
		}
		if targetManages && managers == 0 {
			return ErrLastManager
		}
		if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, targetID); err != nil {
			return err
		}
		deletedID = targetID
		return nil
	})
	return deletedID, err
}

// CatalogEntry is one line of the portal-managed scope catalog.
type CatalogEntry struct {
	Scope       string `json:"scope"`
	Description string `json:"description"`
}

// ListCatalog reads the ASSIGNABLE vocabulary (the scopes table) — the
// users page's picker and the catalog surface both read this.
func (g *PG) ListCatalog(ctx context.Context) ([]CatalogEntry, error) {
	rows, err := g.pool.Query(ctx,
		`SELECT scope, description FROM scopes ORDER BY scope`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CatalogEntry{}
	for rows.Next() {
		var c CatalogEntry
		if err := rows.Scan(&c.Scope, &c.Description); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddScope onboards a scope into the catalog (portal-managed per the spine
// ruling — never manual SQL). Re-adding an existing scope is a no-op that
// reports false.
func (g *PG) AddScope(ctx context.Context, scope, description string) (bool, error) {
	tag, err := g.pool.Exec(ctx,
		`INSERT INTO scopes (scope, description) VALUES ($1, $2)
		 ON CONFLICT (scope) DO NOTHING`,
		scope, description)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteScope removes a scope from the catalog. It REFUSES while any user
// holds the scope (the FK is RESTRICT-shaped by the explicit check — the
// admin gets the counts, never a silent cascade) and returns whether it
// deleted. Deleting a nonexistent scope is not an error (idempotent).
func (g *PG) DeleteScope(ctx context.Context, scope string) (bool, int, error) {
	var holders int
	if err := g.pool.QueryRow(ctx,
		`SELECT count(*) FROM user_scopes WHERE scope = $1`, scope).Scan(&holders); err != nil {
		return false, holders, err
	}
	if holders > 0 {
		return false, holders, nil
	}
	tag, err := g.pool.Exec(ctx, `DELETE FROM scopes WHERE scope = $1`, scope)
	if err != nil {
		return false, holders, err
	}
	return tag.RowsAffected() > 0, holders, nil
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

// jwt removed: the store only READS what the sessions package wrote — the
// claims work belongs to internal/session (L4).
