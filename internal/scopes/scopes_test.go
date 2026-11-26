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
	// The CLASSIC bootstrap set must never vanish. A hardcoded TOTAL is a
	// duplicated golden that drifts (the migration-subset count taught it):
	// feature migrations may seed their own scopes (0014's tasks trio), and
	// the RUNTIME catalog stays portal-managed.
	for _, required := range []string{ContentManage, SecretsAdmin, UsersManage, SettingsManage} {
		if !seen[required] {
			t.Fatalf("bootstrap scope %q vanished from the seed — the classic four are load-bearing", required)
		}
	}
}

func TestContentBundle(t *testing.T) {
	if len(ContentBundle) != 1 || ContentBundle[0] != ContentManage {
		t.Fatalf("content bundle drifted: %v", ContentBundle)
	}
}
