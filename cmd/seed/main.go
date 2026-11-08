// cmd/seed applies migrations (via the store's embedded runner) and loads the
// contract-twin fixture. Idempotent: it truncates the tables it owns, so
// `make dev` and the integration tests get the SAME corpus every run. In M3 the
// publish-time derivations (preview/read_time/word_count/tag counts) move under
// the publish transaction; the seed remains the deterministic dev/CI loader.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanatabi/lumen/internal/domain"
	"github.com/kanatabi/lumen/internal/queue"
	"github.com/kanatabi/lumen/internal/search"
	"github.com/kanatabi/lumen/internal/store"
)

func main() {
	dbURL := flag.String("db", os.Getenv("LUMEN_DB_URL"), "postgres:// connection string")
	fixture := flag.String("fixture", "testdata/earendil-fixture.json", "contract-twin JSON")
	flag.Parse()
	if *dbURL == "" {
		log.Fatal("-db or LUMEN_DB_URL required")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx, *dbURL)
	if err != nil {
		log.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	if err := store.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	if err := queue.MigrateUp(ctx, pool); err != nil {
		log.Fatalf("river migrate: %v", err)
	}
	log.Print("migrations applied (schema + queue)")
	if err := loadFixture(ctx, pool, *fixture); err != nil {
		log.Fatalf("seed: %v", err)
	}
	log.Print("fixture loaded")

	// index everything through the REAL pipeline (engine config from env;
	// the dev loop must exercise what production runs, not a shortcut)
	eng := search.New(mustEnv("LUMEN_MEILI_URL"), mustEnv("LUMEN_MEILI_KEY"))
	if err := eng.Ensure(ctx); err != nil {
		log.Fatalf("meili bootstrap: %v", err)
	}
	pg := store.NewPG(pool)
	qc, err := queue.New(ctx, pool, pg, eng)
	if err != nil {
		log.Fatalf("queue: %v", err)
	}
	if err := qc.Start(ctx); err != nil {
		log.Fatalf("queue start: %v", err)
	}
	ids, err := pg.AllPostIDs(ctx)
	if err != nil {
		log.Fatal(err)
	}
	for _, id := range ids {
		if err := qc.EnqueueIndexPost(ctx, id); err != nil {
			log.Fatal(err)
		}
	}
	pids, err := pg.AllPhotoIDs(ctx)
	if err != nil {
		log.Fatal(err)
	}
	for _, id := range pids {
		if err := qc.EnqueueIndexPhoto(ctx, id); err != nil {
			log.Fatal(err)
		}
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := qc.Drain(dctx); err != nil {
		log.Fatalf("drain: %v", err)
	}
	if err := qc.Stop(ctx); err != nil {
		log.Fatal(err)
	}
	log.Printf("indexed %d posts + %d photos via queue (drained)", len(ids), len(pids))
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("env %s required for indexing (compose dev: LUMEN_MEILI_URL=http://localhost:7700)", k)
	}
	return v
}

func loadFixture(ctx context.Context, pool *pgxpool.Pool, file string) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var f store.Fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}

	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`TRUNCATE post_tags, posts, tags, photos, collections RESTART IDENTITY CASCADE`); err != nil {
			return err
		}

		// tags: two roles, one table. CLOUD tags (from the fixture's curated
		// list) carry the published count and cloud=true — the only rows
		// /api/tags serves. Post-tag vocabulary rows exist solely so post_tags
		// has a target (FK integrity), cloud=false, count 0 until M3 promotes.
		tagID := map[string]int64{}
		upsert := func(name string, count int, cloud bool) error {
			if id, ok := tagID[name]; ok {
				_ = id
				return nil
			}
			var id int64
			err := tx.QueryRow(ctx,
				`INSERT INTO tags(name, posts, cloud) VALUES ($1,$2,$3)
				 ON CONFLICT (name) DO UPDATE
				 SET posts = GREATEST(tags.posts, EXCLUDED.posts),
				     cloud   = tags.cloud OR EXCLUDED.cloud
				 RETURNING id`, name, count, cloud).Scan(&id)
			if err != nil {
				return err
			}
			tagID[name] = id
			return nil
		}
		for _, t := range f.Tags {
			if err := upsert(t.Name, t.Posts, true); err != nil { // curated cloud
				return err
			}
		}

		// posts: published corpus at curation rank (fixture order IS rank, §1),
		// then the leak-bait draft — present in the DB, invisible to §1.
		insert := func(d domain.Detail, status string, rank int) error {
			var pid int64
			err := tx.QueryRow(ctx, `
				INSERT INTO posts
					(slug, title, subtitle, preview, content_md, rank, status, date,
					 read_time, word_count, thumbnail, published_at)
				VALUES
					($1, $2, $3, $4, $5, $6, $7::post_status, $8::date, $9, $10, $11,
					 CASE WHEN $7 = 'published' THEN now() ELSE NULL END)
				RETURNING id`,
				d.Slug, d.Title, d.Subtitle, d.Preview, d.Content, rank, status,
				d.Date, d.ReadTime, len(words(d.Content)), d.Thumbnail).Scan(&pid)
			if err != nil {
				return err
			}
			for pos, name := range d.Tags {
				// vocabulary row for FK integrity (cloud=false; a name already
				// curated as cloud=true keeps its flag via the OR-upsert)
				if err := upsert(name, 0, false); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx,
					`INSERT INTO post_tags(post_id, tag_id, position) VALUES ($1,$2,$3)
					 ON CONFLICT DO NOTHING`,
					pid, tagID[name], pos); err != nil {
					return err
				}
			}
			return nil
		}
		for i, p := range f.Posts {
			if err := insert(p, "published", i+1); err != nil {
				return err
			}
		}
		for _, p := range f.Drafts {
			if err := insert(p, "draft", 0); err != nil {
				return err
			}
		}

		// fixture file order IS the curation order: with no explicit sort in
		// the twin, the seed writes positions so the engine's ordering is
		// tie-free and matches the mock's array order (all-zero sorts make
		// Meilisearch's tie-break the contract's noise — unacceptable)
		for i, p := range f.Photos {
			sortVal := p.Sort
			if sortVal == 0 {
				sortVal = i + 1
			}
			if p.Collection != nil && *p.Collection != "" {
				if _, err := tx.Exec(ctx,
					`INSERT INTO collections(name) VALUES ($1) ON CONFLICT DO NOTHING`,
					*p.Collection); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO photos(src, alt, width, height, sort, featured, gallery, collection)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				p.Src, p.Alt, p.Width, p.Height, sortVal, p.Featured, p.Gallery, p.Collection); err != nil {
				return err
			}
		}
		return nil
	})
}

func words(s string) []string {
	var out []string
	f := []rune(s)
	cur := []rune{}
	for _, r := range f {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = cur[:0]
			}
		} else {
			cur = append(cur, r)
		}
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}
