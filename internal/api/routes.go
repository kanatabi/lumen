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
	{"DELETE", "/admin/session", true, false},        // logout = token_version bump
	{"POST", "/admin/session/password", true, false}, // guided change (428 escape)
	{"GET", "/admin/users", true, false},             // users CRUD: admin role gate in the local authenticator
	{"POST", "/admin/users", true, false},
	{"POST", "/admin/users/{username}/reset-password", true, false},
	{"GET", "/admin/bao/paths", true, false}, // §9 relay (registered when the plane is configured)
	{"GET", "/admin/bao/summary", true, false},
	{"POST", "/admin/bao/apps", true, false},
	{"POST", "/admin/bao/{app}/envs", true, false},
	{"GET", "/admin/bao/{app}/{env}", true, false},
	{"PUT", "/admin/bao/{app}/{env}", true, false},
	{"PUT", "/admin/bao/{app}/{env}/pin", true, false},
	{"DELETE", "/admin/bao/{app}/{env}/pin", true, false},

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
