# AGENTS.md — `lumen` (blog backend)

Design-stage home of the API that powers
[`earendil`](https://github.com/kanatabi/earendil); a submodule of the
[`omicron`](https://github.com/kanatabi/omicron) superproject. There is **no
service code here yet** — the day it's scaffolded, it lands in this repo. (A
throwaway healthz-only Go stub existed in the pre-submodule workspace as k3d
wiring scaffolding; never mistake it for the design.)

## Read first

1. `DESIGN.md` (here) — the contract (§1), the rules, and the two open
   decisions (D1 media serving, D2 RSS path). Implement *that*, not a
   remembered version of it.
2. [`omicron/AGENTS.md`](https://github.com/kanatabi/omicron/blob/main/AGENTS.md)
   — workspace-wide non-negotiables (one public ingress, no secrets in Git,
   pinned versions, app-of-apps layout).
3. [`omicron/INFRA_ARCHITECTURE.md`](https://github.com/kanatabi/omicron/blob/main/INFRA_ARCHITECTURE.md)
   — placement, budget, secrets; supersedes older drafts on conflict.

## Load-bearing rules

- **The FE types are the contract.** `BlogApi` in
  `../earendil/src/lib/api/client.ts` + `types.ts` + `DESIGN.md` §1 move together
  in one coordinated change (this repo's copy + the FE's mock/fixtures). The FE
  is mock-first: build endpoint-by-endpoint against what the mock corpus
  already answers, and keep both in agreement.
- **FE renders payloads as-is.** Ordering, filtering, search ranking,
  `readTime`, `preview`, tag sets, `featured` — computed BE-side, and
  content-derived values at **publish time**, never per-request. If a design
  tempts the FE into post-processing, fix it here.
- **Mermaid renders at publish, never at request time** (§3). A ` ```mermaid `
  fence in a GET response is a publish-pipeline bug. Do **not** add mermaid
  support to the frontend — that ship (Route A) has sailed deliberately.
  **⚠️ Pending reconciliation:** `earendil` has since adopted Route B
  (client-side render) — see the warning at `DESIGN.md` §3; don't act on this
  rule either way without checking the frontend first.
- **The API never has an ingress** (amended 2026-11-08 only in number of
  *public surfaces*: blog + `console.gofar.id` — lumen is behind both). SSR pods
  reach it via `API_BASE` (internal DNS); browsers ride FE/portal same-origin
  relays. Never "fix" a browser 404 by adding a backend Ingress — superproject
  `AGENTS.md` non-negotiable #2.
- Real config/secret values live in OpenBao; Git keeps schemas/templates only.

## Conventions (code has landed — these are rulings, not proposals)

- **Router: chi** (`go-chi/chi/v5`) — owner ruling at HUMAN-GATE #2
  (2026-11-08), overriding the earlier stdlib-first default.
- **SQL is always prepared statements.** All queries use `$n` parameters
  through `store.NewPool`, which asserts
  `pgx.QueryExecModeCacheStatement` explicitly (the doctrine is written, not
  inherited from driver defaults); no value ever concatenates into SQL text,
  and `ORDER BY` variants switch over fixed SQL strings. One honest exception:
  multi-statement DDL migrations run through the simple protocol — trusted
  checked-in files, no user data (extended protocol can't multi-statement, and
  the rule exists to keep *untrusted values* out of SQL text).
- **Config: viper** (owner ruling, 2026-11-08), env-first: precedence env >
  optional `env.yaml` (`LUMEN_CONFIG`) > defaults, so ESO-rendered pod env
  always wins. NO dotenv autoload — production speaks real environment.
- **One file per resource for handlers** (`posts.go`, `search.go`, `photos.go`,
  `tags.go`; plumbing in `router.go`/`respond.go`). A package must not
  accrete into a single file.
- Container image name stays `blog-backend` — CI rewrites only
  the `images:` line via `kustomize edit set image`, and GHCR names are
  lowercase.
- Every response sets `Cache-Control` per §5 (content JSON short-lived;
  `/media/*` `public, max-age=31536000, immutable` — content-hash filenames
  make `immutable` safe).
- Migrations at boot (initContainer), DSN from the Patroni `blog-db` service.
- Validation once code exists: `go test ./...` + `go vet ./...` before commit;
  contract smoke against the FE mock fixtures' shapes; k3d rehearsal via the
  superproject README's test path.

## Commits

Conventional Commits. **Club logically related changes into one commit — never
one commit per file, and no micro-commits**: a handler + its test + the
contract/doc line it satisfies = one commit. Split only genuinely independent
concerns. Branches off `main` — this **is** its own repo now.

## Suggested build order

1. Go module + `/healthz` + DTOs matching `earendil/src/lib/api/types.ts` exactly
2. posts + photos endpoints served from hard-coded fixtures (mirror the FE's
   12-post corpus so the FE can flip `PUBLIC_API_BASE` at this point already)
3. Postgres + migrations + the publish pipeline (§2 derivations)
4. Mermaid render Job + media store — **resolve D1 (and D2) first**
5. Meilisearch index + `/api/search` + suggest; then `feed.xml`
