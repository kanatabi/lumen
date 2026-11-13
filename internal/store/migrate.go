package store

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanatabi/lumen/internal/role"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationRoles is the CLASSIFICATION TABLE (role-split ruling,
// 2026-11-13): every migration file mapped to the planes that apply it.
// The rule is OWNERSHIP, not vocabulary: users/session lineage → admin
// ONLY; posts/media/tags/collections → cms ONLY; the queue's river tables
// ride cms (decided by the caller — the admin plane runs no queue). A
// file missing from this table is an ERROR, never a silent skip: "map
// every file" is enforced by the runner itself, tested by
// migrate_manifest_test against the embedded FS.
//
// 0003_admin.sql is named from the M3 era — it ALTERS posts/photos, so it
// classifies cms; the name is history, the table is truth.
var migrationRoles = map[string]role.Planes{
	"0001_core.sql":           role.PlaneCMS, // content tables; users block MOVED to 0002
	"0002_admin_users.sql":    role.PlaneAdmin,
	"0003_admin.sql":          role.PlaneCMS, // historical name — posts/photos soft-delete
	"0004_media.sql":          role.PlaneCMS,
	"0005_tag_featured.sql":   role.PlaneCMS,
	"0006_tag_timestamps.sql": role.PlaneCMS,
	"0007_users_roles.sql":    role.PlaneAdmin, // layers role/must_change onto 0002's table
}

// Migrate applies the FULL schema (both planes) — the legacy "all" path,
// kept as the honest default for dev and the existing batteries.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := MigrateForRole(ctx, pool, role.All)
	return err
}

// MigrateForRole applies the role's subset in filename order and returns
// the subset size (the verb prints what it covered — names of files,
// never contents, per the migration-log doctrine).
func MigrateForRole(ctx context.Context, pool *pgxpool.Pool, rt role.Role) (int, error) {
	planes := rt.Planes()
	if _, err := pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS _migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return 0, err
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return 0, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		own, ok := migrationRoles[e.Name()]
		if !ok {
			return 0, fmt.Errorf("migration %s: UNCLASSIFIED — add it to migrationRoles (the role split admits no silent skips)", e.Name())
		}
		if !own.Has(planes) {
			continue // belongs to the other plane's database
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM _migrations WHERE name=$1)`, name).Scan(&exists); err != nil {
			return len(names), err
		}
		if exists {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return len(names), err
		}
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, stripCommentsForSingleCall(string(body))); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			_, err := tx.Exec(ctx, `INSERT INTO _migrations(name) VALUES ($1)`, name)
			return err
		})
		if err != nil {
			return len(names), err
		}
	}
	return len(names), nil
}

// MigrationClassification exposes the table for `lumen migrate` output and
// the honesty tests — consumers read the SAME map the runner enforces.
func MigrationClassification() map[string]role.Planes {
	out := make(map[string]role.Planes, len(migrationRoles))
	for k, v := range migrationRoles {
		out[k] = v
	}
	return out
}

// pgx's Exec runs a multi-statement string fine via the simple protocol when
// it contains $-free DDL; comments with semicolons never split here because we
// pass the whole file as one Exec. This helper just guards against trailing
// whitespace surprises.
func stripCommentsForSingleCall(sql string) string {
	return strings.TrimSpace(sql)
}
