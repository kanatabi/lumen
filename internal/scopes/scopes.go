// Package scopes is the permission grammar (the RBAC spine, owner rulings
// 2026-11-24): `<domain>:<verb>`, lowercase. It REPLACES the user roles —
// users carry an explicit scope set (user_scopes), creation grants exact
// scopes, and the CATALOG is DB data managed via the portal (users:manage
// — onboarding a scope never means manual SQL). The constants here are the
// code-side truth for the SEED (the migration inserts them) and the
// bundles the grants map by; the live catalog (what exists, what a user
// holds) lives in the database and the store reads it.
//
// TWO unrelated "role" concepts must not be confused: the USER roles this
// package deprecates (users.role, dropped in 0010) and the PROCESS planes
// (internal/role — LUMEN_ROLE deployment topology), which are untouched.
package scopes

import "regexp"

// The seed catalog — the MIGRATIONS insert exactly these (0010: the first
// four; 0014: the tasks trio). New scopes are onboarded via the portal,
// never by editing this list (the list is the bootstrap, not the
// registry).
const (
	ContentManage  = "content:manage"
	SecretsAdmin   = "secrets:admin"
	UsersManage    = "users:manage"
	SettingsManage = "settings:manage"
	// the tasks application (the app-center direction, 2026-11-27): three
	// verbs — reads and the workspace need ANY of them; project roles are
	// the second key inside a project.
	TasksCreate = "tasks:create"
	TasksUpdate = "tasks:update"
	TasksManage = "tasks:manage"
)

// Seed is the bootstrap catalog in the order the UI lists it.
var Seed = []struct {
	Scope       string
	Description string
}{
	{ContentManage, "the content desks: posts, photos, tags, collections, media"},
	{SecretsAdmin, "the whole Bao cockpit: values, pins, app onboarding, the estate matrix"},
	{UsersManage, "user creation, scope assignment, the scope catalog, password resets"},
	{SettingsManage, "portal-level settings (reserved; gated when settings grow)"},
	{TasksCreate, "the tasks workspace: create projects and tasks"},
	{TasksUpdate, "the tasks workspace: edit tasks, subtasks, comments, toggles"},
	{TasksManage, "the tasks workspace: project settings, invites, members and roles"},
}

// ContentBundle is what today's editors effectively hold — the migration's
// grant for role='editor' and the mental model for "the content set".
var ContentBundle = []string{ContentManage}

var validRe = regexp.MustCompile(`^[a-z]+:[a-z]+$`)

// Valid checks the GRAMMAR only (`<domain>:<verb>`). Whether a scope EXISTS
// is the catalog's question (the scopes table) — a grammatically valid
// scope can still be unassigned-unknown, and assignment answers that via
// the FK, never here.
func Valid(s string) bool { return validRe.MatchString(s) }
