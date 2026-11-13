// migrate_manifest_test.go — the role classification, PROVEN OFFLINE
// against the embedded SQL itself: every file classified; the cms subset
// never mentions the users table (its fresh databases must have ZERO of
// it — the ruling made literal); the admin subset never touches content
// tables. The real-DB witness test (fresh cms schema → no users) rides
// the LUMEN_INTEGRATION gate beside it.
package store

import (
	"strings"
	"testing"

	"github.com/kanatabi/lumen/internal/role"
)

func TestEveryMigrationIsClassified(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := migrationRoles[e.Name()]; !ok {
			t.Fatalf("migration %s is UNCLASSIFIED — the role split admits no silent skips", e.Name())
		}
	}
	for name := range migrationRoles {
		if _, err := migrationsFS.ReadFile("migrations/" + name); err != nil {
			t.Fatalf("classified file %s does not exist: %v", name, err)
		}
	}
}

// sqlCode strips comment lines — the assertions are on EXECUTED
// statements; the files CARRY their history in comments.
func sqlCode(s string) string {
	code := []string{}
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "--") {
			code = append(code, t)
		}
	}
	return strings.Join(code, "\n")
}

func TestCMSSubsetNeverNamesUsers(t *testing.T) {
	for name, planes := range migrationRoles {
		if !planes.Has(role.PlaneCMS) {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, frag := range []string{"users(", "users (", "INTO users", "TABLE users", "FROM users", "UPDATE users", "ALTER TABLE users"} {
			if strings.Contains(sqlCode(string(body)), frag) {
				t.Fatalf("cms-classified %s touches the users table (%q) — the cms DB must not own it", name, frag)
			}
		}
	}
}

func TestAdminSubsetTouchesOnlyCredentialTables(t *testing.T) {
	for _, name := range []string{"0002_admin_users.sql", "0007_users_roles.sql"} {
		planes, ok := migrationRoles[name]
		if !ok || !planes.Has(role.PlaneAdmin) || planes.Has(role.PlaneCMS) {
			t.Fatalf("%s must classify ADMIN-ONLY, got %v", name, planes)
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, content := range []string{"posts", "photos", "tags", "collections", "media_assets"} {
			if strings.Contains(sqlCode(string(body)), content) {
				t.Fatalf("admin-only %s references the content table %q — ownership violation", name, content)
			}
		}
	}
}

func TestUsersBlockLeftCore(t *testing.T) {
	// the split's keystone: 0001_core.sql — the file every legacy database
	// already recorded — must NOT create users anymore; the fresh cms
	// schema gets its zero-users guarantee precisely from this edit.
	body, err := migrationsFS.ReadFile("migrations/0001_core.sql")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	// strip comment lines: the migration CARRIES the story in comments,
	// the assertion is on the EXECUTED statements
	code := []string{}
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "--") {
			code = append(code, t)
		}
	}
	joined := strings.Join(code, "\n")
	if strings.Contains(joined, "CREATE TABLE users") || strings.Contains(joined, "users (") {
		t.Fatal("0001_core still creates the users table — the cms plane would grow it")
	}
	if !strings.Contains(joined, "CREATE TABLE posts") {
		t.Fatal("0001_core lost its content tables")
	}
}
