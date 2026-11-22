-- 0009_photo_exif.sql — Round B (owner sketch 2026-11-22): the upload-time
-- EXIF capture. lumen NEVER parses image metadata (the strip promise is
-- test-pinned, §5) — mellon reads the curated fields client-side (exifr)
-- and lumen stores ONLY the allowlisted subset as jsonb. GPS cannot be
-- stored: it is not a key in the allowlist (denied by construction).
-- NULL = pre-capture uploads (the panel's honest empty state).
ALTER TABLE photos ADD COLUMN exif JSONB;
