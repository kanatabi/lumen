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
	doc, found, err := w.store.PhotoDocByID(ctx, job.Args.PhotoID)
	if err != nil {
		return err
	}
	if !found {
		return w.engine.DeletePhotos(ctx, []int64{job.Args.PhotoID})
	}
	return w.engine.IndexPhotos(ctx, []search.PhotoDoc{doc})
}

func (c *Client) EnqueueIndexPhoto(ctx context.Context, photoID int64) error {
	_, err := c.rc.Insert(ctx, IndexPhotoArgs{PhotoID: photoID}, &river.InsertOpts{
		UniqueOpts: pendingUniqueness(),
	})
	return err
}

func (c *Client) InsertIndexPhotoTx(ctx context.Context, tx pgx.Tx, photoID int64) error {
	_, err := c.rc.InsertTx(ctx, tx, IndexPhotoArgs{PhotoID: photoID}, nil)
	return err
}
