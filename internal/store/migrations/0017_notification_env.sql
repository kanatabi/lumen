-- 0017_notification_env.sql — the app-plane split (2026-11-29). Task
-- notifications are minted by the staging/production cms planes, but the
-- bell is ONE shared admin service over a shared user inbox. env scopes a
-- row to the console whose /tasks link can resolve it; NULL keeps every
-- other app's notifications estate-global.
ALTER TABLE notifications ADD COLUMN env TEXT;
ALTER TABLE notifications ADD CONSTRAINT notifications_env_len
	CHECK (env IS NULL OR char_length(env) BETWEEN 1 AND 32);
ALTER TABLE notifications ADD CONSTRAINT notifications_env_lower
	CHECK (env IS NULL OR env = lower(env));
CREATE INDEX notifications_env_idx ON notifications (user_id, env, created_at DESC);
