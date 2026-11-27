package api_test

// tasks_integration_test.go — M3: the END-TO-END proof of the tasks +
// notifications modules over REAL HTTP and REAL Postgres. Nothing here calls
// the modules directly: every step rides the router the composition root
// builds (the harness's build closure now enables both modules and the
// users-delete lifecycle hook), so the proof covers the wiring itself —
// scope gates, membership resolution, the Notifier seam, the notification
// TTL filters, and the C4 promotion.
//
// Three tests, three voices:
//   - TestTasksNotificationsWalk is the two-user walk: invite → notification
//     (with the invite's own TTL) → accept → assignment → notification →
//     comment/activity → toggle → Jakarta stats/view → complete → soft delete
//     → the send door + mark-read/read-all.
//   - TestTasksNotificationsDenials is the one-denial-voice matrix: a
//     non-member's task read AND write are 404, a non-member assignee is 409,
//     the last admin cannot leave, a plain member cannot manage the project,
//     an expired invite is 410-and-consumed, an expired notification is
//     invisible to list/count/mark-read, and an unset token leaves the SEND
//     door unmounted (405, never a 201).
//   - TestUserDeletePromotesProjectAdmin proves the C4 lifecycle hook through
//     the HTTP user-delete door: deleting a project's only admin promotes the
//     earliest remaining member.
//
// Run-to-run hygiene lives where the harness puts its own purge: created
// users are deleted in t.Cleanup (their notifications cascade), and the
// harness purge removes the itest- projects (a project survives its creator
// by design, so the user delete alone would leak it).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // Asia/Jakarta must resolve on a zoneinfo-less runner

	"github.com/kanatabi/lumen/internal/session"
	"github.com/kanatabi/lumen/internal/store"
)

// ---- wire shapes (only the fields this round asserts; the full contract
// lives in internal/api/admin/tasks.go and notifications.go) ----

type itestProject struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Role        string     `json:"role"`
	CreatorID   *string    `json:"creatorId"`
	CompletedAt *time.Time `json:"completedAt"`
	TaskCount   int        `json:"taskCount"`
	OpenCount   int        `json:"openCount"`
}

type itestProjectEnvelope struct {
	Project itestProject `json:"project"`
}

type itestProjects struct {
	Projects []itestProject `json:"projects"`
}

type itestMember struct {
	UserID   string `json:"userId"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

type itestMembers struct {
	Members []itestMember `json:"members"`
}

type itestInvite struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"projectId"`
	ProjectName string    `json:"projectName"`
	InviterID   string    `json:"inviterId"`
	InviterName string    `json:"inviterName"`
	InviteeID   string    `json:"inviteeId"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

type itestInviteEnvelope struct {
	Invite itestInvite `json:"invite"`
}

type itestInvites struct {
	Invites []itestInvite `json:"invites"`
}

type itestTask struct {
	ID           string     `json:"id"`
	ProjectID    string     `json:"projectId"`
	ProjectName  string     `json:"projectName"`
	AssignedTo   *string    `json:"assignedTo"`
	AssigneeName *string    `json:"assigneeName"`
	Title        string     `json:"title"`
	Priority     string     `json:"priority"`
	DueAt        *time.Time `json:"dueAt"`
	Done         bool       `json:"done"`
	DoneAt       *time.Time `json:"doneAt"`
	CommentCount int        `json:"commentCount"`
}

type itestTaskEnvelope struct {
	Task itestTask `json:"task"`
}

type itestTaskComment struct {
	ID         string  `json:"id"`
	AuthorName *string `json:"authorName"`
	Body       string  `json:"body"`
}

type itestTaskActivity struct {
	Verb   string `json:"verb"`
	Detail string `json:"detail"`
}

type itestTaskDetail struct {
	Task     itestTask           `json:"task"`
	Subtasks []json.RawMessage   `json:"subtasks"`
	Comments []itestTaskComment  `json:"comments"`
	Activity []itestTaskActivity `json:"activity"`
}

type itestStats struct {
	Open          int `json:"open"`
	DueToday      int `json:"dueToday"`
	CompletedWeek int `json:"completedWeek"`
	Overdue       int `json:"overdue"`
}

type itestStatsEnvelope struct {
	Stats itestStats `json:"stats"`
}

type itestTasks struct {
	Projects []itestProject `json:"projects"`
	Tasks    []itestTask    `json:"tasks"`
	Stats    itestStats     `json:"stats"`
}

type itestNotification struct {
	ID        string          `json:"id"`
	App       string          `json:"app"`
	Type      string          `json:"type"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Payload   json.RawMessage `json:"payload"`
	ReadAt    *time.Time      `json:"readAt"`
	ExpiresAt time.Time       `json:"expiresAt"`
	CreatedAt time.Time       `json:"createdAt"`
}

type itestNotificationPage struct {
	Items      []itestNotification `json:"items"`
	NextBefore *string             `json:"nextBefore"`
}

type itestUnreadCount struct {
	Count int `json:"count"`
}

type itestReadAll struct {
	Updated int `json:"updated"`
}

type itestSent struct {
	Sent int `json:"sent"`
}

// ---- helpers ----

// itestDecode unmarshals one harness response and fails with the step name
// and the raw body on drift — a shape change must name the walk step it broke.
func itestDecode[T any](t *testing.T, step string, res []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("%s: invalid JSON (%v): %s", step, err, res)
	}
	return out
}

// itestCode asserts a status and keeps the body in the failure message.
func itestCode(t *testing.T, step string, got, want int, res []byte) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: status %d, want %d: %s", step, got, want, res)
	}
}

// itestUser seeds one full-catalog account (UpsertUser grants every scope,
// must_change stays false) and returns its id plus a live session token. The
// UPDATE resets crash residue so a re-used username can never carry a stale
// token or a flag from an aborted run.
func itestUser(t *testing.T, h *adminHarness, username string) (id, token string) {
	t.Helper()
	ctx := context.Background()
	hash, err := session.HashPassword("integration-pw")
	if err != nil {
		t.Fatalf("hash password for %s: %v", username, err)
	}
	if err := store.NewPG(h.pool).UpsertUser(ctx, username, hash); err != nil {
		t.Fatalf("seed user %s: %v", username, err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE users SET token_version = 1, must_change = false, disabled = false
		  WHERE username = $1`, username); err != nil {
		t.Fatalf("reset user %s: %v", username, err)
	}
	if err := h.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE username = $1`, username).Scan(&id); err != nil {
		t.Fatalf("read user %s: %v", username, err)
	}
	t.Cleanup(func() {
		_, _ = h.pool.Exec(context.Background(), `DELETE FROM users WHERE username = $1`, username)
	})
	return id, h.login(username, "integration-pw")
}

// doWithHeader is h.do plus per-request headers. The timezone leg needs
// X-Client-Timezone, which the shared harness helper does not carry.
func (a *adminHarness) doWithHeader(method, path, body, token string, headers map[string]string) (int, []byte) {
	a.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// itestListNotifications reads one caller's panel page.
func itestListNotifications(t *testing.T, h *adminHarness, token, path string) itestNotificationPage {
	t.Helper()
	code, res := h.do(http.MethodGet, path, "", token)
	itestCode(t, "GET "+path, code, http.StatusOK, res)
	return itestDecode[itestNotificationPage](t, "GET "+path, res)
}

// itestUnread reads the caller's bell count.
func itestUnread(t *testing.T, h *adminHarness, token string) int {
	t.Helper()
	code, res := h.do(http.MethodGet, "/admin/notifications/unread-count", "", token)
	itestCode(t, "GET unread-count", code, http.StatusOK, res)
	return itestDecode[itestUnreadCount](t, "GET unread-count", res).Count
}

// itestFindNotification returns the first matching row.
func itestFindNotification(items []itestNotification, match func(itestNotification) bool) (itestNotification, bool) {
	for _, item := range items {
		if match(item) {
			return item, true
		}
	}
	return itestNotification{}, false
}

// itestPayload decodes a notification's stored JSONB payload.
func itestPayload[T any](t *testing.T, step string, raw json.RawMessage) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: invalid payload (%v): %s", step, err, raw)
	}
	return out
}

// itestProjectByID finds one project in a list envelope.
func itestProjectByID(projects []itestProject, id string) (itestProject, bool) {
	for _, project := range projects {
		if project.ID == id {
			return project, true
		}
	}
	return itestProject{}, false
}

// itestMemberByID finds one roster line.
func itestMemberByID(members []itestMember, id string) (itestMember, bool) {
	for _, member := range members {
		if member.UserID == id {
			return member, true
		}
	}
	return itestMember{}, false
}

// itestTaskByID finds one task in a list envelope.
func itestTaskByID(tasks []itestTask, id string) (itestTask, bool) {
	for _, task := range tasks {
		if task.ID == id {
			return task, true
		}
	}
	return itestTask{}, false
}

// itestActivityHasVerb reports whether the drawer timeline carries a verb.
func itestActivityHasVerb(activity []itestTaskActivity, verb string) bool {
	for _, entry := range activity {
		if entry.Verb == verb {
			return true
		}
	}
	return false
}

// TestTasksNotificationsWalk is the two-user proof. alice = the harness's
// fzr-test (real login), bob = a full-catalog account created through
// UpsertUser. Every call is raw HTTP through the built router.
func TestTasksNotificationsWalk(t *testing.T) {
	if os.Getenv("LUMEN_INTEGRATION") == "" {
		t.Skip("LUMEN_INTEGRATION unset")
	}
	// The notifications service reads the token ONCE at construction; the
	// harness build happens inside newAdminHarness, so set it first.
	t.Setenv("LUMEN_NOTIFICATION_TOKEN", "itest-token")
	h := newAdminHarness(t)
	ctx := context.Background()

	bobID, bobToken := itestUser(t, h, "itest-bob")
	var aliceID string
	if err := h.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE username = 'fzr-test'`).Scan(&aliceID); err != nil {
		t.Fatalf("read alice id: %v", err)
	}
	// crash residue must not pollute the exact-count asserts below.
	if _, err := h.pool.Exec(ctx,
		`DELETE FROM notifications WHERE user_id IN ($1, $2)`, aliceID, bobID); err != nil {
		t.Fatalf("purge stale notifications: %v", err)
	}

	// ---- alice creates the project (creator becomes its first admin) ----
	code, res := h.admin(http.MethodPost, "/admin/task-projects", `{"name":"itest-home"}`)
	itestCode(t, "alice creates itest-home", code, http.StatusCreated, res)
	project := itestDecode[itestProjectEnvelope](t, "alice creates itest-home", res).Project
	if project.ID == "" || project.Name != "itest-home" || project.Role != "admin" {
		t.Fatalf("created project shape: %s", res)
	}
	if project.CreatorID == nil || *project.CreatorID != aliceID {
		t.Fatalf("created project creator: %s", res)
	}

	// ---- alice invites bob: the 24h TTL is real ----
	inviteStart := time.Now()
	code, res = h.admin(http.MethodPost, "/admin/task-projects/"+project.ID+"/invites",
		`{"username":"itest-bob"}`)
	itestCode(t, "alice invites bob", code, http.StatusCreated, res)
	invite := itestDecode[itestInviteEnvelope](t, "alice invites bob", res).Invite
	if invite.ID == "" || invite.ProjectID != project.ID {
		t.Fatalf("invite shape: %s", res)
	}
	if invite.InviteeID != bobID || invite.InviterID != aliceID {
		t.Fatalf("invite identities: %s (bob %s, alice %s)", res, bobID, aliceID)
	}
	if ttl := invite.ExpiresAt.Sub(inviteStart); ttl < 24*time.Hour-2*time.Minute || ttl > 24*time.Hour+2*time.Minute {
		t.Fatalf("invite TTL = %v, want ≈24h: %s", ttl, res)
	}

	// ---- bob's bell: ONE unread project.invite, carrying the invite TTL ----
	bobPage := itestListNotifications(t, h, bobToken, "/admin/notifications?unread=true")
	if len(bobPage.Items) != 1 {
		t.Fatalf("bob's unread page after the invite: %d items, want 1: %+v", len(bobPage.Items), bobPage.Items)
	}
	inviteNotice := bobPage.Items[0]
	if inviteNotice.App != "tasks" || inviteNotice.Type != "project.invite" {
		t.Fatalf("invite notification provenance: app=%q type=%q", inviteNotice.App, inviteNotice.Type)
	}
	if !strings.Contains(inviteNotice.Title, "itest-home") || !strings.Contains(inviteNotice.Title, "📦") {
		t.Fatalf("invite notification title: %q", inviteNotice.Title)
	}
	if !strings.Contains(inviteNotice.Body, "<b>") || !strings.Contains(inviteNotice.Body, "fzr-test") {
		t.Fatalf("invite notification body: %q", inviteNotice.Body)
	}
	invitePayload := itestPayload[struct {
		URL    string `json:"url"`
		Invite string `json:"invite"`
	}](t, "invite notification payload", inviteNotice.Payload)
	if invitePayload.URL != "/tasks" || invitePayload.Invite != invite.ID {
		t.Fatalf("invite notification payload: %s", inviteNotice.Payload)
	}
	if delta := inviteNotice.ExpiresAt.Sub(invite.ExpiresAt); delta > time.Second || delta < -time.Second {
		t.Fatalf("notification TTL %v != invite TTL %v", inviteNotice.ExpiresAt, invite.ExpiresAt)
	}
	if value := itestUnread(t, h, bobToken); value != 1 {
		t.Fatalf("bob unread-count after invite = %d, want 1", value)
	}

	// ---- bob accepts: member; the pending invite is consumed ----
	code, res = h.do(http.MethodPost, "/admin/task-invites/"+invite.ID+"/accept", "", bobToken)
	itestCode(t, "bob accepts the invite", code, http.StatusOK, res)
	accepted := itestDecode[itestInviteEnvelope](t, "bob accepts the invite", res).Invite
	if accepted.ID != invite.ID || accepted.ProjectID != project.ID {
		t.Fatalf("accepted invite shape: %s", res)
	}

	code, res = h.do(http.MethodGet, "/admin/task-projects", "", bobToken)
	itestCode(t, "bob lists projects after accept", code, http.StatusOK, res)
	bobProject, ok := itestProjectByID(
		itestDecode[itestProjects](t, "bob lists projects after accept", res).Projects, project.ID)
	if !ok || bobProject.Role != "member" {
		t.Fatalf("bob's project role after accept: %s", res)
	}

	code, res = h.do(http.MethodGet, "/admin/task-invites", "", bobToken)
	itestCode(t, "bob's invite inbox after accept", code, http.StatusOK, res)
	if inbox := itestDecode[itestInvites](t, "bob's invite inbox after accept", res).Invites; len(inbox) != 0 {
		t.Fatalf("accepted invite still pending: %s", res)
	}

	code, res = h.do(http.MethodPost, "/admin/task-invites/"+invite.ID+"/accept", "", bobToken)
	itestCode(t, "accepting twice is a 404", code, http.StatusNotFound, res)

	// ---- the roster: alice=admin, bob=member ----
	code, res = h.admin(http.MethodGet, "/admin/task-projects/"+project.ID+"/members", "")
	itestCode(t, "alice reads the roster", code, http.StatusOK, res)
	roster := itestDecode[itestMembers](t, "alice reads the roster", res).Members
	aliceMember, aliceSeen := itestMemberByID(roster, aliceID)
	bobMember, bobSeen := itestMemberByID(roster, bobID)
	if len(roster) != 2 || !aliceSeen || !bobSeen {
		t.Fatalf("roster after accept: %+v", roster)
	}
	if aliceMember.Role != "admin" || bobMember.Role != "member" {
		t.Fatalf("roster roles: alice=%q bob=%q", aliceMember.Role, bobMember.Role)
	}

	// ---- alice assigns bob: the assignee's notification ----
	code, res = h.admin(http.MethodPost, "/admin/tasks",
		`{"projectId":"`+project.ID+`","title":"itest-fix the gate","assignedTo":"`+bobID+`"}`)
	itestCode(t, "alice creates the assigned task", code, http.StatusCreated, res)
	task := itestDecode[itestTaskEnvelope](t, "alice creates the assigned task", res).Task
	if task.ID == "" || task.ProjectID != project.ID || task.Title != "itest-fix the gate" {
		t.Fatalf("created task shape: %s", res)
	}
	if task.AssignedTo == nil || *task.AssignedTo != bobID || task.Done {
		t.Fatalf("created task assignment: %s", res)
	}

	bobPage = itestListNotifications(t, h, bobToken, "/admin/notifications?limit=100")
	assignedNotice, ok := itestFindNotification(bobPage.Items,
		func(item itestNotification) bool { return item.Type == "task.assigned" })
	if !ok {
		t.Fatalf("bob has no task.assigned notification: %+v", bobPage.Items)
	}
	if assignedNotice.App != "tasks" || !strings.Contains(assignedNotice.Title, "itest-fix the gate") {
		t.Fatalf("assigned notification shape: %+v", assignedNotice)
	}
	assignedPayload := itestPayload[struct {
		URL  string `json:"url"`
		Task string `json:"task"`
	}](t, "assigned notification payload", assignedNotice.Payload)
	if assignedPayload.Task != task.ID || assignedPayload.URL != "/tasks?task="+task.ID {
		t.Fatalf("assigned notification payload: %s", assignedNotice.Payload)
	}
	if value := itestUnread(t, h, bobToken); value != 2 {
		t.Fatalf("bob unread-count after assignment = %d, want 2 (invite + assignment)", value)
	}

	// ---- bob comments: the drawer agrees with the timeline ----
	code, res = h.do(http.MethodPost, "/admin/tasks/"+task.ID+"/comments", `{"body":"itest-note"}`, bobToken)
	itestCode(t, "bob comments", code, http.StatusCreated, res)
	comment := itestDecode[struct {
		Comment itestTaskComment `json:"comment"`
	}](t, "bob comments", res).Comment
	if comment.Body != "itest-note" || comment.AuthorName == nil || *comment.AuthorName != "itest-bob" {
		t.Fatalf("comment shape: %s", res)
	}

	code, res = h.do(http.MethodGet, "/admin/tasks/"+task.ID, "", bobToken)
	itestCode(t, "bob reads the task drawer", code, http.StatusOK, res)
	detail := itestDecode[itestTaskDetail](t, "bob reads the task drawer", res)
	if len(detail.Comments) != 1 || detail.Comments[0].Body != "itest-note" {
		t.Fatalf("drawer comments: %+v", detail.Comments)
	}
	if !itestActivityHasVerb(detail.Activity, "commented") {
		t.Fatalf("drawer activity lacks the commented verb: %+v", detail.Activity)
	}

	// ---- bob toggles done ----
	code, res = h.do(http.MethodPost, "/admin/tasks/"+task.ID+"/toggle", "", bobToken)
	itestCode(t, "bob toggles the task", code, http.StatusOK, res)
	toggled := itestDecode[itestTaskEnvelope](t, "bob toggles the task", res).Task
	if !toggled.Done || toggled.DoneAt == nil {
		t.Fatalf("toggled task: %s", res)
	}

	// ---- Jakarta day math: one due-today task, one completed-this-week task ----
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatalf("load Asia/Jakarta: %v", err)
	}
	localNow := time.Now().In(jakarta)
	dueToday := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 30, 0, 0, jakarta)
	code, res = h.admin(http.MethodPost, "/admin/tasks",
		`{"projectId":"`+project.ID+`","title":"itest-due-today","dueAt":"`+dueToday.Format(time.RFC3339)+`"}`)
	itestCode(t, "alice creates the Jakarta due-today task", code, http.StatusCreated, res)
	dueTask := itestDecode[itestTaskEnvelope](t, "alice creates the Jakarta due-today task", res).Task
	if dueTask.DueAt == nil || !dueTask.DueAt.Equal(dueToday) {
		t.Fatalf("due-today task instant: %s (want %s)", res, dueToday.Format(time.RFC3339))
	}

	timezoneHeader := map[string]string{"X-Client-Timezone": "Asia/Jakarta"}
	code, res = h.doWithHeader(http.MethodGet, "/admin/task-stats", "", bobToken, timezoneHeader)
	itestCode(t, "bob reads Jakarta stats", code, http.StatusOK, res)
	stats := itestDecode[itestStatsEnvelope](t, "bob reads Jakarta stats", res).Stats
	wantStats := itestStats{Open: 1, DueToday: 1, CompletedWeek: 1, Overdue: 0}
	if stats != wantStats {
		t.Fatalf("Jakarta stats = %+v, want %+v", stats, wantStats)
	}

	code, res = h.doWithHeader(http.MethodGet, "/admin/tasks?view=today", "", bobToken, timezoneHeader)
	itestCode(t, "bob reads the today view", code, http.StatusOK, res)
	todayPage := itestDecode[itestTasks](t, "bob reads the today view", res)
	if len(todayPage.Tasks) != 1 {
		t.Fatalf("today view has %d tasks, want only the due-today one: %+v", len(todayPage.Tasks), todayPage.Tasks)
	}
	if _, ok := itestTaskByID(todayPage.Tasks, dueTask.ID); !ok {
		t.Fatalf("today view dropped the due-today task: %+v", todayPage.Tasks)
	}
	if todayPage.Stats != wantStats {
		t.Fatalf("today view stats = %+v, want %+v", todayPage.Stats, wantStats)
	}

	// ---- complete, then soft delete: the live list and the task both vanish ----
	code, res = h.admin(http.MethodPost, "/admin/task-projects/"+project.ID+"/complete", "")
	itestCode(t, "alice completes the project", code, http.StatusOK, res)
	completed := itestDecode[struct {
		OK        bool `json:"ok"`
		Completed bool `json:"completed"`
	}](t, "alice completes the project", res)
	if !completed.OK || !completed.Completed {
		t.Fatalf("complete response: %s", res)
	}

	code, res = h.admin(http.MethodGet, "/admin/task-projects", "")
	itestCode(t, "alice lists projects after complete", code, http.StatusOK, res)
	completedProject, ok := itestProjectByID(
		itestDecode[itestProjects](t, "alice lists projects after complete", res).Projects, project.ID)
	if !ok || completedProject.CompletedAt == nil {
		t.Fatalf("completedAt not set on the live project: %s", res)
	}

	code, res = h.admin(http.MethodDelete, "/admin/task-projects/"+project.ID, "")
	itestCode(t, "alice soft-deletes the project", code, http.StatusNoContent, res)

	code, res = h.admin(http.MethodGet, "/admin/task-projects", "")
	itestCode(t, "alice lists projects after delete", code, http.StatusOK, res)
	if _, ok := itestProjectByID(
		itestDecode[itestProjects](t, "alice lists projects after delete", res).Projects, project.ID); ok {
		t.Fatalf("soft-deleted project still listed: %s", res)
	}

	code, res = h.do(http.MethodGet, "/admin/tasks/"+task.ID, "", bobToken)
	itestCode(t, "task under a deleted project is a 404", code, http.StatusNotFound, res)

	// ---- the SEND door: one shared service token ----
	sendBody := `{"app":"content","to":["fzr-test"],"type":"content.published",` +
		`"title":"🔔 itest ping","body":"<b>hi</b>","payload":{"url":"/posts/x"}}`
	code, res = h.do(http.MethodPost, "/admin/notifications", sendBody, "itest-token")
	itestCode(t, "send door with the shared token", code, http.StatusCreated, res)
	if sent := itestDecode[itestSent](t, "send door with the shared token", res).Sent; sent != 1 {
		t.Fatalf("send door sent=%d, want 1: %s", sent, res)
	}

	alicePage := itestListNotifications(t, h, h.token, "/admin/notifications?limit=100")
	ping, ok := itestFindNotification(alicePage.Items,
		func(item itestNotification) bool { return item.Title == "🔔 itest ping" })
	if !ok {
		t.Fatalf("alice does not see the send-door notification: %+v", alicePage.Items)
	}
	if ping.App != "content" || ping.Type != "content.published" {
		t.Fatalf("send-door provenance: app=%q type=%q", ping.App, ping.Type)
	}
	if !strings.Contains(ping.Body, "<b>hi</b>") || ping.ReadAt != nil {
		t.Fatalf("send-door body/read state: %+v", ping)
	}
	if payload := itestPayload[struct {
		URL string `json:"url"`
	}](t, "send-door payload", ping.Payload); payload.URL != "/posts/x" {
		t.Fatalf("send-door payload: %s", ping.Payload)
	}

	code, res = h.do(http.MethodPost, "/admin/notifications", sendBody, "wrong-token")
	itestCode(t, "send door with a wrong token", code, http.StatusUnauthorized, res)

	// ---- mark one read, then read-all over a fresh row ----
	if value := itestUnread(t, h, h.token); value != 1 {
		t.Fatalf("alice unread-count after the send = %d, want 1", value)
	}
	code, res = h.do(http.MethodPost, "/admin/notifications/"+ping.ID+"/read", "", h.token)
	itestCode(t, "alice marks the ping read", code, http.StatusNoContent, res)
	if value := itestUnread(t, h, h.token); value != 0 {
		t.Fatalf("alice unread-count after mark-read = %d, want 0", value)
	}

	// A second live row proves read-all's count instead of asserting a 0.
	sendBodyTwo := `{"app":"content","to":["fzr-test"],"type":"content.published",` +
		`"title":"🔔 itest ping two","body":"<b>hi again</b>"}`
	code, res = h.do(http.MethodPost, "/admin/notifications", sendBodyTwo, "itest-token")
	itestCode(t, "second send door call", code, http.StatusCreated, res)
	if sent := itestDecode[itestSent](t, "second send door call", res).Sent; sent != 1 {
		t.Fatalf("second send sent=%d, want 1: %s", sent, res)
	}

	code, res = h.do(http.MethodPost, "/admin/notifications/read-all", "", h.token)
	itestCode(t, "alice marks all read", code, http.StatusOK, res)
	if updated := itestDecode[itestReadAll](t, "alice marks all read", res).Updated; updated != 1 {
		t.Fatalf("read-all updated=%d, want 1: %s", updated, res)
	}
	if value := itestUnread(t, h, h.token); value != 0 {
		t.Fatalf("alice unread-count after read-all = %d, want 0", value)
	}
}

// TestTasksNotificationsDenials is the denial matrix over the same real
// router. bob and carol are full-catalog accounts; only bob is a project
// member, so every carol answer must be the ONE not-found voice.
func TestTasksNotificationsDenials(t *testing.T) {
	if os.Getenv("LUMEN_INTEGRATION") == "" {
		t.Skip("LUMEN_INTEGRATION unset")
	}
	h := newAdminHarness(t)
	ctx := context.Background()

	bobID, bobToken := itestUser(t, h, "itest-bob")
	carolID, carolToken := itestUser(t, h, "itest-carol")
	var aliceID string
	if err := h.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE username = 'fzr-test'`).Scan(&aliceID); err != nil {
		t.Fatalf("read alice id: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`DELETE FROM notifications WHERE user_id IN ($1, $2)`, bobID, carolID); err != nil {
		t.Fatalf("purge stale notifications: %v", err)
	}

	// alice's project; bob joins as a plain member.
	code, res := h.admin(http.MethodPost, "/admin/task-projects", `{"name":"itest-denials"}`)
	itestCode(t, "alice creates itest-denials", code, http.StatusCreated, res)
	project := itestDecode[itestProjectEnvelope](t, "alice creates itest-denials", res).Project

	code, res = h.admin(http.MethodPost, "/admin/task-projects/"+project.ID+"/invites",
		`{"username":"itest-bob"}`)
	itestCode(t, "alice invites bob", code, http.StatusCreated, res)
	invite := itestDecode[itestInviteEnvelope](t, "alice invites bob", res).Invite
	code, res = h.do(http.MethodPost, "/admin/task-invites/"+invite.ID+"/accept", "", bobToken)
	itestCode(t, "bob accepts", code, http.StatusOK, res)

	// a task bob can see; carol cannot.
	code, res = h.admin(http.MethodPost, "/admin/tasks",
		`{"projectId":"`+project.ID+`","title":"itest-bob-task","assignedTo":"`+bobID+`"}`)
	itestCode(t, "alice creates bob's task", code, http.StatusCreated, res)
	task := itestDecode[itestTaskEnvelope](t, "alice creates bob's task", res).Task

	// ---- carol: non-member reads AND writes are the same 404 ----
	code, res = h.do(http.MethodGet, "/admin/tasks/"+task.ID, "", carolToken)
	itestCode(t, "non-member task read", code, http.StatusNotFound, res)

	code, res = h.do(http.MethodPatch, "/admin/tasks/"+task.ID, `{"title":"itest-hijacked"}`, carolToken)
	itestCode(t, "non-member task write", code, http.StatusNotFound, res)

	// The project itself is invisible to carol: creating inside it is 404,
	// not an authorization hint.
	code, res = h.do(http.MethodPost, "/admin/tasks",
		`{"projectId":"`+project.ID+`","title":"itest-intruder"}`, carolToken)
	itestCode(t, "non-member creates in a foreign project", code, http.StatusNotFound, res)

	// ---- an assignee must be a member (the actor CAN see the project) ----
	code, res = h.admin(http.MethodPost, "/admin/tasks",
		`{"projectId":"`+project.ID+`","title":"itest-carol-assignee","assignedTo":"`+carolID+`"}`)
	itestCode(t, "assigning a non-member", code, http.StatusConflict, res)
	if !strings.Contains(string(res), "conflict") {
		t.Fatalf("assigning a non-member: body %s", res)
	}

	// ---- the last admin cannot leave ----
	code, res = h.admin(http.MethodPost, "/admin/task-projects/"+project.ID+"/members/"+aliceID+"/role",
		`{"role":"member"}`)
	itestCode(t, "demoting the last admin", code, http.StatusConflict, res)

	code, res = h.admin(http.MethodDelete, "/admin/task-projects/"+project.ID+"/members/"+aliceID, "")
	itestCode(t, "removing the last admin", code, http.StatusConflict, res)

	// ---- a plain member cannot manage the project ----
	code, res = h.do(http.MethodPatch, "/admin/task-projects/"+project.ID,
		`{"name":"itest-renamed"}`, bobToken)
	itestCode(t, "member renames the project", code, http.StatusForbidden, res)

	code, res = h.do(http.MethodPost, "/admin/task-projects/"+project.ID+"/invites",
		`{"username":"itest-carol"}`, bobToken)
	itestCode(t, "member invites", code, http.StatusForbidden, res)

	// ---- an expired invite is 410 AND consumed ----
	code, res = h.admin(http.MethodPost, "/admin/task-projects", `{"name":"itest-expired-invite"}`)
	itestCode(t, "alice creates the expired-invite project", code, http.StatusCreated, res)
	expiredProject := itestDecode[itestProjectEnvelope](t, "alice creates the expired-invite project", res).Project

	var expiredInviteID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO task_project_invites (project_id, inviter_id, invitee_id, expires_at)
		VALUES ($1, $2, $3, now() - INTERVAL '1 hour')
		RETURNING id`, expiredProject.ID, aliceID, bobID).Scan(&expiredInviteID); err != nil {
		t.Fatalf("seed expired invite: %v", err)
	}

	code, res = h.do(http.MethodPost, "/admin/task-invites/"+expiredInviteID+"/accept", "", bobToken)
	itestCode(t, "accepting an expired invite", code, http.StatusGone, res)
	if !strings.Contains(string(res), "expired — ask to be re-invited") {
		t.Fatalf("expired invite copy: %s", res)
	}

	var remaining int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM task_project_invites WHERE id = $1`, expiredInviteID).Scan(&remaining); err != nil {
		t.Fatalf("count expired invite row: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expired invite row survived the 410: %d", remaining)
	}

	code, res = h.do(http.MethodPost, "/admin/task-invites/"+expiredInviteID+"/accept", "", bobToken)
	itestCode(t, "retrying the consumed invite", code, http.StatusNotFound, res)

	// ---- an expired notification is invisible to list/count/mark-read ----
	var expiredNotificationID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO notifications (user_id, app, type, title, body, payload, expires_at)
		VALUES ($1, 'tasks', 'project.invite', 'itest-expired-ping', '<b>gone</b>', '{}'::jsonb,
		        now() - INTERVAL '1 hour')
		RETURNING id`, bobID).Scan(&expiredNotificationID); err != nil {
		t.Fatalf("seed expired notification: %v", err)
	}

	unreadBefore := itestUnread(t, h, bobToken)
	bobPage := itestListNotifications(t, h, bobToken, "/admin/notifications?limit=100")
	if _, found := itestFindNotification(bobPage.Items,
		func(item itestNotification) bool { return item.Title == "itest-expired-ping" }); found {
		t.Fatalf("expired notification surfaced in the list: %+v", bobPage.Items)
	}
	if unreadAfter := itestUnread(t, h, bobToken); unreadAfter != unreadBefore {
		t.Fatalf("expired notification moved unread-count: %d → %d", unreadBefore, unreadAfter)
	}

	code, res = h.do(http.MethodPost, "/admin/notifications/"+expiredNotificationID+"/read", "", bobToken)
	itestCode(t, "marking an expired notification read", code, http.StatusNotFound, res)

	// ---- the SEND door is never mounted with an empty token ----
	t.Setenv("LUMEN_NOTIFICATION_TOKEN", "")
	offline := newAdminHarness(t) // the harness build reads the token now
	code, res = offline.do(http.MethodPost, "/admin/notifications",
		`{"app":"content","to":["fzr-test"],"type":"content.published","title":"itest-unmounted-ping","body":"<b>no</b>"}`,
		"any-bearer")
	if code == http.StatusCreated {
		t.Fatalf("unmounted send door created a notification: %d %s", code, res)
	}
	if code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Fatalf("unmounted send door = %d, want 404 or 405: %s", code, res)
	}
	var created int
	if err := offline.pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE title = 'itest-unmounted-ping'`).Scan(&created); err != nil {
		t.Fatalf("count unmounted-door rows: %v", err)
	}
	if created != 0 {
		t.Fatalf("unmounted send door wrote %d rows", created)
	}
}

// TestUserDeletePromotesProjectAdmin proves the C4 lifecycle hook through
// the real user-delete door: the project's only admin is deleted, the
// earliest remaining member is promoted, and the project survives with a
// NULL creator.
func TestUserDeletePromotesProjectAdmin(t *testing.T) {
	if os.Getenv("LUMEN_INTEGRATION") == "" {
		t.Skip("LUMEN_INTEGRATION unset")
	}
	h := newAdminHarness(t)
	ctx := context.Background()

	bobID, bobToken := itestUser(t, h, "itest-bob")
	var aliceID string
	if err := h.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE username = 'fzr-test'`).Scan(&aliceID); err != nil {
		t.Fatalf("read alice id: %v", err)
	}

	code, res := h.admin(http.MethodPost, "/admin/task-projects", `{"name":"itest-inherit"}`)
	itestCode(t, "alice creates itest-inherit", code, http.StatusCreated, res)
	project := itestDecode[itestProjectEnvelope](t, "alice creates itest-inherit", res).Project

	code, res = h.admin(http.MethodPost, "/admin/task-projects/"+project.ID+"/invites",
		`{"username":"itest-bob"}`)
	itestCode(t, "alice invites bob", code, http.StatusCreated, res)
	invite := itestDecode[itestInviteEnvelope](t, "alice invites bob", res).Invite
	code, res = h.do(http.MethodPost, "/admin/task-invites/"+invite.ID+"/accept", "", bobToken)
	itestCode(t, "bob accepts", code, http.StatusOK, res)

	code, res = h.admin(http.MethodGet, "/admin/task-projects/"+project.ID+"/members", "")
	itestCode(t, "roster before the delete", code, http.StatusOK, res)
	before := itestDecode[itestMembers](t, "roster before the delete", res).Members
	if member, ok := itestMemberByID(before, bobID); !ok || member.Role != "member" {
		t.Fatalf("bob must start as a plain member: %+v", before)
	}

	// bob holds users:manage (UpsertUser's full catalog), so he is a legal
	// actor for the delete and remains a manager afterwards.
	code, res = h.do(http.MethodDelete, "/admin/users/fzr-test", "", bobToken)
	itestCode(t, "bob deletes the project's only admin", code, http.StatusOK, res)

	code, res = h.do(http.MethodGet, "/admin/task-projects/"+project.ID+"/members", "", bobToken)
	itestCode(t, "roster after the delete", code, http.StatusOK, res)
	after := itestDecode[itestMembers](t, "roster after the delete", res).Members
	if len(after) != 1 {
		t.Fatalf("roster after the delete: %+v, want only bob", after)
	}
	promoted := after[0]
	if promoted.UserID != bobID || promoted.Username != "itest-bob" || promoted.Role != "admin" {
		t.Fatalf("C4 promotion did not make bob the sole admin: %+v", promoted)
	}

	// the project survives its creator (C4) and is still listed for bob.
	code, res = h.do(http.MethodGet, "/admin/task-projects", "", bobToken)
	itestCode(t, "bob lists projects after the delete", code, http.StatusOK, res)
	inherited, ok := itestProjectByID(
		itestDecode[itestProjects](t, "bob lists projects after the delete", res).Projects, project.ID)
	if !ok || inherited.Role != "admin" {
		t.Fatalf("project did not survive with bob as admin: %s", res)
	}
	if inherited.CreatorID != nil {
		t.Fatalf("creator_id not NULLed by the FK: %s", res)
	}
}
