# AGENTS.md — `lumen` (blog backend)

Live Go REST backend powering [`earendil`](https://github.com/kanatabi/earendil)
and relaying for [`mellon`](https://github.com/kanatabi/mellon); a submodule of the
[`omicron`](https://github.com/kanatabi/omicron) superproject. M1–M6 merged to
`main` (2026-11-09); the secrets control plane landed 2026-11-13 (epoch gate +
pin-as-lock). Code is the source of truth; this file is the index.

## Read first

1. `DESIGN.md` (here) — the contract (§1), the rules, and the decision record
   (§7: D1 media **resolved** — object storage + CDN with backend-relay upload;
   D2 RSS still open; Route B **adopted** — the FE renders diagrams, §3).
   Implement *that*, not a remembered version of it.
2. [`omicron/AGENTS.md`](https://github.com/kanatabi/omicron/blob/main/AGENTS.md)
   — workspace-wide non-negotiables (two public surfaces, no secrets in Git,
   pinned versions, app-of-apps layout).
3. [`omicron/INFRA_ARCHITECTURE.md`](https://github.com/kanatabi/omicron/blob/main/INFRA_ARCHITECTURE.md)
   — placement, budget, secrets; supersedes older drafts on conflict.
4. The [secrets control plane decision record](https://github.com/kanatabi/omicron/blob/main/context-knowledge/decisions/secrets-control-plane.md)
   — the frozen `/admin/bao/*` contract (apps-are-data, key-set mirror, pins).

## Load-bearing rules

- **The FE types are the contract.** `BlogApi` in
  `../earendil/src/lib/api/client.ts` + `types.ts` + `DESIGN.md` §1 move together
  in one coordinated change (this repo's copy + the FE's twin). The FE is
  **real-first** (owner ruling 2026-11-09): its runtime always fetches lumen;
  the fixture twin (`earendil/src/lib/api/mock.ts`) is TEST-ONLY — the contract
  baseline. New endpoints: extend `BlogApi` + the twin together in the same
  change.
- **FE renders payloads as-is.** Ordering, filtering, search ranking,
  `readTime`, `preview`, tag sets, `featured` — computed BE-side, and
  content-derived values at **publish time**, never per-request. If a design
  tempts the FE into post-processing, fix it here.
- **Mermaid: Route B is adopted and settled.** The BE ships raw ` ```mermaid `
  fences; the FE renders client-side (the former publish-time SVG path, Route
  A, was evaluated and rejected — DESIGN §3). A GET response may carry raw
  fences; the FE's renderer owns everything after. Don't re-introduce a BE
  render pipeline for FE rendering, and don't add mermaid support to the FE
  either — that ship has sailed in BOTH directions.
- **The API never has an ingress** (amended 2026-11-08 only in number of
  *public surfaces*: blog + `console.gofar.id` — lumen is behind both). SSR pods
  reach it via `API_BASE` (internal DNS); browsers ride FE/portal same-origin
  relays. Never "fix" a browser 404 by adding a backend Ingress — superproject
  `AGENTS.md` non-negotiable #2.
- **Roles are deployment identity, not configuration** (lumen role split,
  2026-11-13): `LUMEN_ROLE=admin` (sessions, users, TOFU, Bao relay — no
  content routes, no ingress), `LUMEN_ROLE=cms` (content only; introspects
  every admin write against the admin plane, ≤30 s cache, 503 fail-closed),
  `LUMEN_ROLE=all` (dev convenience). Vault can never silently re-role a
  fleet — the role stays inline on the pods, never a Bao value.
- Real config/secret values live in OpenBao; Git keeps schemas/templates only.
  The admin identity's ES carries only `LUMEN_DB_URL` +
  `LUMEN_SESSION_JWT_SECRET`; the signing key has exactly ONE home.

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
- **Layered HTTP** (owner ruling, 2026-11-09): `internal/api/public` (§1)
  and `internal/api/admin` (§8/§9) are separate packages of thin handlers —
  one file per resource, decode/call/map/encode ONLY. Business decisions
  live in `internal/adminsvc` (sessions, the publish unit of work with its
  PURE derivations + unit tests, desk orchestration, the §9 relay surface);
  the read path lives in `internal/service`; persistence lives in
  `internal/store` (transactions + the index-job InsertTx stay HERE —
  atomicity is a persistence concern). A handler importing store, queue,
  or bao is a layering violation by definition. Shared transport primitives
  are `internal/httpx`. Composition happens only in `cmd/lumen`.
- **Names are read more often than written: spell them out** (owner ruling,
  2026-09 — a reader should never have to decode what a variable is *about*).
  No abbreviations in fields, params, or meaningful locals: `queueClient` not
  `qc`, `searchEngine` not `eng`, `tokenVersion` not `tv`. Standing exceptions,
  because they are language contract rather than habit: HTTP `w`/`r`/`ctx`,
  consistent single-letter receivers, `i`/`v` loop vars where the element type
  is named on the same line, and **wire-contract names verbatim** (the `tv`
  JWT claim is what the contract calls it — code may be clearer than the spec
  never).
- Container image name stays `lumen` — CI rewrites only
  the `images:` line via `kustomize edit set image`, and GHCR names are
  lowercase.
- Every response sets `Cache-Control` per §5 (content JSON short-lived;
  `/media/*` `public, max-age=31536000, immutable` — content-hash filenames
  make `immutable` safe).
- Migrations run at boot (initContainer); the DSN arrives as `LUMEN_DB_URL`
  (ESO-rendered from each environment's Bao path — admin from
  `kv/lumen/shared`, cms from `kv/lumen/{env}`).

## The test battery (in strictness order)

| Command | What it proves | Needs infra? |
| :-- | :-- | :-- |
| `make test` | unit + harness against the in-memory twin | no |
| `make test-integration` | every gated suite against real pg + meili + embedded S3 + dev OpenBao | containers (`up`) |
| `make test-media` | the upload relay against a REAL S3-protocol server | containers |
| `make contract` | earendil's live vitest gate (`contract.spec.ts`) against a RUNNING lumen | a running lumen |

Integration tests own the `itest/` Bao prefix — never purge or reseed paths a
human browses (the shared-queue incident, 2026-11-13).

## Where things live

```
cmd/lumen/          composition root (role wiring happens ONLY here)
cmd/seed/           fixture + seed tooling
internal/api/{public,admin}/   thin handlers (decode/call/map/encode only)
internal/adminsvc/  sessions, publish unit of work, desk, §9 relay surface
internal/service/   the read path
internal/store/     pgx persistence (transactions live here)
internal/{bao,search,media,queue,session,authz,throttle,role}/   the machinery
internal/httpx/     shared transport primitives
internal/devs3/     test-only embedded S3
```

## Commits

Conventional Commits. **Club logically related changes into one commit — never
one commit per file, and no micro-commits**: a handler + its test + the
contract/doc line it satisfies = one commit. Split only genuinely independent
concerns. Branches off `main` — this **is** its own repo now.

The owner's PLAN.md is a local-only journal (gitignored); never recreate a plan
file in Git.
