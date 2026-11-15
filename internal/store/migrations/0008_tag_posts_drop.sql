-- The counter column dies (design amendment, 2026-11-15): the published
-- count per tag derives at READ from the search engine's facet
-- distribution on the posts index (status = published). The column was
-- the third copy of a truth the index already held fresher (every save
-- re-enqueues the index job; the counter only moved at publish/
-- unpublish/delete — the update path's missing recompute made it lie:
-- the tag-desk zero vs the dialog's live count, walked tonight).
-- The junction (post_tags) stays truth for REFERENCES (any status):
-- the delete guard + the dialog's used-by roster.
ALTER TABLE tags DROP COLUMN IF EXISTS posts;
