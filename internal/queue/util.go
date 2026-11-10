// Package queue is the async indexing pipeline (lumen §4's serving-model
// ruling): river (Postgres tables — jobs commit in the SAME transaction as
// the data they describe, so "row written but index job lost" is
// structurally impossible and no Redis joins the budget). Jobs carry IDS
// only; the worker re-reads current truth at run time, so retries converge
// and a vanished/soft-deleted row deletes its doc — the index reconciles
// toward Postgres, never away from it.
//
// One file per job type (owner structure ruling): each index_*.go owns its
// args, worker, and verbs; util.go owns what is genuinely shared.
package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"github.com/kanatabi/lumen/internal/search"
)

// Store + Engine: the small seams the workers depend on (testable workers,
// honest interfaces — truth and index, respectively).
type Store interface {
	PostDocByID(ctx context.Context, id int64) (search.PostDoc, bool, error)
	PhotoDocByID(ctx context.Context, id int64) (search.PhotoDoc, bool, error)
	TagDocByID(ctx context.Context, id int64) (search.TagDoc, bool, error)
}

type Engine interface {
	IndexPosts(ctx context.Context, docs []search.PostDoc) error
	DeletePosts(ctx context.Context, ids []int64) error
	IndexPhotos(ctx context.Context, docs []search.PhotoDoc) error
	DeletePhotos(ctx context.Context, ids []int64) error
	IndexTags(ctx context.Context, docs []search.TagDoc) error
	DeleteTags(ctx context.Context, ids []int64) error
}

// Client wraps the river client over the SAME pgxpool as everything else —
// one DB, one truth, jobs included.
type Client struct {
	rc   *river.Client[pgx.Tx]
	pool *pgxpool.Pool
}

func New(ctx context.Context, pool *pgxpool.Pool, store Store, eng Engine) (*Client, error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &indexPostWorker{store: store, engine: eng})
	river.AddWorker(workers, &indexPhotoWorker{store: store, engine: eng})
	river.AddWorker(workers, &indexTagWorker{store: store, engine: eng})

	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 4}},
		Workers: workers,
	})
	if err != nil {
		return nil, fmt.Errorf("river client: %w", err)
	}
	return &Client{rc: rc, pool: pool}, nil
}

func (c *Client) Start(ctx context.Context) error { return c.rc.Start(ctx) }
func (c *Client) Stop(ctx context.Context) error  { return c.rc.Stop(ctx) }

// MigrateUp applies river's own schema through its embedded migrator — the
// queue tables are river's to own; we only invoke it.
func MigrateUp(ctx context.Context, pool *pgxpool.Pool) error {
	driver := riverpgxv5.New(pool)
	migrator, err := rivermigrate.New(driver, nil)
	if err != nil {
		return err
	}
	_, err = migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	return err
}

// pendingUniqueness: burst of edits to one row coalesce to one job —
// idempotent work needs no queue spam. river validates the full
// non-terminal state set at insert (learned from its own error).
func pendingUniqueness() river.UniqueOpts {
	return river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStateScheduled,
		rivertype.JobStateRunning, rivertype.JobStateRetryable,
		rivertype.JobStatePending,
	}}
}

// Drain blocks until no job remains in a non-terminal state (seed, reindex,
// and the engine-tier tests settle deterministically before asserting).
func (c *Client) Drain(ctx context.Context) error {
	for {
		var n int
		if err := c.pool.QueryRow(ctx,
			`SELECT count(*) FROM river_job
			 WHERE state NOT IN ('completed','cancelled','discarded')`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
