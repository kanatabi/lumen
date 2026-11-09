package httpapi

// Shared wire plumbing: response encoding, the error ladder, and request
// decoding (query params → domain.ListQuery). Handlers stay three steps:
// parse → call service → encode.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/kanatabi/lumen/internal/domain"
	"github.com/kanatabi/lumen/internal/search"
	"github.com/kanatabi/lumen/internal/store"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// fail is the ONE error ladder: bad param → 400 with the message (it is the
// user's to see), not found → 404, everything else → a 500 whose message
// never crosses the wire (§9 redaction habit, even for boring things).
func (s *Server) fail(w http.ResponseWriter, err error) {
	var p domain.ErrParam
	switch {
	case errors.As(err, &p):
		writeErr(w, http.StatusBadRequest, p.Msg)
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, search.ErrUnavailable):
		// pure-engine posture: engine down = lists/search 503, loudly — the
		// visible failure the ruling chose over a silent dual path
		writeErr(w, http.StatusServiceUnavailable, "search engine unavailable")
	default:
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

func qsInt(r *http.Request, name string, def int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return 0, domain.ErrParam{Msg: name + " must be a non-negative integer"}
	}
	return v, nil
}

func qsOptInt(r *http.Request, name string) (*int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return nil, domain.ErrParam{Msg: name + " must be a non-negative integer"}
	}
	return &v, nil
}

// listQuery decodes the shared §1 facet grammar (both list and search
// endpoints; includeQuery marks search's needle).
func (s *Server) listQuery(r *http.Request, includeQuery bool) (domain.ListQuery, error) {
	query := r.URL.Query()
	limit, err := qsInt(r, "limit", 0) // absent = unbounded (mock getPosts parity)
	if err != nil {
		return domain.ListQuery{}, err
	}
	offset, err := qsInt(r, "offset", 0)
	if err != nil {
		return domain.ListQuery{}, err
	}
	q := domain.ListQuery{
		Offset: offset,
		Limit:  limit,
		Tags:   query["tag"], // repeated param = OR set (§1)
		From:   query.Get("from"),
		To:     query.Get("to"),
		Sort:   query.Get("sort"),
	}
	if includeQuery {
		q.Q = query.Get("q")
	}
	if rm, err := qsOptInt(r, "readMin"); err != nil {
		return q, err
	} else {
		q.ReadMin = rm
	}
	if rx, err := qsOptInt(r, "readMax"); err != nil {
		return q, err
	} else {
		q.ReadMax = rx
	}
	err = q.Normalize()
	return q, err
}
