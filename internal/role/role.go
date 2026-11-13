// Package role is the leaf truth of the process-role split (owner ruling
// 2026-11-13): ONE binary, LUMEN_ROLE = "admin" | "cms".
//
//	ROLE admin (lumen-admin, foundation plane) — the CONTROL PLANE: it owns
//	  the users table, the session MINT (the only place tokens are born),
//	  the verify door other planes introspect through, and the Bao relay.
//	  No content routes, no queue, no Meilisearch, no public /api reads.
//	ROLE cms (lumen-staging / lumen-production) — the CONTENT PLANE: the §1
//	  public reads, the admin content desks (posts/photos/tags/collections/
//	  media), and the index worker. It holds NO users table: auth decisions
//	  are INTROSPECTED from the admin plane (fail closed, see internal/authz).
//	ROLE all — the legacy single-process plane (unset default): everything
//	  above in one binary with local JWT auth, so the dev loop and every
//	  existing test keep working byte-identically. "all" is a DEV affordance,
//	  never a cluster posture.
//
// The type lives in its own package because admin, cms, migrations, route
// matrices and the config layer all classify by it — a string alias in one
// of them would make the others import that one (hotfix lesson: one
// classification table, every consumer reads it, no drift).
package role

import (
	"fmt"
	"strings"
)

type Role string

const (
	Admin Role = "admin"
	CMS   Role = "cms"
	All   Role = "all"
)

// Parse resolves LUMEN_ROLE. The EMPTY value is the legacy default ("all")
// — documented in env.yaml.sample: dev planes and tests set nothing and
// keep every surface; the cluster sets one of the two planes explicitly.
func Parse(s string) (Role, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return All, nil
	case string(Admin):
		return Admin, nil
	case string(CMS):
		return CMS, nil
	case string(All):
		return All, nil
	default:
		return "", fmt.Errorf("LUMEN_ROLE must be \"admin\" | \"cms\" (unset = legacy \"all\"), got %q", s)
	}
}

// Planes is the surface-set bitfield the routers and migration manifests
// classify against. A Role maps to one or both planes; "all" serves both.
type Planes uint8

const (
	PlaneAdmin Planes = 1 << iota // the control plane (users, mint, verify, bao)
	PlaneCMS                      // the content plane (§1 reads, desks, queue)

	PlanesAll = PlaneAdmin | PlaneCMS
)

func (r Role) Planes() Planes {
	switch r {
	case Admin:
		return PlaneAdmin
	case CMS:
		return PlaneCMS
	default: // All — the legacy dev plane serves every surface
		return PlanesAll
	}
}

func (p Planes) Has(plane Planes) bool { return p&plane != 0 }

// Names lists the legal LUMEN_ROLE values in documentation order — the
// sample file and the CLI help read it from here, not from a copy.
func Names() []string { return []string{string(Admin), string(CMS), string(All)} }
