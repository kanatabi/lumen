package httpapi

// POST /admin/photos — the multipart upload relay (§5/§8, D1-revised):
// decode-and-rebuild probe (EXIF stripped by construction), content-addressed
// PUT to S3-compatible storage BEFORE anything is public, then the row +
// index job commit in one transaction. Fresh photos: gallery=false,
// collection=null — surfacing is a deliberate act (mellon doctrine).

import (
	"io"
	"net/http"

	"github.com/kanatabi/lumen/internal/media"
)

func (s *Server) adminUploadPhoto(w http.ResponseWriter, r *http.Request) {
	if s.admin.Media == nil {
		writeErr(w, http.StatusServiceUnavailable, "media store not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, media.MaxBytes+1<<20)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "multipart form 'photo' required")
		return
	}
	file, header, err := r.FormFile("photo")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "field 'photo' missing")
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, media.MaxBytes+1))
	if err != nil || len(raw) > media.MaxBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "upload exceeds 20 MiB")
		return
	}

	proc, err := media.Process(raw)
	if err != nil {
		writeErr(w, http.StatusUnsupportedMediaType, err.Error())
		return
	}
	if err := s.admin.Media.EnsureBucket(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.admin.Media.Put(r.Context(), proc.Key, proc.ContentType, proc.Data); err != nil {
		s.fail(w, err)
		return
	}
	src := media.URL(s.admin.MediaCDNBase, s.admin.MediaEndpoint, s.admin.MediaBucket, proc.Key)
	row, err := s.admin.Pool.CreateAdminPhotoFromUpload(r.Context(), src, altFromName(header.Filename),
		proc.Width, proc.Height, proc.SHA256, proc.Key, s.admin.Queue)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, adminPhotoJSON(row))
}

// altFromName: filename-derived starting alt — the author edits it on the
// detail page (mellon's picker names uploads; truth stays human).
func altFromName(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' || name[i] == '\\' {
			name = name[i+1:]
			break
		}
	}
	if len(name) > 4 {
		if dot := lastDot(name); dot > 0 {
			name = name[:dot]
		}
	}
	return name
}

func lastDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}
