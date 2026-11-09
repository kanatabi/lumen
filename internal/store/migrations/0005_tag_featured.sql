-- 0005: the cloud flag finally says what it means. `cloud` was the old-web
-- TAG CLOUD widget's name — and it kept colliding with "the infrastructure
-- cloud" in every reader's head (owner asked exactly that, 2026-11-10).
-- RENAME to `featured`: the word the tag desk uses, the state the desk
-- toggles. Values survive untouched — the same curated rows; promotion was
-- seed-only until the desk existed, and is desk-only now.
ALTER TABLE tags RENAME COLUMN cloud TO featured;
