-- 0006: recency is finally real for the vocabulary. `created_at` enables the
-- dashboard's "latest tags" card (recency has no other source — sorting by
-- name would be a lie); `updated_at` marks any desk touch (rename, feature,
-- bulk) and is bumped EXPLICITLY in every store UPDATE — no hidden triggers,
-- the mechanism stays greppable.
-- HONESTY NOTE: rows predating this migration share the migration instant;
-- recency is true from today forward, not retroactive.
ALTER TABLE tags ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE tags ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
