// tasks.go — the tasks module's transport (M2.B). The shape mirrors
// notifications.go: handlers DECODE the request, CALL the module, MAP its
// sentinels, and ENCODE the wire. Every decision (length caps, the invite
// TTL, notification copy, membership checks) lives in internal/tasks; this
// file owns only the HTTP grammar.
//
// The mount is the scope matrix made concrete (§1.4 rule 4):
//   - reads: ANY of tasks:create|update|manage (the workspace needs one);
//   - creates: tasks:create;
//   - task edits, subtasks, comments: tasks:update;
//   - project settings, invites, members, roles: tasks:manage.
//
// The project ROLE (admin|member) is the second key, enforced by the store in
// the same queries — the scope gate opens the workspace, membership opens the
// room, and the project admin bit opens the settings.
//
// Time-of-day math (today/upcoming, the stats cards) reads the client's IANA
// zone from X-Client-Timezone; an absent or malformed value falls back to
// UTC, never a 400 (C5: the header is an optimization for correctness, not a
// required credential).
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/httpx"
	"github.com/kanatabi/lumen/internal/scopes"
	"github.com/kanatabi/lumen/internal/tasks"
)

const (
	// taskBodyMax caps every tasks request body. The largest legal payload
	// is a task create with 16 KiB of notes plus JSON escaping, so 64 KiB
	// is comfortably above the validated maximum and still bounded.
	taskBodyMax = 64 << 10

	// TaskRouteCount is the number of routes mountTasks registers. The
	// composition root logs it so an operator can see the whole module is
	// on; tasks_test.go walks the mount and keeps this number honest.
	TaskRouteCount = 25
)

// tasksService is the module surface this package needs. It is an interface
// so the handler tests can prove the wire contract and the scope gates
// without a database, and so the composition root stays the only place the
// real Service is named.
type tasksService interface {
	CreateProject(ctx context.Context, principal tasks.Principal, name string) (tasks.Project, error)
	ListProjects(ctx context.Context, principal tasks.Principal) ([]tasks.Project, error)
	RenameProject(ctx context.Context, principal tasks.Principal, projectID, name string) error
	SetProjectCompleted(ctx context.Context, principal tasks.Principal, projectID string, completed bool) error
	SoftDeleteProject(ctx context.Context, principal tasks.Principal, projectID string) error

	ListMembers(ctx context.Context, principal tasks.Principal, projectID string) ([]tasks.Member, error)
	SetMemberRole(ctx context.Context, principal tasks.Principal, projectID, targetID, role string) error
	RemoveMember(ctx context.Context, principal tasks.Principal, projectID, targetID string) error

	CreateInvite(ctx context.Context, principal tasks.Principal, projectID, username string) (tasks.Invite, error)
	ListInvites(ctx context.Context, principal tasks.Principal) ([]tasks.Invite, error)
	AcceptInvite(ctx context.Context, principal tasks.Principal, inviteID string) (tasks.Invite, error)
	DeclineInvite(ctx context.Context, principal tasks.Principal, inviteID string) error
	RevokeInvite(ctx context.Context, principal tasks.Principal, projectID, inviteID string) error

	CreateTask(ctx context.Context, principal tasks.Principal, projectID string, input tasks.TaskInput) (tasks.Task, error)
	ListTasks(ctx context.Context, principal tasks.Principal, view, projectID string, location *time.Location) ([]tasks.Task, error)
	GetTask(ctx context.Context, principal tasks.Principal, taskID string) (tasks.TaskDetail, error)
	UpdateTask(ctx context.Context, principal tasks.Principal, taskID string, patch tasks.TaskPatch) (tasks.Task, error)
	ToggleTask(ctx context.Context, principal tasks.Principal, taskID string) (tasks.Task, error)
	SoftDeleteTask(ctx context.Context, principal tasks.Principal, taskID string) error

	AddSubtask(ctx context.Context, principal tasks.Principal, taskID, title string) (tasks.Subtask, error)
	UpdateSubtask(ctx context.Context, principal tasks.Principal, taskID, subtaskID string, patch tasks.SubtaskPatch) (tasks.Subtask, error)
	DeleteSubtask(ctx context.Context, principal tasks.Principal, taskID, subtaskID string) error

	AddComment(ctx context.Context, principal tasks.Principal, taskID, body string) (tasks.Comment, error)
	DeleteComment(ctx context.Context, principal tasks.Principal, taskID, commentID string) error

	Stats(ctx context.Context, principal tasks.Principal, location *time.Location) (tasks.Stats, error)
}

// ---- wire shapes (camelCase; mapped explicitly, never marshalled from the
// module's structs — the module may change its types without changing the
// contract) ----

type taskProject struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Role        string     `json:"role"`
	CreatorID   *string    `json:"creatorId"`
	CompletedAt *time.Time `json:"completedAt"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	TaskCount   int        `json:"taskCount"`
	OpenCount   int        `json:"openCount"`
}

type taskMember struct {
	UserID    string    `json:"userId"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	InvitedBy *string   `json:"invitedBy"`
	JoinedAt  time.Time `json:"joinedAt"`
}

type taskInvite struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"projectId"`
	ProjectName string    `json:"projectName"`
	InviterID   string    `json:"inviterId"`
	InviterName string    `json:"inviterName"`
	InviteeID   string    `json:"inviteeId"`
	ExpiresAt   time.Time `json:"expiresAt"`
	CreatedAt   time.Time `json:"createdAt"`
}

type taskItem struct {
	ID           string     `json:"id"`
	ProjectID    string     `json:"projectId"`
	ProjectName  string     `json:"projectName"`
	CreatorID    *string    `json:"creatorId"`
	CreatorName  *string    `json:"creatorName"`
	AssignedTo   *string    `json:"assignedTo"`
	AssigneeName *string    `json:"assigneeName"`
	Title        string     `json:"title"`
	Notes        string     `json:"notes"`
	Priority     string     `json:"priority"`
	DueAt        *time.Time `json:"dueAt"`
	Done         bool       `json:"done"`
	DoneAt       *time.Time `json:"doneAt"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	CommentCount int        `json:"commentCount"`
	SubtaskCount int        `json:"subtaskCount"`
	SubtaskDone  int        `json:"subtaskDone"`
}

type taskSubtask struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"taskId"`
	Title     string    `json:"title"`
	Done      bool      `json:"done"`
	CreatedAt time.Time `json:"createdAt"`
}

type taskComment struct {
	ID         string    `json:"id"`
	TaskID     string    `json:"taskId"`
	AuthorID   *string   `json:"authorId"`
	AuthorName *string   `json:"authorName"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"createdAt"`
}

type taskActivity struct {
	ID        int64     `json:"id"`
	TaskID    string    `json:"taskId"`
	ActorID   *string   `json:"actorId"`
	ActorName *string   `json:"actorName"`
	Verb      string    `json:"verb"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"createdAt"`
}

type taskDetail struct {
	Task     taskItem       `json:"task"`
	Subtasks []taskSubtask  `json:"subtasks"`
	Comments []taskComment  `json:"comments"`
	Activity []taskActivity `json:"activity"`
}

type taskStats struct {
	Open          int `json:"open"`
	DueToday      int `json:"dueToday"`
	CompletedWeek int `json:"completedWeek"`
	Overdue       int `json:"overdue"`
}

func projectWire(project tasks.Project) taskProject {
	return taskProject{
		ID: project.ID, Name: project.Name, Role: project.Role,
		CreatorID: project.CreatorID, CompletedAt: project.CompletedAt,
		CreatedAt: project.CreatedAt, UpdatedAt: project.UpdatedAt,
		TaskCount: project.TaskCount, OpenCount: project.OpenCount,
	}
}

func memberWire(member tasks.Member) taskMember {
	return taskMember{
		UserID: member.UserID, Username: member.Username, Role: member.Role,
		InvitedBy: member.InvitedBy, JoinedAt: member.JoinedAt,
	}
}

func inviteWire(invite tasks.Invite) taskInvite {
	return taskInvite{
		ID: invite.ID, ProjectID: invite.ProjectID, ProjectName: invite.ProjectName,
		InviterID: invite.InviterID, InviterName: invite.InviterName,
		InviteeID: invite.InviteeID, ExpiresAt: invite.ExpiresAt, CreatedAt: invite.CreatedAt,
	}
}

func taskWire(task tasks.Task) taskItem {
	return taskItem{
		ID: task.ID, ProjectID: task.ProjectID, ProjectName: task.ProjectName,
		CreatorID: task.CreatedBy, CreatorName: task.CreatorName,
		AssignedTo: task.AssignedTo, AssigneeName: task.AssigneeName,
		Title: task.Title, Notes: task.Notes, Priority: task.Priority,
		DueAt: task.DueAt, Done: task.Done, DoneAt: task.DoneAt,
		CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
		CommentCount: task.CommentCount, SubtaskCount: task.SubtaskCount,
		SubtaskDone: task.SubtaskDone,
	}
}

func subtaskWire(subtask tasks.Subtask) taskSubtask {
	return taskSubtask{
		ID: subtask.ID, TaskID: subtask.TaskID, Title: subtask.Title,
		Done: subtask.Done, CreatedAt: subtask.CreatedAt,
	}
}

func commentWire(comment tasks.Comment) taskComment {
	return taskComment{
		ID: comment.ID, TaskID: comment.TaskID,
		AuthorID: comment.AuthorID, AuthorName: comment.AuthorName,
		Body: comment.Body, CreatedAt: comment.CreatedAt,
	}
}

func activityWire(activity tasks.Activity) taskActivity {
	return taskActivity{
		ID: activity.ID, TaskID: activity.TaskID,
		ActorID: activity.ActorID, ActorName: activity.ActorName,
		Verb: activity.Verb, Detail: activity.Detail, CreatedAt: activity.CreatedAt,
	}
}

func statsWire(stats tasks.Stats) taskStats {
	return taskStats{
		Open: stats.Open, DueToday: stats.DueToday,
		CompletedWeek: stats.CompletedWeek, Overdue: stats.Overdue,
	}
}

// ---- mount ----

// mountTasks registers the module's routes on an already-authenticated router
// (Mount wraps this in localAuthenticator). Scope gates live on the groups so
// the matrix (§1.4 rule 4) is readable next to the routes it protects.
func (h *Handler) mountTasks(a chi.Router) {
	a.Group(func(read chi.Router) {
		read.Use(h.requireAnyScope(scopes.TasksCreate, scopes.TasksUpdate, scopes.TasksManage))
		read.Get("/task-projects", h.taskProjectList)
		read.Get("/task-projects/{id}/members", h.taskMemberList)
		read.Get("/task-invites", h.taskInviteList)
		read.Get("/tasks", h.taskList)
		read.Get("/tasks/{id}", h.taskGet)
		read.Get("/task-stats", h.taskStats)
	})
	a.Group(func(create chi.Router) {
		create.Use(h.requireScope(scopes.TasksCreate))
		create.Post("/task-projects", h.taskProjectCreate)
		create.Post("/tasks", h.taskCreate)
	})
	a.Group(func(update chi.Router) {
		update.Use(h.requireScope(scopes.TasksUpdate))
		update.Patch("/tasks/{id}", h.taskUpdate)
		update.Post("/tasks/{id}/toggle", h.taskToggle)
		update.Delete("/tasks/{id}", h.taskDelete)
		update.Post("/tasks/{id}/subtasks", h.taskSubtaskCreate)
		update.Patch("/tasks/{id}/subtasks/{sid}", h.taskSubtaskUpdate)
		update.Delete("/tasks/{id}/subtasks/{sid}", h.taskSubtaskDelete)
		update.Post("/tasks/{id}/comments", h.taskCommentCreate)
		update.Delete("/tasks/{id}/comments/{cid}", h.taskCommentDelete)
	})
	a.Group(func(manage chi.Router) {
		manage.Use(h.requireScope(scopes.TasksManage))
		manage.Patch("/task-projects/{id}", h.taskProjectRename)
		manage.Delete("/task-projects/{id}", h.taskProjectDelete)
		manage.Post("/task-projects/{id}/complete", h.taskProjectComplete)
		manage.Post("/task-projects/{id}/invites", h.taskInviteCreate)
		manage.Delete("/task-projects/{id}/invites/{iid}", h.taskInviteRevoke)
		manage.Delete("/task-projects/{id}/members/{user}", h.taskMemberRemove)
		manage.Post("/task-projects/{id}/members/{user}/role", h.taskMemberRole)
	})
	// Accept/decline are the INVITEE's verbs: any of the three scopes, and
	// the store's invitee-id WHERE is the second key (another user's invite
	// is the same not-found).
	a.Group(func(inbox chi.Router) {
		inbox.Use(h.requireAnyScope(scopes.TasksCreate, scopes.TasksUpdate, scopes.TasksManage))
		inbox.Post("/task-invites/{id}/accept", h.taskInviteAccept)
		inbox.Post("/task-invites/{id}/decline", h.taskInviteDecline)
	})
}

// ---- helpers ----

// tasksPrincipal pulls the authenticated principal into the module's own
// value type. The local authenticator already rejected a missing principal;
// the defensive 401 keeps a direct unit call honest.
func (h *Handler) tasksPrincipal(w http.ResponseWriter, r *http.Request) (tasks.Principal, bool) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return tasks.Principal{}, false
	}
	return tasks.Principal{ID: principal.ID, Username: principal.Username}, true
}

// taskFail maps the tasks sentinels to the one status table the FE relies on.
func (h *Handler) taskFail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, tasks.ErrInvalid), errors.Is(err, tasks.ErrInvalidView):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, tasks.ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, tasks.ErrConflict):
		httpx.WriteErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, tasks.ErrExpired):
		// The plan's wire sketch fixes this copy: the FE renders it verbatim,
		// so the sentence names the recovery step, not just the status.
		httpx.WriteErr(w, http.StatusGone, "this invite expired — ask to be re-invited")
	case errors.Is(err, tasks.ErrForbidden):
		httpx.WriteErr(w, http.StatusForbidden, err.Error())
	default:
		httpx.Fail(w, r, err)
	}
}

// decodeTaskBody decodes one bounded JSON body. Handlers use it for every
// request shape; a malformed body is always the caller's 400.
func decodeTaskBody(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskBodyMax)).Decode(target); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// clientLocation reads X-Client-Timezone as an IANA name. Empty or malformed
// is UTC: the header shapes day boundaries, it does not authorize anything,
// so a wrong value must never take the workspace down (C5).
func clientLocation(r *http.Request) *time.Location {
	name := r.Header.Get("X-Client-Timezone")
	if name == "" {
		return time.UTC
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return location
}

// ---- projects ----

func (h *Handler) taskProjectList(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	projects, err := h.tasks.ListProjects(r.Context(), principal)
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	out := make([]taskProject, len(projects))
	for index, project := range projects {
		out[index] = projectWire(project)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"projects": out})
}

func (h *Handler) taskProjectCreate(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !decodeTaskBody(w, r, &in) {
		return
	}
	project, err := h.tasks.CreateProject(r.Context(), principal, in.Name)
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"project": projectWire(project)})
}

func (h *Handler) taskProjectRename(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !decodeTaskBody(w, r, &in) {
		return
	}
	if err := h.tasks.RenameProject(r.Context(), principal, chi.URLParam(r, "id"), in.Name); err != nil {
		h.taskFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) taskProjectDelete(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	if err := h.tasks.SoftDeleteProject(r.Context(), principal, chi.URLParam(r, "id")); err != nil {
		h.taskFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// taskProjectComplete is POST …/complete: the body's `completed` defaults to
// true (the verb's name), and `false` reopens — one route for both flag
// states, so the FE never needs a second verb. An empty body is the plain
// "complete" call; only a real payload is decoded.
func (h *Handler) taskProjectComplete(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	var in struct {
		Completed *bool `json:"completed"`
	}
	// An EMPTY body means "complete": tolerate io.EOF so a chunked request
	// without a payload behaves like ContentLength 0 (the FE sends no body
	// for the plain verb).
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskBodyMax)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	completed := true
	if in.Completed != nil {
		completed = *in.Completed
	}
	if err := h.tasks.SetProjectCompleted(r.Context(), principal, chi.URLParam(r, "id"), completed); err != nil {
		h.taskFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "completed": completed})
}

// ---- members ----

func (h *Handler) taskMemberList(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	members, err := h.tasks.ListMembers(r.Context(), principal, chi.URLParam(r, "id"))
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	out := make([]taskMember, len(members))
	for index, member := range members {
		out[index] = memberWire(member)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"members": out})
}

func (h *Handler) taskMemberRole(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	var in struct {
		Role string `json:"role"`
	}
	if !decodeTaskBody(w, r, &in) {
		return
	}
	if err := h.tasks.SetMemberRole(r.Context(), principal,
		chi.URLParam(r, "id"), chi.URLParam(r, "user"), in.Role); err != nil {
		h.taskFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) taskMemberRemove(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	if err := h.tasks.RemoveMember(r.Context(), principal,
		chi.URLParam(r, "id"), chi.URLParam(r, "user")); err != nil {
		h.taskFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- invites ----

func (h *Handler) taskInviteList(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	invites, err := h.tasks.ListInvites(r.Context(), principal)
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	out := make([]taskInvite, len(invites))
	for index, invite := range invites {
		out[index] = inviteWire(invite)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"invites": out})
}

func (h *Handler) taskInviteCreate(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	var in struct {
		Username string `json:"username"`
	}
	if !decodeTaskBody(w, r, &in) {
		return
	}
	invite, err := h.tasks.CreateInvite(r.Context(), principal, chi.URLParam(r, "id"), in.Username)
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"invite": inviteWire(invite)})
}

func (h *Handler) taskInviteAccept(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	invite, err := h.tasks.AcceptInvite(r.Context(), principal, chi.URLParam(r, "id"))
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"invite": inviteWire(invite)})
}

func (h *Handler) taskInviteDecline(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	if err := h.tasks.DeclineInvite(r.Context(), principal, chi.URLParam(r, "id")); err != nil {
		h.taskFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) taskInviteRevoke(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	if err := h.tasks.RevokeInvite(r.Context(), principal,
		chi.URLParam(r, "id"), chi.URLParam(r, "iid")); err != nil {
		h.taskFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- tasks ----

// taskList is GET /admin/tasks — the grouped workspace page (§2.6): the
// caller's projects, the tasks for the requested view, and the stats cards,
// all in the caller's timezone. One response means the three surfaces cannot
// disagree about which day "today" is.
func (h *Handler) taskList(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	view := r.URL.Query().Get("view")
	if view == "" {
		view = tasks.ViewAll
	}
	projectID := r.URL.Query().Get("project")
	location := clientLocation(r)

	projects, err := h.tasks.ListProjects(r.Context(), principal)
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	list, err := h.tasks.ListTasks(r.Context(), principal, view, projectID, location)
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	stats, err := h.tasks.Stats(r.Context(), principal, location)
	if err != nil {
		h.taskFail(w, r, err)
		return
	}

	projectOut := make([]taskProject, len(projects))
	for index, project := range projects {
		projectOut[index] = projectWire(project)
	}
	taskOut := make([]taskItem, len(list))
	for index, task := range list {
		taskOut[index] = taskWire(task)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"projects": projectOut,
		"tasks":    taskOut,
		"stats":    statsWire(stats),
	})
}

func (h *Handler) taskCreate(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	var in struct {
		ProjectID  string  `json:"projectId"`
		Title      string  `json:"title"`
		Notes      string  `json:"notes"`
		Priority   string  `json:"priority"`
		DueAt      *string `json:"dueAt"`
		AssignedTo *string `json:"assignedTo"`
	}
	if !decodeTaskBody(w, r, &in) {
		return
	}
	dueAt, ok := parseDueAt(w, in.DueAt)
	if !ok {
		return
	}
	task, err := h.tasks.CreateTask(r.Context(), principal, in.ProjectID, tasks.TaskInput{
		Title:      in.Title,
		Notes:      in.Notes,
		Priority:   in.Priority,
		DueAt:      dueAt,
		AssignedTo: in.AssignedTo,
	})
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"task": taskWire(task)})
}

func (h *Handler) taskGet(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	detail, err := h.tasks.GetTask(r.Context(), principal, chi.URLParam(r, "id"))
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	out := taskDetail{Task: taskWire(detail.Task)}
	out.Subtasks = make([]taskSubtask, len(detail.Subtasks))
	for index, subtask := range detail.Subtasks {
		out.Subtasks[index] = subtaskWire(subtask)
	}
	out.Comments = make([]taskComment, len(detail.Comments))
	for index, comment := range detail.Comments {
		out.Comments[index] = commentWire(comment)
	}
	out.Activity = make([]taskActivity, len(detail.Activity))
	for index, activity := range detail.Activity {
		out.Activity[index] = activityWire(activity)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) taskUpdate(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	patch, ok := decodeTaskPatch(w, r)
	if !ok {
		return
	}
	task, err := h.tasks.UpdateTask(r.Context(), principal, chi.URLParam(r, "id"), patch)
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"task": taskWire(task)})
}

func (h *Handler) taskToggle(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	task, err := h.tasks.ToggleTask(r.Context(), principal, chi.URLParam(r, "id"))
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"task": taskWire(task)})
}

func (h *Handler) taskDelete(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	if err := h.tasks.SoftDeleteTask(r.Context(), principal, chi.URLParam(r, "id")); err != nil {
		h.taskFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- subtasks ----

func (h *Handler) taskSubtaskCreate(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	var in struct {
		Title string `json:"title"`
	}
	if !decodeTaskBody(w, r, &in) {
		return
	}
	subtask, err := h.tasks.AddSubtask(r.Context(), principal, chi.URLParam(r, "id"), in.Title)
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"subtask": subtaskWire(subtask)})
}

func (h *Handler) taskSubtaskUpdate(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	var in struct {
		Title *string `json:"title"`
		Done  *bool   `json:"done"`
	}
	if !decodeTaskBody(w, r, &in) {
		return
	}
	patch := tasks.SubtaskPatch{}
	if in.Title != nil {
		patch.Title = tasks.Some(*in.Title)
	}
	if in.Done != nil {
		patch.Done = tasks.Some(*in.Done)
	}
	subtask, err := h.tasks.UpdateSubtask(r.Context(), principal,
		chi.URLParam(r, "id"), chi.URLParam(r, "sid"), patch)
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"subtask": subtaskWire(subtask)})
}

func (h *Handler) taskSubtaskDelete(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	if err := h.tasks.DeleteSubtask(r.Context(), principal,
		chi.URLParam(r, "id"), chi.URLParam(r, "sid")); err != nil {
		h.taskFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- comments ----

func (h *Handler) taskCommentCreate(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if !decodeTaskBody(w, r, &in) {
		return
	}
	comment, err := h.tasks.AddComment(r.Context(), principal, chi.URLParam(r, "id"), in.Body)
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"comment": commentWire(comment)})
}

func (h *Handler) taskCommentDelete(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	if err := h.tasks.DeleteComment(r.Context(), principal,
		chi.URLParam(r, "id"), chi.URLParam(r, "cid")); err != nil {
		h.taskFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- stats ----

func (h *Handler) taskStats(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.tasksPrincipal(w, r)
	if !ok {
		return
	}
	stats, err := h.tasks.Stats(r.Context(), principal, clientLocation(r))
	if err != nil {
		h.taskFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"stats": statsWire(stats)})
}

// ---- PATCH field-presence decoding ----

// decodeTaskPatch is the ONLY place the tasks PATCH's three-way semantics
// live: an ABSENT key leaves the Optional untouched, JSON null marks it
// present-with-nil (clear the due date / unassign), and any other value marks
// it present-with-a-value. A nil-ness-only struct could not tell "clear" from
// "leave alone", which is why the module's patch type carries Optional.
func decodeTaskPatch(w http.ResponseWriter, r *http.Request) (tasks.TaskPatch, bool) {
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskBodyMax)).Decode(&fields); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "invalid JSON body")
		return tasks.TaskPatch{}, false
	}
	var patch tasks.TaskPatch
	for field, raw := range fields {
		switch field {
		case "title":
			value, present, ok := decodePatchString(w, field, raw)
			if !ok {
				return tasks.TaskPatch{}, false
			}
			if present {
				patch.Title = tasks.Some(*value)
			} else {
				patch.Title = tasks.Clear[string]()
			}
		case "notes":
			value, present, ok := decodePatchString(w, field, raw)
			if !ok {
				return tasks.TaskPatch{}, false
			}
			if present {
				patch.Notes = tasks.Some(*value)
			} else {
				patch.Notes = tasks.Clear[string]()
			}
		case "priority":
			value, present, ok := decodePatchString(w, field, raw)
			if !ok {
				return tasks.TaskPatch{}, false
			}
			if present {
				patch.Priority = tasks.Some(*value)
			} else {
				patch.Priority = tasks.Clear[string]()
			}
		case "dueAt":
			if isJSONNull(raw) {
				patch.DueAt = tasks.Clear[*time.Time]()
				continue
			}
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				httpx.WriteErr(w, http.StatusBadRequest, "dueAt must be an RFC 3339 timestamp or null")
				return tasks.TaskPatch{}, false
			}
			parsed, err := time.Parse(time.RFC3339, text)
			if err != nil {
				httpx.WriteErr(w, http.StatusBadRequest, "dueAt must be an RFC 3339 timestamp or null")
				return tasks.TaskPatch{}, false
			}
			utc := parsed.UTC()
			patch.DueAt = tasks.Some(&utc)
		case "assignedTo":
			value, present, ok := decodePatchString(w, field, raw)
			if !ok {
				return tasks.TaskPatch{}, false
			}
			if present {
				patch.AssignedTo = tasks.Some(value)
			} else {
				patch.AssignedTo = tasks.Clear[*string]()
			}
		default:
			// A typo must not silently do nothing: the caller gets an
			// honest 400 naming the field.
			httpx.WriteErr(w, http.StatusBadRequest, "unknown field "+field)
			return tasks.TaskPatch{}, false
		}
	}
	return patch, true
}

// decodePatchString distinguishes absent (the caller never reaches here —
// map iteration only sees present keys), null (present=false) and a value
// (present=true). A non-string is a malformed field type: 400.
func decodePatchString(w http.ResponseWriter, field string, raw json.RawMessage) (value *string, present, ok bool) {
	if isJSONNull(raw) {
		return nil, false, true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, field+" must be a string or null")
		return nil, false, false
	}
	return &text, true, true
}

// isJSONNull recognizes the literal null after whitespace trimming.
func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// parseDueAt parses the create path's optional timestamp. An absent field or
// an empty string means "no due date"; anything else must be RFC 3339.
func parseDueAt(w http.ResponseWriter, raw *string) (*time.Time, bool) {
	if raw == nil || *raw == "" {
		return nil, true
	}
	parsed, err := time.Parse(time.RFC3339, *raw)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "dueAt must be an RFC 3339 timestamp")
		return nil, false
	}
	utc := parsed.UTC()
	return &utc, true
}
