# lumen — Blog Backend Design Document

Part of the [`omicron`](https://github.com/kanatabi/omicron) superproject. The
REST API + content pipeline that
[`earendil/`](https://github.com/kanatabi/earendil) (frontend) consumes. The FE
renders BE payloads **as-is**: ordering, curation, read-times, and previews are
decisions made here; diagram rendering is the FE's (Route B, §3). If the FE ever has to
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
GET /api/photos/featured           → Photo | 404                       // kept for BE parity; the FE hero is a static identity asset — no consumer today
GET /api/tags                      → Tag[]                             // home rail cloud: counts + order curated here
GET /api/feed.xml                  → RSS 2.0                           // absolute URLs built from ORIGIN (FE consumer lands at earendil M8)
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
  content: string            // raw Markdown, ```mermaid fences INTACT (§3: FE renders them)
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
(explicit author text, default = first paragraph), media garbage-collection, Meilisearch upsert, Postgres row write — one unit of
work. Re-publishing unchanged inputs must produce byte-identical outputs so
deterministic caches (see `immutable` in §5) never invalidate falsely.

## 3. Diagram Rendering (Route B — raw fences, FE renders)

> **Adopted cross-repo decision.** The client-side pipeline `earendil` shipped (its
> "Route B": `AGENTS.md` §7, adoption record in its `PLAN.md`) is the contract. The former
> publish-time SVG rendering described here ("Route A") was evaluated and **rejected** —
> do NOT build a mermaid render step.

The backend's entire job with diagrams: **carry the source through untouched.**

- `content` is stored and served as raw Markdown **including ` ```mermaid ` fences** — no
  rewriting, no pre-validation, no SVG generation. Malformed fences fail gracefully in the
  FE (the source is revealed), so the API never needs diagram-grammar opinions.
- One payload serves both palettes: the FE themes from its own token contract (`--diagram-*`
  → mermaid redux themes) and re-renders on theme flip. The API never needs to know which
  theme a reader uses.
- The FE renders client-side: fence → Shiki bypass → SSR placeholder (the shimmer skeleton)
  → hydration lazy-loads pinned `mermaid@12.2.0` → themed SVG; click → lightbox; JS-off /
  parse-failure reveal the escaped source. Authoritative description: `earendil`
  `DESIGN.md` §6.5 + `AGENTS.md` §7.

## 4. Search Index (Meilisearch)

Index `posts`; documents carry display fields only
(`slug, title, subtitle, tags, preview, date, readTime, thumbnail`) + `content`
indexed as searchable-only (never returned). Result pages hydrate summaries
from Postgres by id so the index can never drift the §1 shape.

- searchable & boosted: `title^3, subtitle^2, tags, preview, content`
- `GET /api/search` → ranked + paginated + exact `total`
- `GET /api/search/suggest` → same engine, `limit` default 8, no pagination
- typo tolerance on (it's a blog, not a database console)

## 5. Media Serving — ✅ RESOLVED (D1) **revised**: object storage + CDN, upload via lumen

> **Decision (owner, 2026-11-08, second pass):** the first resolution (media bytes
> riding the SSR relay at `/media/*`) is **superseded**: media lives in an
> **S3-compatible object store fronted by a CDN**; `Photo.src` is an **absolute CDN
> URL** and browsers fetch it from the CDN directly — third-party public edge, so
> "exactly one public ingress *of ours*" stays literally true, and image bytes never
> touch the VPS pods. **Upload is backend-relay**: the portal posts multipart to
> lumen (`§8`), lumen probes dims + strips EXIF *before* the object is public,
> stores, and returns the URL. Presigned-direct upload (browser → bucket) is
> recorded as the future scale option — it would need a confirm step + post-upload
> probe, and the portal contract (`addPhoto → Photo`) is written so the swap touches
> no UI code. **Provider choice (S3 / R2 / Wasabi / …) = owner call, open**
> (`mellon/PLAN.md` HUMAN-GATE ledger); values (bucket, endpoint, CDN base, creds)
> live in Bao per §5-of-INFRA, schema in `env.yaml.sample`.

Diagrams, gallery photos, and thumbnails are owned here; API responses carry URLs
**the FE renders unchanged** — the FE never assembles media paths (unchanged rule,
now pointing at the CDN instead of the relay).

~1. **(Superseded) Extend the SSR relay: `/media/*` → `API_BASE/media/*`.**~ Kept
for the record: it was D1's first answer (same-origin pattern as `/api`, bytes
through Node pods). Replaced by the CDN shape above; the earendil `/media/*` relay
work in INFRA Phase 10 is cancelled accordingly.
2. **Object storage (MinIO/S3/R2) for media only.** — **now the decision**, with
the relay-upload ingestion path instead of direct browser→bucket credentials.

## 6. Data Model (sketch)

- `posts(id, slug u, title, subtitle, preview, content_md /* raw Markdown, fences
  intact — §3 Route B, no rendered variant */, rank, published_at, word_count,
  read_time)`
- `tags(id, slug u)`, `post_tags`
- `photos(id, src, alt, width, height, sort, featured bool)` — `featured`
  answers `GET /api/photos/featured`
- `media_assets(id, path u, sha256, kind, post_slug, nth, created_at)` — GC
  rows on re-render

## 7. Open Questions

- ~~**D1** — media path~~ **RESOLVED (revised)**: object storage + CDN with
  **backend-relay upload** (§5 header, owner second pass 2026-11-08); provider
  choice remains with the owner (`mellon/PLAN.md` ledger); execute in Phase 10.
- **D2** — RSS: browser fetches `/feed.xml` at the blog origin; needs either a
  thin FE route `/feed.xml` → `API_BASE/api/feed.xml`, or listing the feed
  under D1's relay decision. Recommend bundling with D1.
- ~~Authoring UI~~ **RESOLVED**: it is **mellon** — see `../mellon/DESIGN.md` (UI-only
  portal) and §8 below (its write contract). Publish auth = lumen-minted sessions;
  the `fzr-portal` AppRole lives server-side (§9).
- Upload pipeline (photo thumbnails/derivatives): deferred until real uploads
  exist; today `Photo` is single-size + natural dims and the FE scales via CSS.
- Contract changes: coordinated in one commit set — §1 here **and**
  `BlogApi`/types/mock in `earendil`. The FE mock corpus (12 posts, featured
  photo, "queue" search hits) doubles as the acceptance fixture.

## 8. Admin API (mellon's contract)

The write half of the service. **Never consumed by earendil** — the blog's read path is
§1 only; the portal (`../mellon`) is the sole client, through its same-origin relay
(UI holds no credentials; doctrine in mellon's DESIGN §2).

Auth: `POST /admin/session` (single-user credentials in → short-TTL bearer out); every
other endpoint 401s without it. Session state is minted *here*; mellon only carries it.

```
POST   /admin/session                      → { token, expires_in }
GET    /admin/posts?status&tag&sort&page   → list incl. drafts (§1 shape + status,
                                             sort = newest|oldest|title on updated;
                                             rows carry createdAt + publishedAt|null)
POST   /admin/posts                        → create draft; body = PostDetail minus
                                             readTime (derived at publish)
PUT    /admin/posts/:slug                  → update draft (optimistic-locked rev)
POST   /admin/posts/:slug/publish          → the §2 unit of work: derivations,
                                             Postgres row, Meilisearch upsert,
                                             media GC — one transaction, idempotent
POST   /admin/posts/:slug/unpublish        → unpublished (the §4 machine: draft →
                                             published ⇄ unpublished; a shipped post
                                             NEVER returns to draft); 409 otherwise
DELETE /admin/posts/:slug                  → soft-delete (retain media for GC window)
POST   /admin/photos  (multipart)          → relay to object store (§5): probe dims
                                             + strip EXIF server-side BEFORE the
                                             object is public, return Photo with the
                                             absolute CDN src (§1 shape)
GET    /admin/tags                         → vocabulary (every created tag, used
                                             or not; counts = published posts)
POST   /admin/tags                         → create a vocabulary entry; name is
                                             slugified, 409 on duplicates — the
                                             portal creates eagerly, saves never
                                             guess which names are new
POST   /admin/bao/…                        → secrets passthrough (§9; thin, explicit:
                                             read / versions / cas-write — no proxying
                                             sys/, no list beyond the policy's mounts)
```

Rules:
- **Drafts never leak to §1** — the read API returns published-only, always.
- Publish input is **raw Markdown with fences intact** (§3): the admin API validates
  grammar permissively (a broken fence publishes with a warning; the FE reveals source).
- Contract changes follow the same ritual as §1: one commit set, mirror updated.

## 9. Secrets-plane integration (Bao passthrough)

For the portal's env/secret management (mellon `/secrets`), lumen proxies a **minimal,
explicit** subset of OpenBao KV v2 — per `INFRA_ARCHITECTURE.md` §5.5:

- Identity: the `fzr-portal` AppRole, bound to worker CIDRs, TTL ≤ 1 h, re-login
  agent-side; **the secret-id lives only in lumen's env** (ESO-rendered from
  `bao-approle-credentials`, the §5.6 seed). Mellon never sees it.
- Exposed operations: list metadata (policy-scoped mounts/paths), read
  (`/v1/kv/data/<app>/<env>`, `?version=N`), version list (`/v1/kv/metadata/...`),
  CAS write (`PUT` with `cas`), nothing else. No `sys/`, no unauthenticated paths.
- Redaction: values never logged (structured logs redact by key); audit is **Bao's own
  audit devices** — lumen logs the *operation*, not the *payload*.
- The same client runs the CI drift check (§5.5) — one implementation, two consumers.

## 10. Phase

P0 = §1 read API + §8 admin API (posts/photos/publish core) + §9 (basic secrets
management). The GitOps/infra tab (Argo status, vortex commit-writer) is **deferred** —
when it arrives it follows the same doctrine: lumen holds the token, writes go to *git*,
the cluster keeps taking orders only from Git.
