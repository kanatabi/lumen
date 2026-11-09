package queue

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/kanatabi/lumen/internal/search"
)

type IndexPostArgs struct {
	PostID int64 `json:"post_id"`
}

func (IndexPostArgs) Kind() string { return "index_post" }

type indexPostWorker struct {
	river.WorkerDefaults[IndexPostArgs]
	store  Store
	engine Engine
}

// Work reads CURRENT truth at run time: found → upsert the doc; gone or
// soft-deleted → delete the doc. Retries and edit bursts converge either way.
func (w *indexPostWorker) Work(ctx context.Context, job *river.Job[IndexPostArgs]) error {
	doc, found, err := w.store.PostDocByID(ctx, job.Args.PostID)
	if err != nil {
		return err
	}
	if !found {
		return w.engine.DeletePosts(ctx, []int64{job.Args.PostID})
	}
	return w.engine.IndexPosts(ctx, []search.PostDoc{doc})
}

// EnqueueIndexPost: the out-of-transaction path (seed/reconcile tools).
func (c *Client) EnqueueIndexPost(ctx context.Context, postID int64) error {
	_, err := c.rc.Insert(ctx, IndexPostArgs{PostID: postID}, &river.InsertOpts{
		UniqueOpts: pendingUniqueness(),
	})
	return err
}

// InsertIndexPostTx: the request-path form — job commits INSIDE the caller's
// transaction (the §2 publish unit of work made atomic).
func (c *Client) InsertIndexPostTx(ctx context.Context, tx pgx.Tx, postID int64) error {
	_, err := c.rc.InsertTx(ctx, tx, IndexPostArgs{PostID: postID}, nil)
	return err
}
