-- 0001_core.sql — lumen §6 data model, M1 scope.
-- Plain-SQL migrations, applied in filename order by cmd/seed (embedded FS);
-- `make dev` runs them. No ORM, no runner dependency: the files are the truth.

CREATE TYPE post_status AS ENUM ('draft', 'published', 'unpublished');

CREATE TABLE posts (
	id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	slug        TEXT NOT NULL UNIQUE,
	title       TEXT NOT NULL,
	subtitle    TEXT NOT NULL DEFAULT '',
	-- BE-owned display text (§1): explicit author text, default = first paragraph;
	-- computed at PUBLISH (M3), stored, never re-derived at read.
	preview     TEXT NOT NULL DEFAULT '',
	content_md  TEXT NOT NULL,           -- raw Markdown, ```mermaid fences INTACT (§3 Route B)
	-- Curation order for /api/posts: an explicit rank, not date desc (§1).
	rank        INT NOT NULL DEFAULT 0,
	status      post_status NOT NULL DEFAULT 'draft',
	rev         INT NOT NULL DEFAULT 1,  -- CAS for admin updates (M3); read API never exposes it
	-- The EDITORIAL date the contract displays (`date`, YYYY-MM-DD): always set,
	-- drafts included (admin surfaces show it); §1 filters/`from/to` window it.
	date        DATE NOT NULL,
	-- The publication instant; NULL until published. The public contract's
	-- `date` maps here-adjacent but the FE only ever sees the editorial `date`.
	published_at TIMESTAMPTZ,
	word_count  INT NOT NULL DEFAULT 0,  -- computed at publish (M3)
	read_time   INT NOT NULL DEFAULT 0,  -- minutes, computed at publish (M3); seed carries the corpus value
	thumbnail   TEXT,                    -- URL or NULL ⇒ listings render text-only
	created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX posts_status_rank ON posts (status, rank);
CREATE INDEX posts_date ON posts (date);

CREATE TABLE tags (
	id     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	-- name IS the slug (the corpus tag names are already slug-safe; lumen
	-- slugifies at creation, §8 semantics later).
	name   TEXT NOT NULL UNIQUE,
	-- Curation cloud count (§1: maintained at publish, NEVER aggregated at
	-- read). Seed writes the corpus counts; M3's publish transaction owns them.
	posts  INT NOT NULL DEFAULT 0,
	-- Curation membership, DISTINCT from vocabulary existence (§1: "the BE
	-- decides how many tags the cloud ships"). A tag row MUST exist so
	-- post_tags can reference any post tag (FK), but only cloud=true tags
	-- surface on GET /api/tags — earendil's cloud is a curated subset of the
	-- corpus (10 of 26 here), NOT every tag with a count. M3's publish owns
	-- promotion; the flag is the honest representation of that decision.
	cloud  BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE post_tags (
	post_id BIGINT NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
	tag_id  BIGINT NOT NULL REFERENCES tags(id)  ON DELETE CASCADE,
	-- Author-declared tag ORDER is payload (the FE renders as-is, doctrine #1):
	-- 0-based position within the post, written at seed/publish.
	position SMALLINT NOT NULL DEFAULT 0,
	PRIMARY KEY (post_id, tag_id)
);
CREATE INDEX post_tags_tag ON post_tags (tag_id);

CREATE TABLE collections (
	id   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	name TEXT NOT NULL UNIQUE  -- curation vocabulary, admin-internal (never a public axis)
);

CREATE TABLE photos (
	id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	src        TEXT NOT NULL,
	alt        TEXT NOT NULL DEFAULT '',
	width      INT NOT NULL,
	height     INT NOT NULL,
	sort       INT NOT NULL DEFAULT 0,        -- curation order for the gallery
	featured   BOOLEAN NOT NULL DEFAULT FALSE, -- answers /api/photos/featured
	gallery    BOOLEAN NOT NULL DEFAULT FALSE, -- THE public flag: only true reaches §1
	collection TEXT REFERENCES collections(name) -- admin-internal theming; NULL = uncategorized
);
CREATE INDEX photos_gallery ON photos (gallery, sort);

-- Session era of record (lumen §8): credentials are a HASHED DB row. The
-- users table LIVED here until the role split (owner ruling 2026-11-13):
-- the admin plane owns credentials, so the table moved to
-- 0002_admin_users.sql, classified admin-only. The cms plane's databases
-- must have NO users table at all — the mint, the revocation handle and
-- the setup wizard are not this plane's decisions. Databases that already
-- applied the original 0001 keep their table (the runner tracks names,
-- never re-runs applied files); fresh databases get the split.

-- Deferred by milestone, kept honest about it: media_assets (M5 GC), and the
-- publish-time derivations that populate preview/word_count/read_time (M3).
