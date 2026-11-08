// Package store is the read boundary: a Source yields PUBLISHED content only
// (lumen §1 rule — the read API returns published-only, always; for Postgres
// that is WHERE status='published', enforced below so a draft can never leak).
// All §1 subtlety (facets, sort whitelist, relevance, suggest/search split,
// pagination) lives in the service above, tested once against the in-memory
// Source; the Postgres Source is the thin real-data side of that seam.
package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanatabi/lumen/internal/domain"
)

// ErrNotFound surfaces as HTTP 404 (unknown slug, unset featured photo).
var ErrNotFound = errors.New("not found")

// NewPool is the ONLY way this codebase opens Postgres: the prepared-statement
// doctrine is ASSERTED here rather than inherited from driver defaults —
// every Query/Exec then goes through a per-connection cached prepared
// statement (values never touch SQL text; the cache also removes the per-call
// parse round-trip). The single sanctioned exception: multi-statement DDL
// migrations (lumen AGENTS "SQL doctrine").
func NewPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	// The driver DEFAULT is already CacheStatement; this assignment is not
	// behavior, it is the doctrine made explicit — a future dependency bump
	// cannot silently drop us to unprepared execution.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	return pgxpool.NewWithConfig(ctx, cfg)
}

// Source supplies the curation-ordered content the service filters and shapes.
type Source interface {
	// PublishedPosts in CURATION order (rank), fully hydrated with tags.
	PublishedPosts(ctx context.Context) ([]domain.Detail, error)
	// Photos in curation order, gallery/featured flags populated (the service
	// filters to gallery=true for §1 — kept here so ordering is the source's).
	Photos(ctx context.Context) ([]domain.Photo, error)
	// TagCloud in curated order (counts + sequence decided BE-side, §1).
	TagCloud(ctx context.Context) ([]domain.Tag, error)
}
