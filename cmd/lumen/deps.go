package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanatabi/lumen/internal/config"
	"github.com/kanatabi/lumen/internal/queue"
	"github.com/kanatabi/lumen/internal/role"
	"github.com/kanatabi/lumen/internal/search"
	"github.com/kanatabi/lumen/internal/service"
	"github.com/kanatabi/lumen/internal/store"
)

// dependencies is the shared world per process role — and since the role
// split (2026-11-13) the worlds DIFFER: the admin plane needs only its
// credential DB (no Meilisearch, no queue, no content reads — those
// tables are not even in its schema), while the cms plane keeps the full
// content world (engine with CONFIGURED index names, river queue, tag-doc
// backfill). Building is one honest switch, not a pile of nil checks.
type dependencies struct {
	config      config.Config
	pool        *pgxpool.Pool
	store       *store.PG
	engine      *search.Service // nil on the admin plane (honest absence)
	queueClient *queue.Client   // nil on the admin plane (no queue there)
}

// initDependencies loads config and converges the role's world.
// Fail-fast per doctrine: absent infra is a boot error, never a runtime
// surprise. The session secret is NOT required here — that is a
// server-role requirement (Config.ValidateServer), and the plane switch
// says WHICH secret a server may demand.
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

	pg := store.NewPG(pool) // ONE truth wrapper — the queue client and every
	// role share this exact instance, not parallel constructions
	deps := &dependencies{config: cfg, pool: pool, store: pg}

	if cfg.Role == role.Admin {
		// the control plane by ruling: its database is lumen_admin and
		// holds ONLY the users/session lineage; reaching for meili/river
		// here would BOOT-FAIL honestly rather than half-build a cms
		log.Print("deps: admin plane — postgres only (no engine, no queue, no content surface)")
		return deps, nil
	}

	if cfg.MeiliURL == "" {
		pool.Close()
		return nil, fmt.Errorf("LUMEN_MEILI_URL required: Meilisearch is the read path (§4 ruling)")
	}

	bootCtx, bootCancel := context.WithTimeout(ctx, 15*time.Second)
	defer bootCancel()

	// index names ride CONFIG (role split): dev defaults keep the loop
	// byte-identical; cluster planes may name their engine's indexes
	engine := search.NewWithIndexes(cfg.MeiliURL, cfg.MeiliKey, cfg.MeiliIndexPosts, cfg.MeiliIndexPhotos)
	if err := engine.Ensure(bootCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("meili bootstrap: %w", err)
	}

	queueClient, err := queue.New(bootCtx, pool, pg, engine)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("queue: %w", err)
	}

	// Tags docs: replace-synchronous at boot (vocabulary is ~dozens of rows;
	// the engine call is milliseconds). Idempotent by construction — every
	// boot re-upserts {id,name} for every row, which ALSO self-heals any
	// vocabulary doc lost between boots. Runtime deltas ride index_tag jobs.
	// (ruling 2026-11-10: tag matching is the engine's.)
	tdocs, err := pg.ListAllTagDocs(bootCtx)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("tag docs list: %w", err)
	}
	if err := engine.IndexTags(bootCtx, tdocs); err != nil {
		pool.Close()
		return nil, fmt.Errorf("tag docs backfill: %w", err)
	}

	log.Printf("deps: postgres + meilisearch (indexes %q/%q) + river client ready",
		cfg.MeiliIndexPosts, cfg.MeiliIndexPhotos)

	deps.engine = engine
	deps.queueClient = queueClient
	return deps, nil
}

func (d *dependencies) Close() { d.pool.Close() }

// ping is the admin plane's whole readiness story (truth only).
func (d *dependencies) ping(ctx context.Context) error { return d.pool.Ping(ctx) }

// engineForService avoids the typed-nil trap: a nil *search.Service
// wrapped into the Engine interface would be non-nil to the service
// layer, so the admin plane hands a true nil and reads nothing.
func (d *dependencies) engineForService() service.Engine {
	if d.engine == nil {
		return nil
	}
	return d.engine
}
