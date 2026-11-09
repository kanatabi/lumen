package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// allCommand: server + worker in ONE process — the dev-loop convenience
// (and today's container default). No new logic by design: it composes the
// two roles' lifecycle primitives, so what runs in dev is literally what
// runs split in prod.
func allCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "all",
		Short: "serve HTTP AND consume the index queue (the dev loop)",
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
