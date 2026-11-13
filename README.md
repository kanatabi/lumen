# lumen

REST API + content pipeline for the blog. **Phase B is executed code**: the §1
read API (Meilisearch-served, Postgres-hydrated), the §8 admin API (JWT sessions
with instant token-version revocation, CAS writes, the §2 publish transaction),
the §5 media relay (decode-and-rebuild uploads to S3-compatible storage), and the
§9 OpenBao passthrough — all accepted live by earendil's own contract suite.
Submodule of the [`omicron`](https://github.com/kanatabi/omicron) superproject.

## Quickstart (local dev)

```
make dev            # infra + seed + dev admin user + serve :8080 (Ctrl-C stops lumen only)
```

That one line brings up the whole loop (`-d` details in `make help`):

| Piece | What `make dev` does | Where |
| :--- | :--- | :--- |
| Postgres 16 | container `:5433`, user/pass `lumen`/`lumen-dev` | compose, volume persists |
| Meilisearch | pinned `v1.56.1` on `:7700`, key `dev-meili-key` — **the read path** (lists & search answer here) | compose, volume persists |
| OpenBao | pinned `ghcr.io/openbao/openbao:2.5.3` on `:8200`, dev mode (root `dev-root-token`) — **the same product production runs** | compose, storage in-memory |
| Migrations + corpus | `cmd/seed`: schema + queue tables + fixture, then indexes everything **through the real river queue** (drained before exit) | `internal/store/migrations/` |
| Admin account | upserts `gofar` / `mellon-dev-password` via `lumen user` (argon2id in DB; password never leaves env) | `users.passhash` |
| The server | `go run ./cmd/lumen` on `:8080` | — |

Every dev credential above is a throwaway placeholder in Git **on purpose** —
real values live in OpenBao (`lumen/{env}`), rendered by ESO (INFRA §5). The
config layer fail-fasts, so the honest absence of anything critical is a
start-up message, never a runtime surprise: no `LUMEN_MEILI_URL` with Postgres
→ refuses to boot (the engine IS the read path); no `LUMEN_SESSION_JWT_SECRET`
→ refuses; no media/bao config → the matching surface 503s or isn't mounted.
(The dev loop runs the legacy `all` plane — `LUMEN_ROLE` unset; the cluster
deploys `admin`/`cms` per DESIGN §8b, each binding only its own key family —
`go run ./cmd/lumen config keys --role admin|cms` prints the exact set.)

### Optional surfaces

```
make devs3          # S3-protocol dev endpoint on :4566 (gofakes3, in-memory)
                    # → uploads (POST /admin/photos) work against a REAL
                    #   SigV4 server. MinIO withdrew public images and
                    #   LocalStack wants an auth token; gofakes3 keeps the
                    #   provider choice (HUMAN-GATE #9) a VALUES decision.
make dev-plane      # bootstrap the dev OpenBao into the §5.6 shape:
                    #   kv-v2 mount, fzr-portal policy (kv/* ONLY), AppRole,
                    #   seeded kv/lumen/{dev,staging,production}
                    # → writes dev-plane.env (gitignored); the NEXT `make dev`
                    #   picks it up and serves /admin/bao (§9 relay live).
```

## Postman collection

[`postman/lumen.postman_collection.json`](postman/lumen.postman_collection.json) is an
**endpoint catalog**: one request per API endpoint (32, in 10 resource folders), and
**every scenario case is a saved EXAMPLE on its endpoint** — pick `GET /api/posts/{slug}`
and its Examples tab shows the 200 detail, the draft-leak 404, and the unknown-slug 404,
each with the request that produced it **and the real response captured live** from the
dev binary (2026-11-09; ids/timestamps are dev artifacts, JWTs truncated). Markdown docs
on every endpoint: auth, parameter tables, status codes with the *why*, doctrine notes.

**Environments** (`postman/environments/`): `lumen-dev` (prefilled with the throwaway
compose placeholders), `lumen-staging`, `lumen-production`. The API has **no public
ingress, ever** (INFRA §2) — so staging/prod reach is port-forward, and all three
environments keep `baseUrl = http://localhost:8080`:

```
kubectl -n <ns> port-forward svc/lumen 8080:8080    # then switch environment, press Send
```

Staging/prod env files ship with an **empty password by doctrine** — type it at runtime
and do NOT save the environment; real values live in OpenBao, never in Git.

Working the collection: pick an environment, run `POST /admin/session` once (it captures
`{{token}}`; every admin request inherits it), then browse. Folder descriptions explain
how to re-drive the lifecycle from zero (`x-lumen-state: historical` marks examples whose
meaning is the captured moment — e.g. the revoked-token replay). The §9 folder needs
`make dev-plane` + a server restart; without the plane those routes honestly 404.

Every read-only example and every default request was **replayed against the live binary
and matched its stored status** before this file was committed — a collection is a
contract statement, and unverified statements are the bug class this repo has now been
burned by twice. `postman/sample.png` is a real 8×8 PNG so the upload endpoint is
Send-able out of the box (re-pick it once in the file field — paths are machine-local).

## Commands

```
make help               # everything below, self-documented
make up / make down     # just the containers (pg + meili + openbao)
make seed               # migrations + fixture + index-through-the-queue
make user               # upsert the dev admin account (DEV_USER/DEV_PASSWORD override)
make run                # serve (what `dev` runs last, minus the infra steps)
make test               # OFFLINE gate: unit + contract harness vs the in-memory twin
make test-integration   # the gated suites against REAL infra:
                        #   pg + meili + embedded S3 + dev OpenBao
                        #   (incl. login→use→logout→replay-401 and the throttle battery)
make contract           # the acceptance test: earendil's vitest vs a RUNNING lumen
                        #   (start `make dev` in another shell first; 13 tests)
make devs3              # S3-protocol dev server (see above)
make dev-plane          # OpenBao §5.6 bootstrap for this box (see above)
make tidy
```

Go 1.26 · chi · pgx · viper · Meilisearch `v1.56.1` · river `v0.51.1` ·
OpenBao (KV v2 + AppRole). Migrations are plain SQL at
`internal/store/migrations/`, applied by `cmd/seed` (river's queue tables
migrate through river's own migrator — the queue schema belongs to river).

| Doc | What |
| :--- | :--- |
| [`DESIGN.md`](DESIGN.md) | API contract, serving model (§4), publish pipeline, media (§5), admin API (§8), secrets relay (§9) |
| [`AGENTS.md`](AGENTS.md) | Working rules for AI agents / contributors |

**The living contract** is `BlogApi` in
[`earendil/src/lib/api/client.ts`](https://github.com/kanatabi/earendil/blob/main/src/lib/api/client.ts)
— this repo's `DESIGN.md` §1 mirrors it and the two move together; the admin
mirror is `mellon/src/lib/api/types.ts` (`AdminApi`).

Siblings: [`earendil`](https://github.com/kanatabi/earendil) (frontend, source
of truth) · [`mellon`](https://github.com/kanatabi/mellon) (admin portal — the
only client of §8/§9, relay-only, holds no credentials) ·
[`vortex`](https://github.com/kanatabi/vortex) (GitOps manifests this service
deploys through).
