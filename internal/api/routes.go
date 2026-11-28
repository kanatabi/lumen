// routes.go — THE route-role matrix, declared as data. The mounts build
// the router; this table states what each PLANE is supposed to serve;
// routes_test.go walks the actual chi routers of every role and demands
// an exact set match against here. The point of keeping the table as a
// separate, readable value: it is the document (and the report artifact),
// and the drift test makes the document and the binaries one promise.
package api

import "github.com/kanatabi/lumen/internal/role"

// MatrixRow is one route pattern with its plane classification.
type MatrixRow struct {
	Method  string
	Pattern string
	Admin   bool // served by role=admin (lumen-admin, foundation plane)
	CMS     bool // served by role=cms (lumen-staging / lumen-production)
}

// Matrix is the full surface of the binary. Read it as: which routes
// exist when LUMEN_ROLE=admin (column A), when LUMEN_ROLE=cms (column
// C). The legacy role=all serves the union. Probes ride both planes —
// a pod without a liveness endpoint is a pod that gets killed for the
// wrong reason.
var Matrix = []MatrixRow{
	// ---- probes: both planes ----
	{"GET", "/healthz", true, true},
	{"GET", "/readyz", true, true},

	// ---- CONTROL PLANE (admin only): the mint, the door, the relay ----
	{"GET", "/admin/setup", true, false},             // TOFU wizard pair — users is admin's
	{"POST", "/admin/setup/register", true, false},   // atomically self-disabling
	{"POST", "/admin/session", true, false},          // THE session mint — the only place tokens are born
	{"POST", "/admin/session/verify", true, false},   // introspection door for other planes
	{"GET", "/admin/session", true, false},           // whoami
	{"GET", "/admin/profile", true, false},           // the self profile (name/email)
	{"PATCH", "/admin/profile", true, false},         // self-service profile update
	{"DELETE", "/admin/session", true, false},        // logout = token_version bump
	{"POST", "/admin/session/password", true, false}, // guided change (428 escape)
	// the auth-method seam (the passkey round, C1): login begin/finish
	// are PUBLIC (the mint happens inside finish); the setup surfaces
	// ride the authed mount (handler.go). The methods endpoint is the
	// login UI's data source.
	{"GET", "/admin/session/auth/methods", true, false},
	{"POST", "/admin/session/webauthn/login/begin", true, false},
	{"POST", "/admin/session/webauthn/login/finish", true, false},
	{"GET", "/admin/session/webauthn", true, false},
	{"POST", "/admin/session/webauthn/register/begin", true, false},
	{"POST", "/admin/session/webauthn/register/finish", true, false},
	{"DELETE", "/admin/session/webauthn/{id}", true, false},
	// users + the scope catalog: the RBAC spine — the mgmt group gates on
	// the users:manage scope (the local authenticator verifies the bearer)
	{"GET", "/admin/users", true, false},
	{"POST", "/admin/users", true, false},
	{"POST", "/admin/users/{username}/reset-password", true, false},
	{"PATCH", "/admin/users/{username}/scopes", true, false},
	{"POST", "/admin/users/{username}/revoke-sessions", true, false},
	{"POST", "/admin/users/{username}/force-change", true, false},
	{"POST", "/admin/users/{username}/disable", true, false},
	{"POST", "/admin/users/{username}/enable", true, false},
	{"DELETE", "/admin/users/{username}", true, false},
	{"GET", "/admin/scopes", true, false},
	// the internal identity door (2026-11-29): token-gated, in-cluster,
	// read-only. The app planes resolve people here; email never returns.
	{"GET", "/admin/identity/people", true, false},
	{"POST", "/admin/identity/users", true, false},
	{"GET", "/admin/identity/by-username/{username}", true, false},
	{"POST", "/admin/scopes", true, false},
	{"DELETE", "/admin/scopes/{scope}", true, false},
	{"GET", "/admin/bao/paths", true, false}, // §9 relay (registered when the plane is configured)
	{"GET", "/admin/bao/summary", true, false},
	{"POST", "/admin/bao/apps", true, false},
	{"POST", "/admin/bao/{app}/envs", true, false},
	{"GET", "/admin/bao/{app}/{env}", true, false},
	{"PUT", "/admin/bao/{app}/{env}", true, false},
	{"PUT", "/admin/bao/{app}/{env}/pin", true, false},
	{"DELETE", "/admin/bao/{app}/{env}/pin", true, false},
	// notifications (M2.A): the bell reads are any authenticated user (no
	// scope); the SEND door is a PUBLIC mount, token-gated by ONE shared
	// env value, and absent entirely when that value is unset. The matrix
	// states the CONFIGURED surface — the handler tests prove the
	// unmounted honesty.
	{"GET", "/admin/notifications", true, false},
	{"GET", "/admin/notifications/unread-count", true, false},
	{"POST", "/admin/notifications/{id}/read", true, false},
	{"POST", "/admin/notifications/read-all", true, false},
	{"POST", "/admin/notifications", true, false},
	// tasks (M2.B): the first lumen-native application. It serves on the
	// CMS plane (2026-11-29 app-plane ruling: the admin plane is control +
	// identity only); its tables live in the env's DB. Reads need ANY of
	// the tasks trio; creates tasks:create; task edits tasks:update;
	// project settings/invites/members/roles tasks:manage. The project
	// membership role is the store's second key (invisible in the matrix).
	{"GET", "/admin/task-projects", false, true},
	{"POST", "/admin/task-projects", false, true},
	{"GET", "/admin/task-projects/{id}/members", false, true},
	{"GET", "/admin/task-projects/{id}/people", false, true},
	{"PATCH", "/admin/task-projects/{id}", false, true},
	{"DELETE", "/admin/task-projects/{id}", false, true},
	{"POST", "/admin/task-projects/{id}/complete", false, true},
	{"POST", "/admin/task-projects/{id}/invites", false, true},
	{"DELETE", "/admin/task-projects/{id}/invites/{iid}", false, true},
	{"DELETE", "/admin/task-projects/{id}/members/{user}", false, true},
	{"POST", "/admin/task-projects/{id}/members/{user}/role", false, true},
	{"GET", "/admin/task-invites", false, true},
	{"POST", "/admin/task-invites/{id}/accept", false, true},
	{"POST", "/admin/task-invites/{id}/decline", false, true},
	{"GET", "/admin/tasks", false, true},
	{"POST", "/admin/tasks", false, true},
	{"GET", "/admin/tasks/{id}", false, true},
	{"PATCH", "/admin/tasks/{id}", false, true},
	{"POST", "/admin/tasks/{id}/toggle", false, true},
	{"DELETE", "/admin/tasks/{id}", false, true},
	{"POST", "/admin/tasks/{id}/subtasks", false, true},
	{"PATCH", "/admin/tasks/{id}/subtasks/{sid}", false, true},
	{"DELETE", "/admin/tasks/{id}/subtasks/{sid}", false, true},
	{"POST", "/admin/tasks/{id}/comments", false, true},
	{"DELETE", "/admin/tasks/{id}/comments/{cid}", false, true},
	{"GET", "/admin/task-stats", false, true},

	// ---- CONTENT PLANE (cms only): the §1 reads ----
	{"GET", "/api/posts", false, true},
	{"GET", "/api/posts/{slug}", false, true},
	{"GET", "/api/search", false, true},
	{"GET", "/api/search/suggest", false, true},
	{"GET", "/api/photos", false, true},
	{"GET", "/api/photos/featured", false, true},
	{"GET", "/api/tags", false, true},

	// ---- CONTENT PLANE (cms only): the desks (auth: introspection) ----
	{"GET", "/admin/posts", false, true},
	{"POST", "/admin/posts", false, true},
	{"GET", "/admin/posts/{slug}", false, true},
	{"PUT", "/admin/posts/{slug}", false, true},
	{"POST", "/admin/posts/{slug}/publish", false, true},
	{"POST", "/admin/posts/{slug}/unpublish", false, true},
	{"DELETE", "/admin/posts/{slug}", false, true},
	{"POST", "/admin/photos", false, true}, // multipart relay (§5)
	{"GET", "/admin/photos", false, true},
	{"GET", "/admin/photos/{id}", false, true},
	{"GET", "/admin/photos/{id}/references", false, true},
	{"PATCH", "/admin/photos/{id}", false, true},
	{"DELETE", "/admin/photos/{id}", false, true},
	{"GET", "/admin/tags", false, true},
	{"GET", "/admin/tags/search", false, true},
	{"POST", "/admin/tags", false, true},
	{"PATCH", "/admin/tags/bulk", false, true},
	{"PATCH", "/admin/tags/{name}", false, true},
	{"DELETE", "/admin/tags/{name}", false, true},
	{"GET", "/admin/collections", false, true},
	{"POST", "/admin/collections", false, true},
	{"GET", "/admin/search", false, true}, // the palette's content door
}

// Serves reports whether the matrix row belongs to the given plane set.
func (m MatrixRow) Serves(planes role.Planes) bool {
	return (m.Admin && planes.Has(role.PlaneAdmin)) || (m.CMS && planes.Has(role.PlaneCMS))
}
