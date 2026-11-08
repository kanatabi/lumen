# lumen

REST API + content pipeline for the blog — **M1 (read API) is live code**;
admin write API (§8), sessions (JWT) and the Bao passthrough follow per the
mellon PLAN milestones. Submodule of the
[`omicron`](https://github.com/kanatabi/omicron) superproject.

```
make up && make seed && make run      # dev: compose postgres → migrations+fixture → serve :8080
make test                             # offline: unit + contract suite vs the in-memory twin
make test-integration                 # the same suite against REAL postgres
make contract                         # the acceptance test: earendil's vitest vs live lumen
                                      # (LUMEN_URL-gated; 13 tests — skipped when offline)
```

Go 1.26 · chi · pgx (prepared statements, doctrine in AGENTS.md) · viper.
Migrations are plain SQL at `internal/store/migrations/`, applied by `cmd/seed`.

| Doc | What |
| :--- | :--- |
| [`DESIGN.md`](DESIGN.md) | API contract, publish pipeline, open decisions (D1 media serving, D2 RSS path) |
| [`AGENTS.md`](AGENTS.md) | Working rules for AI agents / contributors |

**The living contract** is `BlogApi` in
[`earendil/src/lib/api/client.ts`](https://github.com/kanatabi/earendil/blob/main/src/lib/api/client.ts)
— this repo's `DESIGN.md` §1 mirrors it and the two move together.

Siblings: [`earendil`](https://github.com/kanatabi/earendil) (frontend, source
of truth) · [`vortex`](https://github.com/kanatabi/vortex) (GitOps manifests
this service will deploy through).
