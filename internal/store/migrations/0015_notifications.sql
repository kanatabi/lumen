-- 0015_notifications.sql — the estate-generic notification facility (the
-- app-center direction, owner decisions 2026-11-25/26/27).
--
-- Admin-plane lineage (user_id FK; notifications are personal data, and
-- the admin plane owns the users family). Any lumen module can Send
-- through the in-process Notifier seam; the HTTP send door is a SEPARATE
-- concern (see below).
--
-- Shapes:
--   app  — the sender's slug ('tasks', 'content', ...), validated.
--   type — the event kind ('project.invite', 'task.assigned', ...).
--   title — emoji-friendly PLAIN TEXT (rendered escaped).
--   body  — emoji-friendly SANITIZED HTML (a narrow inline allowlist:
--           b/strong/i/em/u/s/code/br/a[href]/span[style=color|background-
--           color|font-weight]); sanitized on WRITE (bluemonday) and
--           again on render (DOMPurify). Never scripts/iframes/images.
--   payload — JSONB click-through: { url: "/tasks", refs... }; `url` MUST
--             be a same-origin path (validated on write and on click).
--
-- TTL is DATA (owner: Postgres, not Redis). There is NO purge job yet
-- (owner: hold) — every read filters `expires_at > now()`, and the
-- expires index is ready for the day a janitor lands (the ADMIN plane
-- runs no River worker, so that janitor will be in-process or a CronJob).
--
-- NO service-token rows (§5: tokens are never data). The SEND door
-- presents ONE shared token, read from the environment (a MANUAL k8s
-- Secret, `lumen-notification-token`, §5.6 family) — deliberately NOT a
-- kv/lumen/shared key, so no epoch flip rides an in-cluster-only door.
-- Unset = the route is unmounted. The `app` slug comes from the body
-- (display/audit); with one token it is not an authentication boundary
-- (owner: internal, in-cluster use only).

CREATE TABLE notifications (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	app TEXT NOT NULL,
	type TEXT NOT NULL,
	title TEXT NOT NULL,
	body TEXT NOT NULL DEFAULT '',
	payload JSONB NOT NULL DEFAULT '{}'::jsonb,
	read_at TIMESTAMPTZ,
	expires_at TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '30 days',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- the panel's list: newest first, per user
CREATE INDEX notifications_user_idx ON notifications(user_id, created_at DESC);
-- the bell's count: unread only
CREATE INDEX notifications_unread_idx ON notifications(user_id) WHERE read_at IS NULL;
-- the future janitor's scan
CREATE INDEX notifications_expires_idx ON notifications(expires_at);
