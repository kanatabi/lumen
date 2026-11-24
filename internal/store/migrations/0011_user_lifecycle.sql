-- 0011_user_lifecycle.sql — the user-lifecycle verbs (the RBAC round's
-- second cut, owner call 2026-11-24): an admin can DISABLE an account
-- (denies login AND kills its live sessions via the token_version bump —
-- the existing revocation lever), enable it again, and DELETE it. The
-- admin-plane-only rule holds (0002's): this file never touches the cms
-- databases.

ALTER TABLE users ADD COLUMN disabled BOOLEAN NOT NULL DEFAULT false;
