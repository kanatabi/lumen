// cmd/userctl is the bootstrap credential CLI (lumen §6/§8, owner decision
// 2026-11-08: credentials are a hashed DB row, created once at bootstrap,
// never a git value nor an env secret). The password arrives via
// LUMEN_PASSWORD — argv would leak it into shell history and process
// listings; the environment is the honest local channel.
package main

import (
	"context"
	"flag"
	"log"
	"os"

	"github.com/kanatabi/lumen/internal/session"
	"github.com/kanatabi/lumen/internal/store"
)

func main() {
	username := flag.String("username", "fzr", "account name (single-user portal today)")
	dbURL := flag.String("db", os.Getenv("LUMEN_DB_URL"), "postgres:// connection string")
	flag.Parse()

	pw := os.Getenv("LUMEN_PASSWORD")
	if pw == "" {
		log.Fatal("LUMEN_PASSWORD required — the password never travels in argv")
	}
	if *dbURL == "" {
		log.Fatal("-db or LUMEN_DB_URL required")
	}
	hash, err := session.HashPassword(pw)
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx, *dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		log.Fatal(err)
	}
	if err := store.NewPG(pool).UpsertUser(ctx, *username, hash); err != nil {
		log.Fatal(err)
	}
	// Upsert keeps token_version stable across password updates — sessions
	// survive a password rotation; explicit revocation is logout's job (§8).
	log.Printf("user %q upserted (argon2id passhash written; token_version preserved)", *username)
}
