-- 0016_profile.sql — the account profile (owner round 2026-11-29): display
-- name + email on the admin-plane users lineage. 0002's rule holds: the cms
-- databases never see this file — users are the Control Plane's.
--
-- display_name is the human-facing name (later the public byline once
-- authoring lands); username stays the immutable login handle. email is the
-- private account contact — optional at first, unique when set, normalized
-- lowercase by the service, never published.

ALTER TABLE users ADD COLUMN display_name TEXT;
UPDATE users SET display_name = username;
ALTER TABLE users ALTER COLUMN display_name SET NOT NULL;
ALTER TABLE users ADD CONSTRAINT users_display_name_len
	CHECK (char_length(btrim(display_name)) BETWEEN 1 AND 80);

ALTER TABLE users ADD COLUMN email TEXT;
ALTER TABLE users ADD CONSTRAINT users_email_len
	CHECK (email IS NULL OR char_length(email) <= 254);
ALTER TABLE users ADD CONSTRAINT users_email_lower
	CHECK (email IS NULL OR email = lower(email));

CREATE UNIQUE INDEX users_email_lower_key ON users (lower(email)) WHERE email IS NOT NULL;
