# lumen

REST API + content pipeline for the blog — **design stage only**, no service
code yet. Submodule of the [`omicron`](https://github.com/kanatabi/omicron)
superproject.

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
