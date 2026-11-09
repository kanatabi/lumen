package adminsvc

import (
	"regexp"
	"sort"
)

// ---- the PURE heart of the secrets control plane (PLAN-SECRETS-CONTROL A1):
// no Bao, no DB, no network — the key-set invariant as deterministic data
// functions. A3's fan-out calls exactly these; every mirror promise is
// proven here first, offline. "Apps are data" (§2.2-1) binds: slugs are
// validated shapes, never enumerated sets.

// DefaultEnvs is the trio an app is onboarded with (D4: a DEFAULT, not a
// schema — POST /bao/{app}/envs extends an app beyond it, and the mirror
// then targets whatever envs actually exist, never this list).
var DefaultEnvs = []string{"dev", "staging", "production"}

// Slug shapes. Leading [a-z] makes `_` reserved roots (and digits/dashes)
// structurally impossible; underscores are banned outright so a slug never
// hides a Bao path trick. Length ≤ 32 keeps audit lines sane.
var (
	appSlugRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	envSlugRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

func ValidAppSlug(s string) bool { return appSlugRE.MatchString(s) }
func ValidEnvSlug(s string) bool { return envSlugRE.MatchString(s) }

// KeyDelta diffs a saved bundle against that env's PREVIOUS head: the
// structural changes the key-set mirror must apply to sibling envs
// (§2.1, PUT row). Values never appear — this is shape, not content.
// Results are sorted: logs, tests, and retry paths all read them.
func KeyDelta(oldHead, saved map[string]string) (adds, removes []string) {
	for k := range saved {
		if _, ok := oldHead[k]; !ok {
			adds = append(adds, k)
		}
	}
	for k := range oldHead {
		if _, ok := saved[k]; !ok {
			removes = append(removes, k)
		}
	}
	sort.Strings(adds)
	sort.Strings(removes)
	return adds, removes
}

// ApplyDelta mirrors a key-set change onto ONE sibling env's current keys.
// Adds land EMPTY — values are per-env by the invariant (§2.2-2): "every
// env shares the keys, no env shares another's secrets". Removes drop the
// key. `changed` is the write-gate: false means this sibling already has
// the shape (another tab raced the add, or the key never existed here) and
// MUST NOT be written — no structural change, no gratuitous version bump,
// no fake CAS conflict.
func ApplyDelta(sibling map[string]string, adds, removes []string) (next map[string]string, changed bool) {
	drop := make(map[string]struct{}, len(removes))
	for _, k := range removes {
		drop[k] = struct{}{}
	}
	next = make(map[string]string, len(sibling)+len(adds))
	for k, v := range sibling {
		if _, gone := drop[k]; gone {
			changed = true
			continue
		}
		next[k] = v // existing values survive untouched
	}
	for _, k := range adds {
		if _, exists := next[k]; exists {
			continue // sibling already carries the key — its value stays
		}
		next[k] = ""
		changed = true
	}
	return next, changed
}

// KeySet projects a payload to its sorted key list — the shape unionKeys
// consumes, and the canonical form tests assert against.
func KeySet(payload map[string]string) []string {
	out := make([]string, 0, len(payload))
	for k := range payload {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CanonicalKeySet is the app-wide key set a NEW env is seeded with
// (POST /bao/{app}/envs, §2.1): the UNION of sibling envs — an env added
// late must not drift a smaller shape than its siblings (the invariant is
// structural, not just write-time). Map iteration is deliberately absent
// from the result order: sorted, deterministic, empty-vs-nil equal.
func CanonicalKeySet(envPayloads map[string]map[string]string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, payload := range envPayloads {
		for k := range payload {
			if _, ok := seen[k]; !ok {
				seen[k] = struct{}{}
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}
