// Package httpx holds the transport primitives shared by the public and
// admin API packages: JSON writing, the error ladder, and query parsing.
// Rules live in services; this file only moves bytes.
package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/kanatabi/lumen/internal/domain"
	"github.com/kanatabi/lumen/internal/search"
	"github.com/kanatabi/lumen/internal/store"
)

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func WriteErr(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]any{"error": msg})
}

// Fail maps the store/engine error ladder onto statuses. Anything unmapped
// is a 500 whose real error is worth a server-side log line, not a client leak.
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	var pe domain.ErrParam
	switch {
	case errors.As(err, &pe):
		WriteErr(w, http.StatusBadRequest, pe.Error())
	case errors.Is(err, store.ErrNotFound):
		WriteErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrNoPosts), errors.Is(err, store.ErrNoPhoto):
		WriteErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, search.ErrUnavailable):
		// pure-engine posture: lists/search 503 when the read path is down
		WriteErr(w, http.StatusServiceUnavailable, "search engine unavailable")
	default:
		_ = r
		WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

// ListQuery parses the §1 query contract (shared: public list/search use it;
// includeQuery admits q for the search faces).
func ListQuery(r *http.Request, includeQuery bool) (domain.ListQuery, error) {
	qy := r.URL.Query()
	limit, err := QueryInt(r, "limit", 0)
	if err != nil {
		return domain.ListQuery{}, err
	}
	offset, err := QueryInt(r, "offset", 0)
	if err != nil {
		return domain.ListQuery{}, err
	}
	lq := domain.ListQuery{
		Offset: offset,
		Limit:  limit,
		Tags:   qy["tag"],
		From:   qy.Get("from"),
		To:     qy.Get("to"),
		Sort:   qy.Get("sort"),
	}
	if includeQuery {
		lq.Q = qy.Get("q")
	}
	if rm, err := qsOptInt(r, "readMin"); err != nil {
		return lq, err
	} else {
		lq.ReadMin = rm
	}
	if rx, err := qsOptInt(r, "readMax"); err != nil {
		return lq, err
	} else {
		lq.ReadMax = rx
	}
	return lq, lq.Normalize()
}

func QueryInt(r *http.Request, key string, def int) (int, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, domain.ErrParam{Msg: key + " must be a non-negative integer"}
	}
	return n, nil
}

func qsOptInt(r *http.Request, key string) (*int, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return nil, domain.ErrParam{Msg: key + " must be a non-negative integer"}
	}
	return &n, nil
}
