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
| Admin account | upserts `gofar` / `mellon-dev-password` via `cmd/userctl` (argon2id in DB; password never leaves env) | `users.passhash` |
| The server | `go run ./cmd/lumen` on `:8080` | — |

Every dev credential above is a throwaway placeholder in Git **on purpose** —
real values live in OpenBao (`lumen/{env}`), rendered by ESO (INFRA §5). The
config layer fail-fasts, so the honest absence of anything critical is a
start-up message, never a runtime surprise: no `LUMEN_MEILI_URL` with Postgres
→ refuses to boot (the engine IS the read path); no `LUMEN_SESSION_JWT_SECRET`
→ refuses; no media/bao config → the matching surface 503s or isn't mounted.

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

[`postman/lumen.postman_collection.json`](postman/lumen.postman_collection.json)
covers the full surface — 59 requests in 6 ordered folders: health, public §1
(every facet/sort/window, the draft-leak 404 proof), admin §8 posts lifecycle
(CAS 409s, publish derivations, idempotent re-publish, soft-delete-frees-the-slug,
the admin search door seeing drafts), photos + upload relay, vocabulary, the
revocation proof (logout → replay the same token → 401), and the §9 Bao folder
(paths / version pin / `?meta` / CAS conflicts).

Import it, run `make dev`, and press Send: variables ship with the dev
placeholders, and any **login** request captures `{{token}}` into the
collection — every admin request inherits it (collection-level Bearer auth).
The folders are self-cleaning and re-runnable; only two deliberate exceptions
documented in-request: vocabulary rows persist by design (201-or-409 proves
eager creation), and `postman/sample.png` must be re-picked in the upload's
file field once (paths are machine-local). The §9 folder needs `make
dev-plane` first — without it those routes honestly do not exist (404), which
is itself the design.

Requests carry `x-lumen-*` extensions (Postman ignores unknown keys): the
executable expectations — status, body substrings, variable captures. A
throwaway harness walks the collection against the live binary before it is
ever shipped, so the collection is proven, not presumed.

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
