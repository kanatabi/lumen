-- 0014_tasks.sql — the first lumen-native APPLICATION (the app-center
-- direction, owner decisions 2026-11-25/26, revised 2026-11-27).
--
-- Admin-plane lineage (users.id FKs, the tasks workspace is personal
-- data — the cms databases never see these tables; the manifest test
-- enforces the classification). The module is self-contained: a future
-- micro-service split lifts internal/tasks + this schema + the three
-- seams (users.id, the authz principal, the scope gate).
--
-- v1 decisions baked in:
--   * NO positions/ordering (owner: the portal sorts client-side).
--   * SOFT delete (`deleted_at`) on projects AND tasks; completion is a
--     flag (`completed_at` on projects; done/done_at on tasks).
--   * a project may hold MANY admins (members.role); the creator is its
--     first admin, and the last-admin guard lives in the service.
--   * the creator can be deleted: the FK is SET NULL and the project
--     survives (a lifecycle hook promotes a member if no admin remains).
--   * invites are 24 HOURS old at most (code sets expires_at; one PENDING
--     invite per project+invitee; accept/decline/revoke DELETE the row).
--   * no purge jobs (owner: hold) — expired invites are filtered/checked
--     at use, never presumed gone.

INSERT INTO scopes (scope, description) VALUES
	('tasks:create', 'the tasks workspace: create projects and tasks'),
	('tasks:update', 'the tasks workspace: edit tasks, subtasks, comments, toggles'),
	('tasks:manage', 'the tasks workspace: project settings, invites, members and roles')
ON CONFLICT (scope) DO NOTHING;

CREATE TABLE task_projects (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	-- the creator may be deleted; the project survives (C4)
	creator_id UUID, -- users.id (no cross-DB FK: tasks owns its own DB)
	name TEXT NOT NULL,
	completed_at TIMESTAMPTZ,          -- the "completed" flag
	deleted_at   TIMESTAMPTZ,          -- the "deleted" flag: SOFT, reads filter
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX task_projects_live_idx ON task_projects(created_at)
	WHERE deleted_at IS NULL;

CREATE TABLE task_project_members (
	project_id UUID NOT NULL REFERENCES task_projects(id) ON DELETE CASCADE,
	user_id UUID NOT NULL, -- users.id (identity resolved via the admin directory)
	role TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('admin','member')),
	invited_by UUID, -- users.id
	joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (project_id, user_id)
);
-- "my projects" scans by user, not by project
CREATE INDEX task_project_members_user_idx ON task_project_members(user_id);
-- the creator's member row (role 'admin') is inserted in the same tx as
-- the project row — a project without its creator-member cannot exist.

CREATE TABLE task_project_invites (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES task_projects(id) ON DELETE CASCADE,
	inviter_id UUID NOT NULL, -- users.id
	-- resolved from the username at the door; the FK is the identity seam
	invitee_id UUID NOT NULL, -- users.id
	expires_at TIMESTAMPTZ NOT NULL,   -- 24h, computed in code
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- one PENDING invite per (project, invitee); accept/decline/revoke DELETE
CREATE UNIQUE INDEX task_project_invites_pending_idx
	ON task_project_invites(project_id, invitee_id);
CREATE INDEX task_project_invites_invitee_idx
	ON task_project_invites(invitee_id);

CREATE TABLE task_tasks (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES task_projects(id) ON DELETE CASCADE,
	created_by UUID, -- users.id
	assigned_to UUID, -- users.id; v1: ONE assignee
	title TEXT NOT NULL,
	notes TEXT NOT NULL DEFAULT '',
	priority TEXT NOT NULL DEFAULT 'none'
		CHECK (priority IN ('high','medium','low','none')),
	due_at TIMESTAMPTZ,
	done BOOLEAN NOT NULL DEFAULT false,
	done_at TIMESTAMPTZ,
	deleted_at TIMESTAMPTZ,            -- SOFT delete: reads filter
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX task_tasks_project_idx ON task_tasks(project_id, created_at)
	WHERE deleted_at IS NULL;
CREATE INDEX task_tasks_assignee_idx ON task_tasks(assigned_to)
	WHERE assigned_to IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX task_tasks_due_idx ON task_tasks(due_at)
	WHERE done = false AND deleted_at IS NULL;
-- multi-assignee extension path (recorded, not built): a task_assignees
-- join table replacing assigned_to.

CREATE TABLE task_subtasks (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	task_id UUID NOT NULL REFERENCES task_tasks(id) ON DELETE CASCADE,
	title TEXT NOT NULL,
	done BOOLEAN NOT NULL DEFAULT false,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX task_subtasks_task_idx ON task_subtasks(task_id);

CREATE TABLE task_comments (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	task_id UUID NOT NULL REFERENCES task_tasks(id) ON DELETE CASCADE,
	author_id UUID, -- users.id; history survives
	body TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX task_comments_task_idx ON task_comments(task_id);

CREATE TABLE task_activity (
	id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	task_id UUID NOT NULL REFERENCES task_tasks(id) ON DELETE CASCADE,
	actor_id UUID, -- users.id
	verb TEXT NOT NULL,          -- created|updated|assigned|completed|reopened|commented|deleted
	detail TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX task_activity_task_idx ON task_activity(task_id);
