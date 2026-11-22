package adminsvc

import "testing"

// The delete's object-GC decision table (owner call 2026-11-22 — the eager
// best-effort object delete amends §8's keep-until-GC line). PURE: no DB,
// no S3 — the store resolution and the object call ride the integration
// walk (TestAdminLifecycle's GC step).
func TestGCDecide(t *testing.T) {
	tcs := []struct {
		name       string
		mediaWired bool
		path       string
		siblings   int
		want       gcAction
	}{
		{"no store wired — dev/pre-M5 row-only", false, "media/aa/x.jpg", 0, gcRowOnly},
		{"no store wired wins over everything", false, "", 3, gcRowOnly},
		{"unresolved ledger — hand-inserted or pre-ledger row", true, "", 0, gcUnresolved},
		{"sibling photo cites the same object — it stays", true, "media/aa/x.jpg", 1, gcShared},
		{"sole owner — delete", true, "media/aa/x.jpg", 0, gcDelete},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			if got := gcDecide(tc.mediaWired, tc.path, tc.siblings); got != tc.want {
				t.Fatalf("gcDecide(wired=%v, path=%q, sib=%d) = %v, want %v",
					tc.mediaWired, tc.path, tc.siblings, got, tc.want)
			}
		})
	}
}
