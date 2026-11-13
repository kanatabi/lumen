// Package search is the read-serving engine (owner ruling, 2026-11-08):
// Meilisearch answers lists, facets and relevance; Postgres is the source of
// truth that results hydrate from. The doctrine made structural:
// there is no Postgres fallback path for search — engine down means
// visible 503, per the pure-engine ruling.
//
// Pins: Meilisearch v1.56.1 (compose), client meilisearch-go v0.36.3.
package search

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	meili "github.com/meilisearch/meilisearch-go"
)

// Index uid DEFAULTS. The role split made index names config —
// LUMEN_MEILI_INDEX_POSTS / LUMEN_MEILI_INDEX_PHOTOS (empty = these), so
// dev and the planes can live in different engine deployments with
// separate index spaces. "tags" stays a constant by ruling: the config
// surface enumerates posts/photos only; the vocabulary index is the one
// shared engine story.
const (
	IndexPosts  = "posts"
	IndexPhotos = "photos"
	IndexTags   = "tags"
)

// PostDoc is the indexed projection of a post. Display truth does NOT live
// here — results hydrate from Postgres (§4); fields exist for search/filter/
// sort only, and `content` is searchable-but-never-returned.
type PostDoc struct {
	ID       int64    `json:"id"`
	Slug     string   `json:"slug"`
	Title    string   `json:"title"`
	Subtitle string   `json:"subtitle"`
	Preview  string   `json:"preview"`
	Tags     []string `json:"tags"`
	Content  string   `json:"content"`
	Status   string   `json:"status"` // draft|published|unpublished — public queries filter it
	Day      int      `json:"day"`    // yyyymmdd int: inclusive from/to windows by integer compare (no date-format ambiguity)
	Date     string   `json:"date"`   // display copy, YYYY-MM-DD
	ReadTime int      `json:"readTime"`
	Rank     int      `json:"rank"` // curation order (§1) — an attribute, so determinism survives the engine
}

// PhotoDoc — the admin-door index (alt/collection searchable; gallery is the
// public filter the service always applies for §1 reads).
type PhotoDoc struct {
	ID         int64   `json:"id"`
	Src        string  `json:"src"`
	Alt        string  `json:"alt"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	Gallery    bool    `json:"gallery"`
	Featured   bool    `json:"featured"`
	Sort       int     `json:"sort"`
	Collection *string `json:"collection"`
}

// TagDoc is the vocabulary's indexed projection — {id, name} and NOTHING
// else (owner ruling 2026-11-10: tag SEARCH rides the engine so typo-
// tolerance is one story everywhere; counts and `featured` deliberately do
// NOT live here — they hydrate from Postgres per response). The engine
// answers "which names does this query resemble"; the DB remains truth.
type TagDoc struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// DayInt converts the editorial YYYY-MM-DD to its sortable/filterable integer.
func DayInt(date string) int {
	d := strings.ReplaceAll(date[:10], "-", "")
	var n int
	for _, r := range d {
		n = n*10 + int(r-'0')
	}
	return n
}

type Service struct {
	client      meili.ServiceManager
	postsIndex  string // from config (role split) — Ensure/index/query all honor it
	photosIndex string
}

// New builds the engine client with the DEFAULT index uids (the offline
// twin wiring and any caller that has no config layer).
func New(url, key string) *Service {
	return NewWithIndexes(url, key, "", "")
}

// NewWithIndexes is the CONFIGURED constructor: empty names fall back to
// the constants, so the dev loop is byte-identical when the keys are unset.
func NewWithIndexes(url, key, posts, photos string) *Service {
	return &Service{
		client:      meili.New(url, meili.WithAPIKey(key)),
		postsIndex:  orDefault(posts, IndexPosts),
		photosIndex: orDefault(photos, IndexPhotos),
	}
}

func orDefault(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

// Ensure bootstraps the indexes idempotently: create-if-missing, then
// settings every boot (harmless re-application is the honest way to keep
// dev-recreated and long-lived engines identical). The NAMES ride config —
// the worker/server bootstrap and the seed all converge here.
func (s *Service) Ensure(ctx context.Context) error {
	indexes := []struct {
		uid      string
		settings *meili.Settings
	}{
		{s.postsIndex, postsSettings()},
		{s.photosIndex, photosSettings()},
		{IndexTags, tagsSettings()},
	}
	for _, ix := range indexes {
		uid := ix.uid
		if _, err := s.client.GetIndex(uid); err != nil {
			if _, err := s.client.CreateIndex(&meili.IndexConfig{Uid: uid, PrimaryKey: "id"}); err != nil &&
				!strings.Contains(err.Error(), "already_exists") {
				return fmt.Errorf("create index %s: %w", uid, err)
			}
			if err := s.waitIndex(ctx, uid); err != nil {
				return err
			}
		}
		task, err := s.client.Index(uid).UpdateSettingsWithContext(ctx, ix.settings)
		if err != nil {
			return fmt.Errorf("settings %s: %w", uid, err)
		}
		if err := s.waitTask(ctx, task); err != nil {
			return err
		}
	}
	return nil
}

func postsSettings() *meili.Settings {
	return &meili.Settings{
		// SEARCHABLE ORDER IS THE WEIGHTING (§4: title^3 subtitle^2 tags
		// preview content). Meilisearch ranks by attribute order; the ^n in
		// the docs is our intent, this list is the mechanism.
		SearchableAttributes: []string{"title", "subtitle", "tags", "preview", "content"},
		// content never returns — hydration owns display (§4: the index can
		// never drift the §1 shape)
		DisplayedAttributes:  []string{"id", "slug", "date", "rank"},
		FilterableAttributes: []string{"status", "tags", "day", "readTime"},
		SortableAttributes:   []string{"rank", "day", "readTime"},
	}
}

func photosSettings() *meili.Settings {
	return &meili.Settings{
		SearchableAttributes: []string{"alt", "collection"},
		DisplayedAttributes:  []string{"id"},
		FilterableAttributes: []string{"gallery", "collection"},
		SortableAttributes:   []string{"sort"},
	}
}

func tagsSettings() *meili.Settings {
	return &meili.Settings{
		SearchableAttributes: []string{"name"},
		DisplayedAttributes:  []string{"id"},
		// typo tolerance is the ENTIRE point of moving vocabulary here from
		// the Postgres prefix lookup — default ON is the engine's honest
		// middle; single-token tag names are exactly its sweet spot.
	}
}

func (s *Service) waitIndex(ctx context.Context, uid string) error {
	for i := 0; i < 100; i++ {
		if _, err := s.client.GetIndex(uid); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("index creation timed out: " + uid)
}

func (s *Service) waitTask(ctx context.Context, task *meili.TaskInfo) error {
	if task == nil {
		return nil
	}
	if _, err := s.client.WaitForTaskWithContext(ctx, task.TaskUID, 50*time.Millisecond); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	return nil
}

func primaryKey() *meili.DocumentOptions {
	pk := "id"
	return &meili.DocumentOptions{PrimaryKey: &pk}
}

// IndexPosts upserts post docs; DeletePosts removes docs by id (a row gone
// from truth must vanish from serving).
func (s *Service) IndexPosts(ctx context.Context, docs []PostDoc) error {
	if len(docs) == 0 {
		return nil
	}
	task, err := s.client.Index(s.postsIndex).UpdateDocumentsWithContext(ctx, docs, primaryKey())
	if err != nil {
		return err
	}
	return s.waitTask(ctx, task)
}

func (s *Service) DeletePosts(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	task, err := s.client.Index(s.postsIndex).DeleteDocumentsWithContext(ctx, strIDs(ids), nil)
	if err != nil {
		return err
	}
	return s.waitTask(ctx, task)
}

func (s *Service) IndexPhotos(ctx context.Context, docs []PhotoDoc) error {
	if len(docs) == 0 {
		return nil
	}
	task, err := s.client.Index(s.photosIndex).UpdateDocumentsWithContext(ctx, docs, primaryKey())
	if err != nil {
		return err
	}
	return s.waitTask(ctx, task)
}

func (s *Service) DeletePhotos(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	task, err := s.client.Index(s.photosIndex).DeleteDocumentsWithContext(ctx, strIDs(ids), nil)
	if err != nil {
		return err
	}
	return s.waitTask(ctx, task)
}

func strIDs(ids []int64) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.FormatInt(id, 10)
	}
	return out
}

func (s *Service) IndexTags(ctx context.Context, docs []TagDoc) error {
	if len(docs) == 0 {
		return nil
	}
	task, err := s.client.Index(IndexTags).UpdateDocumentsWithContext(ctx, docs, primaryKey())
	if err != nil {
		return err
	}
	return s.waitTask(ctx, task)
}

func (s *Service) DeleteTags(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	task, err := s.client.Index(IndexTags).DeleteDocumentsWithContext(ctx, strIDs(ids), nil)
	if err != nil {
		return err
	}
	return s.waitTask(ctx, task)
}

// Ready powers /readyz: the engine must ANSWER, not just accept TCP.
func (s *Service) Ready(_ context.Context) error {
	_, err := s.client.Health()
	return err
}
