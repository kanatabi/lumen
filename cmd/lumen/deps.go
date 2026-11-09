package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanatabi/lumen/internal/config"
	"github.com/kanatabi/lumen/internal/queue"
	"github.com/kanatabi/lumen/internal/search"
	"github.com/kanatabi/lumen/internal/store"
)

// dependencies is the shared world every process role needs: config, the
// truth store, the read-path engine, and the queue client. Building it is
// the same for server, worker, and all — what the ROLES do with it is
// deliberately not shared (one run* function per process, no flag-pairs).
type dependencies struct {
	config      config.Config
	pool        *pgxpool.Pool
	store       *store.PG
	engine      *search.Service
	queueClient *queue.Client
}

// initDependencies loads config and connects/converges the shared world.
// Fail-fast per doctrine: absent infra is a boot error, never a runtime
// surprise. The session secret is NOT required here — that is a
// server-role requirement (Config.ValidateServer), because the worker
// mints no sessions and must boot without the signing key.
func initDependencies(ctx context.Context) (*dependencies, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	pool, err := store.NewPool(ctx, cfg.DBURL)
	if err != nil {
		return nil, fmt.Errorf("db pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db ping: %w", err)
	}
	if cfg.MeiliURL == "" {
		pool.Close()
		return nil, fmt.Errorf("LUMEN_MEILI_URL required: Meilisearch is the read path (§4 ruling)")
	}
	engine := search.New(cfg.MeiliURL, cfg.MeiliKey)
	bootCtx, bootCancel := context.WithTimeout(ctx, 15*time.Second)
	defer bootCancel()
	if err := engine.Ensure(bootCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("meili bootstrap: %w", err)
	}
	queueClient, err := queue.New(bootCtx, pool, store.NewPG(pool), engine)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("queue: %w", err)
	}
	log.Print("deps: postgres + meilisearch + river client ready")
	return &dependencies{config: cfg, pool: pool, store: store.NewPG(pool), engine: engine, queueClient: queueClient}, nil
}

func (d *dependencies) Close() { d.pool.Close() }
