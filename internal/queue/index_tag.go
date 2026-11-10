package queue

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/kanatabi/lumen/internal/search"
)

// IndexTagArgs — the vocabulary doc job. ONE kind converges both directions:
// the worker reads CURRENT truth at run time, so a tag whose row is gone
// (desk delete) DELETES the doc, and create/rename upserts it. Same
// retry-coalescing contract as index_post — no second job type needed.
type IndexTagArgs struct {
	TagID int64 `json:"tag_id"`
}

func (IndexTagArgs) Kind() string { return "index_tag" }

type indexTagWorker struct {
	river.WorkerDefaults[IndexTagArgs]
	store  Store
	engine Engine
}

func (w *indexTagWorker) Work(ctx context.Context, job *river.Job[IndexTagArgs]) error {
	doc, found, err := w.store.TagDocByID(ctx, job.Args.TagID)
	if err != nil {
		return err
	}
	if !found {
		return w.engine.DeleteTags(ctx, []int64{job.Args.TagID})
	}
	return w.engine.IndexTags(ctx, []search.TagDoc{doc})
}

// EnqueueIndexTag: the out-of-transaction path (backfill tooling).
func (c *Client) EnqueueIndexTag(ctx context.Context, tagID int64) error {
	_, err := c.rc.Insert(ctx, IndexTagArgs{TagID: tagID}, &river.InsertOpts{
		UniqueOpts: pendingUniqueness(),
	})
	return err
}

// InsertIndexTagTx: the request-path form — the job commits inside the
// caller's transaction (vocabulary writes and their index intent are ONE
// unit of work, §2 doctrine).
func (c *Client) InsertIndexTagTx(ctx context.Context, tx pgx.Tx, tagID int64) error {
	_, err := c.rc.InsertTx(ctx, tx, IndexTagArgs{TagID: tagID}, nil)
	return err
}
