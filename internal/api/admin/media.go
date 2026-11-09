package admin

// POST /admin/photos — the multipart relay (§5/§8, D1-revised). The HTTP
// SHAPE stays here (multipart parse, size ceiling); the use case — probe,
// strip, S3, row + ledger + index job — is adminsvc.UploadPhoto.

import (
	"errors"
	"io"
	"net/http"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/httpx"
	"github.com/kanatabi/lumen/internal/media"
)

func (h *Handler) uploadPhoto(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, media.MaxBytes+1<<20)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "multipart form 'photo' required")
		return
	}
	file, header, err := r.FormFile("photo")
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "field 'photo' missing")
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, media.MaxBytes+1))
	if err != nil || len(raw) > media.MaxBytes {
		httpx.WriteErr(w, http.StatusRequestEntityTooLarge, "upload exceeds 20 MiB")
		return
	}
	photo, err := h.svc.UploadPhoto(r.Context(), header.Filename, raw)
	var rejected *adminsvc.UploadRejected
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusCreated, photo)
	case errors.Is(err, adminsvc.ErrNoMediaStore):
		httpx.WriteErr(w, http.StatusServiceUnavailable, err.Error())
	case errors.As(err, &rejected):
		httpx.WriteErr(w, http.StatusUnsupportedMediaType, err.Error())
	default:
		httpx.Fail(w, r, err)
	}
}
