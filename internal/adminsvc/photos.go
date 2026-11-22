package adminsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kanatabi/lumen/internal/httpx"
	"github.com/kanatabi/lumen/internal/media"
	"github.com/kanatabi/lumen/internal/search"
	"github.com/kanatabi/lumen/internal/store"
)

func (s *Service) ListPhotos(ctx context.Context, opts store.PhotoListOpts) (int, []store.AdminPhoto, error) {
	if opts.Q != "" {
		return s.deskSearchPhotos(ctx, opts)
	}
	return s.store.ListAdminPhotos(ctx, opts)
}

// deskSearchPhotos: the engine door for ?q — collection/gallery facets ride
// along; the DATE WINDOW cannot (PhotoDoc carries no day attribute), so the
// UI disables window chips while a query is active — visible honesty beats
// silently ignored filters. Relevance order + full-match total.
func (s *Service) deskSearchPhotos(ctx context.Context, opts store.PhotoListOpts) (int, []store.AdminPhoto, error) {
	page := max(opts.Page, 1)
	size := opts.PageSize
	if size <= 0 {
		size = 24
	}
	pq, err := s.engine.AdminPhotosByQuery(ctx, search.PhotoQuery{
		Q: opts.Q, Collections: opts.Collections, Gallery: opts.Gallery,
		Offset: (page - 1) * size, Limit: size,
	})
	if err != nil {
		return 0, nil, err
	}
	rows, err := s.store.AdminPhotoRowsByIDs(ctx, pq.IDs)
	if err != nil {
		return 0, nil, err
	}
	out := make([]store.AdminPhoto, 0, len(pq.IDs))
	for _, id := range pq.IDs {
		if p, ok := rows[id]; ok {
			out = append(out, p)
		}
	}
	return int(pq.Total), out, nil
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

// gcAction — the delete's object-GC verdict (owner call 2026-11-22: the
// eager best-effort object delete amends §8's keep-until-GC line).
type gcAction int

const (
	gcRowOnly    gcAction = iota // no media store wired (dev/pre-M5): row-only
	gcUnresolved                 // no ledger row resolves the src — nothing to delete
	gcShared                     // sibling photo rows cite the same object — it stays
	gcDelete                     // sole owner — delete the object
)

// gcDecide is PURE on purpose: the delete's whole decision table rides the
// offline gate; the DB resolution (store.GetPhotoGC) and the S3 call ride
// the integration walk.
func gcDecide(mediaWired bool, path string, siblings int) gcAction {
	switch {
	case !mediaWired:
		return gcRowOnly
	case path == "":
		return gcUnresolved
	case siblings > 0:
		return gcShared
	default:
		return gcDelete
	}
}

// DeletePhoto — row FIRST (the request's contract; the worker's truth-read
// converges the doc), then the object GC as a BEST-EFFORT epilogue: the
// media_assets ledger resolves the key, a sibling row sharing the
// content-addressed object keeps it alive, and ANY gc outcome — failure
// included — is logged and never fails the request (the library row is
// already gone; a red status would lie about what happened).
func (s *Service) DeletePhoto(ctx context.Context, id int64) error {
	gc, err := s.store.GetPhotoGC(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNoPhoto) {
		return err
	}
	if err := s.store.DeleteAdminPhoto(ctx, id, s.queue); err != nil {
		return err
	}
	switch gcDecide(s.media != nil, gc.Path, gc.Siblings) {
	case gcDelete:
		if err := s.media.Delete(ctx, gc.Path); err != nil {
			// the S3 error code is the WHOLE diagnosis (the put's lesson,
			// 2026-11-15); the payload never logs, the code always does
			httpx.LoggerCtx(ctx).Error("media.delete.failed",
				"op", "photo.gc", "bucket", s.mediaBucket, "key", gc.Path, "err", err)
		}
	case gcShared:
		httpx.LoggerCtx(ctx).Info("photo.gc.shared",
			"op", "photo.gc", "id", id, "key", gc.Path, "siblings", gc.Siblings)
	case gcUnresolved:
		httpx.LoggerCtx(ctx).Warn("photo.gc.unresolved",
			"op", "photo.gc", "id", id, "src", gc.Src)
	}
	return nil
}

// photoExifKeys — the CURATED capture set (owner sketch, Round B
// 2026-11-22). GPS is NOT a key: the privacy promise ("GPS never stored")
// is denied by CONSTRUCTION — an allowlist, not a denylist (a denylist
// grows typos; an allowlist grows nothing).
var photoExifKeys = map[string]string{
	"takenAt":  "string",
	"camera":   "string",
	"lens":     "string",
	"shutter":  "string",
	"iso":      "number",
	"aperture": "number",
}

// SanitizePhotoExif keeps ONLY the curated keys with the expected JSON
// shape (encoding/json numbers = float64, its only shape); everything
// else — unknown keys, wrong types, empty strings, the GPS family —
// drops. Malformed JSON is an error (the 400 story); an empty or fully-
// dropped set stores NULL (the panel's empty state).
func SanitizePhotoExif(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var full map[string]any
	if err := json.Unmarshal(raw, &full); err != nil {
		return nil, fmt.Errorf("exif field: %w", err)
	}
	out := make(map[string]any, len(photoExifKeys))
	for k, want := range photoExifKeys {
		v, ok := full[k]
		if !ok {
			continue
		}
		switch want {
		case "string":
			s, ok := v.(string)
			if !ok || s == "" {
				continue
			}
			out[k] = s
		case "number":
			f, ok := v.(float64)
			if !ok {
				continue
			}
			out[k] = f
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// UploadPhoto is the §5 relay as a use case: probe (decode-and-REBUILD —
// EXIF dies by construction), content-addressed key SCOPED by the config
// media prefix (LUMEN_MEDIA_PREFIX: applied ONCE here so the object put,
// the returned URL and the stored row all carry the same prefixed key —
// base+prefix+key stays coherent by construction), PUT BEFORE anything
// is public, then row + GC ledger + index job in one transaction. The
// handler keeps the HTTP shape (multipart, MaxBytesReader); everything
// else is here. The round's addition (Round B, 2026-11-22): exifRaw is
// mellon's CLIENT-SIDE capture (exifr, curated fields) — lumen never
// parses image metadata (the strip promise is test-pinned), it only
// ALLOWLISTS what it is willing to store (GPS cannot pass: not a key).
func (s *Service) UploadPhoto(ctx context.Context, filename string, raw, exifRaw []byte) (store.AdminPhoto, error) {
	if s.media == nil {
		return store.AdminPhoto{}, ErrNoMediaStore
	}
	exif, err := SanitizePhotoExif(exifRaw)
	if err != nil {
		return store.AdminPhoto{}, &ExifRejected{Err: err}
	}
	processed, err := media.Process(raw)
	if err != nil {
		return store.AdminPhoto{}, &UploadRejected{Err: err}
	}
	processed.Key = media.WithPrefix(s.mediaPrefix, processed.Key)
	if err := s.media.EnsureBucket(ctx); err != nil {
		httpx.LoggerCtx(ctx).Error("media.bucket.failed",
			"op", "photo.put", "bucket", s.mediaBucket, "err", err)
		return store.AdminPhoto{}, err
	}
	if err := s.media.Put(ctx, processed.Key, processed.ContentType, processed.Data); err != nil {
		// the S3 error code is the WHOLE diagnosis (InvalidRegionName
		// walked 2026-11-15 through a probe pod because this line was
		// silent); payloads never log, error codes always do
		httpx.LoggerCtx(ctx).Error("media.put.failed",
			"op", "photo.put", "bucket", s.mediaBucket,
			"key", processed.Key, "err", err)
		return store.AdminPhoto{}, err
	}
	src := media.URL(s.mediaCDNBase, s.mediaEndpoint, s.mediaBucket, processed.Key)
	row, err := s.store.CreateAdminPhotoFromUpload(ctx, src, filenameToAlt(filename),
		processed.Width, processed.Height, processed.SHA256, processed.Key, exif, s.queue)
	if err != nil {
		return store.AdminPhoto{}, err
	}
	return row.Admin(), nil
}

// UploadRejected wraps the probe's verdict: the handler turns it into 415.
type UploadRejected struct{ Err error }

func (e *UploadRejected) Error() string { return e.Err.Error() }
func (e *UploadRejected) Unwrap() error { return e.Err }

// ExifRejected wraps a malformed exif field: a CLIENT bug (mellon formats
// the field), so a 400 — never a silent swallow (visible honesty beats
// ignored input).
type ExifRejected struct{ Err error }

func (e *ExifRejected) Error() string { return e.Err.Error() }
func (e *ExifRejected) Unwrap() error { return e.Err }

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
