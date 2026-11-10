package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kanatabi/lumen/internal/queue"
	"github.com/kanatabi/lumen/internal/store"
)

// migrateCommand is the CANONICAL cluster schema bootstrap: bring the DB to
// the latest schema (our SQL migrations + river's own queue tables) and
// nothing else. Vortex runs it as an Argo PreSync Job — every deploy migrates
// first, and a failed migration blocks the rollout instead of crash-looping
// pods (decision 2026-11-10, first live walk: nothing in the deployed path
// created the queue tables; `user`'s inline migration is dev-loop convenience
// only and this verb is the production truth).
//
// Idempotent by design: cheap no-op when current, safe to re-run every sync.
// Prints versions/names only — no data, no values; a migration log that
// echoes row content is not a migration log.
func migrateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "bring the database schema to the latest version (store + queue; idempotent)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dbURL, _ := cmd.Flags().GetString("db")
			if dbURL == "" {
				dbURL = os.Getenv("LUMEN_DB_URL")
			}
			if dbURL == "" {
				return fmt.Errorf("migrate: db_url (LUMEN_DB_URL) required")
			}
			pool, err := store.NewPool(cmd.Context(), dbURL)
			if err != nil {
				return err
			}
			defer pool.Close()
			ctx := context.Background()
			if err := store.Migrate(ctx, pool); err != nil {
				return fmt.Errorf("store migrations: %w", err)
			}
			if err := queue.MigrateUp(ctx, pool); err != nil {
				return fmt.Errorf("queue migrations: %w", err)
			}
			fmt.Println("schema up to date (store + queue)")
			return nil
		},
	}
	cmd.Flags().String("db", "", "postgres:// connection string (default: LUMEN_DB_URL)")
	return cmd
}
