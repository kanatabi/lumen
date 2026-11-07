# lumen — Blog Backend Design Document

Part of the [`omicron`](https://github.com/kanatabi/omicron) superproject. The
REST API + content pipeline that
[`earendil/`](https://github.com/kanatabi/earendil) (frontend) consumes. The FE
renders BE payloads **as-is**: ordering, curation, read-times, previews, and
rendered diagrams are all decisions made here. If the FE ever has to
post-process data, this document was not followed.

Status: **design only** — implementation not started. (A throwaway healthz-only
Go stub existed in the pre-submodule workspace for k3d wiring tests; it
implements nothing here.) The real service will live in this repo.

Deployment context (see the superproject
[`INFRA_ARCHITECTURE.md`](https://github.com/kanatabi/omicron/blob/main/INFRA_ARCHITECTURE.md)
+ [`AGENTS.md`](https://github.com/kanatabi/omicron/blob/main/AGENTS.md)): Go
service, 2 replicas, **ClusterIP only — no public ingress, ever**. SSR pods call
it via `API_BASE` (internal DNS); the browser reaches it *only* through the
frontend's same-origin relays (`/api/*` today; `/media/*` and `/feed.xml` per
§5/D2). Search runs on Meilisearch (worker-2), storage on Patroni Postgres.

## 1. API Contract

Mirror of `earendil/src/lib/api/client.ts::BlogApi` — **the FE interface is the
source of truth**; a drift between the two is a bug in whichever moved.

```
GET /api/posts?offset&limit&tag&from&to&readMin&readMax&sort → { total, items: PostSummary[] }  // tag repeatable (OR); from/to + readMin/readMax inclusive windows; sort whitelist
GET /api/posts/:slug               → PostDetail
GET /api/search?q&offset&limit&tag&from&to&readMin&readMax&sort → { total, items: PostSummary[] } // ranked, paginated (/search page); facets as on /api/posts
GET /api/search/suggest?q&limit    → { results: PostSummary[] }        // capped launcher (⌘K palette)
GET /api/photos?limit              → { total, items: Photo[] }         // /gallery masonry
GET /api/photos/featured           → Photo | 404                       // hero photo; FE is null-tolerant
GET /api/tags                      → Tag[]                             // home rail cloud: counts + order curated here
GET /api/feed.xml                  → RSS 2.0                           // absolute URLs built from ORIGIN
```

```
PostSummary = {
  slug: string
  title: string
  subtitle: string           // secondary headline, shown in every listing
  preview: string            // BE-owned display text (article opening is fine); FE only line-clamps
  date: string               // ISO 8601
  readTime: number           // minutes, computed AT PUBLISH from final content
  thumbnail: string | null   // URL; null ⇒ listings render text-only
}

PostDetail  = PostSummary & {
  tags: string[]
  content: string            // Markdown, ALREADY diagram-rendered (see §3)
}

Photo = { src, alt, width, height }   // natural dimensions; mixed aspect ratios are the point
Tag   = { name: string, posts: number }  // posts = corpus count carrying the tag
```

Semantics the FE depends on:

- **`/api/tags` is curation, not aggregation-at-read.** Counts are maintained at
  publish time (like `readTime`); the BE sorts by `posts` desc with a stable tie
  order and decides how many tags the cloud ships. The rail renders the payload
  as-is — the FE never counts, filters, or sorts.

- **`/api/posts` order is curation.** The home feed renders exactly this order,
  paginated — an explicit rank column, not merely date desc (date is displayed,
  not decided by consumers). `total` reflects the full (tag-filtered) count.
- **`tag` filtering is server-side** (Postgres), repeatable and **OR-matched** (`ANY`):
  the search rail ships the selected set; the FE never filters arrays. `from`/`to` are an
  inclusive window on the post date, applied together with tags (and with `q` on search).
- **`readMin`/`readMax`** are an inclusive minute window on `readTime` (computed at
  publish), and **`sort`** accepts `newest|oldest|quickest|longest|relevance`
  (`relevance` = search scoring, the implicit default when `q` is present). Absent `sort`
  means curation order — the home feed never sends it, and only an explicit pick
  re-orders the result set.
- **`q` search is ranked** (Meilisearch relevance). `total` must be the real
  hit count for the "N posts" line. Suggest = same engine, low cap, no
  pagination; it is the only call a browser makes directly.
- Field presence: `subtitle`/`preview` may be `null`-ish, and `thumbnail` is
  `string | null` — but keys are **always present**; the FE types are
  non-optional. Unknown slug ⇒ `404`. No auth on reads (yet).

## 2. Publish-time derivations (never request-time)

At publish — not on GET: `readTime` (word count of final content), `preview`
(explicit author text, default = first paragraph), diagram replacement (§3),
media garbage-collection, Meilisearch upsert, Postgres row write — one unit of
work. Re-publishing unchanged inputs must produce byte-identical outputs so
deterministic caches (see `immutable` in §5) never invalidate falsely.

## 3. Diagram Rendering (Route A — publish-time, BE-side)

> ⚠️ **Stale — reconcile before implementing.** `earendil` has since adopted
> **Route B** (client-side mermaid; its `AGENTS.md` §7 says the backend ships
> raw fences, no publish render). This section still describes the original
> Route A contract. Do not build the publish-render pipeline as-is.

Mermaid is rendered **here, at publish time**, into static SVG. The FE never
loads mermaid.js, never sees ` ```mermaid ` fences, and needs zero code for
diagrams: the API's `content` contains ordinary images, so the FE's existing
prose pipeline gives them framing, lazy-loading, **and click-to-zoom (lightbox)
for free**.

```
for each ```mermaid fence in content (nth = 1..n):
    svg = mermaid-cli render(source,
              securityLevel: "strict",     # no HTML/JS in labels
              theme: light-card)           # baked colors, see below
    hash = sha256(svg)[:8]
    store → /media/diagrams/<slug>-<nth>-<hash>.svg
    replace fence with: ![<gist alt>](/media/diagrams/<slug>-<nth>-<hash>.svg)
```

**Theme-transparent output (A-inline contract).** The FE **inlines** these SVGs
server-side into the SSR'd article HTML (fetch → sanitize → swap for the `<img>`),
which lets the page's CSS vars theme diagrams in both modes — while the same file
remains a correct standalone `<img>` for fallbacks. Requirements:

- every paint (fill/stroke/text color) is `var(--diagram-*, <fallback>)` written
  into **inline `style` attributes** — no `<style>` blocks, and never bare hex in
  presentation attributes. Fallbacks = the light-card colors, so the asset is
  right on its own too
- implementation: pin `@mermaid-js/mermaid-cli`, then post-process with a
  deterministic palette mapping (known theme hex → `var(--diagram-x, <hex>)`);
  golden-file tests own the mapping (pin + golden-file is the agreed contract)
- the var names are the shared BE↔FE contract (FE defines them light + dark,
  `earendil/src/routes/layout.css`):

| var | role | light | dark |
| --- | --- | --- | --- |
| `--diagram-bg` | card/background | `#ffffff` | `#131316` |
| `--diagram-surface` | node fill | `#f4f4f5` | `#27272a` |
| `--diagram-ink` | text | `#09090b` | `#fafafa` |
| `--diagram-muted` | secondary text/arrows | `#71717a` | `#a1a1aa` |
| `--diagram-line` | strokes/borders | `#e4e4e7` | `#27272a` |
| `--diagram-accent` | highlighted nodes/labels | `#7c3aed` | `#a78bfa` |

Rules:

- **Standalone-safe**: with the vars absent (plain `<img>`), fallback paints give
  the baked light card — one asset, both worlds; no separate light/dark renders
- **`securityLevel: "strict"` stays mandatory**; the FE additionally
  DOMPurify-sanitizes each SVG before inlining (defense in depth)
- **No diagram zoom** in the FE (inline SVG isn't an `<img>`; vector renders
  crisp) — deliberately skipped in v1
- **Alt = the diagram's semantic gist** ("Request flow through Traefik to SSR
  pods"), never just "diagram" — it is the lightbox caption and the a11y
  surface.
- **Deterministic output** (pinned `@mermaid-js/mermaid-cli` version, stable
  ids, content-hash filename) ⇒ unchanged source = same bytes = same URL =
  caches stay valid; changed source = new URL, old asset garbage-collected.
- **Render failure fails the publish** with a clear error. A raw ` ```mermaid `
  fence in an API response is a pipeline bug, never user-facing content — the
  FE's degradation (fence shows as code text) exists only to *surface* that
  bug.
- Implementation: mermaid-cli needs headless Chromium → run it as a
  **Job/sidecar invoked at publish**, never inside the request path.
- Same recipe generalizes to future math or chart extensions: publish-time
  render → plain `<img>`.

## 4. Search Index (Meilisearch)

Index `posts`; documents carry display fields only
(`slug, title, subtitle, tags, preview, date, readTime, thumbnail`) + `content`
indexed as searchable-only (never returned). Result pages hydrate summaries
from Postgres by id so the index can never drift the §1 shape.

- searchable & boosted: `title^3, subtitle^2, tags, preview, content`
- `GET /api/search` → ranked + paginated + exact `total`
- `GET /api/search/suggest` → same engine, `limit` default 8, no pagination
- typo tolerance on (it's a blog, not a database console)

## 5. Media Serving — ⚠️ OPEN DECISION D1

Diagrams, gallery photos, and thumbnails are owned here under `/media/*`, but
browsers can only reach the frontend's origin, and the SSR relay currently
forwards `/api/*` only. Two viable answers — the choice changes `earendil`:

1. **(Recommended default) Extend the SSR relay: `/media/*` →
   `API_BASE/media/*`.** The same-origin pattern already exists for `/api`;
   the FE gains ~15 lines and "exactly one public ingress" stays literally
   true. Image bytes transit the Node pods — fine at blog traffic, given
   `Cache-Control: public, max-age=31536000, immutable` (content-hash
   filenames, §3, make `immutable` safe).
2. **Dedicated public object storage (MinIO/S3/R2) for media only.** Offloads
   bytes entirely from SSR pods; bends the network rule (a public *read-only
   bucket* ≈ not an app ingress, but it's a call the manifest repo must make
   explicitly).

Whatever is chosen, API responses carry URLs **the FE renders unchanged** — the
FE never assembles media paths.

## 6. Data Model (sketch)

- `posts(id, slug u, title, subtitle, preview, content_md /* with fences */,
  content_rendered /* §3 applied */, rank, published_at, word_count,
  read_time)`
- `tags(id, slug u)`, `post_tags`
- `photos(id, src, alt, width, height, sort, featured bool)` — `featured`
  answers `GET /api/photos/featured`
- `media_assets(id, path u, sha256, kind, post_slug, nth, created_at)` — GC
  rows on re-render

## 7. Open Questions

- **D1** — media path (above). Decide before the first real post.
- **D2** — RSS: browser fetches `/feed.xml` at the blog origin; needs either a
  thin FE route `/feed.xml` → `API_BASE/api/feed.xml`, or listing the feed
  under D1's relay decision. Recommend bundling with D1.
- Authoring UI (future `cms.` app): its own doc; its output contract is §1–§3.
  Publish auth/API design pending.
- Upload pipeline (photo thumbnails/derivatives): deferred until real uploads
  exist; today `Photo` is single-size + natural dims and the FE scales via CSS.
- Contract changes: coordinated in one commit set — §1 here **and**
  `BlogApi`/types/mock in `earendil`. The FE mock corpus (12 posts, featured
  photo, "queue" search hits) doubles as the acceptance fixture.
