package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kanatabi/lumen/internal/session"
	"github.com/kanatabi/lumen/internal/store"
)

// userCommand is the bootstrap credential verb (moved from cmd/userctl):
// the password arrives via LUMEN_PASSWORD — argv would leak it into shell
// history and process listings; the environment is the honest local
// channel. Upsert keeps token_version stable across password updates.
func userCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "bootstrap/upsert an admin account (argon2id passhash in the DB)",
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
			if err := store.Migrate(context.Background(), pool); err != nil {
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
