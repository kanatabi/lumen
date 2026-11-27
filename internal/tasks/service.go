// service.go — the tasks module's decision layer (M2.B). It owns every rule
// the store must never guess: the wire's length and enum grammar, the invite
// TTL, the username resolution, and the two BEST-EFFORT notifications (owner
// decision C10: the invite/assignment row is the truth; a delivery failure is
// logged and never rolls the mutation back).
//
// The notifications dependency is the Notifier SEAM declared HERE, exactly as
// the split doctrine requires (§1.4): tasks imports the notifications module's
// SendParams type (the wire it sends) but not its store or handlers; a future
// micro-service split lifts this file's call sites with the interface.
//
// Every interpolated user string that lands in an HTML body is escaped at the
// interpolation site (html.EscapeString). Titles are plain text by contract —
// escaping them would show entities in the panel — so they are TRUNCATED to
// the notification module's 200-rune budget instead.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kanatabi/lumen/internal/notifications"
)

// ErrInvalid is the tasks module's ONE validation voice. Every rule below
// wraps it and the HTTP door maps it to 400: a well-formed request with an
// empty title, an oversized note, an unknown priority, a malformed view or a
// mistyped username is the caller's copy being wrong, not the estate.
var ErrInvalid = errors.New("invalid task input")

const (
	// maxProjectNameRunes is the sidebar/project-picker one-line budget.
	maxProjectNameRunes = 100

	// maxTaskTitleRunes mirrors the notification title budget: a title
	// that cannot fit a notification title would have to be truncated
	// twice for no benefit.
	maxTaskTitleRunes = 200

	// maxTaskNotesBytes caps the drawer's free-text notes. Bytes, not
	// runes: the column stores text and the wire cap is a size guard.
	maxTaskNotesBytes = 16 * 1024

	// maxCommentBytes caps one comment; the same 8 KiB the send door
	// allows for a notification body.
	maxCommentBytes = 8 * 1024

	// inviteTTL is the owner's 24-hour invite window (2026-11-27b). The
	// store writes whatever instant it is handed; the TTL is a product
	// decision and lives here.
	inviteTTL = 24 * time.Hour

	// maxNotificationTitleRunes is the notifications module's title
	// budget. Truncation here keeps the module from rejecting a copy
	// assembled from a legal (but long) task title.
	maxNotificationTitleRunes = 200
)

// Principal is the caller as the authz seam sees them: the users.id for every
// membership lookup and the username for the human-readable notification copy.
type Principal struct {
	ID       string
	Username string
}

// Notifier is the consumer-side seam to the estate-generic notifications
// module (§1.4 rule 1). The notifications Service satisfies it; tests use a
// fake. Batch, not single, because that is the module's send contract.
type Notifier interface {
	Send(ctx context.Context, batch []notifications.SendParams) (int, error)
}

// Service is the tasks module's facade. The store owns persistence and
// membership; this layer owns validation, the TTL, identity resolution, and
// best-effort notification sends.
type Service struct {
	store    *Store
	notifier Notifier
}

// NewService binds the module. A nil notifier is legal (the route walk builds
// the handler without one); notifications are then simply not sent.
func NewService(store *Store, notifier Notifier) *Service {
	return &Service{store: store, notifier: notifier}
}

// ---- validation ----

// trimName validates a display name (project name, task title, subtask
// title): trimmed, non-empty, at most limit runes.
func trimName(what, value string, limit int) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%w: %s is required", ErrInvalid, what)
	}
	if utf8.RuneCountInString(trimmed) > limit {
		return "", fmt.Errorf("%w: %s must be at most %d characters", ErrInvalid, what, limit)
	}
	return trimmed, nil
}

// validateSize caps a free-text field in bytes without rewriting it (notes
// keep their whitespace verbatim; only the cap matters).
func validateSize(what, value string, limit int) error {
	if len(value) > limit {
		return fmt.Errorf("%w: %s must be at most %d bytes", ErrInvalid, what, limit)
	}
	return nil
}

// validatePriority checks the four schema values. Empty is NOT legal here:
// the create path fills the schema default first, and a present patch value
// must name a real priority.
func validatePriority(priority string) error {
	switch priority {
	case "high", "medium", "low", "none":
		return nil
	default:
		return fmt.Errorf("%w: priority must be high, medium, low or none", ErrInvalid)
	}
}

// validateRole checks the project membership role grammar (C2).
func validateRole(role string) error {
	switch role {
	case "admin", "member":
		return nil
	default:
		return fmt.Errorf("%w: role must be admin or member", ErrInvalid)
	}
}

// validateAssignee rejects an empty id: null/unset is "nobody", while an
// empty string is a malformed caller value (the store would report it as a
// non-member conflict, which would misdescribe the bug).
func validateAssignee(assignedTo *string) error {
	if assignedTo != nil && strings.TrimSpace(*assignedTo) == "" {
		return fmt.Errorf("%w: assignedTo must be a user id or null", ErrInvalid)
	}
	return nil
}

// validateTaskInput applies the create rules and returns the normalized input
// (trimmed title, explicit schema-default priority).
func validateTaskInput(input TaskInput) (TaskInput, error) {
	title, err := trimName("title", input.Title, maxTaskTitleRunes)
	if err != nil {
		return TaskInput{}, err
	}
	if err := validateSize("notes", input.Notes, maxTaskNotesBytes); err != nil {
		return TaskInput{}, err
	}
	if input.Priority == "" {
		input.Priority = "none" // the schema default, made explicit here
	}
	if err := validatePriority(input.Priority); err != nil {
		return TaskInput{}, err
	}
	if err := validateAssignee(input.AssignedTo); err != nil {
		return TaskInput{}, err
	}
	input.Title = title
	return input, nil
}

// validateTaskPatch validates only the PRESENT fields and normalizes them in
// place: an absent Optional must stay untouched, or a PATCH would erase a
// field the caller never mentioned.
func validateTaskPatch(patch *TaskPatch) error {
	if patch.Title.Set {
		title, err := trimName("title", patch.Title.Value, maxTaskTitleRunes)
		if err != nil {
			return err
		}
		patch.Title.Value = title
	}
	if patch.Notes.Set {
		if err := validateSize("notes", patch.Notes.Value, maxTaskNotesBytes); err != nil {
			return err
		}
	}
	if patch.Priority.Set {
		if err := validatePriority(patch.Priority.Value); err != nil {
			return err
		}
	}
	if patch.AssignedTo.Set {
		if err := validateAssignee(patch.AssignedTo.Value); err != nil {
			return err
		}
	}
	return nil
}

// ---- projects ----

// CreateProject validates the name and delegates the creator-member
// transaction (the store keeps the two inserts atomic).
func (s *Service) CreateProject(ctx context.Context, principal Principal, name string) (Project, error) {
	trimmed, err := trimName("name", name, maxProjectNameRunes)
	if err != nil {
		return Project{}, err
	}
	return s.store.CreateProject(ctx, principal.ID, trimmed)
}

// ListProjects delegates the caller's live project list.
func (s *Service) ListProjects(ctx context.Context, principal Principal) ([]Project, error) {
	return s.store.ListProjects(ctx, principal.ID)
}

// RenameProject validates the same name rule as CreateProject.
func (s *Service) RenameProject(ctx context.Context, principal Principal, projectID, name string) error {
	trimmed, err := trimName("name", name, maxProjectNameRunes)
	if err != nil {
		return err
	}
	return s.store.RenameProject(ctx, principal.ID, projectID, trimmed)
}

// SetProjectCompleted delegates the completed_at flag (the store owns the
// original-instant COALESCE and the project-admin check).
func (s *Service) SetProjectCompleted(ctx context.Context, principal Principal, projectID string, completed bool) error {
	return s.store.SetProjectCompleted(ctx, principal.ID, projectID, completed)
}

// SoftDeleteProject delegates the deleted_at flag (v1 has no destructive
// project delete, C4).
func (s *Service) SoftDeleteProject(ctx context.Context, principal Principal, projectID string) error {
	return s.store.SoftDeleteProject(ctx, principal.ID, projectID)
}

// ---- members ----

// ListMembers delegates the roster read (membership-scoped in the store).
func (s *Service) ListMembers(ctx context.Context, principal Principal, projectID string) ([]Member, error) {
	return s.store.ListMembers(ctx, principal.ID, projectID)
}

// SetMemberRole validates the role grammar, then delegates; the store's
// last-admin guard is the invariant, not a second rule here (C2).
func (s *Service) SetMemberRole(ctx context.Context, principal Principal, projectID, targetID, role string) error {
	if err := validateRole(role); err != nil {
		return err
	}
	return s.store.SetMemberRole(ctx, principal.ID, projectID, targetID, role)
}

// RemoveMember delegates the membership removal and its unassign step.
func (s *Service) RemoveMember(ctx context.Context, principal Principal, projectID, targetID string) error {
	return s.store.RemoveMember(ctx, principal.ID, projectID, targetID)
}

// ---- invites ----

// CreateInvite resolves the wire's username to the identity id, applies the
// 24-hour TTL, commits the invite, and only THEN notifies — best-effort, so a
// notification failure cannot undo the invite.
func (s *Service) CreateInvite(ctx context.Context, principal Principal, projectID, username string) (Invite, error) {
	trimmed := strings.TrimSpace(username)
	if trimmed == "" {
		return Invite{}, fmt.Errorf("%w: username is required", ErrInvalid)
	}
	inviteeID, err := s.store.UserIDByUsername(ctx, trimmed)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// A mistyped invitee is the caller's copy being wrong (400),
			// not a hidden estate resource (404).
			return Invite{}, fmt.Errorf("%w: unknown username %q", ErrInvalid, trimmed)
		}
		return Invite{}, err
	}
	invite, err := s.store.CreateInvite(ctx, principal.ID, projectID, inviteeID, time.Now().Add(inviteTTL))
	if err != nil {
		return Invite{}, err
	}
	s.notifyInvite(ctx, principal, invite)
	return invite, nil
}

// ListInvites delegates the caller's pending inbox (the store filters the TTL
// and dead projects).
func (s *Service) ListInvites(ctx context.Context, principal Principal) ([]Invite, error) {
	return s.store.ListInvitesFor(ctx, principal.ID)
}

// AcceptInvite delegates the consume-and-join transaction (the store answers
// ErrExpired with the row already deleted).
func (s *Service) AcceptInvite(ctx context.Context, principal Principal, inviteID string) (Invite, error) {
	return s.store.AcceptInvite(ctx, principal.ID, inviteID)
}

// DeclineInvite delegates the invitee's own delete.
func (s *Service) DeclineInvite(ctx context.Context, principal Principal, inviteID string) error {
	return s.store.DeclineInvite(ctx, principal.ID, inviteID)
}

// RevokeInvite delegates the project-admin's cancel.
func (s *Service) RevokeInvite(ctx context.Context, principal Principal, projectID, inviteID string) error {
	return s.store.RevokeInvite(ctx, principal.ID, projectID, inviteID)
}

// ---- tasks ----

// CreateTask validates, delegates, and notifies the assignee when the new
// task is born assigned to somebody other than the actor.
func (s *Service) CreateTask(ctx context.Context, principal Principal, projectID string, input TaskInput) (Task, error) {
	normalized, err := validateTaskInput(input)
	if err != nil {
		return Task{}, err
	}
	task, err := s.store.CreateTask(ctx, principal.ID, projectID, normalized)
	if err != nil {
		return Task{}, err
	}
	if task.AssignedTo != nil && *task.AssignedTo != principal.ID {
		s.notifyAssignment(ctx, task)
	}
	return task, nil
}

// ListTasks validates the view's closed set, then delegates. The store treats
// an unknown view as a bug; the caller gets an honest 400 instead of "all".
func (s *Service) ListTasks(ctx context.Context, principal Principal, view, projectID string, location *time.Location) ([]Task, error) {
	switch view {
	case ViewToday, ViewUpcoming, ViewAll:
	default:
		return nil, fmt.Errorf("%w: view must be today, upcoming or all", ErrInvalid)
	}
	return s.store.ListTasks(ctx, principal.ID, view, projectID, location)
}

// GetTask delegates the drawer's snapshot read.
func (s *Service) GetTask(ctx context.Context, principal Principal, taskID string) (TaskDetail, error) {
	return s.store.GetTask(ctx, principal.ID, taskID)
}

// UpdateTask validates the present patch fields, delegates, and notifies the
// NEW assignee only when the assignment moved to somebody other than the
// actor. The pre-read is deliberate: the store's returned row carries the new
// assignee, so the old one must be read to tell a real change from a repeated
// PATCH of the same person. Best-effort means a race can at worst drop or
// duplicate a notification, never corrupt the assignment.
func (s *Service) UpdateTask(ctx context.Context, principal Principal, taskID string, patch TaskPatch) (Task, error) {
	if err := validateTaskPatch(&patch); err != nil {
		return Task{}, err
	}
	previous, err := s.store.GetTask(ctx, principal.ID, taskID)
	if err != nil {
		return Task{}, err
	}
	updated, err := s.store.UpdateTask(ctx, principal.ID, taskID, patch)
	if err != nil {
		return Task{}, err
	}
	assigneeChanged := patch.AssignedTo.Set && !sameStringID(patch.AssignedTo.Value, previous.Task.AssignedTo)
	if assigneeChanged && updated.AssignedTo != nil && *updated.AssignedTo != principal.ID {
		s.notifyAssignment(ctx, updated)
	}
	return updated, nil
}

// ToggleTask delegates the done flip.
func (s *Service) ToggleTask(ctx context.Context, principal Principal, taskID string) (Task, error) {
	return s.store.ToggleTask(ctx, principal.ID, taskID)
}

// SoftDeleteTask delegates the deleted_at flag.
func (s *Service) SoftDeleteTask(ctx context.Context, principal Principal, taskID string) error {
	return s.store.SoftDeleteTask(ctx, principal.ID, taskID)
}

// ---- subtasks ----

// AddSubtask validates the checklist title and delegates.
func (s *Service) AddSubtask(ctx context.Context, principal Principal, taskID, title string) (Subtask, error) {
	trimmed, err := trimName("title", title, maxTaskTitleRunes)
	if err != nil {
		return Subtask{}, err
	}
	return s.store.AddSubtask(ctx, principal.ID, taskID, trimmed)
}

// UpdateSubtask validates a present title and delegates the presence patch.
func (s *Service) UpdateSubtask(ctx context.Context, principal Principal, taskID, subtaskID string, patch SubtaskPatch) (Subtask, error) {
	if patch.Title.Set {
		trimmed, err := trimName("title", patch.Title.Value, maxTaskTitleRunes)
		if err != nil {
			return Subtask{}, err
		}
		patch.Title.Value = trimmed
	}
	return s.store.UpdateSubtask(ctx, principal.ID, taskID, subtaskID, patch)
}

// DeleteSubtask delegates the checklist row delete.
func (s *Service) DeleteSubtask(ctx context.Context, principal Principal, taskID, subtaskID string) error {
	return s.store.DeleteSubtask(ctx, principal.ID, taskID, subtaskID)
}

// ---- comments ----

// AddComment trims the body, enforces the 1..8192-byte rule, and delegates.
func (s *Service) AddComment(ctx context.Context, principal Principal, taskID, body string) (Comment, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return Comment{}, fmt.Errorf("%w: comment body is required", ErrInvalid)
	}
	if err := validateSize("comment body", trimmed, maxCommentBytes); err != nil {
		return Comment{}, err
	}
	return s.store.AddComment(ctx, principal.ID, taskID, trimmed)
}

// DeleteComment delegates the author-or-admin delete.
func (s *Service) DeleteComment(ctx context.Context, principal Principal, taskID, commentID string) error {
	return s.store.DeleteComment(ctx, principal.ID, taskID, commentID)
}

// ---- stats ----

// Stats delegates the four-card rollup, which is computed in the caller's
// location (the handler passes the X-Client-Timezone value or UTC).
func (s *Service) Stats(ctx context.Context, principal Principal, location *time.Location) (Stats, error) {
	return s.store.Stats(ctx, principal.ID, location)
}

// ---- notifications (best-effort, after the mutation committed) ----

// inviteNotificationPayload is the invite click-through: the workspace plus
// the invite id so the panel can mark the row consumed without a lookup.
type inviteNotificationPayload struct {
	URL    string `json:"url"`
	Invite string `json:"invite"`
}

// assignmentNotificationPayload is the assignment click-through: the task
// drawer deep link plus the id.
type assignmentNotificationPayload struct {
	URL  string `json:"url"`
	Task string `json:"task"`
}

// notifyInvite sends the invitee's one notification. The store committed the
// invite already; every failure here is a log line, never a returned error.
func (s *Service) notifyInvite(ctx context.Context, principal Principal, invite Invite) {
	inviter := invite.InviterName
	if inviter == "" {
		// The store hydrates the name from the users row; the principal is
		// the honest fallback if that join could not (it always can).
		inviter = principal.Username
	}
	payload, err := json.Marshal(inviteNotificationPayload{URL: "/tasks", Invite: invite.ID})
	if err != nil {
		slog.Warn("tasks.notify.payload", "type", "project.invite", "err", err)
		return
	}
	expiresAt := invite.ExpiresAt.UTC()
	s.notify(ctx, notifications.SendParams{
		UserID:    invite.InviteeID,
		App:       "tasks",
		Type:      "project.invite",
		Title:     truncateRunes("📦 You've been invited to "+invite.ProjectName, maxNotificationTitleRunes),
		Body:      "<b>" + html.EscapeString(inviter) + "</b> invited you to collaborate — open Tasks to accept.",
		Payload:   payload,
		ExpiresAt: &expiresAt,
	})
}

// notifyAssignment sends the assignee's one notification. The task was read
// back through the caller's membership, so AssignedTo is a real member.
func (s *Service) notifyAssignment(ctx context.Context, task Task) {
	if task.AssignedTo == nil {
		return
	}
	payload, err := json.Marshal(assignmentNotificationPayload{
		URL:  "/tasks?task=" + task.ID,
		Task: task.ID,
	})
	if err != nil {
		slog.Warn("tasks.notify.payload", "type", "task.assigned", "err", err)
		return
	}
	s.notify(ctx, notifications.SendParams{
		UserID:  *task.AssignedTo,
		App:     "tasks",
		Type:    "task.assigned",
		Title:   truncateRunes("📋 "+task.Title, maxNotificationTitleRunes),
		Body:    html.EscapeString(task.Title) + " in " + html.EscapeString(task.ProjectName),
		Payload: payload,
	})
}

// notify is the ONE best-effort send site: one recipient, one log line on
// failure, no error returned (owner C10).
func (s *Service) notify(ctx context.Context, params notifications.SendParams) {
	if s.notifier == nil {
		return
	}
	if _, err := s.notifier.Send(ctx, []notifications.SendParams{params}); err != nil {
		slog.Warn("tasks.notify.failed",
			"app", params.App, "type", params.Type, "recipient", params.UserID, "err", err)
	}
}

// truncateRunes cuts a plain-text title to the notification module's budget
// without splitting a multi-byte character (emoji are the point of titles).
func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
