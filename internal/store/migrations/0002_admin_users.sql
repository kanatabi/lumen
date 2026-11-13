-- 0002_admin_users.sql — the role split (owner ruling 2026-11-13): the
-- users table was CUT OUT of 0001_core and lives here, classified
-- ADMIN-plane-only. The admin DB (lumen_admin) owns credentials and the
-- revocation handle; the cms DBs must never carry a users table — auth
-- there is introspected, not held (see internal/authz, DESIGN §8).
--
-- IF NOT EXISTS is load-bearing for databases that applied the ORIGINAL
-- 0001 (which created this table): the runner tracks file NAMES, never
-- re-runs applied files, so this file lands as a no-op there — while a
-- fresh admin/all database gets the table exactly where the era of record
-- put it: before 0007, which layers role/must_change/created_at onto it.

CREATE TABLE IF NOT EXISTS users (
	username       TEXT PRIMARY KEY,
	passhash       TEXT NOT NULL,             -- argon2id + per-user salt
	token_version  INT NOT NULL DEFAULT 1     -- logout bumps; JWTs carry `tv`, must match
);
