package queue

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/kanatabi/lumen/internal/obs"
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
func (w *indexPostWorker) Work(ctx context.Context, job *river.Job[IndexPostArgs]) (err error) {
	logger := jobLogger(job)
	started := time.Now()
	defer func() { obs.ObserveJob(job.Kind, started, err) }()
	// the metrics mirror the log lines (queue_job_total / queue_job_duration)
	defer logger.Debug("job.done", "kind", "index_post")
	doc, found, err := w.store.PostDocByID(ctx, job.Args.PostID)
	if err != nil {
		logger.Error("job.failed", "err", err)
		return err
	}
	if !found {
		logger.Info("job.delete", "target", job.Args.PostID)
		return w.engine.DeletePosts(ctx, []int64{job.Args.PostID})
	}
	return w.engine.IndexPosts(ctx, []search.PostDoc{doc})
}

// EnqueueIndexPost: the out-of-transaction path (seed/reconcile tools).
func (c *Client) EnqueueIndexPost(ctx context.Context, postID int64) error {
	_, err := c.rc.Insert(ctx, IndexPostArgs{PostID: postID}, insertOpts(ctx))
	return err
}

// InsertIndexPostTx: the request-path form — job commits INSIDE the caller's
// transaction (the §2 publish unit of work made atomic).
func (c *Client) InsertIndexPostTx(ctx context.Context, tx pgx.Tx, postID int64) error {
	_, err := c.rc.InsertTx(ctx, tx, IndexPostArgs{PostID: postID}, nil)
	return err
}
