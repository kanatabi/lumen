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
# dev signing secret — a throwaway placeholder in the same spirit as the
# compose creds below it; prod values live in Bao lumen/{env} (§9, and
# config fail-fasts on an empty one)
LUMEN_SESSION_JWT_SECRET ?= dev-session-secret-32-bytes-local!
# dev admin account for the Postman collection / manual testing
DEV_USER ?= gofar
DEV_PASSWORD ?= mellon-dev-password
# make dev-plane writes dev-plane.env (gitignored); sinclude = absent is OK:
# no LUMEN_BAO_* ⇒ the §9 surface is honestly unmounted (honest absence)
-include dev-plane.env
export LUMEN_DB_URL
export LUMEN_SESSION_JWT_SECRET
export LUMEN_BAO_ADDR LUMEN_BAO_ROLE_ID LUMEN_BAO_SECRET_ID
export LUMEN_MEILI_URL
export LUMEN_MEILI_KEY
export LUMEN_MEDIA_ENDPOINT LUMEN_MEDIA_REGION LUMEN_MEDIA_BUCKET
export LUMEN_MEDIA_ACCESS_KEY LUMEN_MEDIA_SECRET_KEY
export LUMEN_MEDIA_FORCE_PATH LUMEN_MEDIA_CREATE_BUCKET

.PHONY: help dev dev-plane up down seed user run test test-integration test-media contract devs3 tidy

help:                  ## list targets
	@grep -hE '^[a-z][a-z-]*:.*## ' Makefile | sed 's/:.*## /  — /'

dev:                   ## THE one-command loop — reuses whatever already runs (containers, data, server); creates only what's missing
	@docker compose -f docker-compose.dev.yml ps --status running --services 2>/dev/null | grep -qx pg 		&& echo "  containers: reusing what's up (compose up is a no-op for matching containers)" 		|| docker compose -f docker-compose.dev.yml up -d --wait
	@if curl -sf http://localhost:8080/readyz >/dev/null 2>&1; then 		echo "  lumen: already serving :8080 — nothing to do (its Makefile siblings start what's missing)"; 	else 		$(MAKE) --no-print-directory user seed; 		echo "  serving :8080 (Ctrl-C stops the server; containers and data stay)"; 		go run ./cmd/lumen; 	fi

dev-plane:             ## bootstrap the dev OpenBao §5.6 shape (kv/policy/approle) → writes dev-plane.env; then restart make dev for /admin/bao
	bash scripts/dev-plane.sh

user:                  ## upsert the dev admin account (idempotent; token_version preserved)
	LUMEN_PASSWORD=$(DEV_PASSWORD) go run ./cmd/userctl -username $(DEV_USER)

seed:                  ## migrations + engine reconcile; reloads the fixture ONLY on an empty DB (FORCE=1 to reset)

up:                    ## start dev stack: postgres + meilisearch + openbao (bao storage is in-memory dev mode)
	docker compose -f docker-compose.dev.yml up -d --wait

devs3:                 ## S3-protocol dev server on :4566 (in-memory) — reuses one that's already up
	@curl -sf http://localhost:4566/ >/dev/null 2>&1 		&& echo "devs3: already serving :4566 — reusing it" 		|| LUMEN_MEDIA_DEV_ADDR=:4566 LUMEN_MEDIA_BUCKET=$(LUMEN_MEDIA_BUCKET) go run ./cmd/devs3

down:                  ## stop the dev stack (pg/meili volumes persist; openbao dev storage does not — rerun make dev-plane after)
	docker compose -f docker-compose.dev.yml down

seed:                  ## apply migrations + load the contract twin
	go run ./cmd/seed

run:                   ## serve the API (no infra steps — what `dev` runs at the end)
	go run ./cmd/lumen

test:                  ## offline gate: unit + harness against the in-memory twin
	go test ./...

test-integration: up   ## EVERY gated suite against real infra: pg + meili + embedded S3 + dev OpenBao
	FORCE=1 go run ./cmd/seed   ## tests need the deterministic corpus — explicit reset
	LUMEN_INTEGRATION=1 LUMEN_MEDIA=1 LUMEN_BAO_ADDR=http://localhost:8200 go test -count=1 ./...
	FORCE=1 go run ./cmd/seed   ## lifecycles mutate the corpus — restore it for whatever follows

test-media: up         ## upload relay against a REAL S3-protocol server (gofakes3 embedded)
	go run ./cmd/seed
	LUMEN_INTEGRATION=1 LUMEN_MEDIA=1 go test -count=1 ./internal/httpapi/ -run AdminLifecycle

contract:                  ## the earendil-side gate: real vitest against a RUNNING lumen (make run first)
	@curl -sf http://localhost:8080/readyz >/dev/null || { echo "lumen not reachable on :8080 — run 'make run' in another shell"; exit 1; }
	@cd ../earendil && LUMEN_URL=http://localhost:8080 npx vitest run src/lib/api/contract.spec.ts

tidy:
	go mod tidy
