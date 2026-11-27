// store_test.go — the tasks store against REAL Postgres (LUMEN_INTEGRATION=1).
//
// Every test gets a FRESH SCHEMA (tasks_it_<unixnano>) and the full migration
// set, then a Store bound to it. The schema is dropped afterwards; nothing
// touches the dev data. The tests are the store contract made runnable:
// membership is the ONLY door (reads and writes), soft delete makes rows
// invisible, the last-admin guard holds, member removal unassigns, invites
// expire with a consumed row, stats follow the CLIENT's calendar, and the C4
// user-delete repair promotes or soft-deletes.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // Asia/Jakarta must exist even on a zoneinfo-less runner

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanatabi/lumen/internal/store"
)

const defaultTestDBURL = "postgres://lumen:lumen-dev@localhost:5433/lumen?sslmode=disable"

// fixture is one isolated schema plus the users every test talks through.
type fixture struct {
	t     *testing.T
	store *Store
	pool  *pgxpool.Pool
	alice string
	bob   string
	carol string
}

// newFixture opens the dev cluster, creates a throwaway schema, migrates it
// fully, and seeds three identities (UpsertUser grants the whole catalog,
// which is irrelevant here — the store never reads scopes).
func newFixture(t *testing.T) *fixture {
	t.Helper()
	if os.Getenv("LUMEN_INTEGRATION") == "" {
		t.Skip("LUMEN_INTEGRATION unset")
	}
	baseURL := os.Getenv("LUMEN_DB_URL")
	if baseURL == "" {
		baseURL = defaultTestDBURL
	}
	ctx := context.Background()

	maintenance, err := store.NewPool(ctx, baseURL)
	if err != nil {
		t.Fatalf("maintenance pool: %v", err)
	}
	t.Cleanup(maintenance.Close)

	schema := fmt.Sprintf("tasks_it_%d", time.Now().UnixNano())
	if _, err := maintenance.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := maintenance.Exec(context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})

	dsn := baseURL
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + schema
	} else {
		dsn += "?search_path=" + schema
	}
	pool, err := store.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("schema pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := maintenance.Exec(ctx, `GRANT ALL ON SCHEMA `+schema+` TO CURRENT_USER`); err != nil {
		t.Logf("grant on %s skipped: %v", schema, err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate schema %s: %v", schema, err)
	}

	pg := store.NewPG(pool)
	seed := func(username string) string {
		if err := pg.UpsertUser(ctx, username, "x"); err != nil {
			t.Fatalf("seed user %s: %v", username, err)
		}
		auth, ok, err := pg.AuthFor(ctx, username)
		if err != nil || !ok {
			t.Fatalf("read back user %s: ok=%v err=%v", username, ok, err)
		}
		return auth.ID
	}
	return &fixture{
		t:     t,
		store: NewStore(pool),
		pool:  pool,
		alice: seed("alice"),
		bob:   seed("bob"),
		carol: seed("carol"),
	}
}

func ptr[T any](value T) *T { return &value }

// mustNoErr fails the test with context when a store call unexpectedly errors.
func mustNoErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// mustErr asserts the sentinel voice — the wire's status map depends on it.
func mustErr(t *testing.T, err, want error, what string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err = %v, want %v", what, err, want)
	}
}

func (f *fixture) mustProject(ownerID, name string) Project {
	f.t.Helper()
	project, err := f.store.CreateProject(context.Background(), ownerID, name)
	if err != nil {
		f.t.Fatalf("create project %q: %v", name, err)
	}
	return project
}

// addMember walks the REAL invite path (create + accept) so the roster is
// built the way the product builds it.
func (f *fixture) addMember(projectID, adminID, memberID string) {
	f.t.Helper()
	ctx := context.Background()
	invite, err := f.store.CreateInvite(ctx, adminID, projectID, memberID, time.Now().Add(24*time.Hour))
	if err != nil {
		f.t.Fatalf("invite member: %v", err)
	}
	if _, err := f.store.AcceptInvite(ctx, memberID, invite.ID); err != nil {
		f.t.Fatalf("accept invite: %v", err)
	}
}

func (f *fixture) mustTask(callerID, projectID string, in TaskInput) Task {
	f.t.Helper()
	task, err := f.store.CreateTask(context.Background(), callerID, projectID, in)
	if err != nil {
		f.t.Fatalf("create task %q: %v", in.Title, err)
	}
	return task
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		f.t.Fatalf("count query: %v", err)
	}
	return n
}

func activityVerbs(t *testing.T, pool *pgxpool.Pool, taskID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT verb FROM task_activity WHERE task_id = $1::uuid ORDER BY id`, taskID)
	if err != nil {
		t.Fatalf("activity query: %v", err)
	}
	defer rows.Close()
	verbs := []string{}
	for rows.Next() {
		var verb string
		if err := rows.Scan(&verb); err != nil {
			t.Fatalf("scan activity: %v", err)
		}
		verbs = append(verbs, verb)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("activity rows: %v", err)
	}
	return verbs
}

// ---------------------------------------------------------------------------

// TestCreateProjectRosterAndAdminFlags proves the creator-member transaction,
// the two denial voices (non-member = not-found, member = forbidden), and the
// completed flag's stability/reopen behavior.
func TestCreateProjectRosterAndAdminFlags(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	project, err := f.store.CreateProject(ctx, f.alice, "Home")
	mustNoErr(t, err, "create project")
	if project.Role != "admin" {
		t.Fatalf("creator role = %q, want admin", project.Role)
	}
	if project.CreatorID == nil || *project.CreatorID != f.alice {
		t.Fatalf("creator id = %v, want %s", project.CreatorID, f.alice)
	}

	// the creator's roster row exists because it was inserted in the same tx
	members, err := f.store.ListMembers(ctx, f.alice, project.ID)
	mustNoErr(t, err, "list members")
	if len(members) != 1 || members[0].UserID != f.alice || members[0].Role != "admin" {
		t.Fatalf("creator roster = %+v, want one admin row for alice", members)
	}

	// bob is a stranger: the project is invisible and every verb is not-found
	projects, err := f.store.ListProjects(ctx, f.bob)
	mustNoErr(t, err, "bob list projects")
	if len(projects) != 0 {
		t.Fatalf("non-member saw %d projects", len(projects))
	}
	_, err = f.store.ListMembers(ctx, f.bob, project.ID)
	mustErr(t, err, ErrNotFound, "non-member roster")
	mustErr(t, f.store.RenameProject(ctx, f.bob, project.ID, "stolen"), ErrNotFound, "non-member rename")
	_, err = f.store.CreateTask(ctx, f.bob, project.ID, TaskInput{Title: "intruder"})
	mustErr(t, err, ErrNotFound, "non-member create")

	// once bob is a member (not admin) the room key is real and the verb denied
	f.addMember(project.ID, f.alice, f.bob)
	members, err = f.store.ListMembers(ctx, f.bob, project.ID)
	mustNoErr(t, err, "member roster")
	if len(members) != 2 {
		t.Fatalf("roster size = %d, want 2", len(members))
	}
	mustErr(t, f.store.RenameProject(ctx, f.bob, project.ID, "nope"), ErrForbidden, "member rename")

	// admin flags: rename, complete (stable instant), reopen
	mustNoErr(t, f.store.RenameProject(ctx, f.alice, project.ID, "House"), "admin rename")
	mustNoErr(t, f.store.SetProjectCompleted(ctx, f.alice, project.ID, true), "complete project")
	projects, err = f.store.ListProjects(ctx, f.alice)
	mustNoErr(t, err, "list projects")
	if len(projects) != 1 || projects[0].Name != "House" || projects[0].CompletedAt == nil {
		t.Fatalf("project after rename/complete = %+v", projects)
	}
	firstCompletion := *projects[0].CompletedAt
	mustNoErr(t, f.store.SetProjectCompleted(ctx, f.alice, project.ID, true), "complete project again")
	projects, err = f.store.ListProjects(ctx, f.alice)
	mustNoErr(t, err, "list projects")
	if projects[0].CompletedAt == nil || !projects[0].CompletedAt.Equal(firstCompletion) {
		t.Fatalf("repeated complete rewrote completed_at: %v -> %v", firstCompletion, projects[0].CompletedAt)
	}
	mustNoErr(t, f.store.SetProjectCompleted(ctx, f.alice, project.ID, false), "reopen project")
	projects, err = f.store.ListProjects(ctx, f.alice)
	mustNoErr(t, err, "list projects")
	if projects[0].CompletedAt != nil {
		t.Fatalf("reopen left completed_at = %v", projects[0].CompletedAt)
	}
}

// TestNonMemberIsDeniedEverywhere walks EVERY exported project/task verb from
// a stranger's perspective: one denial voice, no oracle, no write.
func TestNonMemberIsDeniedEverywhere(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	project := f.mustProject(f.alice, "Home")
	task := f.mustTask(f.alice, project.ID, TaskInput{Title: "Fix gate", Priority: "high"})
	subtask, err := f.store.AddSubtask(ctx, f.alice, task.ID, "buy hinge")
	mustNoErr(t, err, "add subtask")
	comment, err := f.store.AddComment(ctx, f.alice, task.ID, "the gate squeaks")
	mustNoErr(t, err, "add comment")
	invite, err := f.store.CreateInvite(ctx, f.alice, project.ID, f.carol, time.Now().Add(time.Hour))
	mustNoErr(t, err, "create carol invite")

	// reads
	_, err = f.store.ListMembers(ctx, f.bob, project.ID)
	mustErr(t, err, ErrNotFound, "stranger roster")
	if _, err := f.store.ListTasks(ctx, f.bob, ViewAll, project.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stranger list tasks: %v", err)
	}
	_, err = f.store.GetTask(ctx, f.bob, task.ID)
	mustErr(t, err, ErrNotFound, "stranger get task")
	if invites, err := f.store.ListInvitesFor(ctx, f.bob); err != nil || len(invites) != 0 {
		t.Fatalf("stranger inbox = %v, %v", invites, err)
	}
	stats, err := f.store.Stats(ctx, f.bob, nil)
	mustNoErr(t, err, "stranger stats")
	if stats != (Stats{}) {
		t.Fatalf("stranger stats = %+v, want zero", stats)
	}

	// project writes
	mustErr(t, f.store.RenameProject(ctx, f.bob, project.ID, "x"), ErrNotFound, "stranger rename")
	mustErr(t, f.store.SetProjectCompleted(ctx, f.bob, project.ID, true), ErrNotFound, "stranger complete")
	mustErr(t, f.store.SoftDeleteProject(ctx, f.bob, project.ID), ErrNotFound, "stranger project delete")
	mustErr(t, f.store.SetMemberRole(ctx, f.bob, project.ID, f.alice, "member"), ErrNotFound, "stranger role")
	mustErr(t, f.store.RemoveMember(ctx, f.bob, project.ID, f.alice), ErrNotFound, "stranger remove member")
	_, err = f.store.CreateInvite(ctx, f.bob, project.ID, f.carol, time.Now().Add(time.Hour))
	mustErr(t, err, ErrNotFound, "stranger invite")
	mustErr(t, f.store.RevokeInvite(ctx, f.bob, project.ID, invite.ID), ErrNotFound, "stranger revoke")

	// task writes
	_, err = f.store.CreateTask(ctx, f.bob, project.ID, TaskInput{Title: "x"})
	mustErr(t, err, ErrNotFound, "stranger create task")
	_, err = f.store.UpdateTask(ctx, f.bob, task.ID, TaskPatch{Title: Some("x")})
	mustErr(t, err, ErrNotFound, "stranger update task")
	_, err = f.store.ToggleTask(ctx, f.bob, task.ID)
	mustErr(t, err, ErrNotFound, "stranger toggle")
	mustErr(t, f.store.SoftDeleteTask(ctx, f.bob, task.ID), ErrNotFound, "stranger delete task")
	_, err = f.store.AddSubtask(ctx, f.bob, task.ID, "x")
	mustErr(t, err, ErrNotFound, "stranger add subtask")
	_, err = f.store.UpdateSubtask(ctx, f.bob, task.ID, subtask.ID, SubtaskPatch{Done: Some(true)})
	mustErr(t, err, ErrNotFound, "stranger update subtask")
	mustErr(t, f.store.DeleteSubtask(ctx, f.bob, task.ID, subtask.ID), ErrNotFound, "stranger delete subtask")
	_, err = f.store.AddComment(ctx, f.bob, task.ID, "x")
	mustErr(t, err, ErrNotFound, "stranger add comment")
	mustErr(t, f.store.DeleteComment(ctx, f.bob, task.ID, comment.ID), ErrNotFound, "stranger delete comment")

	// the invite is carol's, not bob's: bob can neither accept nor decline it
	_, err = f.store.AcceptInvite(ctx, f.bob, invite.ID)
	mustErr(t, err, ErrNotFound, "non-invitee accept")
	mustErr(t, f.store.DeclineInvite(ctx, f.bob, invite.ID), ErrNotFound, "non-invitee decline")

	// malformed ids from the wire are the same not-found, never a 500-ish error
	mustErr(t, f.store.RenameProject(ctx, f.alice, "not-a-uuid", "x"), ErrNotFound, "malformed project id")
	_, err = f.store.GetTask(ctx, f.alice, "not-a-uuid")
	mustErr(t, err, ErrNotFound, "malformed task id")
	_, err = f.store.CreateTask(ctx, f.alice, "not-a-uuid", TaskInput{Title: "x"})
	mustErr(t, err, ErrNotFound, "malformed project id on create")

	// and nothing was touched: alice still sees all three child rows
	detail, err := f.store.GetTask(ctx, f.alice, task.ID)
	mustNoErr(t, err, "owner task after stranger walk")
	if len(detail.Subtasks) != 1 || len(detail.Comments) != 1 || detail.Task.Title != "Fix gate" {
		t.Fatalf("stranger walk mutated the task: %+v", detail)
	}
}

// TestSoftDeleteVisibility proves both flags: a soft-deleted task or project
// is invisible to every live read while the rows (and their history) remain.
func TestSoftDeleteVisibility(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	project := f.mustProject(f.alice, "Home")
	task := f.mustTask(f.alice, project.ID, TaskInput{Title: "Fix gate"})
	_, err := f.store.AddComment(ctx, f.alice, task.ID, "before delete")
	mustNoErr(t, err, "comment")

	mustNoErr(t, f.store.SoftDeleteTask(ctx, f.alice, task.ID), "soft delete task")
	if f.count(`SELECT count(*) FROM task_tasks WHERE id = $1::uuid AND deleted_at IS NOT NULL`, task.ID) != 1 {
		t.Fatal("task row is not flagged deleted")
	}
	_, err = f.store.GetTask(ctx, f.alice, task.ID)
	mustErr(t, err, ErrNotFound, "deleted task get")
	listed, err := f.store.ListTasks(ctx, f.alice, ViewAll, "", nil)
	mustNoErr(t, err, "list after task delete")
	if len(listed) != 0 {
		t.Fatalf("deleted task still listed: %+v", listed)
	}
	mustErr(t, f.store.SoftDeleteTask(ctx, f.alice, task.ID), ErrNotFound, "second task delete")
	// the history survives the soft delete
	if n := f.count(`SELECT count(*) FROM task_comments WHERE task_id = $1::uuid`, task.ID); n != 1 {
		t.Fatalf("soft delete removed comments: %d", n)
	}

	other := f.mustTask(f.alice, project.ID, TaskInput{Title: "Paint fence"})
	mustNoErr(t, f.store.SoftDeleteProject(ctx, f.alice, project.ID), "soft delete project")
	if f.count(`SELECT count(*) FROM task_projects WHERE id = $1::uuid AND deleted_at IS NOT NULL`, project.ID) != 1 {
		t.Fatal("project row is not flagged deleted")
	}
	projects, err := f.store.ListProjects(ctx, f.alice)
	mustNoErr(t, err, "list after project delete")
	if len(projects) != 0 {
		t.Fatalf("deleted project still listed: %+v", projects)
	}
	_, err = f.store.ListMembers(ctx, f.alice, project.ID)
	mustErr(t, err, ErrNotFound, "deleted project roster")
	_, err = f.store.GetTask(ctx, f.alice, other.ID)
	mustErr(t, err, ErrNotFound, "task in deleted project")
	_, err = f.store.CreateTask(ctx, f.alice, project.ID, TaskInput{Title: "x"})
	mustErr(t, err, ErrNotFound, "create in deleted project")
	listed, err = f.store.ListTasks(ctx, f.alice, ViewAll, "", nil)
	mustNoErr(t, err, "list after project delete")
	if len(listed) != 0 {
		t.Fatalf("tasks of deleted project still listed: %+v", listed)
	}
	mustErr(t, f.store.SoftDeleteProject(ctx, f.alice, project.ID), ErrNotFound, "second project delete")
}

// TestInvites walks the state machine: create, duplicate, accept (invitee
// only), already-a-member, revoke (admin only), decline (invitee only), and
// expiry — the 410 consumes the row so a retry is a plain not-found.
func TestInvites(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	project := f.mustProject(f.alice, "Home")
	expires := time.Now().Add(24 * time.Hour)

	invite, err := f.store.CreateInvite(ctx, f.alice, project.ID, f.bob, expires)
	mustNoErr(t, err, "create invite")
	if invite.ProjectName != "Home" || invite.InviterName != "alice" {
		t.Fatalf("invite hydration = %+v", invite)
	}

	// one PENDING invite per project+invitee (the unique index + sentinel)
	_, err = f.store.CreateInvite(ctx, f.alice, project.ID, f.bob, expires)
	mustErr(t, err, ErrConflict, "duplicate pending invite")

	// the invitee's inbox sees it; the inviter's does not
	inbox, err := f.store.ListInvitesFor(ctx, f.bob)
	mustNoErr(t, err, "bob inbox")
	if len(inbox) != 1 || inbox[0].ID != invite.ID || inbox[0].ProjectName != "Home" {
		t.Fatalf("bob inbox = %+v", inbox)
	}
	if aliceInbox, err := f.store.ListInvitesFor(ctx, f.alice); err != nil || len(aliceInbox) != 0 {
		t.Fatalf("alice inbox = %+v, %v", aliceInbox, err)
	}

	accepted, err := f.store.AcceptInvite(ctx, f.bob, invite.ID)
	mustNoErr(t, err, "accept invite")
	if accepted.ID != invite.ID || accepted.ProjectName != "Home" {
		t.Fatalf("accepted invite = %+v", accepted)
	}
	if inbox, err := f.store.ListInvitesFor(ctx, f.bob); err != nil || len(inbox) != 0 {
		t.Fatalf("consumed invite still pending: %+v, %v", inbox, err)
	}
	members, err := f.store.ListMembers(ctx, f.alice, project.ID)
	mustNoErr(t, err, "roster after accept")
	if len(members) != 2 {
		t.Fatalf("roster after accept = %d, want 2", len(members))
	}
	for _, member := range members {
		if member.UserID == f.bob && member.Role != "member" {
			t.Fatalf("accepted member role = %q, want member", member.Role)
		}
	}

	// accepting twice is not-found (the row is consumed)
	_, err = f.store.AcceptInvite(ctx, f.bob, invite.ID)
	mustErr(t, err, ErrNotFound, "accept consumed invite")

	// an existing member cannot be invited again
	_, err = f.store.CreateInvite(ctx, f.alice, project.ID, f.bob, expires)
	mustErr(t, err, ErrConflict, "invite existing member")

	// -- expiry: 410 now, and the row is GONE (consumed, not rolled back)
	expired, err := f.store.CreateInvite(ctx, f.alice, project.ID, f.carol, time.Now().Add(-time.Minute))
	mustNoErr(t, err, "create expired invite")
	_, err = f.store.AcceptInvite(ctx, f.carol, expired.ID)
	mustErr(t, err, ErrExpired, "expired accept")
	if n := f.count(`SELECT count(*) FROM task_project_invites WHERE id = $1::uuid`, expired.ID); n != 0 {
		t.Fatalf("expired invite row still exists: %d", n)
	}
	if inbox, err := f.store.ListInvitesFor(ctx, f.carol); err != nil || len(inbox) != 0 {
		t.Fatalf("expired invite in inbox: %+v, %v", inbox, err)
	}
	_, err = f.store.AcceptInvite(ctx, f.carol, expired.ID)
	mustErr(t, err, ErrNotFound, "expired retry is 404")

	// -- revoke: a plain member cannot, an admin can; the row is gone
	revoked, err := f.store.CreateInvite(ctx, f.alice, project.ID, f.carol, expires)
	mustNoErr(t, err, "create revoke invite")
	mustErr(t, f.store.RevokeInvite(ctx, f.bob, project.ID, revoked.ID), ErrForbidden, "member revoke")
	mustNoErr(t, f.store.RevokeInvite(ctx, f.alice, project.ID, revoked.ID), "admin revoke")
	_, err = f.store.AcceptInvite(ctx, f.carol, revoked.ID)
	mustErr(t, err, ErrNotFound, "accept revoked invite")

	// -- decline: only the invitee, and it consumes the row
	declined, err := f.store.CreateInvite(ctx, f.alice, project.ID, f.carol, expires)
	mustNoErr(t, err, "create decline invite")
	mustErr(t, f.store.DeclineInvite(ctx, f.bob, declined.ID), ErrNotFound, "non-invitee decline")
	mustNoErr(t, f.store.DeclineInvite(ctx, f.carol, declined.ID), "invitee decline")
	_, err = f.store.AcceptInvite(ctx, f.carol, declined.ID)
	mustErr(t, err, ErrNotFound, "accept declined invite")
}

// TestMemberRolesLastAdminAndUnassign proves the C2 guard and the removal
// transaction: a project always keeps one admin, and removing a member clears
// their assignments (same tx) so the assignee invariant survives.
func TestMemberRolesLastAdminAndUnassign(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	project := f.mustProject(f.alice, "Home")
	f.addMember(project.ID, f.alice, f.bob)
	f.addMember(project.ID, f.alice, f.carol)

	// alice is the only admin: she can neither step down nor walk out
	mustErr(t, f.store.SetMemberRole(ctx, f.alice, project.ID, f.alice, "member"), ErrConflict, "demote last admin")
	mustErr(t, f.store.RemoveMember(ctx, f.alice, project.ID, f.alice), ErrConflict, "remove last admin")

	// a non-member target is not on the roster
	mustErr(t, f.store.SetMemberRole(ctx, f.alice, project.ID, "00000000-0000-0000-0000-000000000000", "admin"),
		ErrNotFound, "role of non-member")

	// promotion; then the other admin can manage
	mustNoErr(t, f.store.SetMemberRole(ctx, f.alice, project.ID, f.bob, "admin"), "promote bob")
	mustErr(t, f.store.SetMemberRole(ctx, f.carol, project.ID, f.bob, "member"), ErrForbidden, "member role change")
	mustNoErr(t, f.store.SetMemberRole(ctx, f.bob, project.ID, f.alice, "member"), "demote alice")

	// bob alone is admin now: alice (a member) cannot remove him at all, and
	// bob cannot remove himself because that would leave zero admins.
	mustErr(t, f.store.RemoveMember(ctx, f.alice, project.ID, f.bob), ErrForbidden, "member removes admin")
	mustErr(t, f.store.RemoveMember(ctx, f.bob, project.ID, f.bob), ErrConflict, "admin removes self as last admin")
	mustErr(t, f.store.SetMemberRole(ctx, f.alice, project.ID, f.alice, "admin"), ErrForbidden, "promote self as member")
	mustNoErr(t, f.store.SetMemberRole(ctx, f.bob, project.ID, f.alice, "admin"), "re-promote alice")

	// assignment must be a roster member; the task detail names the assignee
	task := f.mustTask(f.alice, project.ID, TaskInput{Title: "Fix gate", AssignedTo: ptr(f.bob)})
	detail, err := f.store.GetTask(ctx, f.alice, task.ID)
	mustNoErr(t, err, "get assigned task")
	if detail.Task.AssignedTo == nil || *detail.Task.AssignedTo != f.bob ||
		detail.Task.AssigneeName == nil || *detail.Task.AssigneeName != "bob" {
		t.Fatalf("assignee hydration = %+v", detail.Task)
	}

	// removing bob (an admin with alice still admin) unassigns his tasks
	mustNoErr(t, f.store.RemoveMember(ctx, f.alice, project.ID, f.bob), "remove bob")
	detail, err = f.store.GetTask(ctx, f.alice, task.ID)
	mustNoErr(t, err, "task after member removal")
	if detail.Task.AssignedTo != nil || detail.Task.AssigneeName != nil {
		t.Fatalf("removed member still assigned: %+v", detail.Task)
	}
	if n := f.count(`SELECT count(*) FROM task_tasks WHERE id = $1::uuid AND assigned_to IS NULL`, task.ID); n != 1 {
		t.Fatal("assigned_to not cleared in the row")
	}

	// bob is no longer a member: assigning him is a conflict, on create AND patch
	_, err = f.store.CreateTask(ctx, f.alice, project.ID, TaskInput{Title: "x", AssignedTo: ptr(f.bob)})
	mustErr(t, err, ErrConflict, "assign non-member on create")
	_, err = f.store.UpdateTask(ctx, f.alice, task.ID, TaskPatch{AssignedTo: Some(ptr(f.bob))})
	mustErr(t, err, ErrConflict, "assign non-member on patch")
	_, err = f.store.CreateTask(ctx, f.alice, project.ID, TaskInput{Title: "x", AssignedTo: ptr("not-a-uuid")})
	mustErr(t, err, ErrConflict, "assign malformed user id")
	if err := f.store.SetMemberRole(ctx, f.alice, project.ID, f.alice, "owner"); err == nil {
		t.Fatal("an unknown project role was silently accepted")
	}

	// carol (plain member) leaves cleanly; alice remains the one admin
	mustNoErr(t, f.store.RemoveMember(ctx, f.alice, project.ID, f.carol), "remove carol")
	members, err := f.store.ListMembers(ctx, f.alice, project.ID)
	mustNoErr(t, err, "final roster")
	if len(members) != 1 || members[0].UserID != f.alice || members[0].Role != "admin" {
		t.Fatalf("final roster = %+v", members)
	}
}

// TestTaskCRUDSubtasksCommentsActivity is the full task walk: patch presence,
// assign/unassign, toggle, checklist, comments and the timeline verbs.
func TestTaskCRUDSubtasksCommentsActivity(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	project := f.mustProject(f.alice, "Home")
	f.addMember(project.ID, f.alice, f.bob)
	f.addMember(project.ID, f.alice, f.carol)

	due := time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)
	task := f.mustTask(f.alice, project.ID, TaskInput{
		Title: "Fix gate", Notes: "the hinge", Priority: "high", DueAt: &due, AssignedTo: ptr(f.bob),
	})
	detail, err := f.store.GetTask(ctx, f.alice, task.ID)
	mustNoErr(t, err, "get created task")
	if detail.Task.Title != "Fix gate" || detail.Task.Notes != "the hinge" ||
		detail.Task.Priority != "high" || detail.Task.AssigneeName == nil ||
		*detail.Task.AssigneeName != "bob" || detail.Task.Done {
		t.Fatalf("created task = %+v", detail.Task)
	}
	if len(detail.Subtasks) != 0 || len(detail.Comments) != 0 || len(detail.Activity) != 2 {
		t.Fatalf("new task children/activity = %d/%d/%d, want 0/0/2",
			len(detail.Subtasks), len(detail.Comments), len(detail.Activity))
	}

	// the member patches: change three fields, CLEAR due and assignee
	updated, err := f.store.UpdateTask(ctx, f.bob, task.ID, TaskPatch{
		Title:      Some("Fix the gate"),
		Notes:      Some(""),
		Priority:   Some("low"),
		DueAt:      Clear[*time.Time](),
		AssignedTo: Clear[*string](),
	})
	mustNoErr(t, err, "update task")
	if updated.Title != "Fix the gate" || updated.Notes != "" || updated.Priority != "low" ||
		updated.DueAt != nil || updated.AssignedTo != nil || updated.AssigneeName != nil {
		t.Fatalf("patched task = %+v", updated)
	}

	// a patch of unset fields leaves everything alone (presence, not zero values)
	same, err := f.store.UpdateTask(ctx, f.bob, task.ID, TaskPatch{})
	mustNoErr(t, err, "empty patch")
	if same.Title != "Fix the gate" || same.Priority != "low" {
		t.Fatalf("empty patch changed the task: %+v", same)
	}

	// toggle: done_at follows the flag; verbs are completed/reopened
	toggled, err := f.store.ToggleTask(ctx, f.bob, task.ID)
	mustNoErr(t, err, "toggle done")
	if !toggled.Done || toggled.DoneAt == nil {
		t.Fatalf("toggle done = %+v", toggled)
	}
	toggled, err = f.store.ToggleTask(ctx, f.bob, task.ID)
	mustNoErr(t, err, "toggle reopen")
	if toggled.Done || toggled.DoneAt != nil {
		t.Fatalf("toggle reopen = %+v", toggled)
	}

	// subtasks: add, patch title+done, then the task rollup sees them
	subtask, err := f.store.AddSubtask(ctx, f.bob, task.ID, "buy hinge")
	mustNoErr(t, err, "add subtask")
	subtask, err = f.store.UpdateSubtask(ctx, f.bob, task.ID, subtask.ID,
		SubtaskPatch{Title: Some("buy two hinges"), Done: Some(true)})
	mustNoErr(t, err, "update subtask")
	if subtask.Title != "buy two hinges" || !subtask.Done {
		t.Fatalf("updated subtask = %+v", subtask)
	}
	detail, err = f.store.GetTask(ctx, f.alice, task.ID)
	mustNoErr(t, err, "get with subtask")
	if detail.Task.SubtaskCount != 1 || detail.Task.SubtaskDone != 1 {
		t.Fatalf("subtask rollup = %d/%d", detail.Task.SubtaskCount, detail.Task.SubtaskDone)
	}
	// the (task, subtask) pair is bound: a wrong task id cannot delete it
	other := f.mustTask(f.alice, project.ID, TaskInput{Title: "Paint fence"})
	mustErr(t, f.store.DeleteSubtask(ctx, f.alice, other.ID, subtask.ID), ErrNotFound, "cross-task subtask delete")
	mustNoErr(t, f.store.DeleteSubtask(ctx, f.alice, task.ID, subtask.ID), "delete subtask")
	mustErr(t, f.store.DeleteSubtask(ctx, f.alice, task.ID, subtask.ID), ErrNotFound, "second subtask delete")

	// comments: author-or-admin deletion
	first, err := f.store.AddComment(ctx, f.bob, task.ID, "looks good")
	mustNoErr(t, err, "add comment")
	if first.AuthorName == nil || *first.AuthorName != "bob" {
		t.Fatalf("comment author = %+v", first)
	}
	second, err := f.store.AddComment(ctx, f.bob, task.ID, "one more thing")
	mustNoErr(t, err, "add second comment")
	mustErr(t, f.store.DeleteComment(ctx, f.carol, task.ID, second.ID), ErrForbidden, "member deletes foreign comment")
	mustNoErr(t, f.store.DeleteComment(ctx, f.bob, task.ID, second.ID), "author deletes own comment")
	third, err := f.store.AddComment(ctx, f.bob, task.ID, "admin may delete this")
	mustNoErr(t, err, "add third comment")
	mustNoErr(t, f.store.DeleteComment(ctx, f.alice, task.ID, third.ID), "admin deletes comment")

	detail, err = f.store.GetTask(ctx, f.alice, task.ID)
	mustNoErr(t, err, "get with comments")
	if detail.Task.CommentCount != 1 || len(detail.Comments) != 1 || detail.Comments[0].ID != first.ID {
		t.Fatalf("comment state = count %d, rows %+v", detail.Task.CommentCount, detail.Comments)
	}

	// exact timeline: one row per mutation verb, in transaction order
	wantVerbs := []string{
		"created", "assigned",
		"updated", "assigned",
		"completed", "reopened",
		"commented", "commented", "commented",
		"deleted",
	}
	mustNoErr(t, f.store.SoftDeleteTask(ctx, f.bob, task.ID), "soft delete task")
	gotVerbs := activityVerbs(t, f.pool, task.ID)
	if len(gotVerbs) != len(wantVerbs) {
		t.Fatalf("activity verbs = %v, want %v", gotVerbs, wantVerbs)
	}
	for i := range wantVerbs {
		if gotVerbs[i] != wantVerbs[i] {
			t.Fatalf("activity verbs = %v, want %v", gotVerbs, wantVerbs)
		}
	}
	detail, err = f.store.GetTask(ctx, f.alice, task.ID)
	mustErr(t, err, ErrNotFound, "deleted task detail")
	if !detail.Task.CreatedAt.IsZero() {
		t.Fatalf("not-found detail leaked a task: %+v", detail.Task)
	}
}

// TestListViewsAndStatsInJakarta proves the day/week math uses the CLIENT's
// calendar, not the server's: Jakarta is UTC+7, so a task due at 00:30 local
// is on the PREVIOUS UTC date. Both local-today tasks (00:30 and 23:30) must
// land in "today" — a UTC-day implementation can only ever include one of
// them, so this pair is the timezone witness.
func TestListViewsAndStatsInJakarta(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatalf("load Asia/Jakarta: %v", err)
	}

	project := f.mustProject(f.alice, "Home")
	nowLocal := time.Now().In(loc)
	year, month, day := nowLocal.Date()
	todayEarly := time.Date(year, month, day, 0, 30, 0, 0, loc)
	todayLate := time.Date(year, month, day, 23, 30, 0, 0, loc)
	tomorrowEarly := todayEarly.AddDate(0, 0, 1)
	yesterdayLate := todayLate.AddDate(0, 0, -1)
	if todayEarly.UTC().Format(time.DateOnly) == todayEarly.Format(time.DateOnly) {
		t.Fatalf("fixture is not a UTC-midnight crossing: %v", todayEarly)
	}

	create := func(title string, due *time.Time) Task {
		return f.mustTask(f.alice, project.ID, TaskInput{Title: title, DueAt: due})
	}
	create("early today", &todayEarly)
	create("late today", &todayLate)
	create("tomorrow", &tomorrowEarly)
	create("yesterday", &yesterdayLate)
	undated := create("undated", nil)
	_, err = f.store.ToggleTask(ctx, f.alice, undated.ID)
	mustNoErr(t, err, "complete the undated task")

	// "My day" = due today + OVERDUE (still open). The fixture's
	// yesterday task is open, so it belongs here; a completed past-due one
	// would not (proved by the undated done task staying out — it has no due
	// date, and by the fixture's only done task being undated).
	todayTasks, err := f.store.ListTasks(ctx, f.alice, ViewToday, "", loc)
	mustNoErr(t, err, "today view")
	if len(todayTasks) != 3 {
		t.Fatalf("today view = %d tasks, want 3 (both local-day tasks + overdue): %+v",
			len(todayTasks), todayTasks)
	}
	titles := map[string]bool{}
	for _, task := range todayTasks {
		titles[task.Title] = true
	}
	if !titles["early today"] || !titles["late today"] || !titles["yesterday"] {
		t.Fatalf("today view titles = %v, want both local-day tasks + overdue", titles)
	}

	upcoming, err := f.store.ListTasks(ctx, f.alice, ViewUpcoming, "", loc)
	mustNoErr(t, err, "upcoming view")
	if len(upcoming) != 1 || upcoming[0].Title != "tomorrow" {
		t.Fatalf("upcoming = %+v, want only tomorrow", upcoming)
	}

	all, err := f.store.ListTasks(ctx, f.alice, ViewAll, "", loc)
	mustNoErr(t, err, "all view")
	if len(all) != 5 {
		t.Fatalf("all view = %d tasks, want 5", len(all))
	}
	filtered, err := f.store.ListTasks(ctx, f.alice, ViewAll, project.ID, loc)
	mustNoErr(t, err, "project-filtered view")
	if len(filtered) != 5 {
		t.Fatalf("project-filtered view = %d, want 5", len(filtered))
	}
	_, err = f.store.ListTasks(ctx, f.alice, "someday", "", loc)
	mustErr(t, err, ErrInvalidView, "unknown view")

	stats, err := f.store.Stats(ctx, f.alice, loc)
	mustNoErr(t, err, "stats")
	if stats.Open != 4 || stats.DueToday != 2 || stats.CompletedWeek != 1 || stats.Overdue != 1 {
		t.Fatalf("stats = %+v, want {4 2 1 1}", stats)
	}
	// membership scoping: bob shares no project, so his cards are empty
	stats, err = f.store.Stats(ctx, f.bob, loc)
	mustNoErr(t, err, "stranger stats")
	if stats != (Stats{}) {
		t.Fatalf("stranger stats = %+v", stats)
	}
}

// TestOnUserDeletedRepairsProjects is the C4 hook, observed AFTER the hard
// delete that the FK cascade rides: a project that loses its only admin
// promotes the earliest-joined member; a project that loses its last member is
// soft-deleted; a project with another admin is untouched; a re-run is a no-op.
func TestOnUserDeletedRepairsProjects(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A: alice (admin) + bob (member) — bob must inherit.
	projectA := f.mustProject(f.alice, "Shared")
	f.addMember(projectA.ID, f.alice, f.bob)

	// B: alice alone — nobody to inherit.
	projectB := f.mustProject(f.alice, "Solo")

	// C: alice + carol promoted — an admin remains, nothing to do.
	projectC := f.mustProject(f.alice, "Covered")
	f.addMember(projectC.ID, f.alice, f.carol)
	mustNoErr(t, f.store.SetMemberRole(ctx, f.alice, projectC.ID, f.carol, "admin"), "promote carol")

	// the production user-delete path hard-deletes the users row; the FK
	// CASCADE removes memberships and SET NULL clears the creator ids.
	if _, err := f.pool.Exec(ctx, `DELETE FROM users WHERE id = $1::uuid`, f.alice); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	mustNoErr(t, f.store.OnUserDeleted(ctx, f.alice), "lifecycle hook")

	members, err := f.store.ListMembers(ctx, f.bob, projectA.ID)
	mustNoErr(t, err, "project A roster")
	if len(members) != 1 || members[0].UserID != f.bob || members[0].Role != "admin" {
		t.Fatalf("project A after hook = %+v, want bob admin", members)
	}
	if n := f.count(`SELECT count(*) FROM task_projects WHERE id = $1::uuid AND creator_id IS NULL`, projectA.ID); n != 1 {
		t.Fatal("creator_id was not SET NULL by the FK")
	}

	if n := f.count(`SELECT count(*) FROM task_projects WHERE id = $1::uuid AND deleted_at IS NOT NULL`, projectB.ID); n != 1 {
		t.Fatal("empty project was not soft-deleted")
	}
	members, err = f.store.ListMembers(ctx, f.carol, projectC.ID)
	mustNoErr(t, err, "project C roster")
	if len(members) != 1 || members[0].Role != "admin" {
		t.Fatalf("project C after hook = %+v, want carol admin", members)
	}

	// idempotent: a second run changes nothing and errors nowhere
	mustNoErr(t, f.store.OnUserDeleted(ctx, f.alice), "hook re-run")
	members, err = f.store.ListMembers(ctx, f.bob, projectA.ID)
	mustNoErr(t, err, "project A roster after re-run")
	if len(members) != 1 || members[0].Role != "admin" {
		t.Fatalf("re-run changed project A: %+v", members)
	}
}

// TestDisplayNameRollupsAndInviteeSearch pins the identity boundary: display
// names are LABELS (the assignee/member references stay users.id UUIDs), and
// the invite typeahead matches username/display prefixes or an EXACT email
// while excluding members, pending invitees and disabled accounts.
func TestDisplayNameRollupsAndInviteeSearch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	setProfile := func(id, name, email string) {
		t.Helper()
		if _, err := f.pool.Exec(ctx,
			`UPDATE users SET display_name = $2, email = NULLIF($3, '') WHERE id = $1::uuid`,
			id, name, email); err != nil {
			t.Fatalf("set profile: %v", err)
		}
	}
	setProfile(f.alice, "Alice Archer", "alice@example.com")
	setProfile(f.bob, "Bob Builder", "bob@example.com")
	setProfile(f.carol, "Carol Danvers", "carol@example.com")

	project := f.mustProject(f.alice, "Home")
	f.addMember(project.ID, f.alice, f.bob)
	task := f.mustTask(f.alice, project.ID, TaskInput{Title: "Fix gate", AssignedTo: ptr(f.bob)})

	// the FK is the id; the label is the display name
	detail, err := f.store.GetTask(ctx, f.alice, task.ID)
	mustNoErr(t, err, "get assigned task")
	if detail.Task.AssignedTo == nil || *detail.Task.AssignedTo != f.bob {
		t.Fatalf("assignee FK = %v, want bob's users.id", detail.Task.AssignedTo)
	}
	if detail.Task.AssigneeName == nil || *detail.Task.AssigneeName != "Bob Builder" {
		t.Fatalf("assignee label = %v, want the display name", detail.Task.AssigneeName)
	}
	if detail.Task.CreatorName == nil || *detail.Task.CreatorName != "Alice Archer" {
		t.Fatalf("creator label = %v, want the display name", detail.Task.CreatorName)
	}
	members, err := f.store.ListMembers(ctx, f.alice, project.ID)
	mustNoErr(t, err, "roster")
	names := map[string]string{}
	for _, member := range members {
		names[member.UserID] = member.Name
	}
	if names[f.alice] != "Alice Archer" || names[f.bob] != "Bob Builder" {
		t.Fatalf("roster names = %+v", names)
	}

	// display-name prefix finds the non-member; members and the caller never
	// appear (both are on the roster)
	people, err := f.store.SearchInvitees(ctx, f.alice, project.ID, "car", 8)
	mustNoErr(t, err, "search by display prefix")
	if len(people) != 1 || people[0].Username != "carol" || people[0].Name != "Carol Danvers" {
		t.Fatalf("display-prefix search = %+v", people)
	}
	if people, _ := f.store.SearchInvitees(ctx, f.alice, project.ID, "bob", 8); len(people) != 0 {
		t.Fatalf("member leaked into suggestions: %+v", people)
	}
	if people, _ := f.store.SearchInvitees(ctx, f.alice, project.ID, "ali", 8); len(people) != 0 {
		t.Fatalf("caller leaked into suggestions: %+v", people)
	}

	// username prefix works too (carol is still a non-member here)
	if people, _ := f.store.SearchInvitees(ctx, f.alice, project.ID, "caro", 8); len(people) != 1 {
		t.Fatalf("username-prefix search = %+v", people)
	}

	// email is EXACT and case-insensitive; a partial address never matches
	if people, _ := f.store.SearchInvitees(ctx, f.alice, project.ID, "carol@example.com", 8); len(people) != 1 {
		t.Fatalf("exact email search = %+v", people)
	}
	if people, _ := f.store.SearchInvitees(ctx, f.alice, project.ID, "carol@example", 8); len(people) != 0 {
		t.Fatalf("partial email matched (must be exact-only): %+v", people)
	}

	// a live pending invite removes the person from suggestions
	invite, err := f.store.CreateInvite(ctx, f.alice, project.ID, f.carol, time.Now().Add(time.Hour))
	mustNoErr(t, err, "invite carol")
	if people, _ := f.store.SearchInvitees(ctx, f.alice, project.ID, "carol", 8); len(people) != 0 {
		t.Fatalf("pending invitee still suggested: %+v (invite %s)", people, invite.ID)
	}

	// expired invites do not suppress the row (only live pending ones)
	if _, err := f.pool.Exec(ctx,
		`UPDATE task_project_invites SET expires_at = now() - interval '1 hour' WHERE id = $1::uuid`,
		invite.ID); err != nil {
		t.Fatalf("expire invite: %v", err)
	}
	if people, _ := f.store.SearchInvitees(ctx, f.alice, project.ID, "carol", 8); len(people) != 1 {
		t.Fatalf("expired invite suppressed the candidate: %+v", people)
	}

	// disabled accounts are not suggested
	if _, err := f.pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1::uuid`, f.carol); err != nil {
		t.Fatalf("disable carol: %v", err)
	}
	if people, _ := f.store.SearchInvitees(ctx, f.alice, project.ID, "carol", 8); len(people) != 0 {
		t.Fatalf("disabled account suggested: %+v", people)
	}

	// the admin check is the second key: a member is forbidden, a non-member
	// project is not-found (the tasks module's one denial voice)
	mustErr(t, func() error {
		_, err := f.store.SearchInvitees(ctx, f.bob, project.ID, "carol", 8)
		return err
	}(), ErrForbidden, "member search")
	mustErr(t, func() error {
		_, err := f.store.SearchInvitees(ctx, f.carol, project.ID, "alice", 8)
		return err
	}(), ErrNotFound, "non-member search")

	// the cap is the store's too (a limit of 0 is an empty menu, not a dump)
	if people, _ := f.store.SearchInvitees(ctx, f.alice, project.ID, "a", 0); len(people) != 0 {
		t.Fatalf("zero limit returned rows: %+v", people)
	}
}
