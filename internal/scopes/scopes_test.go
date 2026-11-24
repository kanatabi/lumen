package scopes

import "testing"

func TestValid(t *testing.T) {
	for _, s := range []string{"content:manage", "budget:read", "secrets:admin", "a:b"} {
		if !Valid(s) {
			t.Fatalf("Valid(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "content", "content:", ":manage", "Content:Manage", "content:manage:extra", "content manage", "content:manage1"} {
		if Valid(s) {
			t.Fatalf("Valid(%q) = true, want false", s)
		}
	}
}

func TestSeedCoversTheCatalog(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Seed {
		if !Valid(s.Scope) {
			t.Fatalf("seed scope %q fails the grammar", s.Scope)
		}
		if s.Description == "" {
			t.Fatalf("seed scope %q has no description — the catalog's UI reads it", s.Scope)
		}
		if seen[s.Scope] {
			t.Fatalf("duplicate seed scope %q", s.Scope)
		}
		seen[s.Scope] = true
	}
	if len(Seed) != 4 {
		t.Fatalf("seed catalog drifted: %d entries, want 4 (new scopes are PORTAL-ONBOARDED, not code-seeded)", len(Seed))
	}
}

func TestContentBundle(t *testing.T) {
	if len(ContentBundle) != 1 || ContentBundle[0] != ContentManage {
		t.Fatalf("content bundle drifted: %v", ContentBundle)
	}
}
