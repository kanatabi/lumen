-- 0010_rbac_scopes.sql — the RBAC spine (owner rulings 2026-11-24): USER
-- roles are deprecated and DROPPED; users carry an explicit scope set; the
-- catalog is DB data the portal manages (users:manage-gated), so onboarding
-- a scope never means manual SQL. users gains a surrogate `id` (UUID) —
-- every relational reference (user_scopes, future provider-identity and
-- app tables) keys on it; username stays UNIQUE as the human login handle.
-- The process planes (LUMEN_ROLE, internal/role) are deployment topology
-- and untouched — two different "role" concepts, only one dies here.
--
-- Grants map the OLD roles to their effective access (the acceptance test
-- of this file: the estate behaves identically — admin held everything,
-- editor held the content desks): admin → every seeded scope, editor →
-- content:manage. The cms plane never applies this file (0002's rule: the
-- users lineage is admin-plane-only).

ALTER TABLE users ADD COLUMN id UUID NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE users ALTER COLUMN id SET NOT NULL;
ALTER TABLE users DROP CONSTRAINT users_pkey;
ALTER TABLE users ADD PRIMARY KEY (id);
ALTER TABLE users ADD CONSTRAINT users_username_key UNIQUE (username);

CREATE TABLE scopes (
	scope        TEXT PRIMARY KEY,   -- 'content:manage' — natural key, immutable
	description  TEXT NOT NULL,
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO scopes (scope, description) VALUES
	('content:manage',  'the content desks: posts, photos, tags, collections, media'),
	('secrets:admin',   'the whole Bao cockpit: values, pins, app onboarding, the estate matrix'),
	('users:manage',    'user creation, scope assignment, the scope catalog, password resets'),
	('settings:manage', 'portal-level settings (reserved; gated when settings grow)');

CREATE TABLE user_scopes (
	user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	scope   TEXT NOT NULL REFERENCES scopes(scope) ON DELETE CASCADE,
	PRIMARY KEY (user_id, scope)
);

-- the grants: editor → content:manage; admin → the full catalog.
INSERT INTO user_scopes (user_id, scope)
SELECT u.id, s.scope FROM users u JOIN scopes s ON s.scope = 'content:manage'
WHERE u.role = 'editor';

INSERT INTO user_scopes (user_id, scope)
SELECT u.id, s.scope FROM users u CROSS JOIN scopes s
WHERE u.role = 'admin';

ALTER TABLE users DROP COLUMN role;
