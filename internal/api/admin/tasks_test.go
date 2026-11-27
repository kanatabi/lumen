// tasks_test.go — the tasks transport, offline: the sentinel→status table,
// the PATCH's absent/null/value semantics, the timezone fallback, the wire
// shape, and the scope matrix (which route needs which tasks:* verb). The
// real Service is exercised against Postgres in internal/tasks; here a fake
// isolates THIS package's obligations (decode/call/map/encode and the gates).
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/scopes"
	"github.com/kanatabi/lumen/internal/tasks"
)

// fakeTasks records what the handler asked for and returns the test's chosen
// verdict. Every method is deliberately trivial: the value under test is the
// handler's contract, never a second service.
type fakeTasks struct {
	projects     []tasks.Project
	projectsErr  error
	projectsCall int

	listTasks     []tasks.Task
	listTasksErr  error
	listTasksView string
	listProjectID string
	listTasksLoc  *time.Location

	stats    tasks.Stats
	statsErr error
	statsLoc *time.Location

	updatePatch tasks.TaskPatch
	updateErr   error

	createTask      tasks.Task
	createTaskInput tasks.TaskInput
	createTaskErr   error

	projectName string

	createProjectErr error
	renameErr        error
	completeErr      error
	deleteProjectErr error

	members         []tasks.Member
	membersErr      error
	people          []tasks.InviteeCandidate
	peopleErr       error
	peopleQuery     string
	roleErr         error
	removeMemberErr error

	createInvite    tasks.Invite
	createInviteErr error
	invites         []tasks.Invite
	invitesErr      error
	acceptInvite    tasks.Invite
	acceptErr       error
	declineErr      error
	revokeErr       error

	getTask       tasks.TaskDetail
	getTaskErr    error
	toggleTask    tasks.Task
	toggleErr     error
	deleteTaskErr error

	subtask          tasks.Subtask
	subtaskErr       error
	updatedSubtask   tasks.Subtask
	deleteSubtaskErr error

	comment          tasks.Comment
	commentErr       error
	deleteCommentErr error
}

func (f *fakeTasks) CreateProject(_ context.Context, _ tasks.Principal, name string) (tasks.Project, error) {
	f.projectName = name
	if f.createProjectErr != nil {
		return tasks.Project{}, f.createProjectErr
	}
	return tasks.Project{ID: "project-1", Name: name, Role: "admin"}, nil
}

func (f *fakeTasks) ListProjects(_ context.Context, _ tasks.Principal) ([]tasks.Project, error) {
	f.projectsCall++
	if f.projectsErr != nil {
		return nil, f.projectsErr
	}
	return f.projects, nil
}

func (f *fakeTasks) RenameProject(_ context.Context, _ tasks.Principal, _ string, name string) error {
	f.projectName = name
	return f.renameErr
}

func (f *fakeTasks) SetProjectCompleted(_ context.Context, _ tasks.Principal, _ string, _ bool) error {
	return f.completeErr
}

func (f *fakeTasks) SoftDeleteProject(_ context.Context, _ tasks.Principal, _ string) error {
	return f.deleteProjectErr
}

func (f *fakeTasks) ListMembers(_ context.Context, _ tasks.Principal, _ string) ([]tasks.Member, error) {
	if f.membersErr != nil {
		return nil, f.membersErr
	}
	return f.members, nil
}

func (f *fakeTasks) SearchInvitees(_ context.Context, _ tasks.Principal, _ string, query string) ([]tasks.InviteeCandidate, error) {
	f.peopleQuery = query
	if f.peopleErr != nil {
		return nil, f.peopleErr
	}
	return f.people, nil
}

func (f *fakeTasks) SetMemberRole(_ context.Context, _ tasks.Principal, _, _, _ string) error {
	return f.roleErr
}

func (f *fakeTasks) RemoveMember(_ context.Context, _ tasks.Principal, _, _ string) error {
	return f.removeMemberErr
}

func (f *fakeTasks) CreateInvite(_ context.Context, _ tasks.Principal, _, _ string) (tasks.Invite, error) {
	if f.createInviteErr != nil {
		return tasks.Invite{}, f.createInviteErr
	}
	return f.createInvite, nil
}

func (f *fakeTasks) ListInvites(_ context.Context, _ tasks.Principal) ([]tasks.Invite, error) {
	if f.invitesErr != nil {
		return nil, f.invitesErr
	}
	return f.invites, nil
}

func (f *fakeTasks) AcceptInvite(_ context.Context, _ tasks.Principal, _ string) (tasks.Invite, error) {
	if f.acceptErr != nil {
		return tasks.Invite{}, f.acceptErr
	}
	return f.acceptInvite, nil
}

func (f *fakeTasks) DeclineInvite(_ context.Context, _ tasks.Principal, _ string) error {
	return f.declineErr
}

func (f *fakeTasks) RevokeInvite(_ context.Context, _ tasks.Principal, _, _ string) error {
	return f.revokeErr
}

func (f *fakeTasks) CreateTask(_ context.Context, _ tasks.Principal, _ string, input tasks.TaskInput) (tasks.Task, error) {
	f.createTaskInput = input
	if f.createTaskErr != nil {
		return tasks.Task{}, f.createTaskErr
	}
	if f.createTask.ID != "" {
		return f.createTask, nil
	}
	return tasks.Task{ID: "task-1", Title: input.Title}, nil
}

func (f *fakeTasks) ListTasks(_ context.Context, _ tasks.Principal, view, projectID string, loc *time.Location) ([]tasks.Task, error) {
	f.listTasksView = view
	f.listProjectID = projectID
	f.listTasksLoc = loc
	if f.listTasksErr != nil {
		return nil, f.listTasksErr
	}
	return f.listTasks, nil
}

func (f *fakeTasks) GetTask(_ context.Context, _ tasks.Principal, _ string) (tasks.TaskDetail, error) {
	if f.getTaskErr != nil {
		return tasks.TaskDetail{}, f.getTaskErr
	}
	return f.getTask, nil
}

func (f *fakeTasks) UpdateTask(_ context.Context, _ tasks.Principal, taskID string, patch tasks.TaskPatch) (tasks.Task, error) {
	f.updatePatch = patch
	if f.updateErr != nil {
		return tasks.Task{}, f.updateErr
	}
	return tasks.Task{ID: taskID}, nil
}

func (f *fakeTasks) ToggleTask(_ context.Context, _ tasks.Principal, taskID string) (tasks.Task, error) {
	if f.toggleErr != nil {
		return tasks.Task{}, f.toggleErr
	}
	return tasks.Task{ID: taskID}, nil
}

func (f *fakeTasks) SoftDeleteTask(_ context.Context, _ tasks.Principal, _ string) error {
	return f.deleteTaskErr
}

func (f *fakeTasks) AddSubtask(_ context.Context, _ tasks.Principal, _, title string) (tasks.Subtask, error) {
	if f.subtaskErr != nil {
		return tasks.Subtask{}, f.subtaskErr
	}
	return tasks.Subtask{ID: "subtask-1", Title: title}, nil
}

func (f *fakeTasks) UpdateSubtask(_ context.Context, _ tasks.Principal, _, _ string, _ tasks.SubtaskPatch) (tasks.Subtask, error) {
	if f.subtaskErr != nil {
		return tasks.Subtask{}, f.subtaskErr
	}
	return f.updatedSubtask, nil
}

func (f *fakeTasks) DeleteSubtask(_ context.Context, _ tasks.Principal, _, _ string) error {
	return f.deleteSubtaskErr
}

func (f *fakeTasks) AddComment(_ context.Context, _ tasks.Principal, _, body string) (tasks.Comment, error) {
	if f.commentErr != nil {
		return tasks.Comment{}, f.commentErr
	}
	return tasks.Comment{ID: "comment-1", Body: body}, nil
}

func (f *fakeTasks) DeleteComment(_ context.Context, _ tasks.Principal, _, _ string) error {
	return f.deleteCommentErr
}

func (f *fakeTasks) Stats(_ context.Context, _ tasks.Principal, loc *time.Location) (tasks.Stats, error) {
	f.statsLoc = loc
	if f.statsErr != nil {
		return tasks.Stats{}, f.statsErr
	}
	return f.stats, nil
}

// taskTestHandler builds a handler with the fake enabled but WITHOUT the
// local authenticator: principals are injected at the ctx key exactly where
// the authenticator puts them (the gate itself is proven separately).
func taskTestHandler(fake *fakeTasks) *Handler {
	handler := New(nil)
	handler.EnableTasks(fake)
	return handler
}

// taskRequest injects the principal and its scopes.
func taskRequest(method, target, body string, scopeList ...string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	return request.WithContext(context.WithValue(request.Context(), principalKey,
		adminsvc.Principal{ID: "user-1", Username: "alice", Scopes: scopeList}))
}

// taskScopeRouter mounts the REAL tasks groups behind a principal injector,
// so the route→scope mapping is exercised without the session mint.
func taskScopeRouter(handler *Handler, scopeList ...string) http.Handler {
	router := chi.NewRouter()
	router.Route("/admin", func(adminRouter chi.Router) {
		adminRouter.Group(func(taskRoutes chi.Router) {
			taskRoutes.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ctx := context.WithValue(r.Context(), principalKey, adminsvc.Principal{
						ID: "user-1", Username: "alice", Scopes: scopeList,
					})
					next.ServeHTTP(w, r.WithContext(ctx))
				})
			})
			handler.mountTasks(taskRoutes)
		})
	})
	return router
}

// TestTaskSentinelMap pins the one status table every tasks route shares.
func TestTaskSentinelMap(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		err      error
		wantCode int
	}{
		{"invalid input → 400", tasks.ErrInvalid, http.StatusBadRequest},
		{"invalid view → 400", tasks.ErrInvalidView, http.StatusBadRequest},
		{"wrapped invalid input → 400", fmt.Errorf("%w: title is required", tasks.ErrInvalid), http.StatusBadRequest},
		{"not found → 404", tasks.ErrNotFound, http.StatusNotFound},
		{"conflict → 409", tasks.ErrConflict, http.StatusConflict},
		{"expired invite → 410", tasks.ErrExpired, http.StatusGone},
		{"forbidden → 403", tasks.ErrForbidden, http.StatusForbidden},
		{"unmapped → 500", errors.New("database down"), http.StatusInternalServerError},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fake := &fakeTasks{projectsErr: testCase.err}
			recorder := httptest.NewRecorder()
			taskTestHandler(fake).taskProjectList(recorder,
				taskRequest(http.MethodGet, "/admin/task-projects", "", scopes.TasksCreate))
			if recorder.Code != testCase.wantCode {
				t.Fatalf("status %d, want %d (body %s)",
					recorder.Code, testCase.wantCode, recorder.Body.String())
			}
		})
	}
}

// TestTaskPatchPresence pins absent ≠ null ≠ value for every patchable field.
func TestTaskPatchPresence(t *testing.T) {
	dueValue, err := time.Parse(time.RFC3339, "2026-11-28T10:00:00+07:00")
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	assignedTo := "user-2"

	cases := []struct {
		name   string
		body   string
		want   int
		verify func(t *testing.T, patch tasks.TaskPatch)
	}{
		{
			name: "absent fields stay unset",
			body: `{}`,
			want: http.StatusOK,
			verify: func(t *testing.T, patch tasks.TaskPatch) {
				if patch.Title.Set || patch.Notes.Set || patch.Priority.Set || patch.DueAt.Set || patch.AssignedTo.Set {
					t.Fatalf("empty PATCH set a field: %+v", patch)
				}
			},
		},
		{
			name: "values are present",
			body: `{"title":"  Fix  ","notes":"n","priority":"high","dueAt":"2026-11-28T10:00:00+07:00","assignedTo":"user-2"}`,
			want: http.StatusOK,
			verify: func(t *testing.T, patch tasks.TaskPatch) {
				if !patch.Title.Set || patch.Title.Value != "  Fix  " {
					t.Fatalf("title = %+v", patch.Title)
				}
				if !patch.Notes.Set || patch.Notes.Value != "n" {
					t.Fatalf("notes = %+v", patch.Notes)
				}
				if !patch.Priority.Set || patch.Priority.Value != "high" {
					t.Fatalf("priority = %+v", patch.Priority)
				}
				if !patch.DueAt.Set || patch.DueAt.Value == nil || !patch.DueAt.Value.Equal(dueValue) {
					t.Fatalf("dueAt = %+v", patch.DueAt)
				}
				if !patch.AssignedTo.Set || patch.AssignedTo.Value == nil || *patch.AssignedTo.Value != assignedTo {
					t.Fatalf("assignedTo = %+v", patch.AssignedTo)
				}
			},
		},
		{
			name: "null clears the nullable fields",
			body: `{"dueAt":null,"assignedTo":null}`,
			want: http.StatusOK,
			verify: func(t *testing.T, patch tasks.TaskPatch) {
				if !patch.DueAt.Set || patch.DueAt.Value != nil {
					t.Fatalf("dueAt = %+v, want set-and-nil", patch.DueAt)
				}
				if !patch.AssignedTo.Set || patch.AssignedTo.Value != nil {
					t.Fatalf("assignedTo = %+v, want set-and-nil", patch.AssignedTo)
				}
				if patch.Title.Set || patch.Notes.Set || patch.Priority.Set {
					t.Fatalf("null on one field touched another: %+v", patch)
				}
			},
		},
		{
			name: "null title clears to empty (the service rejects it, not the decoder)",
			body: `{"title":null}`,
			want: http.StatusOK,
			verify: func(t *testing.T, patch tasks.TaskPatch) {
				if !patch.Title.Set || patch.Title.Value != "" {
					t.Fatalf("title = %+v, want set-and-empty", patch.Title)
				}
			},
		},
		{
			name: "wrong field type",
			body: `{"title":42}`,
			want: http.StatusBadRequest,
		},
		{
			name: "unknown field",
			body: `{"titel":"typo"}`,
			want: http.StatusBadRequest,
		},
		{
			name: "malformed json",
			body: `{`,
			want: http.StatusBadRequest,
		},
		{
			name: "bad dueAt",
			body: `{"dueAt":"tomorrow"}`,
			want: http.StatusBadRequest,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := &fakeTasks{}
			handler := taskTestHandler(fake)
			router := chi.NewRouter()
			router.Patch("/admin/tasks/{id}", handler.taskUpdate)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder,
				taskRequest(http.MethodPatch, "/admin/tasks/task-1", testCase.body, scopes.TasksUpdate))
			if recorder.Code != testCase.want {
				t.Fatalf("status %d, want %d (body %s)",
					recorder.Code, testCase.want, recorder.Body.String())
			}
			if testCase.verify != nil {
				testCase.verify(t, fake.updatePatch)
			}
		})
	}
}

// TestTaskClientTimezoneFallback proves X-Client-Timezone shapes the location
// and that an empty or unknown zone falls back to UTC instead of failing.
func TestTaskClientTimezoneFallback(t *testing.T) {
	for _, testCase := range []struct {
		name string
		zone string
		want string
	}{
		{"absent → UTC", "", "UTC"},
		{"IANA name is honored", "Asia/Jakarta", "Asia/Jakarta"},
		{"unknown zone falls back", "Mars/Olympus", "UTC"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fake := &fakeTasks{}
			handler := taskTestHandler(fake)
			request := taskRequest(http.MethodGet, "/admin/task-stats", "", scopes.TasksUpdate)
			if testCase.zone != "" {
				request.Header.Set("X-Client-Timezone", testCase.zone)
			}
			recorder := httptest.NewRecorder()
			handler.taskStats(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d (body %s)", recorder.Code, recorder.Body.String())
			}
			if fake.statsLoc == nil || fake.statsLoc.String() != testCase.want {
				t.Fatalf("stats location = %v, want %s", fake.statsLoc, testCase.want)
			}
		})
	}
}

// TestTaskListWireShape proves GET /admin/tasks returns the grouped workspace
// (projects, tasks, stats) in camelCase and passes view/project/location on.
func TestTaskListWireShape(t *testing.T) {
	projectID := "project-1"
	assignedTo := "user-2"
	assignee := "sarah"
	fake := &fakeTasks{
		projects: []tasks.Project{{
			ID: "project-1", Name: "Home", Role: "admin", CreatedAt: time.Unix(0, 0).UTC(),
			UpdatedAt: time.Unix(0, 0).UTC(), TaskCount: 3, OpenCount: 2,
		}},
		listTasks: []tasks.Task{{
			ID: "task-1", ProjectID: projectID, ProjectName: "Home",
			AssignedTo: &assignedTo, AssigneeName: &assignee,
			Title: "Fix the gate", Priority: "high", DueAt: nil,
			CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
			CommentCount: 2, SubtaskCount: 3, SubtaskDone: 1,
		}},
		stats: tasks.Stats{Open: 5, DueToday: 2, CompletedWeek: 7, Overdue: 1},
	}
	handler := taskTestHandler(fake)
	request := taskRequest(http.MethodGet,
		"/admin/tasks?view=today&project=project-1", "", scopes.TasksManage)
	request.Header.Set("X-Client-Timezone", "Asia/Jakarta")
	recorder := httptest.NewRecorder()
	handler.taskList(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d (body %s)", recorder.Code, recorder.Body.String())
	}
	if fake.listTasksView != "today" || fake.listProjectID != "project-1" {
		t.Fatalf("view/project = %q/%q", fake.listTasksView, fake.listProjectID)
	}
	if fake.listTasksLoc == nil || fake.listTasksLoc.String() != "Asia/Jakarta" {
		t.Fatalf("list location = %v", fake.listTasksLoc)
	}

	var body struct {
		Projects []map[string]any `json:"projects"`
		Tasks    []map[string]any `json:"tasks"`
		Stats    map[string]int   `json:"stats"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, recorder.Body.String())
	}
	if len(body.Projects) != 1 || body.Projects[0]["taskCount"] != float64(3) ||
		body.Projects[0]["openCount"] != float64(2) || body.Projects[0]["role"] != "admin" {
		t.Fatalf("projects = %+v", body.Projects)
	}
	if len(body.Tasks) != 1 {
		t.Fatalf("tasks = %+v", body.Tasks)
	}
	taskRow := body.Tasks[0]
	if taskRow["projectId"] != projectID || taskRow["assignedTo"] != assignedTo ||
		taskRow["assigneeName"] != assignee || taskRow["commentCount"] != float64(2) ||
		taskRow["subtaskCount"] != float64(3) || taskRow["subtaskDone"] != float64(1) {
		t.Fatalf("task row = %+v", taskRow)
	}
	if body.Stats["open"] != 5 || body.Stats["dueToday"] != 2 ||
		body.Stats["completedWeek"] != 7 || body.Stats["overdue"] != 1 {
		t.Fatalf("stats = %+v", body.Stats)
	}
}

// TestTaskScopeMatrix proves the REAL mount's route→scope mapping: no scopes
// is 403, tasks:update cannot create, tasks:create can, and tasks:manage owns
// the project settings.
func TestTaskScopeMatrix(t *testing.T) {
	anyTasks := []string{scopes.TasksCreate, scopes.TasksUpdate, scopes.TasksManage}
	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		scopeList  []string
		wantStatus int
	}{
		{"no scopes cannot read", http.MethodGet, "/admin/task-projects", "", nil, http.StatusForbidden},
		{"no scopes cannot create", http.MethodPost, "/admin/tasks", "{}", nil, http.StatusForbidden},
		{"update-only cannot create a task", http.MethodPost, "/admin/tasks", "{}",
			[]string{scopes.TasksUpdate}, http.StatusForbidden},
		{"update-only cannot create a project", http.MethodPost, "/admin/task-projects", "{}",
			[]string{scopes.TasksUpdate}, http.StatusForbidden},
		{"update-only can read the workspace", http.MethodGet, "/admin/tasks", "",
			[]string{scopes.TasksUpdate}, http.StatusOK},
		{"update-only can patch a task", http.MethodPatch, "/admin/tasks/task-1", "{}",
			[]string{scopes.TasksUpdate}, http.StatusOK},
		{"update-only can toggle", http.MethodPost, "/admin/tasks/task-1/toggle", "",
			[]string{scopes.TasksUpdate}, http.StatusOK},
		{"update-only can delete a task", http.MethodDelete, "/admin/tasks/task-1", "",
			[]string{scopes.TasksUpdate}, http.StatusNoContent},
		{"create-only can create a task", http.MethodPost, "/admin/tasks", "{}",
			[]string{scopes.TasksCreate}, http.StatusCreated},
		{"create-only cannot manage a project", http.MethodPatch, "/admin/task-projects/project-1", "{}",
			[]string{scopes.TasksCreate}, http.StatusForbidden},
		{"manage-only can manage a project", http.MethodPatch, "/admin/task-projects/project-1", `{"name":"Home"}`,
			[]string{scopes.TasksManage}, http.StatusOK},
		{"manage-only can list members", http.MethodGet, "/admin/task-projects/project-1/members", "",
			[]string{scopes.TasksManage}, http.StatusOK},
		{"manage-only can search invitees", http.MethodGet, "/admin/task-projects/project-1/people?q=sa", "",
			[]string{scopes.TasksManage}, http.StatusOK},
		{"update-only cannot search invitees", http.MethodGet, "/admin/task-projects/project-1/people?q=sa", "",
			[]string{scopes.TasksUpdate}, http.StatusForbidden},
		{"manage-only can read invites", http.MethodGet, "/admin/task-invites", "",
			[]string{scopes.TasksManage}, http.StatusOK},
		{"any scope can accept an invite", http.MethodPost, "/admin/task-invites/invite-1/accept", "",
			[]string{scopes.TasksUpdate}, http.StatusOK},
		{"any scope can decline an invite", http.MethodPost, "/admin/task-invites/invite-1/decline", "",
			[]string{scopes.TasksCreate}, http.StatusNoContent},
		{"no scope cannot accept an invite", http.MethodPost, "/admin/task-invites/invite-1/accept", "",
			nil, http.StatusForbidden},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler := taskTestHandler(&fakeTasks{})
			router := taskScopeRouter(handler, testCase.scopeList...)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder,
				taskRequest(testCase.method, testCase.path, testCase.body, testCase.scopeList...))
			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status %d, want %d (body %s)",
					recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
			if recorder.Code == http.StatusForbidden &&
				!strings.Contains(recorder.Body.String(), "tasks:") {
				t.Fatalf("403 must name the scope: %s", recorder.Body.String())
			}
		})
	}

	// The empty-scope refusal names EVERY acceptable scope (the portal
	// renders the copy verbatim).
	recorder := httptest.NewRecorder()
	taskScopeRouter(taskTestHandler(&fakeTasks{})).
		ServeHTTP(recorder, taskRequest(http.MethodGet, "/admin/task-projects", ""))
	want := strings.Join(anyTasks, " or ") + " required"
	if !strings.Contains(recorder.Body.String(), want) {
		t.Fatalf("refusal = %s, want %q", recorder.Body.String(), want)
	}
}

// TestTaskRouteCount keeps the boot log's number honest: the mount's real
// route count is what the composition root prints.
func TestTaskRouteCount(t *testing.T) {
	router := chi.NewRouter()
	router.Route("/admin", func(adminRouter chi.Router) {
		taskTestHandler(&fakeTasks{}).mountTasks(adminRouter)
	})
	count := 0
	if err := chi.Walk(router,
		func(_, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			if strings.HasPrefix(route, "/admin/task") {
				count++
			}
			return nil
		}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if count != TaskRouteCount {
		t.Fatalf("mount has %d task routes, boot log says %d", count, TaskRouteCount)
	}
}

// TestTaskPeopleWire pins the typeahead's returned shape: name + username,
// and NOTHING else (the email is a match key, never a response field).
func TestTaskPeopleWire(t *testing.T) {
	fake := &fakeTasks{people: []tasks.InviteeCandidate{
		{Username: "sarah", Name: "Sarah Lim"},
	}}
	router := taskScopeRouter(taskTestHandler(fake), scopes.TasksManage)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder,
		taskRequest(http.MethodGet, "/admin/task-projects/project-1/people?q=%40sarah", "", scopes.TasksManage))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	if fake.peopleQuery != "@sarah" {
		t.Fatalf("query = %q, want the raw parameter (the service normalizes)", fake.peopleQuery)
	}
	var body struct {
		People []map[string]any `json:"people"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.People) != 1 || body.People[0]["username"] != "sarah" ||
		body.People[0]["name"] != "Sarah Lim" || len(body.People[0]) != 2 {
		t.Fatalf("people wire drift: %s", recorder.Body.String())
	}
}
