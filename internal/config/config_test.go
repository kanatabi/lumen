package config

import (
	"os"
	"path/filepath"
	"testing"
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

func TestPostgresRequiresSessionSecret(t *testing.T) {
	t.Setenv("LUMEN_DB_URL", "postgres://env/db")
	t.Setenv("LUMEN_SESSION_JWT_SECRET", "")
	t.Setenv("LUMEN_CONFIG", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected fail-fast: Postgres without a signing secret")
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
