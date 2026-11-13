package main

import (
	"context"
	"errors"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// workerCommand: the index-queue consumer process — a CMS-plane verb
// (role split 2026-11-13: the admin plane has no queue tables in its
// database by classification, so `worker` with LUMEN_ROLE=admin is a
// deployment mistake and refuses at boot, not at first job). No HTTP,
// no session secret — its whole scope is: keep the projection converging.
func workerCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "worker",
		Short: "consume the index queue only (cms plane); no HTTP server",
		RunE:  func(*cobra.Command, []string) error { return runWorker() },
	}
}

func runWorker() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	deps, err := initDependencies(ctx)
	if err != nil {
		return err
	}
	defer deps.Close()
	if deps.queueClient == nil {
		return errors.New("worker: no queue on the admin plane (LUMEN_ROLE=admin) — index jobs are the cms plane's projection story")
	}
	if err := startConsumers(deps); err != nil {
		return err
	}

	<-ctx.Done()
	log.Print("worker: shutdown signal — draining")
	return stopConsumers(deps)
}

// startConsumers / stopConsumers are the reusable worker lifecycle
// primitives (runAll composes them with the HTTP ones).
func startConsumers(deps *dependencies) error {
	if err := deps.queueClient.Start(context.Background()); err != nil {
		return err
	}

	log.Print("worker: queue consumer running")
	return nil
}

func stopConsumers(deps *dependencies) error {
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return deps.queueClient.Stop(stopCtx)
}
