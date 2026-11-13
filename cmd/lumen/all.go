package main

import (
	"context"
	"errors"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/kanatabi/lumen/internal/role"
)

// allCommand: server + worker in ONE process — the dev-loop convenience
// (and today's container default). No new logic by design: it composes the
// two roles' lifecycle primitives, so what runs in dev is literally what
// runs split in prod. Post role split it is the LEGACY all plane:
// LUMEN_ROLE must be unset/all; splitting IS the server/worker verbs.
func allCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "all",
		Short: "serve HTTP AND consume the index queue (legacy dev loop; refuses a split LUMEN_ROLE)",
		RunE:  func(*cobra.Command, []string) error { return runAll() },
	}
}

func runAll() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	deps, err := initDependencies(ctx)
	if err != nil {
		return err
	}
	defer deps.Close()
	if deps.config.Role != role.All {
		return errors.New(`lumen all is the legacy dev plane (LUMEN_ROLE unset/"all"); ` +
			`with a split role deploy "lumen server" (+ "lumen worker") — the planes are separate on purpose`)
	}
	if err := deps.config.ValidateServer(); err != nil {
		return err
	}
	if err := startConsumers(deps); err != nil {
		return err
	}
	srv := startHTTP(deps)

	<-ctx.Done()
	log.Print("all: shutdown signal — draining both roles")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(10)*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return stopConsumers(deps)
}
