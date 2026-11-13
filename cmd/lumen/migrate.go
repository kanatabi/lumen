package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kanatabi/lumen/internal/queue"
	"github.com/kanatabi/lumen/internal/role"
	"github.com/kanatabi/lumen/internal/store"
)

// migrateCommand is the CANONICAL cluster schema bootstrap: bring the DB to
// the latest schema FOR ITS ROLE and nothing else. Vortex runs it as an Argo
// PreSync Job per plane — lumen-admin migrates lumen_admin (users/session
// lineage ONLY), lumen-staging/production migrate their content DBs (which
// must NEVER grow a users table — the role split ruling, 2026-11-13). The
// river queue tables ride the cms plane only; the admin plane runs no queue.
//
// --role (or LUMEN_ROLE) decides the subset; unset = the legacy "all"
// (today's dev truth: everything, including the IF-NOT EXISTS no-op of the
// relocated users table on databases that created it under the original
// 0001 — the ledger tracks NAMES, applied files never re-run).
//
// Idempotent by design: cheap no-op when current, safe to re-run every sync.
// Prints versions/names only — no data, no values; a migration log that
// echoes row content is not a migration log.
func migrateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "bring the database schema to the latest version FOR THE ROLE (idempotent)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dbURL, _ := cmd.Flags().GetString("db")
			if dbURL == "" {
				dbURL = os.Getenv("LUMEN_DB_URL")
			}
			if dbURL == "" {
				return fmt.Errorf("migrate: db_url (LUMEN_DB_URL) required")
			}
			roleFlag, _ := cmd.Flags().GetString("role")
			if roleFlag == "" {
				roleFlag = os.Getenv("LUMEN_ROLE")
			}
			rt, err := role.Parse(roleFlag)
			if err != nil {
				return err
			}
			pool, err := store.NewPool(cmd.Context(), dbURL)
			if err != nil {
				return err
			}
			defer pool.Close()
			ctx := context.Background()
			n, err := store.MigrateForRole(ctx, pool, rt)
			if err != nil {
				return fmt.Errorf("store migrations: %w", err)
			}
			msg := fmt.Sprintf("role=%s: store migrations (%d files in subset)", rt, n)
			if rt.Planes().Has(role.PlaneCMS) {
				if err := queue.MigrateUp(ctx, pool); err != nil {
					return fmt.Errorf("queue migrations: %w", err)
				}
				msg += " + queue (cms surface)"
			} else {
				msg += "; queue tables SKIPPED — the admin plane runs no queue by design"
			}
			fmt.Println("schema up to date (" + msg + ")")
			return nil
		},
	}
	cmd.Flags().String("db", "", "postgres:// connection string (default: LUMEN_DB_URL)")
	cmd.Flags().String("role", "", `plane subset: "admin" | "cms" (default: LUMEN_ROLE, else legacy "all")`)
	return cmd
}
