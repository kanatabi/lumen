package queue

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/kanatabi/lumen/internal/search"
)

type IndexPhotoArgs struct {
	PhotoID int64 `json:"photo_id"`
}

func (IndexPhotoArgs) Kind() string { return "index_photo" }

type indexPhotoWorker struct {
	river.WorkerDefaults[IndexPhotoArgs]
	store  Store
	engine Engine
}

func (w *indexPhotoWorker) Work(ctx context.Context, job *river.Job[IndexPhotoArgs]) error {
	logger := jobLogger(job)
	defer logger.Debug("job.done", "kind", "index_photo")
	doc, found, err := w.store.PhotoDocByID(ctx, job.Args.PhotoID)
	if err != nil {
		logger.Error("job.failed", "err", err)
		return err
	}
	if !found {
		logger.Info("job.delete", "target", job.Args.PhotoID)
		return w.engine.DeletePhotos(ctx, []int64{job.Args.PhotoID})
	}
	return w.engine.IndexPhotos(ctx, []search.PhotoDoc{doc})
}

func (c *Client) EnqueueIndexPhoto(ctx context.Context, photoID int64) error {
	_, err := c.rc.Insert(ctx, IndexPhotoArgs{PhotoID: photoID}, insertOpts(ctx))
	return err
}

func (c *Client) InsertIndexPhotoTx(ctx context.Context, tx pgx.Tx, photoID int64) error {
	_, err := c.rc.InsertTx(ctx, tx, IndexPhotoArgs{PhotoID: photoID}, nil)
	return err
}
