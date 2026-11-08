-- 0004_media.sql — §6 GC ledger, realized with M5 (upload relay).
CREATE TABLE media_assets (
	id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	path       TEXT NOT NULL UNIQUE,       -- object key (content-addressed)
	sha256     TEXT NOT NULL,
	kind       TEXT NOT NULL,             -- 'upload' now; fenced-media GC joins the publish round
	post_slug  TEXT,
	nth        INT NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
