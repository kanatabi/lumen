// Package queue is the async indexing pipeline (owner ruling 2026-11-08:
// indexing Meilisearch is never synchronous with the request path).
//
// The library is river (v0.51.1) — chosen over asynq/gocraft/work precisely
// because its queue IS Postgres: jobs commit in the SAME transaction as the
// data they describe, so "row written but index job lost" — the classic
// dual-write failure — is structurally impossible, and no Redis joins the
// 2 GB budget table.
//
// Jobs carry IDS, never payloads: the worker reads current truth at run
// time. Retries therefore converge (five rapid publishes of one post coalesce
// into "whatever the row says now"), and a missing row means DELETE — the
// index reconciles toward Postgres, never away from it.
package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/riverqueue/river/rivermigrate"

	"github.com/kanatabi/lumen/internal/search"
)

// ---- job args ----

type IndexPostArgs struct {
	PostID int64 `json:"post_id"`
}

func (IndexPostArgs) Kind() string { return "index_post" }

type IndexPhotoArgs struct {
	PhotoID int64 `json:"photo_id"`
}

func (IndexPhotoArgs) Kind() string { return "index_photo" }

// ---- dependencies (small interfaces: testable workers, honest seams) ----

type Store interface {
	PostDocByID(ctx context.Context, id int64) (search.PostDoc, bool, error)
	PhotoDocByID(ctx context.Context, id int64) (search.PhotoDoc, bool, error)
}

type Engine interface {
	IndexPosts(ctx context.Context, docs []search.PostDoc) error
	DeletePosts(ctx context.Context, ids []int64) error
	IndexPhotos(ctx context.Context, docs []search.PhotoDoc) error
	DeletePhotos(ctx context.Context, ids []int64) error
}

// ---- workers ----

type indexPostWorker struct {
	river.WorkerDefaults[IndexPostArgs]
	store Store
	eng   Engine
}

func (w *indexPostWorker) Work(ctx context.Context, job *river.Job[IndexPostArgs]) error {
	doc, found, err := w.store.PostDocByID(ctx, job.Args.PostID)
	if err != nil {
		return err
	}
	if !found {
		return w.eng.DeletePosts(ctx, []int64{job.Args.PostID})
	}
	return w.eng.IndexPosts(ctx, []search.PostDoc{doc})
}

type indexPhotoWorker struct {
	river.WorkerDefaults[IndexPhotoArgs]
	store Store
	eng   Engine
}

func (w *indexPhotoWorker) Work(ctx context.Context, job *river.Job[IndexPhotoArgs]) error {
	doc, found, err := w.store.PhotoDocByID(ctx, job.Args.PhotoID)
	if err != nil {
		return err
	}
	if !found {
		return w.eng.DeletePhotos(ctx, []int64{job.Args.PhotoID})
	}
	return w.eng.IndexPhotos(ctx, []search.PhotoDoc{doc})
}

// ---- client ----

type Client struct {
	rc   *river.Client[pgx.Tx]
	pool *pgxpool.Pool
}

// New builds the river client over the SAME pgxpool as everything else —
// one DB, one truth, jobs included.
func New(ctx context.Context, pool *pgxpool.Pool, store Store, eng Engine) (*Client, error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &indexPostWorker{store: store, eng: eng})
	river.AddWorker(workers, &indexPhotoWorker{store: store, eng: eng})

	driver := riverpgxv5.New(pool)
	rc, err := river.NewClient(driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 4},
		},
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
	res, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	if err != nil {
		return err
	}
	for _, v := range res.Versions {
		_ = v // applied silently; the ledger table is river's record
	}
	return nil
}

// EnqueueIndexPost: the fire-and-forget path for out-of-transaction writes.
// Unique across pending states: a burst of edits to one post collapses to one
// job — idempotent work needs no queue spam.
func (c *Client) EnqueueIndexPost(ctx context.Context, postID int64) error {
	_, err := c.rc.Insert(ctx, IndexPostArgs{PostID: postID}, &river.InsertOpts{
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			// river requires the full non-terminal set in a uniqueness
			// ByState list (validated at insert time — missing one = error)
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable, rivertype.JobStateScheduled,
				rivertype.JobStateRunning, rivertype.JobStateRetryable,
				rivertype.JobStatePending,
			},
		},
	})
	return err
}

func (c *Client) EnqueueIndexPhoto(ctx context.Context, photoID int64) error {
	_, err := c.rc.Insert(ctx, IndexPhotoArgs{PhotoID: photoID}, &river.InsertOpts{
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			// river requires the full non-terminal set in a uniqueness
			// ByState list (validated at insert time — missing one = error)
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable, rivertype.JobStateScheduled,
				rivertype.JobStateRunning, rivertype.JobStateRetryable,
				rivertype.JobStatePending,
			},
		},
	})
	return err
}

// InsertIndexPostTx is the publish-transaction form (§2's unit of work):
// M3's publish handler calls THIS inside its pgx.Tx — row + job commit
// atomically, which is the whole reason river was chosen.
func (c *Client) InsertIndexPostTx(ctx context.Context, tx pgx.Tx, postID int64) error {
	_, err := c.rc.InsertTx(ctx, tx, IndexPostArgs{PostID: postID}, nil)
	return err
}

func (c *Client) InsertIndexPhotoTx(ctx context.Context, tx pgx.Tx, photoID int64) error {
	_, err := c.rc.InsertTx(ctx, tx, IndexPhotoArgs{PhotoID: photoID}, nil)
	return err
}

// Drain blocks until no job remains in a non-terminal state (used by seed,
// reindex and the engine-tier tests: deterministic settle before assertions).
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
