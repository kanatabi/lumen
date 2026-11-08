-- 0003_admin.sql — M3 additions to §6:
-- soft delete + publish idempotence hash + photo created_at (admin windows
-- filter on createdAt per §8).

ALTER TABLE posts ADD COLUMN deleted_at TIMESTAMPTZ;
-- soft delete frees the SLUG: uniqueness is a property of the LIVE namespace,
-- not of history rows (re-creating a deleted post's slug must work; §8)
ALTER TABLE posts DROP CONSTRAINT posts_slug_key;
CREATE UNIQUE INDEX posts_slug_live ON posts (slug) WHERE deleted_at IS NULL;
ALTER TABLE posts ADD COLUMN content_hash TEXT;
CREATE INDEX posts_alive ON posts (status) WHERE deleted_at IS NULL;

ALTER TABLE photos ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();
