package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kanatabi/lumen/internal/role"
	"github.com/kanatabi/lumen/internal/session"
	"github.com/kanatabi/lumen/internal/store"
)

// userCommand is the bootstrap credential verb (moved from cmd/userctl):
// the password arrives via LUMEN_PASSWORD — argv would leak it into shell
// history and process listings; the environment is the honest local
// channel. Upsert keeps token_version stable across password updates.
//
// Role split (2026-11-13): the users table is the ADMIN plane's, so this
// verb targets it — its inline migration applies the ADMIN subset
// (0002/0007), never the content tables. Point --db (or LUMEN_DB_URL) at
// lumen_admin. On legacy dev databases (which carry everything) the
// subset is a recorded no-op and behavior is unchanged.
func userCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "bootstrap/upsert an admin account (argon2id passhash in the ADMIN-plane DB)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			username, _ := cmd.Flags().GetString("username")
			dbURL, _ := cmd.Flags().GetString("db")
			if dbURL == "" {
				dbURL = os.Getenv("LUMEN_DB_URL")
			}
			pw := os.Getenv("LUMEN_PASSWORD")
			if pw == "" {
				return fmt.Errorf("LUMEN_PASSWORD required — the password never travels in argv")
			}
			if dbURL == "" {
				return fmt.Errorf("--db or LUMEN_DB_URL required")
			}
			hash, err := session.HashPassword(pw)
			if err != nil {
				return err
			}
			pool, err := store.NewPool(cmd.Context(), dbURL)
			if err != nil {
				return err
			}
			defer pool.Close()
			if _, err := store.MigrateForRole(context.Background(), pool, role.Admin); err != nil {
				return err
			}
			if err := store.NewPG(pool).UpsertUser(cmd.Context(), username, hash); err != nil {
				return err
			}
			fmt.Printf("user %q upserted (argon2id passhash written; role=admin; token_version preserved)\n", username)
			return nil
		},
	}
	cmd.Flags().String("username", "fzr", "account name (single-user portal today)")
	cmd.Flags().String("db", "", "postgres:// connection string (default: LUMEN_DB_URL)")
	return cmd
}
