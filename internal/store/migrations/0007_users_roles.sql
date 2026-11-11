-- Multi-user portal + first-boot setup wizard (owner ruling 2026-11-11).
-- role gates user administration; must_change drives the forced password
-- update (HTTP 428) for seeded temps and resets; created_at is lineage.
-- Every pre-existing user becomes admin: the single-user era's only account
-- IS the owner — a wizard that locked them out would be a bug, not a gate.
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'editor' CHECK (role IN ('admin', 'editor'));
ALTER TABLE users ADD COLUMN must_change BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();
UPDATE users SET role = 'admin';
