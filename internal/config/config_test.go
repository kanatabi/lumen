package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kanatabi/lumen/internal/role"
)

func TestDefaultsAndEnv(t *testing.T) {
	t.Setenv("LUMEN_DB_URL", "postgres://env/db")
	t.Setenv("LUMEN_SESSION_JWT_SECRET", "test-secret")
	t.Setenv("LUMEN_CONFIG", "")
	t.Setenv("PORT", "")
	t.Setenv("LUMEN_PORT", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.DBURL != "postgres://env/db" || c.Port != "8080" {
		t.Fatalf("got %+v", c)
	}

	// plain PORT is the workspace container convention — it must bind too
	t.Setenv("PORT", "8091")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != "8091" {
		t.Fatalf("PORT not honored: %+v", c)
	}
}

func TestServerRoleRequiresSessionSecret(t *testing.T) {
	t.Setenv("LUMEN_DB_URL", "postgres://env/db")
	t.Setenv("LUMEN_SESSION_JWT_SECRET", "")
	t.Setenv("LUMEN_CONFIG", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load itself must not need the secret (worker role boots without it): %v", err)
	}
	if err := c.ValidateServer(); err == nil {
		t.Fatal("expected fail-fast: HTTP role without a signing secret")
	}
	t.Setenv("LUMEN_SESSION_JWT_SECRET", "s")
	c, _ = Load()
	if err := c.ValidateServer(); err != nil {
		t.Fatalf("with secret present ValidateServer must pass: %v", err)
	}
}

func TestMediaEnvBinds(t *testing.T) {
	// the M5 patch BOUND media_* but never POPULATED the struct — a bucket
	// in env, an empty Config.Media, and a 503 that outlived the whole
	// milestone. This test is the difference between bound and wired.
	t.Setenv("LUMEN_DB_URL", "postgres://env/db")
	t.Setenv("LUMEN_SESSION_JWT_SECRET", "s")
	t.Setenv("LUMEN_CONFIG", "")
	t.Setenv("LUMEN_MEDIA_BUCKET", "lumen-media-dev")
	t.Setenv("LUMEN_MEDIA_ENDPOINT", "http://localhost:4566")
	t.Setenv("LUMEN_MEDIA_FORCE_PATH", "true")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Media.Bucket != "lumen-media-dev" || c.Media.Endpoint != "http://localhost:4566" || !c.Media.ForcePath {
		t.Fatalf("media env not populated: %+v", c.Media)
	}
}

func TestRequiresDB(t *testing.T) {
	t.Setenv("LUMEN_DB_URL", "")
	t.Setenv("LUMEN_SESSION_JWT_SECRET", "")
	t.Setenv("LUMEN_CONFIG", "")
	// also ensure no stray ./env.yaml in the package dir can satisfy it
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected failure without db_url (fixture mode is retired)")
	}
}

func TestFileFillsEnvWins(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "env.yaml")
	if err := os.WriteFile(file, []byte("db_url: postgres://file/db\norigin: https://from.file\nsession_jwt_secret: file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LUMEN_CONFIG", file)
	t.Setenv("LUMEN_DB_URL", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.DBURL != "postgres://file/db" || c.Origin != "https://from.file" || c.SessionSecret != "file-secret" {
		t.Fatalf("file not applied: %+v", c)
	}

	// env must WIN over the file — the deployment truth (ESO renders env)
	t.Setenv("LUMEN_DB_URL", "postgres://env/wins")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.DBURL != "postgres://env/wins" {
		t.Fatalf("precedence broken: %+v", c)
	}
}

// ---- role split (2026-11-13) ----

func TestRoleDefaultIsLegacyAll(t *testing.T) {
	t.Setenv("LUMEN_DB_URL", "postgres://env/db")
	t.Setenv("LUMEN_SESSION_JWT_SECRET", "s")
	t.Setenv("LUMEN_ROLE", "")
	t.Setenv("LUMEN_CONFIG", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Role != role.All {
		t.Fatalf("unset LUMEN_ROLE must be the documented legacy default, got %q", c.Role)
	}
	if c.SessionSecret != "s" { // all binds BOTH planes' keys — dev unchanged
		t.Fatalf("all plane lost the session binding: %+v", c)
	}
	if c.MeiliIndexPosts != "posts" || c.MeiliIndexPhotos != "photos" {
		t.Fatalf("index defaults drifted (dev loop must be byte-identical): %+v", c)
	}
}

func TestRoleRejectsGarbage(t *testing.T) {
	t.Setenv("LUMEN_DB_URL", "postgres://env/db")
	t.Setenv("LUMEN_ROLE", "content")
	t.Setenv("LUMEN_CONFIG", "")
	if _, err := Load(); err == nil {
		t.Fatal("a typo in LUMEN_ROLE must be a boot error, never a silent all")
	}
}

func TestCMSPlaneDoesNotReadSessionSecret(t *testing.T) {
	t.Setenv("LUMEN_ROLE", "cms")
	t.Setenv("LUMEN_DB_URL", "postgres://env/db")
	t.Setenv("LUMEN_CONFIG", "")
	// even a LEAKED signing key in the pod env must not reach the cms
	// struct — the ruling removed it from cms BINDINGS, not just usage
	t.Setenv("LUMEN_SESSION_JWT_SECRET", "leaked-but-ignored")
	t.Setenv("LUMEN_ADMIN_URL", "http://lumen-admin:8080")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.SessionSecret != "" {
		t.Fatalf("cms plane bound the signing key: %q", c.SessionSecret)
	}
	if c.AdminURL != "http://lumen-admin:8080" {
		t.Fatalf("admin_url not bound: %+v", c)
	}
	if err := c.ValidateServer(); err != nil {
		t.Fatalf("cms server validates with admin_url: %v", err)
	}

	t.Setenv("LUMEN_ADMIN_URL", "")
	c, _ = Load()
	if err := c.ValidateServer(); err == nil {
		t.Fatal("cms without LUMEN_ADMIN_URL must refuse to serve (introspection has no door)")
	}
}

func TestAdminPlaneDoesNotReadMediaOrMeili(t *testing.T) {
	t.Setenv("LUMEN_ROLE", "admin")
	t.Setenv("LUMEN_DB_URL", "postgres://env/db")
	t.Setenv("LUMEN_CONFIG", "")
	t.Setenv("LUMEN_SESSION_JWT_SECRET", "s")
	t.Setenv("LUMEN_MEILI_URL", "http://localhost:7700")
	t.Setenv("LUMEN_MEDIA_BUCKET", "lumen-media")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.MeiliURL != "" || c.Media.Bucket != "" {
		t.Fatalf("admin plane bound content-plane keys: %+v", c)
	}
	if err := c.ValidateServer(); err != nil {
		t.Fatalf("admin validates with the signing secret: %v", err)
	}
	t.Setenv("LUMEN_SESSION_JWT_SECRET", "")
	c, _ = Load()
	if err := c.ValidateServer(); err == nil {
		t.Fatal("admin without the signing secret is a mint that mints garbage")
	}
}

func TestMediaPrefixAndIndexNamesBind(t *testing.T) {
	t.Setenv("LUMEN_ROLE", "cms")
	t.Setenv("LUMEN_DB_URL", "postgres://env/db")
	t.Setenv("LUMEN_CONFIG", "")
	t.Setenv("LUMEN_ADMIN_URL", "http://x")
	t.Setenv("LUMEN_MEDIA_PREFIX", "staging/")
	t.Setenv("LUMEN_MEILI_INDEX_POSTS", "cms-posts")
	t.Setenv("LUMEN_MEILI_INDEX_PHOTOS", "cms-photos")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Media.Prefix != "staging/" || c.MeiliIndexPosts != "cms-posts" || c.MeiliIndexPhotos != "cms-photos" {
		t.Fatalf("new cms-plane keys not populated: %+v", c)
	}
}

func TestEnvKeysPerRole(t *testing.T) {
	in := func(keys []string, want string) bool {
		for _, k := range keys {
			if k == want {
				return true
			}
		}
		return false
	}
	admin := EnvKeys(role.Admin)
	cms := EnvKeys(role.CMS)
	all := EnvKeys(role.All)

	// every plane reads truth + liveness + meta
	for _, keys := range [][]string{admin, cms, all} {
		for _, k := range []string{"LUMEN_DB_URL", "LUMEN_PORT", "LUMEN_ROLE", "LUMEN_CONFIG"} {
			if !in(keys, k) {
				t.Fatalf("role keys %v miss meta key %s", keys, k)
			}
		}
	}
	// the rulings, one assert each
	for _, k := range []string{"LUMEN_SESSION_JWT_SECRET", "LUMEN_BAO_ADDR"} {
		if !in(admin, k) || in(cms, k) {
			t.Fatalf("%s must bind for admin ONLY (all keeps it as union): admin=%v cms=%v", k, admin, cms)
		}
	}
	for _, k := range []string{"LUMEN_ADMIN_URL", "LUMEN_MEILI_URL", "LUMEN_MEDIA_BUCKET", "LUMEN_MEILI_INDEX_POSTS", "LUMEN_MEILI_INDEX_PHOTOS", "LUMEN_MEDIA_PREFIX"} {
		if !in(cms, k) || in(admin, k) {
			t.Fatalf("%s must bind for cms ONLY: cms=%v admin=%v", k, cms, admin)
		}
	}
	if len(all) != len(admin)+len(cms)-4 { // the four shared meta keys counted once
		t.Fatalf("all must be the exact union: %d vs admin=%d cms=%d", len(all), len(admin), len(cms))
	}
	// sorted stability (the command's machine-readable promise)
	if !slices.IsSorted(admin) || !slices.IsSorted(cms) {
		t.Fatal("EnvKeys must be sorted")
	}
}
