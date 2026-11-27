// Package tasks is the self-contained tasks application store (owner
// direction 2026-11-26: monolith first, seams for the split). It reaches the
// rest of lumen through exactly three seams: the users.id FK (identity), the
// caller's id (the authz principal), and the scope gate in front of the
// routes (M2). Nothing else is shared — a future micro-service split lifts
// this package plus the schema.
//
// The layer's ONE authorization rule: every project or task addressed by an
// id is resolved through task_project_members for the CALLING user. An id
// from the wire is never evidence of access. Membership is authorization for
// reads AND writes; the route's scope gate is the front door, the membership
// row is the room key. Non-membership is indistinguishable from a nonexistent
// row (ErrNotFound), and soft-deleted rows are invisible to every live read.
//
// Mutations that change task state write their activity row INSIDE the same
// transaction (one row per verb: created|updated|assigned|completed|
// reopened|commented|deleted) — the timeline can never lag the task it
// describes.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is the ONE denial voice: an unknown id, a row belonging to
// another project, a soft-deleted row and a non-member's request all answer
// identically — existence is not a signal, and the wire cannot probe the
// estate by id.
var ErrNotFound = errors.New("not found")

// ErrConflict backs 409: the operation would break a project invariant (the
// last admin leaving, a duplicate membership or pending invite, an assignee
// who is not a member). The caller's state is valid, the request is not.
var ErrConflict = errors.New("conflict")

// ErrExpired backs 410 for invite acceptance: the invite existed but its TTL
// passed. The row is consumed on the way out, so a retry is a plain 404.
var ErrExpired = errors.New("invite expired")

// ErrForbidden backs 403: the caller IS in the project but the project role
// is below the verb (a member renaming the project, a non-author deleting
// someone else's comment). Unlike ErrNotFound this deliberately confirms the
// caller's membership — the room key is real, the action is not allowed.
var ErrForbidden = errors.New("forbidden")

// ErrInvalidView rejects an unknown task-list view at the store boundary:
// silently treating it as "all" would hand the UI a wrong list instead of an
// honest 400 from the caller's validator.
var ErrInvalidView = errors.New("invalid task view")

// The list views (owner C5: the day boundaries are the CLIENT's, passed in
// as *time.Location; nil means UTC).
const (
	ViewToday    = "today"
	ViewUpcoming = "upcoming"
	ViewAll      = "all"
)

// Optional carries field PRESENCE for a patch. A zero Optional leaves the
// column alone; Set with a nil pointer-value writes NULL. PATCH SQL must tell
// the two apart or clearing a due date / unassigning a task would be
// impossible — nil-ness alone cannot express both "omitted" and "clear".
type Optional[T any] struct {
	Set   bool
	Value T
}

// Some marks a patch field present with a value.
func Some[T any](value T) Optional[T] { return Optional[T]{Set: true, Value: value} }

// Clear marks a patch field present with its zero value — for the pointer
// fields of TaskPatch that writes NULL (clear the due date / unassign).
func Clear[T any]() Optional[T] { return Optional[T]{Set: true} }

// Project is one project as the CALLER sees it: Role is the caller's own
// membership role, and the counts are the live (non-deleted) task rollup the
// workspace renders. DeletedAt is always nil on a live read; it is carried so
// the flag is never implicitly forgotten.
type Project struct {
	ID          string
	CreatorID   *string // NULL once the creator's account is deleted (C4: the project survives)
	Name        string
	Role        string // the caller's role in this project: admin|member
	CompletedAt *time.Time
	DeletedAt   *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
	TaskCount   int
	OpenCount   int
}

// Member is one roster line. Username rides the users.id identity seam for
// display only — no other user column is read.
type Member struct {
	UserID    string
	Username  string
	Role      string
	InvitedBy *string
	JoinedAt  time.Time
}

// Invite is one pending invitation, hydrated with the project and inviter
// names the notification copy needs (M2 sends; this store never notifies).
type Invite struct {
	ID          string
	ProjectID   string
	ProjectName string
	InviterID   string
	InviterName string
	InviteeID   string
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

// Task is one task row plus the display rollups the grouped workspace needs.
// AssignedTo/CreatedBy are the users.id handles; AssigneeName/CreatorName are
// the display names (NULL when the account was deleted — the row survives).
type Task struct {
	ID           string
	ProjectID    string
	ProjectName  string
	CreatedBy    *string
	CreatorName  *string
	AssignedTo   *string
	AssigneeName *string
	Title        string
	Notes        string
	Priority     string
	DueAt        *time.Time
	Done         bool
	DoneAt       *time.Time
	DeletedAt    *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	CommentCount int
	SubtaskCount int
	SubtaskDone  int
}

// TaskInput is a create. Empty Priority defaults to 'none' (the schema's
// default verbatim) and empty Notes to ”; AssignedTo, when set, MUST be a
// member of the project (validated in the create transaction).
type TaskInput struct {
	Title      string
	Notes      string
	Priority   string
	DueAt      *time.Time
	AssignedTo *string
}

// TaskPatch is a field-presence patch. Only fields marked Set are written;
// a Set pointer-field with nil clears the column.
type TaskPatch struct {
	Title      Optional[string]
	Notes      Optional[string]
	Priority   Optional[string]
	DueAt      Optional[*time.Time]
	AssignedTo Optional[*string]
}

// Subtask is one checklist row.
type Subtask struct {
	ID        string
	TaskID    string
	Title     string
	Done      bool
	CreatedAt time.Time
}

// SubtaskPatch is a field-presence patch for a checklist row.
type SubtaskPatch struct {
	Title Optional[string]
	Done  Optional[bool]
}

// Comment is one comment; a deleted author leaves AuthorID/AuthorName nil and
// the body (history survives the account, per the schema).
type Comment struct {
	ID         string
	TaskID     string
	AuthorID   *string
	AuthorName *string
	Body       string
	CreatedAt  time.Time
}

// Activity is one timeline row. ActorID is *string on purpose: the FK is SET
// NULL on user delete, and a NULL actor is the honest "deleted user" display.
type Activity struct {
	ID        int64
	TaskID    string
	ActorID   *string
	ActorName *string
	Verb      string
	Detail    string
	CreatedAt time.Time
}

// TaskDetail is the drawer payload: the task plus its whole child set, read
// from one repeatable-read snapshot so the tabs agree with each other.
type TaskDetail struct {
	Task     Task
	Subtasks []Subtask
	Comments []Comment
	Activity []Activity
}

// Stats is the rollup row. CompletedWeek counts a MONDAY-START week (documented
// because the boundary is a product decision, not a database fact); every
// number is computed against the caller's location so the cards match the
// client's calendar, not the server's.
type Stats struct {
	Open          int
	DueToday      int
	CompletedWeek int
	Overdue       int
}

// Store is the tasks persistence layer. One pgxpool: every query is a
// parameterized prepared statement per the lumen SQL doctrine.
type Store struct{ pool *pgxpool.Pool }

// NewStore binds the tasks store to the estate's pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// querier is the slice of pgx shared by the pool (autocommit reads) and a
// transaction (mutations that must carry their activity row atomically).
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// notFound folds the two "this id cannot resolve" database answers into the
// one denial voice: no row, or a malformed UUID that cannot name a row.
func notFound(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) || sqlState(err, "22P02") {
		return ErrNotFound
	}
	return err
}

// sqlState reports one PostgreSQL SQLSTATE class.
func sqlState(err error, state string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == state
}

// dayBounds is the caller's local calendar day as a half-open [start, end)
// interval. The bounds are real instants (carrying loc), so pgx sends UTC
// timestamptz values and the comparison happens on the server without ever
// converting the stored due_at — the client's day, not the server's.
func dayBounds(now time.Time, loc *time.Location) (time.Time, time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	local := now.In(loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 0, 1)
}

// mondayStart is the Monday 00:00 before/at now in loc: the stats card's
// "this week" is a calendar week (owner picks Monday as the first day).
func mondayStart(now time.Time, loc *time.Location) time.Time {
	start, _ := dayBounds(now, loc)
	weekday := int(start.Weekday())
	if weekday == 0 {
		weekday = 7 // Sunday closes a Monday-start week
	}
	return start.AddDate(0, 0, -(weekday - 1))
}

// sameTime reports whether two nullable timestamps are the same instant; nil
// equals only nil (a due date that was never set and a cleared one are the
// same column state).
func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// sameStringID compares nullable UUID strings (nil only equals nil).
func sameStringID(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// lockProjectRole resolves the caller's membership AND locks the live project
// row. All project-admin mutations take this lock first: it serializes role
// changes, last-admin decisions, invites and project flags against each
// other, so two admins cannot both observe "another admin remains" and both
// leave. A non-member, a soft-deleted project and an unknown id all present
// as ErrNotFound — one denial voice.
func (s *Store) lockProjectRole(ctx context.Context, tx pgx.Tx, callerID, projectID string) (string, error) {
	var role string
	err := tx.QueryRow(ctx, `
		SELECT m.role
		  FROM task_projects p
		  JOIN task_project_members m
		    ON m.project_id = p.id AND m.user_id = $2::uuid
		 WHERE p.id = $1::uuid
		   AND p.deleted_at IS NULL
		   FOR UPDATE OF p`, projectID, callerID).Scan(&role)
	if err != nil {
		return "", notFound(err)
	}
	return role, nil
}

// requireProjectAdmin is lockProjectRole plus the role verdict: the caller
// must be the project's admin (ErrForbidden otherwise — membership is
// confirmed, the verb is not).
func (s *Store) requireProjectAdmin(ctx context.Context, tx pgx.Tx, callerID, projectID string) error {
	role, err := s.lockProjectRole(ctx, tx, callerID, projectID)
	if err != nil {
		return err
	}
	if role != "admin" {
		return ErrForbidden
	}
	return nil
}

// lockMember checks the caller's membership for a task write and takes a
// SHARE lock: a concurrent RemoveMember must wait for this transaction, so a
// row can never be created/edited under a membership that is already gone.
func (s *Store) lockMember(ctx context.Context, tx pgx.Tx, callerID, projectID string) (string, error) {
	var role string
	err := tx.QueryRow(ctx, `
		SELECT m.role
		  FROM task_project_members m
		  JOIN task_projects p ON p.id = m.project_id
		 WHERE m.project_id = $1::uuid
		   AND m.user_id = $2::uuid
		   AND p.deleted_at IS NULL
		   FOR SHARE OF m`, projectID, callerID).Scan(&role)
	if err != nil {
		return "", notFound(err)
	}
	return role, nil
}

// assigneeForAssignment resolves a prospective assignee through the project
// roster and LOCKS that membership row (KEY SHARE): RemoveMember deletes the
// membership first and clears assignments second, both inside one tx, so a
// racing assignment either blocks until the removal is done (and then sees no
// member → ErrConflict) or is already committed when the removal clears it.
// The returned username is the activity detail (a person, not a UUID).
func (s *Store) assigneeForAssignment(ctx context.Context, tx pgx.Tx, projectID, userID string) (string, error) {
	var username string
	err := tx.QueryRow(ctx, `
		SELECT u.username
		  FROM task_project_members m
		  JOIN users u ON u.id = m.user_id
		  JOIN task_projects p ON p.id = m.project_id
		 WHERE m.project_id = $1::uuid
		   AND m.user_id = $2::uuid
		   AND p.deleted_at IS NULL
		   FOR KEY SHARE OF m`, projectID, userID).Scan(&username)
	return username, notFound(err)
}

// writeActivity appends one timeline row inside the caller's transaction.
// actorID is written as a UUID; the column itself is nullable for the
// SET NULL user-delete path. Detail is display text chosen by the verb.
func writeActivity(ctx context.Context, tx pgx.Tx, taskID, actorID, verb, detail string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO task_activity (task_id, actor_id, verb, detail)
		VALUES ($1::uuid, $2::uuid, $3, $4)`, taskID, actorID, verb, detail)
	return err
}

// ---- projects ----

// CreateProject inserts the project and its creator's admin membership in ONE
// transaction: per the schema, a project without its creator-member cannot
// exist, so a failure between the two statements must leave neither row. The
// returned Role is the creator's, by construction.
func (s *Store) CreateProject(ctx context.Context, callerID, name string) (Project, error) {
	var project Project
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO task_projects (creator_id, name)
			VALUES ($1::uuid, $2)
			RETURNING id, creator_id, name, completed_at, deleted_at, created_at, updated_at`,
			callerID, name).
			Scan(&project.ID, &project.CreatorID, &project.Name, &project.CompletedAt,
				&project.DeletedAt, &project.CreatedAt, &project.UpdatedAt)
		if err != nil {
			if sqlState(err, "23503") {
				return ErrNotFound // a caller id that no longer exists is a dead session
			}
			return notFound(err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO task_project_members (project_id, user_id, role)
			VALUES ($1::uuid, $2::uuid, 'admin')`, project.ID, callerID); err != nil {
			return err
		}
		project.Role = "admin"
		return nil
	})
	return project, err
}

// ListProjects returns the caller's live projects with the caller's role and
// the live task/open counts. The membership join is the scope: projects the
// caller cannot see are not filtered out afterwards, they are never selected.
// Completed projects stay listed (completed_at is a flag, C4 — the client can
// reopen or archive in presentation); only deleted_at removes one.
func (s *Store) ListProjects(ctx context.Context, callerID string) ([]Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.creator_id, p.name, p.completed_at, p.deleted_at,
		       p.created_at, p.updated_at, m.role,
		       (SELECT count(*) FROM task_tasks t
		         WHERE t.project_id = p.id AND t.deleted_at IS NULL),
		       (SELECT count(*) FROM task_tasks t
		         WHERE t.project_id = p.id AND t.deleted_at IS NULL AND NOT t.done)
		  FROM task_projects p
		  JOIN task_project_members m
		    ON m.project_id = p.id AND m.user_id = $1::uuid
		 WHERE p.deleted_at IS NULL
		 ORDER BY p.created_at, p.id`, callerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var project Project
		if err := rows.Scan(&project.ID, &project.CreatorID, &project.Name, &project.CompletedAt,
			&project.DeletedAt, &project.CreatedAt, &project.UpdatedAt, &project.Role,
			&project.TaskCount, &project.OpenCount); err != nil {
			return nil, err
		}
		out = append(out, project)
	}
	return out, rows.Err()
}

// RenameProject is admin-only. The project row is locked first, so the rename
// cannot land on a project that a concurrent settings verb just soft-deleted.
func (s *Store) RenameProject(ctx context.Context, callerID, projectID, name string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.requireProjectAdmin(ctx, tx, callerID, projectID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE task_projects
			   SET name = $2, updated_at = now()
			 WHERE id = $1::uuid AND deleted_at IS NULL`, projectID, name)
		if err != nil {
			return notFound(err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetProjectCompleted is admin-only and carries the completed_at flag: setting
// keeps the ORIGINAL completion instant (COALESCE) so repeated "complete"
// calls do not rewrite history; clearing writes NULL for a reopen. The project
// row lock serializes this against rename/delete.
func (s *Store) SetProjectCompleted(ctx context.Context, callerID, projectID string, completed bool) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.requireProjectAdmin(ctx, tx, callerID, projectID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE task_projects
			   SET completed_at = CASE WHEN $2::boolean THEN COALESCE(completed_at, now()) ELSE NULL END,
			       updated_at = now()
			 WHERE id = $1::uuid AND deleted_at IS NULL`, projectID, completed)
		if err != nil {
			return notFound(err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SoftDeleteProject is admin-only and sets deleted_at (owner C4: there is no
// destructive project delete in v1). The row itself stays — tasks, members and
// comments survive as history — and every live read filters it out. Refusing
// to delete an already-deleted project (via the live WHERE) also means the
// verb cannot be used to probe hidden ids.
func (s *Store) SoftDeleteProject(ctx context.Context, callerID, projectID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.requireProjectAdmin(ctx, tx, callerID, projectID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE task_projects
			   SET deleted_at = now(), updated_at = now()
			 WHERE id = $1::uuid AND deleted_at IS NULL`, projectID)
		if err != nil {
			return notFound(err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ---- members ----

// ListMembers returns the live project's roster, hydrated with usernames via
// the users.id identity seam. The caller-membership join is mandatory: a
// non-member gets zero rows, which this method reports as ErrNotFound (a live
// project always has at least one member, so zero is denial, not empty).
func (s *Store) ListMembers(ctx context.Context, callerID, projectID string) ([]Member, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.user_id, u.username, m.role, m.invited_by, m.joined_at
		  FROM task_project_members m
		  JOIN users u ON u.id = m.user_id
		  JOIN task_projects p ON p.id = m.project_id
		  JOIN task_project_members me
		    ON me.project_id = m.project_id AND me.user_id = $2::uuid
		 WHERE m.project_id = $1::uuid
		   AND p.deleted_at IS NULL
		 ORDER BY m.joined_at, m.user_id`, projectID, callerID)
	if err != nil {
		return nil, notFound(err)
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var member Member
		if err := rows.Scan(&member.UserID, &member.Username, &member.Role,
			&member.InvitedBy, &member.JoinedAt); err != nil {
			return nil, err
		}
		out = append(out, member)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// SetMemberRole is admin-only (C2: many admins, promote AND demote). It locks
// the project row so the last-admin count is decided one admin change at a
// time; demoting the final admin is ErrConflict — a project must always keep
// at least one admin, or nobody could ever manage it again. Setting the role
// a member already holds is a no-op (the admin's write is the whole truth).
func (s *Store) SetMemberRole(ctx context.Context, callerID, projectID, targetID, role string) error {
	if role != "admin" && role != "member" {
		return fmt.Errorf("tasks: invalid role %q", role)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.requireProjectAdmin(ctx, tx, callerID, projectID); err != nil {
			return err
		}
		var current string
		err := tx.QueryRow(ctx, `
			SELECT role
			  FROM task_project_members
			 WHERE project_id = $1::uuid AND user_id = $2::uuid
			   FOR UPDATE`, projectID, targetID).Scan(&current)
		if err != nil {
			return notFound(err)
		}
		if current == role {
			return nil
		}
		if current == "admin" && role == "member" {
			var admins int
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM task_project_members
				 WHERE project_id = $1::uuid AND role = 'admin'`, projectID).Scan(&admins); err != nil {
				return err
			}
			if admins <= 1 {
				return ErrConflict
			}
		}
		_, err = tx.Exec(ctx, `
			UPDATE task_project_members
			   SET role = $3
			 WHERE project_id = $1::uuid AND user_id = $2::uuid`, projectID, targetID, role)
		return err
	})
}

// RemoveMember is admin-only and does TWO things in one transaction: drop the
// membership, then clear that user's assignee on the project's tasks. The
// delete goes FIRST so the assignment clear sees every task that committed
// before the membership row was free (a racing CreateTask/UpdateTask holds a
// KEY SHARE lock on the roster row until it commits); the invariant "an
// assignee is always a member" therefore cannot be broken by the race. The
// last-admin guard is the same invariant as SetMemberRole: a project with
// zero admins is unmanageable.
func (s *Store) RemoveMember(ctx context.Context, callerID, projectID, targetID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.requireProjectAdmin(ctx, tx, callerID, projectID); err != nil {
			return err
		}
		var targetRole string
		err := tx.QueryRow(ctx, `
			SELECT role
			  FROM task_project_members
			 WHERE project_id = $1::uuid AND user_id = $2::uuid
			   FOR UPDATE`, projectID, targetID).Scan(&targetRole)
		if err != nil {
			return notFound(err)
		}
		if targetRole == "admin" {
			var admins int
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM task_project_members
				 WHERE project_id = $1::uuid AND role = 'admin'`, projectID).Scan(&admins); err != nil {
				return err
			}
			if admins <= 1 {
				return ErrConflict
			}
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM task_project_members
			 WHERE project_id = $1::uuid AND user_id = $2::uuid`, projectID, targetID); err != nil {
			return err
		}
		// Soft-deleted tasks are cleared too: a future restore must not
		// resurrect an assignee who is no longer in the project.
		if _, err := tx.Exec(ctx, `
			UPDATE task_tasks
			   SET assigned_to = NULL, updated_at = now()
			 WHERE project_id = $1::uuid AND assigned_to = $2::uuid`, projectID, targetID); err != nil {
			return err
		}
		return nil
	})
}

// ---- invites ----

// UserIDByUsername resolves the invite wire's username to the identity id
// (the users.id seam). An unknown name is ErrNotFound; the service maps it to
// ErrInvalid because a mistyped invitee is the caller's typo, not a hidden
// estate resource. This is the ONLY user lookup the tasks module performs
// beyond its identity-display joins.
func (s *Store) UserIDByUsername(ctx context.Context, username string) (string, error) {
	var id string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM users WHERE username = $1`, username).Scan(&id); err != nil {
		return "", notFound(err)
	}
	return id, nil
}

const inviteSelect = `
	SELECT i.id, i.project_id, p.name, i.inviter_id, inviter.username,
	       i.invitee_id, i.expires_at, i.created_at
	  FROM task_project_invites i
	  JOIN task_projects p ON p.id = i.project_id
	  JOIN users inviter ON inviter.id = i.inviter_id`

func scanInvite(row pgx.Row) (Invite, error) {
	var invite Invite
	err := row.Scan(&invite.ID, &invite.ProjectID, &invite.ProjectName,
		&invite.InviterID, &invite.InviterName, &invite.InviteeID,
		&invite.ExpiresAt, &invite.CreatedAt)
	return invite, err
}

// CreateInvite is admin-only. It refuses an invitee who is ALREADY a member
// (ErrConflict — no duplicate roster membership) and relies on the partial
// unique index for the one-pending-invite rule, mapping its 23505 to the same
// ErrConflict; that index is the concurrency backstop for two admins inviting
// the same person at once. The expires_at is set by the caller (24h in the
// service) — this store never guesses a TTL.
func (s *Store) CreateInvite(ctx context.Context, callerID, projectID, inviteeID string, expiresAt time.Time) (Invite, error) {
	var invite Invite
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.requireProjectAdmin(ctx, tx, callerID, projectID); err != nil {
			return err
		}
		var one int
		err := tx.QueryRow(ctx, `
			SELECT 1
			  FROM task_project_members
			 WHERE project_id = $1::uuid AND user_id = $2::uuid`, projectID, inviteeID).Scan(&one)
		switch {
		case err == nil:
			return ErrConflict // already on the roster
		case errors.Is(err, pgx.ErrNoRows):
			// the expected path: not a member yet
		default:
			return notFound(err) // malformed invitee id cannot name a person
		}
		var inviteID string
		err = tx.QueryRow(ctx, `
			INSERT INTO task_project_invites (project_id, inviter_id, invitee_id, expires_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4)
			RETURNING id`, projectID, callerID, inviteeID, expiresAt).Scan(&inviteID)
		if err != nil {
			switch {
			case sqlState(err, "23505"):
				return ErrConflict // the pending-invite unique index is the backstop
			case sqlState(err, "23503"):
				return ErrNotFound // no such invitee account
			default:
				return notFound(err)
			}
		}
		invite, err = scanInvite(tx.QueryRow(ctx, inviteSelect+` WHERE i.id = $1::uuid`, inviteID))
		return err
	})
	return invite, err
}

// ListInvitesFor is the INVITEE's inbox: pending (unexpired) invites only,
// for live projects only (an invite into a soft-deleted project is dead
// weight and must not offer an accept that will 404). Expired rows are left
// in place (no purge job, owner: hold) — the filter is the truth, not a
// presumed cleanup.
func (s *Store) ListInvitesFor(ctx context.Context, callerID string) ([]Invite, error) {
	rows, err := s.pool.Query(ctx, inviteSelect+`
		 WHERE i.invitee_id = $1::uuid
		   AND i.expires_at > now()
		   AND p.deleted_at IS NULL
		 ORDER BY i.created_at, i.id`, callerID)
	if err != nil {
		return nil, notFound(err)
	}
	defer rows.Close()
	out := []Invite{}
	for rows.Next() {
		invite, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, invite)
	}
	return out, rows.Err()
}

// AcceptInvite turns the caller's pending invite into a membership in ONE
// transaction: load FOR UPDATE (the row is the claim), reject anyone but the
// invitee as not-found, consume the row, insert role='member'. An EXPIRED
// invite is deleted before ErrExpired is returned, so the 410 is final (the
// next attempt is a plain 404) and no purge job is needed; that delete is
// committed deliberately, not rolled back with the rejection. A soft-deleted
// project consumes the invite and answers not-found. ON CONFLICT DO NOTHING
// keeps an existing row — it can never DOWNGRADE an admin who accepted twice.
func (s *Store) AcceptInvite(ctx context.Context, callerID, inviteID string) (Invite, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Invite{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var invite Invite
	var projectDeleted, expired bool
	err = tx.QueryRow(ctx, `
		SELECT i.id, i.project_id, p.name, i.inviter_id, inviter.username,
		       i.invitee_id, i.expires_at, i.created_at,
		       p.deleted_at IS NOT NULL,
		       i.expires_at <= now()
		  FROM task_project_invites i
		  JOIN task_projects p ON p.id = i.project_id
		  JOIN users inviter ON inviter.id = i.inviter_id
		 WHERE i.id = $1::uuid AND i.invitee_id = $2::uuid
		   FOR UPDATE OF i`, inviteID, callerID).
		Scan(&invite.ID, &invite.ProjectID, &invite.ProjectName,
			&invite.InviterID, &invite.InviterName, &invite.InviteeID,
			&invite.ExpiresAt, &invite.CreatedAt, &projectDeleted, &expired)
	if err != nil {
		return Invite{}, notFound(err)
	}
	if projectDeleted {
		if _, err := tx.Exec(ctx, `
			DELETE FROM task_project_invites WHERE id = $1::uuid`, invite.ID); err != nil {
			return Invite{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Invite{}, err
		}
		return Invite{}, ErrNotFound
	}
	if expired {
		if _, err := tx.Exec(ctx, `
			DELETE FROM task_project_invites WHERE id = $1::uuid`, invite.ID); err != nil {
			return Invite{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Invite{}, err
		}
		return Invite{}, ErrExpired
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM task_project_invites WHERE id = $1::uuid`, invite.ID); err != nil {
		return Invite{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_project_members (project_id, user_id, role, invited_by)
		VALUES ($1::uuid, $2::uuid, 'member', $3::uuid)
		ON CONFLICT (project_id, user_id) DO NOTHING`,
		invite.ProjectID, callerID, invite.InviterID); err != nil {
		return Invite{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Invite{}, err
	}
	return invite, nil
}

// DeclineInvite deletes the invitee's own invite. Expired invites are
// declinable too (the delete is cleanup either way); only the invitee can see
// or consume the row, so another user's id is the same not-found.
func (s *Store) DeclineInvite(ctx context.Context, callerID, inviteID string) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM task_project_invites
		 WHERE id = $1::uuid AND invitee_id = $2::uuid`, inviteID, callerID)
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeInvite is admin-only: the invite must belong to the project the admin
// manages, so knowing another project's invite id is not enough to cancel it.
func (s *Store) RevokeInvite(ctx context.Context, callerID, projectID, inviteID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.requireProjectAdmin(ctx, tx, callerID, projectID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			DELETE FROM task_project_invites
			 WHERE id = $1::uuid AND project_id = $2::uuid`, inviteID, projectID)
		if err != nil {
			return notFound(err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ---- tasks ----

const taskSelect = `
	SELECT t.id, t.project_id, p.name, t.created_by, creator.username,
	       t.assigned_to, assignee.username,
	       t.title, t.notes, t.priority, t.due_at, t.done, t.done_at,
	       t.deleted_at, t.created_at, t.updated_at,
	       (SELECT count(*) FROM task_comments c WHERE c.task_id = t.id),
	       (SELECT count(*) FROM task_subtasks s WHERE s.task_id = t.id),
	       (SELECT count(*) FROM task_subtasks s WHERE s.task_id = t.id AND s.done)
	  FROM task_tasks t
	  JOIN task_projects p ON p.id = t.project_id
	  LEFT JOIN users creator ON creator.id = t.created_by
	  LEFT JOIN users assignee ON assignee.id = t.assigned_to`

func scanTask(row pgx.Row) (Task, error) {
	var task Task
	err := row.Scan(&task.ID, &task.ProjectID, &task.ProjectName,
		&task.CreatedBy, &task.CreatorName, &task.AssignedTo, &task.AssigneeName,
		&task.Title, &task.Notes, &task.Priority, &task.DueAt, &task.Done, &task.DoneAt,
		&task.DeletedAt, &task.CreatedAt, &task.UpdatedAt,
		&task.CommentCount, &task.SubtaskCount, &task.SubtaskDone)
	return task, err
}

// taskForCaller is the ONE task read: it joins the caller's membership, the
// live project and the live task. Every verb that returns a task rides this
// after its write, so the returned row is exactly what the caller may see.
func (s *Store) taskForCaller(ctx context.Context, q querier, callerID, taskID string) (Task, error) {
	task, err := scanTask(q.QueryRow(ctx, taskSelect+`
		  JOIN task_project_members me
		    ON me.project_id = t.project_id AND me.user_id = $2::uuid
		 WHERE t.id = $1::uuid
		   AND t.deleted_at IS NULL
		   AND p.deleted_at IS NULL`, taskID, callerID))
	if err != nil {
		return Task{}, notFound(err)
	}
	return task, nil
}

// CreateTask is member-only. The caller's membership is share-locked and a
// set assignee is resolved through the roster (KEY SHARE) inside this
// transaction: assigning a non-member is ErrConflict, and the lock makes the
// member-check and the insert atomic against a concurrent RemoveMember. The
// timeline gets 'created', plus 'assigned' when the task is born assigned.
func (s *Store) CreateTask(ctx context.Context, callerID, projectID string, in TaskInput) (Task, error) {
	var created Task
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := s.lockMember(ctx, tx, callerID, projectID); err != nil {
			return err
		}
		assigneeName := ""
		if in.AssignedTo != nil {
			name, err := s.assigneeForAssignment(ctx, tx, projectID, *in.AssignedTo)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					return ErrConflict // the assignee must be a project member (C3)
				}
				return err
			}
			assigneeName = name
		}
		priority := in.Priority
		if priority == "" {
			priority = "none" // the schema default, made explicit
		}
		var taskID string
		err := tx.QueryRow(ctx, `
			INSERT INTO task_tasks (project_id, created_by, assigned_to, title, notes, priority, due_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7::timestamptz)
			RETURNING id`,
			projectID, callerID, in.AssignedTo, in.Title, in.Notes, priority, in.DueAt).Scan(&taskID)
		if err != nil {
			return notFound(err)
		}
		if err := writeActivity(ctx, tx, taskID, callerID, "created", ""); err != nil {
			return err
		}
		if in.AssignedTo != nil {
			if err := writeActivity(ctx, tx, taskID, callerID, "assigned", assigneeName); err != nil {
				return err
			}
		}
		created, err = s.taskForCaller(ctx, tx, callerID, taskID)
		return err
	})
	return created, err
}

// ListTasks returns the caller's live tasks in one of the three views. The
// membership join is the scope and the view filter owns the day math:
//
//   - today:    the "My day" tab — due inside the caller's local day OR
//     OVERDUE (due before the local day start) while still open.
//     A completed task due in the past must not flood the tab,
//     so the overdue half carries `NOT done`; the today half
//     keeps done tasks (the checkbox is the client's render).
//   - upcoming: due at or after the local day end.
//   - all:      every live task.
//
// Done is otherwise a FLAG, not a filter — the client renders the checkbox
// state. A project filter is itself membership-checked, so a foreign project
// id is not-found rather than an empty list (the wire cannot tell "no tasks"
// from "not yours" by omission).
func (s *Store) ListTasks(ctx context.Context, callerID, view, projectID string, loc *time.Location) ([]Task, error) {
	var projectFilter *string
	if projectID != "" {
		if _, err := s.readMemberRole(ctx, callerID, projectID); err != nil {
			return nil, err
		}
		projectFilter = &projectID
	}
	dayStart, dayEnd := dayBounds(time.Now(), loc)

	const listBase = `
		  JOIN task_project_members me
		    ON me.project_id = t.project_id AND me.user_id = $1::uuid
		 WHERE t.deleted_at IS NULL
		   AND p.deleted_at IS NULL
		   AND ($2::uuid IS NULL OR t.project_id = $2::uuid)`
	const listOrder = `
		 ORDER BY t.due_at ASC NULLS LAST, t.created_at, t.id`

	var (
		rows pgx.Rows
		err  error
	)
	switch view {
	case ViewToday:
		rows, err = s.pool.Query(ctx, taskSelect+listBase+`
		   AND t.due_at < $4
		   AND (t.due_at >= $3 OR NOT t.done)`+listOrder,
			callerID, projectFilter, dayStart, dayEnd)
	case ViewUpcoming:
		rows, err = s.pool.Query(ctx, taskSelect+listBase+`
		   AND t.due_at >= $3`+listOrder,
			callerID, projectFilter, dayEnd)
	case ViewAll:
		rows, err = s.pool.Query(ctx, taskSelect+listBase+listOrder,
			callerID, projectFilter)
	default:
		return nil, ErrInvalidView
	}
	if err != nil {
		return nil, notFound(err)
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, task)
	}
	return out, rows.Err()
}

// readMemberRole is the membership probe for a read (the list filter): no
// lock is needed, but the same denial voice applies — an unknown project, a
// soft-deleted project and a non-member are one ErrNotFound.
func (s *Store) readMemberRole(ctx context.Context, callerID, projectID string) (string, error) {
	var role string
	err := s.pool.QueryRow(ctx, `
		SELECT m.role
		  FROM task_project_members m
		  JOIN task_projects p ON p.id = m.project_id
		 WHERE m.project_id = $1::uuid
		   AND m.user_id = $2::uuid
		   AND p.deleted_at IS NULL`, projectID, callerID).Scan(&role)
	if err != nil {
		return "", notFound(err)
	}
	return role, nil
}

// GetTask is the drawer read: member-only, live-only, and one repeatable-read
// snapshot for the task, checklist, comments and timeline, so the tabs cannot
// disagree with each other if a mutation lands mid-read. Child slices are
// non-nil empties (the wire renders [], never null).
func (s *Store) GetTask(ctx context.Context, callerID, taskID string) (TaskDetail, error) {
	var detail TaskDetail
	err := pgx.BeginTxFunc(ctx, s.pool,
		pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			task, err := s.taskForCaller(ctx, tx, callerID, taskID)
			if err != nil {
				return err
			}
			detail.Task = task

			subtaskRows, err := tx.Query(ctx, `
				SELECT id, task_id, title, done, created_at
				  FROM task_subtasks
				 WHERE task_id = $1::uuid
				 ORDER BY created_at, id`, taskID)
			if err != nil {
				return notFound(err)
			}
			detail.Subtasks = []Subtask{}
			for subtaskRows.Next() {
				var subtask Subtask
				if err := subtaskRows.Scan(&subtask.ID, &subtask.TaskID, &subtask.Title,
					&subtask.Done, &subtask.CreatedAt); err != nil {
					subtaskRows.Close()
					return err
				}
				detail.Subtasks = append(detail.Subtasks, subtask)
			}
			subtaskRows.Close()
			if err := subtaskRows.Err(); err != nil {
				return err
			}

			commentRows, err := tx.Query(ctx, `
				SELECT c.id, c.task_id, c.author_id, u.username, c.body, c.created_at
				  FROM task_comments c
				  LEFT JOIN users u ON u.id = c.author_id
				 WHERE c.task_id = $1::uuid
				 ORDER BY c.created_at, c.id`, taskID)
			if err != nil {
				return notFound(err)
			}
			detail.Comments = []Comment{}
			for commentRows.Next() {
				var comment Comment
				if err := commentRows.Scan(&comment.ID, &comment.TaskID, &comment.AuthorID,
					&comment.AuthorName, &comment.Body, &comment.CreatedAt); err != nil {
					commentRows.Close()
					return err
				}
				detail.Comments = append(detail.Comments, comment)
			}
			commentRows.Close()
			if err := commentRows.Err(); err != nil {
				return err
			}

			activityRows, err := tx.Query(ctx, `
				SELECT a.id, a.task_id, a.actor_id, u.username, a.verb, a.detail, a.created_at
				  FROM task_activity a
				  LEFT JOIN users u ON u.id = a.actor_id
				 WHERE a.task_id = $1::uuid
				 ORDER BY a.created_at, a.id`, taskID)
			if err != nil {
				return notFound(err)
			}
			detail.Activity = []Activity{}
			for activityRows.Next() {
				var activity Activity
				if err := activityRows.Scan(&activity.ID, &activity.TaskID, &activity.ActorID,
					&activity.ActorName, &activity.Verb, &activity.Detail, &activity.CreatedAt); err != nil {
					activityRows.Close()
					return err
				}
				detail.Activity = append(detail.Activity, activity)
			}
			activityRows.Close()
			if err := activityRows.Err(); err != nil {
				return err
			}
			return nil
		})
	return detail, err
}

// UpdateTask is member-only. It locks the live task row through the caller's
// membership, validates a new assignee against the roster (ErrConflict when
// not a member), writes only the present patch fields (presence flags, so a
// nil due date/assignee CLEARS, while an omitted one stays), and records the
// change: one 'updated' row naming the changed fields plus one 'assigned' row
// when the assignee moved. The returned row is re-read through the same
// membership ladder as GetTask.
func (s *Store) UpdateTask(ctx context.Context, callerID, taskID string, patch TaskPatch) (Task, error) {
	var updated Task
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var projectID, oldTitle, oldNotes, oldPriority string
		var oldDueAt *time.Time
		var oldAssignedTo *string
		err := tx.QueryRow(ctx, `
			SELECT t.project_id, t.title, t.notes, t.priority, t.due_at, t.assigned_to
			  FROM task_tasks t
			  JOIN task_projects p ON p.id = t.project_id
			  JOIN task_project_members me
			    ON me.project_id = t.project_id AND me.user_id = $2::uuid
			 WHERE t.id = $1::uuid
			   AND t.deleted_at IS NULL
			   AND p.deleted_at IS NULL
			   FOR UPDATE OF t`, taskID, callerID).
			Scan(&projectID, &oldTitle, &oldNotes, &oldPriority, &oldDueAt, &oldAssignedTo)
		if err != nil {
			return notFound(err)
		}

		assigneeChanged := patch.AssignedTo.Set && !sameStringID(patch.AssignedTo.Value, oldAssignedTo)
		assigneeName := ""
		if assigneeChanged && patch.AssignedTo.Value != nil {
			name, err := s.assigneeForAssignment(ctx, tx, projectID, *patch.AssignedTo.Value)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					return ErrConflict // C3: the assignee must be a project member
				}
				return err
			}
			assigneeName = name
		}

		if _, err := tx.Exec(ctx, `
			UPDATE task_tasks
			   SET title       = CASE WHEN $2::boolean THEN $3::text ELSE title END,
			       notes       = CASE WHEN $4::boolean THEN $5::text ELSE notes END,
			       priority    = CASE WHEN $6::boolean THEN $7::text ELSE priority END,
			       due_at      = CASE WHEN $8::boolean THEN $9::timestamptz ELSE due_at END,
			       assigned_to = CASE WHEN $10::boolean THEN $11::uuid ELSE assigned_to END,
			       updated_at  = now()
			 WHERE id = $1::uuid`,
			taskID,
			patch.Title.Set, patch.Title.Value,
			patch.Notes.Set, patch.Notes.Value,
			patch.Priority.Set, patch.Priority.Value,
			patch.DueAt.Set, patch.DueAt.Value,
			patch.AssignedTo.Set, patch.AssignedTo.Value); err != nil {
			return notFound(err)
		}

		changedFields := []string{}
		if patch.Title.Set && patch.Title.Value != oldTitle {
			changedFields = append(changedFields, "title")
		}
		if patch.Notes.Set && patch.Notes.Value != oldNotes {
			changedFields = append(changedFields, "notes")
		}
		if patch.Priority.Set && patch.Priority.Value != oldPriority {
			changedFields = append(changedFields, "priority")
		}
		if patch.DueAt.Set && !sameTime(patch.DueAt.Value, oldDueAt) {
			changedFields = append(changedFields, "dueAt")
		}
		if len(changedFields) > 0 {
			if err := writeActivity(ctx, tx, taskID, callerID, "updated",
				strings.Join(changedFields, ", ")); err != nil {
				return err
			}
		}
		if assigneeChanged {
			detail := assigneeName
			if patch.AssignedTo.Value == nil {
				detail = "unassigned"
			}
			if err := writeActivity(ctx, tx, taskID, callerID, "assigned", detail); err != nil {
				return err
			}
		}

		updated, err = s.taskForCaller(ctx, tx, callerID, taskID)
		return err
	})
	return updated, err
}

// ToggleTask is member-only: it flips done and derives done_at from the OLD
// state in the same statement (SET expressions see the pre-update row), so a
// completion instant can never be paired with done=false. The timeline verb is
// 'completed' on close and 'reopened' on open — two verbs, one truth.
func (s *Store) ToggleTask(ctx context.Context, callerID, taskID string) (Task, error) {
	var toggled Task
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var lockedID string
		err := tx.QueryRow(ctx, `
			SELECT t.id
			  FROM task_tasks t
			  JOIN task_projects p ON p.id = t.project_id
			  JOIN task_project_members me
			    ON me.project_id = t.project_id AND me.user_id = $2::uuid
			 WHERE t.id = $1::uuid
			   AND t.deleted_at IS NULL
			   AND p.deleted_at IS NULL
			   FOR UPDATE OF t`, taskID, callerID).Scan(&lockedID)
		if err != nil {
			return notFound(err)
		}
		var done bool
		if err := tx.QueryRow(ctx, `
			UPDATE task_tasks
			   SET done = NOT done,
			       done_at = CASE WHEN done THEN NULL ELSE now() END,
			       updated_at = now()
			 WHERE id = $1::uuid
			 RETURNING done`, taskID).Scan(&done); err != nil {
			return err
		}
		verb := "reopened"
		if done {
			verb = "completed"
		}
		if err := writeActivity(ctx, tx, taskID, callerID, verb, ""); err != nil {
			return err
		}
		toggled, err = s.taskForCaller(ctx, tx, callerID, taskID)
		return err
	})
	return toggled, err
}

// SoftDeleteTask is member-only and sets deleted_at; the activity row is
// written after the flag in the same transaction, so the timeline survives
// (the task row and its children are still physically there). Every live read
// filters the flag; a second delete sees no live row and answers not-found.
func (s *Store) SoftDeleteTask(ctx context.Context, callerID, taskID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var lockedID string
		err := tx.QueryRow(ctx, `
			SELECT t.id
			  FROM task_tasks t
			  JOIN task_projects p ON p.id = t.project_id
			  JOIN task_project_members me
			    ON me.project_id = t.project_id AND me.user_id = $2::uuid
			 WHERE t.id = $1::uuid
			   AND t.deleted_at IS NULL
			   AND p.deleted_at IS NULL
			   FOR UPDATE OF t`, taskID, callerID).Scan(&lockedID)
		if err != nil {
			return notFound(err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE task_tasks
			   SET deleted_at = now(), updated_at = now()
			 WHERE id = $1::uuid AND deleted_at IS NULL`, taskID); err != nil {
			return notFound(err)
		}
		return writeActivity(ctx, tx, taskID, callerID, "deleted", "")
	})
}

// ---- subtasks ----

// AddSubtask is member-only and resolves the task through the caller's
// membership in the SAME statement as the insert: a non-member's task id
// inserts nothing and is reported not-found. A soft-deleted task (or project)
// is equally invisible.
func (s *Store) AddSubtask(ctx context.Context, callerID, taskID, title string) (Subtask, error) {
	var subtask Subtask
	err := s.pool.QueryRow(ctx, `
		INSERT INTO task_subtasks (task_id, title)
		SELECT t.id, $3::text
		  FROM task_tasks t
		  JOIN task_projects p ON p.id = t.project_id
		  JOIN task_project_members m
		    ON m.project_id = t.project_id AND m.user_id = $2::uuid
		 WHERE t.id = $1::uuid
		   AND t.deleted_at IS NULL
		   AND p.deleted_at IS NULL
		RETURNING id, task_id, title, done, created_at`, taskID, callerID, title).
		Scan(&subtask.ID, &subtask.TaskID, &subtask.Title, &subtask.Done, &subtask.CreatedAt)
	if err != nil {
		return Subtask{}, notFound(err)
	}
	return subtask, nil
}

// UpdateSubtask is member-only and patches title and/or done with presence
// flags. The task is joined through the caller's membership, so a member can
// change a row only in a live task of their own project; the (task, subtask)
// pair is bound in the WHERE, so a subtask id from another task cannot be
// edited by pairing it with a task the caller can see.
func (s *Store) UpdateSubtask(ctx context.Context, callerID, taskID, subtaskID string, patch SubtaskPatch) (Subtask, error) {
	var subtask Subtask
	err := s.pool.QueryRow(ctx, `
		UPDATE task_subtasks s
		   SET title = CASE WHEN $4::boolean THEN $5::text ELSE s.title END,
		       done  = CASE WHEN $6::boolean THEN $7::boolean ELSE s.done END
		  FROM task_tasks t
		  JOIN task_projects p ON p.id = t.project_id
		  JOIN task_project_members m
		    ON m.project_id = t.project_id AND m.user_id = $3::uuid
		 WHERE s.id = $1::uuid
		   AND s.task_id = $2::uuid
		   AND t.id = s.task_id
		   AND t.deleted_at IS NULL
		   AND p.deleted_at IS NULL
		RETURNING s.id, s.task_id, s.title, s.done, s.created_at`,
		subtaskID, taskID, callerID,
		patch.Title.Set, patch.Title.Value,
		patch.Done.Set, patch.Done.Value).
		Scan(&subtask.ID, &subtask.TaskID, &subtask.Title, &subtask.Done, &subtask.CreatedAt)
	if err != nil {
		return Subtask{}, notFound(err)
	}
	return subtask, nil
}

// DeleteSubtask is member-only for the same reason as UpdateSubtask: the task
// and subtask ids are both bound, and the membership join is part of the
// DELETE itself — no row is deleted unless the caller may see the parent.
func (s *Store) DeleteSubtask(ctx context.Context, callerID, taskID, subtaskID string) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM task_subtasks s
		 USING task_tasks t, task_projects p, task_project_members m
		 WHERE s.id = $1::uuid
		   AND s.task_id = $2::uuid
		   AND t.id = s.task_id
		   AND p.id = t.project_id
		   AND m.project_id = t.project_id AND m.user_id = $3::uuid
		   AND t.deleted_at IS NULL
		   AND p.deleted_at IS NULL`, subtaskID, taskID, callerID)
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- comments ----

// AddComment is member-only. The comment insert selects through the caller's
// membership (a non-member's task id writes nothing), and the 'commented'
// activity row commits with it — a comment without its timeline entry would
// be a lying drawer. The author name is the caller's, read for the response.
func (s *Store) AddComment(ctx context.Context, callerID, taskID, body string) (Comment, error) {
	var comment Comment
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO task_comments (task_id, author_id, body)
			SELECT t.id, $2::uuid, $3::text
			  FROM task_tasks t
			  JOIN task_projects p ON p.id = t.project_id
			  JOIN task_project_members m
			    ON m.project_id = t.project_id AND m.user_id = $2::uuid
			 WHERE t.id = $1::uuid
			   AND t.deleted_at IS NULL
			   AND p.deleted_at IS NULL
			RETURNING id, task_id, author_id, body, created_at`,
			taskID, callerID, body).
			Scan(&comment.ID, &comment.TaskID, &comment.AuthorID, &comment.Body, &comment.CreatedAt)
		if err != nil {
			return notFound(err)
		}
		var username string
		if err := tx.QueryRow(ctx, `
			SELECT username FROM users WHERE id = $1::uuid`, callerID).Scan(&username); err != nil {
			return notFound(err)
		}
		comment.AuthorName = &username
		return writeActivity(ctx, tx, comment.TaskID, callerID, "commented", "")
	})
	return comment, err
}

// DeleteComment enforces the author-or-project-admin rule IN the transaction:
// the comment row is locked together with the caller's project role, and a
// member who is neither the author nor an admin gets ErrForbidden (membership
// confirmed, verb denied). A deleted author's NULL author_id can only be
// removed by an admin — history has no owner to defend it.
func (s *Store) DeleteComment(ctx context.Context, callerID, taskID, commentID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var mine bool
		var role string
		err := tx.QueryRow(ctx, `
			SELECT COALESCE(c.author_id = $2::uuid, false), m.role
			  FROM task_comments c
			  JOIN task_tasks t ON t.id = c.task_id
			  JOIN task_projects p ON p.id = t.project_id
			  JOIN task_project_members m
			    ON m.project_id = t.project_id AND m.user_id = $2::uuid
			 WHERE c.id = $1::uuid
			   AND c.task_id = $3::uuid
			   AND t.deleted_at IS NULL
			   AND p.deleted_at IS NULL
			   FOR UPDATE OF c`, commentID, callerID, taskID).Scan(&mine, &role)
		if err != nil {
			return notFound(err)
		}
		if !mine && role != "admin" {
			return ErrForbidden
		}
		_, err = tx.Exec(ctx, `
			DELETE FROM task_comments WHERE id = $1::uuid`, commentID)
		return err
	})
}

// ---- stats ----

// Stats is the four-card rollup, computed over the caller's WHOLE membership
// (not one project) and in the caller's location:
//
//   - Open:          live tasks, not done.
//   - DueToday:      not done and due inside the caller's local day.
//   - CompletedWeek: done and done_at inside the Monday-start local week
//     (documented product choice; a Sunday evening completion belongs to the
//     week that started the previous Monday, not the server's UTC week).
//   - Overdue:       not done and due before the local day start (a task due
//     earlier TODAY is "today", not overdue — the card counts missed days,
//     not missed hours).
//
// Membership-scoped and soft-delete-filtered exactly like the list reads.
func (s *Store) Stats(ctx context.Context, callerID string, loc *time.Location) (Stats, error) {
	now := time.Now()
	dayStart, dayEnd := dayBounds(now, loc)
	weekStart := mondayStart(now, loc)
	weekEnd := weekStart.AddDate(0, 0, 7)

	var stats Stats
	err := s.pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE NOT t.done),
		  count(*) FILTER (WHERE NOT t.done AND t.due_at >= $2 AND t.due_at < $3),
		  count(*) FILTER (WHERE t.done AND t.done_at >= $4 AND t.done_at < $5),
		  count(*) FILTER (WHERE NOT t.done AND t.due_at < $2)
		  FROM task_tasks t
		  JOIN task_projects p ON p.id = t.project_id
		  JOIN task_project_members m
		    ON m.project_id = t.project_id AND m.user_id = $1::uuid
		 WHERE t.deleted_at IS NULL
		   AND p.deleted_at IS NULL`,
		callerID, dayStart, dayEnd, weekStart, weekEnd).
		Scan(&stats.Open, &stats.DueToday, &stats.CompletedWeek, &stats.Overdue)
	return stats, err
}

// ---- lifecycle ----

// OnUserDeleted is the C4 repair hook, run best-effort AFTER the user row is
// deleted. By then the FK CASCADE has already removed the user's membership
// rows (and invites; actor/creator/assignee references are SET NULL), so the
// user's former projects cannot be looked up FROM the user — instead the hook
// scans live projects for the injury the deletion can cause: zero admins
// remaining. That scan is what makes the hook idempotent and able to heal a
// project left adminless by any earlier deletion.
//
// The userID is still load-bearing: memberships are filtered as if the user
// were already gone (user_id <> $1), so the hook is also correct if it is
// wired BEFORE the delete — the doomed user can neither block the "no admin"
// test nor be chosen as the inheriting admin. Per project: promote the
// earliest-joined remaining member to admin; if no member remains, soft-delete
// the project (never a destructive delete, C4). The whole repair is one
// transaction; re-running it is a no-op.
func (s *Store) OnUserDeleted(ctx context.Context, userID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT p.id
			  FROM task_projects p
			 WHERE p.deleted_at IS NULL
			   AND NOT EXISTS (
			         SELECT 1
			           FROM task_project_members admin
			          WHERE admin.project_id = p.id
			            AND admin.role = 'admin'
			            AND admin.user_id <> $1::uuid)
			 ORDER BY p.id
			 FOR UPDATE OF p`, userID)
		if err != nil {
			return notFound(err) // a malformed id names no user and no project
		}
		projectIDs := []string{}
		for rows.Next() {
			var projectID string
			if err := rows.Scan(&projectID); err != nil {
				rows.Close()
				return err
			}
			projectIDs = append(projectIDs, projectID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, projectID := range projectIDs {
			var successorID string
			err := tx.QueryRow(ctx, `
				SELECT user_id
				  FROM task_project_members
				 WHERE project_id = $1::uuid AND user_id <> $2::uuid
				 ORDER BY joined_at, user_id
				 LIMIT 1
				 FOR UPDATE`, projectID, userID).Scan(&successorID)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				if _, err := tx.Exec(ctx, `
					UPDATE task_projects
					   SET deleted_at = now(), updated_at = now()
					 WHERE id = $1::uuid AND deleted_at IS NULL`, projectID); err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				if _, err := tx.Exec(ctx, `
					UPDATE task_project_members
					   SET role = 'admin'
					 WHERE project_id = $1::uuid AND user_id = $2::uuid`, projectID, successorID); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
