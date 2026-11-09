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
GET /api/photos?limit              → { total, items: Photo[] }         // /gallery masonry — BE serves gallery=true ONLY (curation flag; collection is admin-internal and never filters this list)
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

- **Serving model (owner ruling, 2026-11-08):** Meilisearch is the READ PATH —
  `GET /api/posts`, `GET /api/photos` and both search faces answer from the engine;
  Postgres is the source of truth: writes, point reads (slug, featured, vocabulary)
  and hydration. Hydration RE-CHECKS truth (published/gallery at the DB edge too),
  so a lagging index can at worst omit a just-committed row — never leak one.
  Failure posture: **pure engine** — lists/search 503 when it is down, loudly; no
  silent Postgres fallback, no untested dual path.
- **Async indexing (same ruling; river v0.51.1):** the queue IS Postgres tables;
  index jobs commit transactionally INSIDE §2's publish unit of work (InsertTx —
  "row written but index job lost" becomes structurally impossible, and no Redis
  enters the 2 GB budget). Jobs carry IDS only; workers re-read current truth, so
  retries converge, edit bursts coalesce (uniqueness by args), and vanished rows
  delete their docs — the index reconciles toward Postgres, never away from it.
  The visible cost is honest: the feed is a projection, consistency window equals
  the drain time (sub-second local; seed/tests drain before asserting).
- searchable & boosted: `title^3, subtitle^2, tags, preview, content` — attribute
  ORDER is the mechanism; `content` is searchable-but-never-returned (display
  truth is hydrated — §4's original promise kept structural); `rank`, `day`
  (yyyymmdd int) and `readTime` are SORTABLE attributes, so curation determinism
  survives the engine; relevance ordering is the engine's own (contract assertions
  for search/suggest moved to rules — the mock's naive score is a fixture, not the spec)
- `GET /api/search` → ranked + paginated + exact `total`
- `GET /api/search/suggest` → same engine, `limit` default 8, no pagination
- typo tolerance on (it's a blog, not a database console)
- **Photos ride the same engine for the admin door only** (§8
  `GET /admin/search`): a second index `photos` with documents
  `id, alt, collection, gallery`; searchable `alt^2, collection` — the
  vocabulary curators actually remember. Public §1 search never reads it;
  gallery=false photos may, because the admin palette must find everything.
  The §8 `tags` group does NOT ride the index — vocabulary prefix match comes
  straight from Postgres (name lookup, not relevance search).

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

Keys are content-addressed — `media/<sha[:2]>/<sha>.<ext>` after a decode-and-
REBUILD pass (EXIF and every ancillary chunk die by construction: the re-encoded
artifact carries what the encoder writes, not what arrived — proven by a test that
splices a fake APP1/Exif segment and demands its absence in the output). Same bytes
⇒ same key forever, so `immutable` Cache-Control here is honest and dedupe is free.
DEV ENDPOINT: gofakes3 — embedded in the integration battery, `make devs3` for the
manual loop; MinIO withdrew its public images and LocalStack v4+ demands an auth
token (2025's registry rug-pull), and the relay speaks real SigV4 S3 regardless:
HUMAN-GATE #9 stays a VALUES decision (Bao config), never an architecture one.
Formats pinned jpeg|png, loud 415 elsewhere (webp awaits a trusted encoder —
recorded, not silently dropped); 20 MiB cap; the content type is what the DECODE
says, never the client's header.

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

- `posts(id, slug, title, subtitle, preview, content_md /* raw Markdown, fences
  intact — §3 Route B, no rendered variant */, rank, status ENUM, rev, date,
  published_at, word_count, read_time, thumbnail, deleted_at, content_hash)`
  — realized (M3): `deleted_at` soft-deletes and slug uniqueness is PARTIAL
  (`WHERE deleted_at IS NULL`): deleting FREES the slug while history keeps its
  row for the GC window; `content_hash` makes §2's re-publish idempotence
  checkable (byte-identical input = no-op, rank unmoved)
- `users(username u, passhash /* argon2id + per-user salt (owner decision
  2026-11-08: credentials move from env/Bao to the DB, hashed; env-credential era
  superseded — the Bao seed now holds only the JWT signing key) */, token_version
  /* bumped by logout; every JWT carries `tv` and must match */)` — one row today,
  the shape stays multi-user-honest without RBAC being real (Parked in mellon PLAN)
- `tags(id, name u, posts, cloud)`, `post_tags(post_id, tag_id, position)`
  — counts publish-maintained (§1, never aggregated at read); `cloud` is curated
  MEMBERSHIP distinct from FK vocabulary (the earendil cloud ships a curated
  subset — vocabulary rows exist so links can point somewhere; only cloud=true
  rows serve `/api/tags`); `position` preserves author-declared tag ORDER — the
  first live diff against the mock proved tag order is payload, not alphabet
- `photos(id, src, alt, width, height, sort, featured bool, gallery bool, collection)`
  — `featured` answers `GET /api/photos/featured`; **`gallery` is the public
  curation flag** (uploads default false; only `gallery=true` reaches §1);
  `collection` is admin-internal theming (null = uncategorized)
- `collections(id, name u)` — curation vocabulary, admin-internal: the blog does
  NOT browse by collection (yet); public list ignores it entirely
- `media_assets(id, path u, sha256, kind, post_slug, nth, created_at)` — the M5
  upload relay writes `kind='upload'` rows (content-addressed path + hash = the
  dedupe and the GC handle in one); fenced-media rows land with the publish
  pipeline's hardening pass

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

Auth (owner decisions, 2026-11-08): **JWT sessions**. `POST /admin/session` checks
credentials (argon2id, `users.passhash` — §6) and mints an HS256 token, claims
`{ sub, iat, exp, tv, jti }`: **TTL 12h**, `tv` = the user's `token_version`, `jti` a
random id. Verification = pinned-HS256 signature (no `alg` negotiation — the classic
JWT foot-guns stay shut) **AND** one indexed row read (`tv` match). That read buys what
pure statelessness cannot: **real revocation** — logout bumps `token_version` and every
outstanding token for the account dies instantly. Revocation is non-negotiable because
the portal is PUBLIC (`console.gofar.id`, INFRA amendment 2026-11-08); a stolen 12h
bearer must not outlive the logout button. **No refresh tokens** (owner ruling: re-login
at 12h is the design — rotation machinery is not worth its theft surface for one user).
The signing key is `SESSION_JWT_SECRET` (32 bytes, generated at bootstrap, lives in
Bao `lumen/{env}` — §5-of-INFRA seed list; never in git). The public login endpoint
MUST be throttled (§8 rules) — single user + public URL makes credential stuffing *the*
attack; argon2id makes each guess expensive, the throttle makes them rare. Session
state is minted *here*; mellon only carries it (cookie + relay), and honors
`expiresIn` for its cookie lifetime.

```
POST   /admin/session                      → { token: JWT, expiresIn } (seconds —
                                             mellon sets its cookie maxAge from it;
                                             camelCase per the AdminApi spec — the
                                             snake_case drift was fixed live, see
                                             milestone history, by the Postman walk)
DELETE /admin/session                      → 204: bumps users.token_version (revoking
                                             every outstanding token) — mellon calls
                                             it best-effort at "end session", then
                                             clears its cookie regardless (the cookie
                                             is the portal's; the REVOCATION is ours)
GET    /admin/posts?status&tag&sort&dir&page → paged envelope { total, items, page,
                                             pageSize } (§1 shape + status; sort =
                                             updated|created|published|title, dir =
                                             asc|desc, default updated/desc, page
                                             1-based clamp, pageSize 20; rows carry
                                             createdAt + publishedAt|null)
GET    /admin/posts/:slug                  → the editor's door: FULL detail incl.
                                             drafts/unpublished, rev, createdAt/
                                             publishedAt/updatedAt (§1 shape + admin
                                             fields); 404 honest
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
GET    /admin/photos?collection&gallery&from&to&sort&dir&page → paged envelope
                                             { total, items, page, pageSize } (collection
                                             repeated = OR, 'uncategorized' = null
                                             bucket; from/to inclusive on createdAt;
                                             sort = created|title|collection, default
                                             created/desc; pageSize 24)
POST   /admin/photos  (multipart)          → relay to object store (§5): probe dims
                                             + strip EXIF server-side BEFORE the
                                             object is public, return Photo with the
                                             absolute CDN src (§1 shape); fresh
                                             photos: gallery=false, collection=null
PATCH  /admin/photos/:id                   → curation metadata {alt?, collection?,
                                             gallery?}; last-write-wins (photos carry
                                             no rev — single-author album truth)
GET    /admin/photos/:id                   → single photo (the portal's detail page;
                                             404 honest)
GET    /admin/photos/:id/references        → posts whose content embeds the photo's
                                             src (content scan). Feeds the delete
                                             dialog's warn-list — deletion is NEVER
                                             blocked; the human decides with full
                                             information (owner call 2026-11-08)
DELETE /admin/photos/:id                   → removes the library row; the OBJECT keeps
                                             serving until storage GC (§5 lifecycle) —
                                             citing posts show a broken picture until
                                             edited
GET    /admin/collections                  → curation vocabulary (name + photo count;
                                             admin-internal — no public browse axis)
POST   /admin/collections                  → create a collection; name slugified,
                                             409 on duplicates (same semantics as tags)
GET    /admin/tags                         → vocabulary (every created tag, used
                                             or not; counts = published posts)
POST   /admin/tags                         → create a vocabulary entry; name is
                                             slugified, 409 on duplicates — the
                                             portal creates eagerly, saves never
                                             guess which names are new
GET    /admin/search?q                     → { posts, photos, tags } — the
                                             palette's content door: posts incl.
                                             drafts (Meilisearch), photos on alt +
                                             collection (§4 admin index), and tags
                                             as a VOCABULARY prefix match from
                                             Postgres — a name lookup, not a
                                             relevance search; selecting one
                                             filters the post list, never opens a
                                             tag page. ≤8 each, no pagination
                                             (finder, not browse axis). Public §1
                                             search stays posts-only
GET    /admin/bao/paths                    → list metadata, policy-scoped mounts only
                                             (§9 — the portal's env/app picker)
GET    /admin/bao/:app/:env                → read current data (?version=N for a pin)
GET    /admin/bao/:app/:env?meta           → version list (history panel, CAS numbers;
                                             presence flag, not value — mirrors the
                                             mellon relay's contract shape)
PUT    /admin/bao/:app/:env                → CAS write ({data, cas}); returns the new
                                             version. The FOUR operations above are
                                             the ENTIRE surface (§9: thin, explicit —
                                             no proxying sys/, nothing else added at
                                             any time)
```

Rules:
- **Login throttle** (public-console era): per-IP + per-account exponential backoff on
  `POST /admin/session`, with structured-log audit of failures (no payloads, §9
  redaction). The edge rate-limit middleware (INFRA Phase 3) is the coarse first layer;
  this is the real gate.
- **JWT verification is pinned**: HS256 only, `exp`+`tv` always checked, `typ` asserted,
  key from `SESSION_JWT_SECRET` — a `None`/RS256-confusion token must fail at the
  library-config level, not at review time.
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
- **Dev plane == prod plane:** compose runs OpenBao itself (`ghcr.io/openbao/openbao`,
  pinned) — the KV-v2/AppRole battery (bootstrap, redaction proofs, CAS conflicts)
  executes against the production product, not a stand-in. History worth keeping:
  the first M6 run substituted `hashicorp/vault:1.19` because `docker.openbao.xyz`
  failed DNS from the dev box — **one unreachable registry is a data point, not a
  decision**; ghcr serves the real thing. The Go client (`hashicorp/vault/api`)
  speaks OpenBao's protocol as its documented compatibility contract.

## 10. Phase

P0 = §1 read API + §8 admin API (posts/photos/publish core) + §9 (basic secrets
management). The GitOps/infra tab (Argo status, vortex commit-writer) is **deferred** —
when it arrives it follows the same doctrine: lumen holds the token, writes go to *git*,
the cluster keeps taking orders only from Git.
