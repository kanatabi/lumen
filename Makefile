LUMEN_DB_URL ?= postgres://lumen:lumen-dev@localhost:5433/lumen?sslmode=disable
LUMEN_MEILI_URL ?= http://localhost:7700
LUMEN_MEILI_KEY ?= dev-meili-key
LUMEN_MEDIA_ENDPOINT ?= http://localhost:4566
LUMEN_MEDIA_REGION ?= us-east-1
LUMEN_MEDIA_BUCKET ?= lumen-media-dev
LUMEN_MEDIA_ACCESS_KEY ?= test
LUMEN_MEDIA_SECRET_KEY ?= test
LUMEN_MEDIA_FORCE_PATH ?= true
LUMEN_MEDIA_CREATE_BUCKET ?= true
export LUMEN_DB_URL
export LUMEN_MEILI_URL
export LUMEN_MEILI_KEY
export LUMEN_MEDIA_ENDPOINT LUMEN_MEDIA_REGION LUMEN_MEDIA_BUCKET
export LUMEN_MEDIA_ACCESS_KEY LUMEN_MEDIA_SECRET_KEY
export LUMEN_MEDIA_FORCE_PATH LUMEN_MEDIA_CREATE_BUCKET

.PHONY: up down seed run test test-integration contract tidy

up:                    ## start dev postgres + meilisearch
	docker compose -f docker-compose.dev.yml up -d --wait

devs3:                 ## S3-protocol dev server on :4566 (gofakes3, in-memory; leave running)
	LUMEN_MEDIA_DEV_ADDR=:4566 LUMEN_MEDIA_BUCKET=$(LUMEN_MEDIA_BUCKET) go run ./cmd/devs3

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

test-media: up         ## upload relay against a REAL S3-protocol server (gofakes3 embedded)
	go run ./cmd/seed
	LUMEN_INTEGRATION=1 LUMEN_MEDIA=1 go test -count=1 ./internal/httpapi/ -run AdminLifecycle

contract:                  ## the earendil-side gate: real vitest against a RUNNING lumen (make run first)
	@curl -sf http://localhost:8080/readyz >/dev/null || { echo "lumen not reachable on :8080 — run 'make run' in another shell"; exit 1; }
	@cd ../earendil && LUMEN_URL=http://localhost:8080 npx vitest run src/lib/api/contract.spec.ts

tidy:
	go mod tidy
