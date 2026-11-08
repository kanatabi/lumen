LUMEN_DB_URL ?= postgres://lumen:lumen-dev@localhost:5433/lumen?sslmode=disable
LUMEN_MEILI_URL ?= http://localhost:7700
LUMEN_MEILI_KEY ?= dev-meili-key
export LUMEN_DB_URL
export LUMEN_MEILI_URL
export LUMEN_MEILI_KEY

.PHONY: up down seed run test test-integration contract tidy

up:                    ## start dev postgres
	docker compose -f docker-compose.dev.yml up -d --wait

down:                  ## stop dev postgres (data persists in the volume)
	docker compose -f docker-compose.dev.yml down

seed:                  ## apply migrations + load the contract twin
	go run ./cmd/seed

run:                   ## serve §1 on :8080 against dev postgres
	go run ./cmd/lumen

test:                  ## offline gate: unit + harness against the in-memory twin
	go test ./...

test-integration: up     ## the SAME suites against real Postgres + Meilisearch (incl. §8 lifecycle)
	go run ./cmd/seed
	LUMEN_INTEGRATION=1 LUMEN_MEILI_URL=$(LUMEN_MEILI_URL) LUMEN_MEILI_KEY=$(LUMEN_MEILI_KEY) go test -count=1 ./...
	go run ./cmd/seed   ## the admin lifecycle mutates the corpus — restore it for whatever follows

contract:                  ## the earendil-side gate: real vitest against a RUNNING lumen (make run first)
	@curl -sf http://localhost:8080/readyz >/dev/null || { echo "lumen not reachable on :8080 — run 'make run' in another shell"; exit 1; }
	@cd ../earendil && LUMEN_URL=http://localhost:8080 npx vitest run src/lib/api/contract.spec.ts

tidy:
	go mod tidy
