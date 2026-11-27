// service_test.go — the tasks service contract. The validation rules run
// OFFLINE (they must reject before persistence); everything that needs rows
// is gated by LUMEN_INTEGRATION and rides the store test's fresh-schema
// fixture. The fake Notifier is the point of the seam: the tests assert the
// notification copy, the recipient, the 24-hour invite TTL, the escaped
// HTML, and that a delivery failure never fails the mutation (owner C10).
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // Asia/Jakarta must exist even on a zoneinfo-less runner

	"github.com/kanatabi/lumen/internal/notifications"
)

// fakeNotifier records every batch the service sends and returns the test's
// chosen verdict. It is deliberately not a service double: the assertion is
// the notification the module decided to send, not a second implementation.
type fakeNotifier struct {
	batches [][]notifications.SendParams
	err     error
}

func (f *fakeNotifier) Send(_ context.Context, batch []notifications.SendParams) (int, error) {
	copied := make([]notifications.SendParams, len(batch))
	copy(copied, batch)
	f.batches = append(f.batches, copied)
	if f.err != nil {
		return 0, f.err
	}
	return len(batch), nil
}

func (f *fakeNotifier) reset() { f.batches = nil }

func (f *fakeNotifier) sent() int { return len(f.batches) }

// only returns the single SendParams of the single batch; a test that expects
// exactly one notification reads it here so a duplicated send fails loudly.
func (f *fakeNotifier) only(t *testing.T) notifications.SendParams {
	t.Helper()
	if len(f.batches) != 1 || len(f.batches[0]) != 1 {
		t.Fatalf("notifier batches = %d, want exactly one batch of one", len(f.batches))
	}
	return f.batches[0][0]
}

func principalFor(id, username string) Principal {
	return Principal{ID: id, Username: username}
}

// TestServiceValidationRules proves every cap and enum rejects with
// ErrInvalid BEFORE the store is touched — the nil store is the witness.
func TestServiceValidationRules(t *testing.T) {
	ctx := context.Background()
	service := NewService(nil, nil) // any store access would panic; none is allowed
	principal := Principal{ID: "user-1", Username: "alice"}

	tooLongTitle := strings.Repeat("t", maxTaskTitleRunes+1)
	tooLongName := strings.Repeat("n", maxProjectNameRunes+1)
	tooManyNotes := strings.Repeat("n", maxTaskNotesBytes+1)
	tooBigComment := strings.Repeat("c", maxCommentBytes+1)

	cases := []struct {
		name string
		call func() error
	}{
		{"project name whitespace", func() error {
			_, err := service.CreateProject(ctx, principal, "   ")
			return err
		}},
		{"project name over 100 runes", func() error {
			_, err := service.CreateProject(ctx, principal, tooLongName)
			return err
		}},
		{"rename to empty", func() error {
			return service.RenameProject(ctx, principal, "project-1", "")
		}},
		{"role outside admin|member", func() error {
			return service.SetMemberRole(ctx, principal, "project-1", "user-2", "owner")
		}},
		{"invite username whitespace", func() error {
			_, err := service.CreateInvite(ctx, principal, "project-1", " ")
			return err
		}},
		{"task title over 200 runes", func() error {
			_, err := service.CreateTask(ctx, principal, "project-1", TaskInput{Title: tooLongTitle})
			return err
		}},
		{"task title whitespace", func() error {
			_, err := service.CreateTask(ctx, principal, "project-1", TaskInput{Title: " \t "})
			return err
		}},
		{"notes over 16 KiB", func() error {
			_, err := service.CreateTask(ctx, principal, "project-1",
				TaskInput{Title: "ok", Notes: tooManyNotes})
			return err
		}},
		{"bad priority", func() error {
			_, err := service.CreateTask(ctx, principal, "project-1",
				TaskInput{Title: "ok", Priority: "urgent"})
			return err
		}},
		{"blank assignee id", func() error {
			blank := "   "
			_, err := service.CreateTask(ctx, principal, "project-1",
				TaskInput{Title: "ok", AssignedTo: &blank})
			return err
		}},
		{"bad view", func() error {
			_, err := service.ListTasks(ctx, principal, "someday", "", nil)
			return err
		}},
		{"patch title over 200 runes", func() error {
			_, err := service.UpdateTask(ctx, principal, "task-1",
				TaskPatch{Title: Some(tooLongTitle)})
			return err
		}},
		{"patch priority empty", func() error {
			_, err := service.UpdateTask(ctx, principal, "task-1",
				TaskPatch{Priority: Some("")})
			return err
		}},
		{"subtask title over 200 runes", func() error {
			_, err := service.AddSubtask(ctx, principal, "task-1", tooLongTitle)
			return err
		}},
		{"subtask patch title over 200 runes", func() error {
			_, err := service.UpdateSubtask(ctx, principal, "task-1", "subtask-1",
				SubtaskPatch{Title: Some(tooLongTitle)})
			return err
		}},
		{"comment body whitespace", func() error {
			_, err := service.AddComment(ctx, principal, "task-1", "  \n ")
			return err
		}},
		{"comment body over 8 KiB", func() error {
			_, err := service.AddComment(ctx, principal, "task-1", tooBigComment)
			return err
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.call(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestServiceInviteNotification is the invite copy contract: one recipient,
// the tasks app/type, the 24-hour TTL carried onto the notification's
// expiresAt, the escaped inviter name, and the invite id in the payload.
func TestServiceInviteNotification(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	notifier := &fakeNotifier{}
	service := NewService(fixture.store, notifier)
	alice := principalFor(fixture.alice, "alice")

	project, err := service.CreateProject(ctx, alice, "Home & <b>")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	invite, err := service.CreateInvite(ctx, alice, project.ID, "bob")
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if remaining := time.Until(invite.ExpiresAt); remaining < 23*time.Hour+59*time.Minute || remaining > 24*time.Hour+time.Minute {
		t.Fatalf("invite TTL = %v, want 24h", remaining)
	}

	params := notifier.only(t)
	if params.UserID != fixture.bob {
		t.Fatalf("recipient = %q, want bob", params.UserID)
	}
	if params.App != "tasks" || params.Type != "project.invite" {
		t.Fatalf("app/type = %q/%q, want tasks/project.invite", params.App, params.Type)
	}
	if params.Title != "📦 You've been invited to Home & <b>" {
		t.Fatalf("title = %q", params.Title)
	}
	wantBody := "<b>alice</b> invited you to collaborate — open Tasks to accept."
	if params.Body != wantBody {
		t.Fatalf("body = %q, want %q", params.Body, wantBody)
	}
	if params.ExpiresAt == nil || !params.ExpiresAt.Equal(invite.ExpiresAt) {
		t.Fatalf("expiresAt = %v, want the invite's %v", params.ExpiresAt, invite.ExpiresAt)
	}
	var payload struct {
		URL    string `json:"url"`
		Invite string `json:"invite"`
	}
	if err := json.Unmarshal(params.Payload, &payload); err != nil {
		t.Fatalf("payload: %v (%s)", err, params.Payload)
	}
	if payload.URL != "/tasks" || payload.Invite != invite.ID {
		t.Fatalf("payload = %+v, want /tasks + the invite id", payload)
	}

	// An unknown username is the caller's typo (400) and must not notify.
	notifier.reset()
	if _, err := service.CreateInvite(ctx, alice, project.ID, "nobody"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown username err = %v, want ErrInvalid", err)
	}
	if notifier.sent() != 0 {
		t.Fatalf("unknown username sent %d notifications", notifier.sent())
	}

	// The inviter's username lands in an HTML body, so it is escaped even
	// though the real onboarding grammar forbids angle brackets. The users
	// table accepts any text; the notification copy must not.
	hostileName := "evil<b>"
	if _, err := fixture.pool.Exec(ctx,
		`INSERT INTO users (username, passhash) VALUES ($1, $2)`, hostileName, "x"); err != nil {
		t.Fatalf("seed hostile username: %v", err)
	}
	var hostileID string
	if err := fixture.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE username = $1`, hostileName).Scan(&hostileID); err != nil {
		t.Fatalf("read hostile user id: %v", err)
	}
	hostile := principalFor(hostileID, hostileName)
	hostileProject, err := service.CreateProject(ctx, hostile, "Escaping")
	if err != nil {
		t.Fatalf("create hostile project: %v", err)
	}
	notifier.reset()
	if _, err := service.CreateInvite(ctx, hostile, hostileProject.ID, "bob"); err != nil {
		t.Fatalf("hostile invite: %v", err)
	}
	params = notifier.only(t)
	if !strings.Contains(params.Body, "<b>evil&lt;b&gt;</b>") {
		t.Fatalf("inviter name was not escaped: %q", params.Body)
	}
}

// TestServiceInviteNotificationBestEffort pins C10: the invite row commits
// first, so a failing notifier leaves a real, listable invite behind.
func TestServiceInviteNotificationBestEffort(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	notifier := &fakeNotifier{err: errors.New("notification store unavailable")}
	service := NewService(fixture.store, notifier)
	alice := principalFor(fixture.alice, "alice")

	project, err := service.CreateProject(ctx, alice, "Home")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	invite, err := service.CreateInvite(ctx, alice, project.ID, "bob")
	if err != nil {
		t.Fatalf("create invite must succeed despite the notifier: %v", err)
	}
	if notifier.sent() != 1 {
		t.Fatalf("send attempts = %d, want 1", notifier.sent())
	}

	inbox, err := service.ListInvites(ctx, principalFor(fixture.bob, "bob"))
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	if len(inbox) != 1 || inbox[0].ID != invite.ID {
		t.Fatalf("inbox = %+v, want the committed invite", inbox)
	}
}

// TestServiceAssignmentNotifications proves the assignment copy and the
// exactly-once rule: create/update notify only a non-nil assignee who is not
// the actor, and re-PATCHing the same person does not notify again.
func TestServiceAssignmentNotifications(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	notifier := &fakeNotifier{}
	service := NewService(fixture.store, notifier)
	alice := principalFor(fixture.alice, "alice")
	bob := principalFor(fixture.bob, "bob")

	project, err := service.CreateProject(ctx, alice, "Home & <b>")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	fixture.addMember(project.ID, fixture.alice, fixture.bob)
	notifier.reset() // the membership invite is not this test's subject

	task, err := service.CreateTask(ctx, alice, project.ID, TaskInput{
		Title: `Fix <gate> & "door"`, AssignedTo: &fixture.bob,
	})
	if err != nil {
		t.Fatalf("create assigned task: %v", err)
	}
	params := notifier.only(t)
	if params.UserID != fixture.bob {
		t.Fatalf("create recipient = %q, want bob", params.UserID)
	}
	if params.App != "tasks" || params.Type != "task.assigned" {
		t.Fatalf("create app/type = %q/%q", params.App, params.Type)
	}
	if !strings.Contains(params.Body, "Fix &lt;gate&gt; &amp; &#34;door&#34;") {
		t.Fatalf("body did not escape the task title: %q", params.Body)
	}
	if !strings.Contains(params.Body, "in Home &amp; &lt;b&gt;") {
		t.Fatalf("body did not escape the project name: %q", params.Body)
	}
	if strings.Contains(params.Body, "<gate>") {
		t.Fatalf("body contains raw HTML from the task title: %q", params.Body)
	}
	var payload struct {
		URL  string `json:"url"`
		Task string `json:"task"`
	}
	if err := json.Unmarshal(params.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.URL != "/tasks?task="+task.ID || payload.Task != task.ID {
		t.Fatalf("payload = %+v, want the task deep link", payload)
	}

	// Assigning a task to the ACTOR is not an assignment notification.
	notifier.reset()
	if _, err := service.CreateTask(ctx, alice, project.ID, TaskInput{
		Title: "Self-assigned", AssignedTo: &fixture.alice,
	}); err != nil {
		t.Fatalf("create self-assigned task: %v", err)
	}
	if notifier.sent() != 0 {
		t.Fatalf("self-assignment sent %d notifications", notifier.sent())
	}

	// Moving the assignee to the ACTOR is a change, but not a notification.
	notifier.reset()
	aliceID := fixture.alice
	if _, err := service.UpdateTask(ctx, alice, task.ID, TaskPatch{AssignedTo: Some(&aliceID)}); err != nil {
		t.Fatalf("assign to alice: %v", err)
	}
	if notifier.sent() != 0 {
		t.Fatalf("assignment to the actor sent %d notifications", notifier.sent())
	}

	// A real assignee change notifies.
	notifier.reset()
	bobID := fixture.bob
	if _, err := service.UpdateTask(ctx, alice, task.ID, TaskPatch{AssignedTo: Some(&bobID)}); err != nil {
		t.Fatalf("assign to bob: %v", err)
	}
	params = notifier.only(t)
	if params.UserID != fixture.bob {
		t.Fatalf("update recipient = %q, want bob", params.UserID)
	}

	// The same assignee again is not a change.
	notifier.reset()
	if _, err := service.UpdateTask(ctx, alice, task.ID, TaskPatch{AssignedTo: Some(&bobID)}); err != nil {
		t.Fatalf("repeat assignment: %v", err)
	}
	if notifier.sent() != 0 {
		t.Fatalf("repeated assignment sent %d notifications", notifier.sent())
	}

	// Clearing the assignee is not an assignment notification.
	notifier.reset()
	if _, err := service.UpdateTask(ctx, alice, task.ID, TaskPatch{AssignedTo: Clear[*string]()}); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	if notifier.sent() != 0 {
		t.Fatalf("unassignment sent %d notifications", notifier.sent())
	}

	// Assigning to the actor (bob assigns himself) is not a notification.
	notifier.reset()
	if _, err := service.UpdateTask(ctx, bob, task.ID, TaskPatch{AssignedTo: Some(&bobID)}); err != nil {
		t.Fatalf("bob self-assign: %v", err)
	}
	if notifier.sent() != 0 {
		t.Fatalf("actor self-assignment sent %d notifications", notifier.sent())
	}

	// A 200-rune title still fits the notification budget after the emoji.
	notifier.reset()
	longTitle := strings.Repeat("x", maxTaskTitleRunes)
	longTask, err := service.CreateTask(ctx, alice, project.ID, TaskInput{
		Title: longTitle, AssignedTo: &fixture.bob,
	})
	if err != nil {
		t.Fatalf("create long-titled task: %v", err)
	}
	params = notifier.only(t)
	if runes := []rune(params.Title); len(runes) > maxNotificationTitleRunes {
		t.Fatalf("notification title is %d runes (task %s)", len(runes), longTask.ID)
	}
}

// TestServiceInviteFlow walks the invite state machine through the service:
// accept joins the roster and consumes the row; revoke is the admin's cancel;
// decline is the invitee's no.
func TestServiceInviteFlow(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	service := NewService(fixture.store, &fakeNotifier{})
	alice := principalFor(fixture.alice, "alice")
	bob := principalFor(fixture.bob, "bob")
	carol := principalFor(fixture.carol, "carol")

	project, err := service.CreateProject(ctx, alice, "Home")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	invite, err := service.CreateInvite(ctx, alice, project.ID, "bob")
	if err != nil {
		t.Fatalf("invite bob: %v", err)
	}
	inbox, err := service.ListInvites(ctx, bob)
	if err != nil {
		t.Fatalf("bob inbox: %v", err)
	}
	if len(inbox) != 1 || inbox[0].ID != invite.ID {
		t.Fatalf("bob inbox = %+v, want the pending invite", inbox)
	}

	accepted, err := service.AcceptInvite(ctx, bob, invite.ID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if accepted.ProjectID != project.ID || accepted.ProjectName != "Home" {
		t.Fatalf("accepted = %+v", accepted)
	}
	members, err := service.ListMembers(ctx, alice, project.ID)
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("roster = %+v, want alice + bob", members)
	}
	if inbox, err = service.ListInvites(ctx, bob); err != nil || len(inbox) != 0 {
		t.Fatalf("bob inbox after accept = %+v, err %v", inbox, err)
	}

	// Another user's invite is the same not-found voice.
	carolInvite, err := service.CreateInvite(ctx, alice, project.ID, "carol")
	if err != nil {
		t.Fatalf("invite carol: %v", err)
	}
	if _, err := service.AcceptInvite(ctx, bob, carolInvite.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("accepting carol's invite err = %v, want ErrNotFound", err)
	}
	if err := service.RevokeInvite(ctx, alice, project.ID, carolInvite.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if inbox, err = service.ListInvites(ctx, carol); err != nil || len(inbox) != 0 {
		t.Fatalf("carol inbox after revoke = %+v, err %v", inbox, err)
	}

	declined, err := service.CreateInvite(ctx, alice, project.ID, "carol")
	if err != nil {
		t.Fatalf("re-invite carol: %v", err)
	}
	if err := service.DeclineInvite(ctx, carol, declined.ID); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if inbox, err = service.ListInvites(ctx, carol); err != nil || len(inbox) != 0 {
		t.Fatalf("carol inbox after decline = %+v, err %v", inbox, err)
	}
}

// TestServiceStatsInJakarta proves the service passes the CLIENT's location
// through: the same rows produce different cards in UTC and Jakarta, so a
// dropped *time.Location would be visible.
func TestServiceStatsInJakarta(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	service := NewService(fixture.store, &fakeNotifier{})
	alice := principalFor(fixture.alice, "alice")

	location, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatalf("load Asia/Jakarta: %v", err)
	}
	project, err := service.CreateProject(ctx, alice, "Home")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	nowLocal := time.Now().In(location)
	year, month, day := nowLocal.Date()
	todayEarly := time.Date(year, month, day, 0, 30, 0, 0, location)
	todayLate := time.Date(year, month, day, 23, 30, 0, 0, location)
	tomorrowEarly := todayEarly.AddDate(0, 0, 1)
	yesterdayLate := todayLate.AddDate(0, 0, -1)
	if todayEarly.UTC().Format(time.DateOnly) == todayEarly.Format(time.DateOnly) {
		t.Fatalf("fixture is not a UTC-midnight crossing: %v", todayEarly)
	}

	create := func(title string, due *time.Time) Task {
		t.Helper()
		task, err := service.CreateTask(ctx, alice, project.ID, TaskInput{Title: title, DueAt: due})
		if err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
		return task
	}
	// The two extremes of the local day are the timezone witness: exactly
	// one of them falls inside the UTC day at ANY hour, so Jakarta's 2
	// against UTC's 1 proves the location was passed through.
	create("early today", &todayEarly)
	create("late today", &todayLate)
	stats, err := service.Stats(ctx, alice, location)
	if err != nil {
		t.Fatalf("jakarta stats: %v", err)
	}
	if stats != (Stats{Open: 2, DueToday: 2}) {
		t.Fatalf("jakarta stats = %+v, want {2 2 0 0}", stats)
	}
	statsUTC, err := service.Stats(ctx, alice, time.UTC)
	if err != nil {
		t.Fatalf("utc stats: %v", err)
	}
	if statsUTC.DueToday != 1 {
		t.Fatalf("utc dueToday = %d, want 1 (the location was dropped)", statsUTC.DueToday)
	}

	create("tomorrow", &tomorrowEarly)
	create("yesterday", &yesterdayLate)
	undated := create("undated", nil)
	if _, err := service.ToggleTask(ctx, alice, undated.ID); err != nil {
		t.Fatalf("complete undated: %v", err)
	}

	stats, err = service.Stats(ctx, alice, location)
	if err != nil {
		t.Fatalf("jakarta stats: %v", err)
	}
	if stats != (Stats{Open: 4, DueToday: 2, CompletedWeek: 1, Overdue: 1}) {
		t.Fatalf("jakarta stats = %+v, want {4 2 1 1}", stats)
	}

	today, err := service.ListTasks(ctx, alice, ViewToday, "", location)
	if err != nil {
		t.Fatalf("jakarta today view: %v", err)
	}
	if len(today) != 3 {
		t.Fatalf("jakarta today view = %d tasks, want 3", len(today))
	}
}
