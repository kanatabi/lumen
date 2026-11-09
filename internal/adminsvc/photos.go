package adminsvc

import (
	"context"

	"github.com/kanatabi/lumen/internal/media"
	"github.com/kanatabi/lumen/internal/store"
)

func (s *Service) ListPhotos(ctx context.Context, opts store.PhotoListOpts) (int, []store.AdminPhoto, error) {
	return s.store.ListAdminPhotos(ctx, opts)
}
func (s *Service) GetPhotoRow(ctx context.Context, id int64) (store.PhotoDocRow, error) {
	return s.store.GetAdminPhotoRow(ctx, id)
}
func (s *Service) PhotoReferences(ctx context.Context, id int64) ([]store.PhotoRef, error) {
	return s.store.ListPhotoReferences(ctx, id)
}
func (s *Service) PatchPhoto(ctx context.Context, id int64, patch store.PhotoPatch) (store.PhotoDocRow, error) {
	return s.store.UpdateAdminPhoto(ctx, id, patch, s.queue)
}
func (s *Service) DeletePhoto(ctx context.Context, id int64) error {
	return s.store.DeleteAdminPhoto(ctx, id, s.queue)
}

// UploadPhoto is the §5 relay as a use case: probe (decode-and-REBUILD —
// EXIF dies by construction), content-addressed PUT BEFORE anything is
// public, then row + GC ledger + index job in one transaction. The handler
// keeps the HTTP shape (multipart, MaxBytesReader); everything else is here.
func (s *Service) UploadPhoto(ctx context.Context, filename string, raw []byte) (store.AdminPhoto, error) {
	if s.media == nil {
		return store.AdminPhoto{}, ErrNoMediaStore
	}
	processed, err := media.Process(raw)
	if err != nil {
		return store.AdminPhoto{}, &UploadRejected{Err: err}
	}
	if err := s.media.EnsureBucket(ctx); err != nil {
		return store.AdminPhoto{}, err
	}
	if err := s.media.Put(ctx, processed.Key, processed.ContentType, processed.Data); err != nil {
		return store.AdminPhoto{}, err
	}
	src := media.URL(s.mediaCDNBase, s.mediaEndpoint, s.mediaBucket, processed.Key)
	row, err := s.store.CreateAdminPhotoFromUpload(ctx, src, filenameToAlt(filename),
		processed.Width, processed.Height, processed.SHA256, processed.Key, s.queue)
	if err != nil {
		return store.AdminPhoto{}, err
	}
	return row.Admin(), nil
}

// UploadRejected wraps the probe's verdict: the handler turns it into 415.
type UploadRejected struct{ Err error }

func (e *UploadRejected) Error() string { return e.Err.Error() }
func (e *UploadRejected) Unwrap() error { return e.Err }

// filenameToAlt: the filename-derived starting alt — the author edits it on
// the detail page (mellon's picker names uploads; truth stays human).
func filenameToAlt(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' || name[i] == '\\' {
			name = name[i+1:]
			break
		}
	}
	if len(name) > 4 {
		for i := len(name) - 1; i >= 0; i-- {
			if name[i] == '.' {
				return name[:i]
			}
		}
	}
	return name
}
