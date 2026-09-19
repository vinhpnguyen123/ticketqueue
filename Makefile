DATABASE_URL ?= postgres://ticketqueue:localdev@localhost:5434/ticketqueue?sslmode=disable
TEST_DATABASE_URL ?= postgres://ticketqueue:localdev@localhost:5434/ticketqueue_test?sslmode=disable

# 8080 is taken by a local Quarkus app, so the API sits on 8081 and the
# simulated provider on 8082. Override any of these on the command line.
PORT ?= 8081
PAYMENT_PORT ?= 8082
PAYMENT_URL ?= http://localhost:8082

.PHONY: up down run run-worker run-payments logs psql migrate-up migrate-down create-test-db migrate-test-up migrate-test-down test test-integration seed demo load-seed load-storm load-journey load-calibrate verify-run sabotage chaos-1 chaos-2 chaos-3 chaos-all docker-build docker-up docker-down test-pgbouncer

up:
	docker compose up -d

down:
	docker compose down

run:
	PORT=$(PORT) PAYMENT_URL=$(PAYMENT_URL) go run ./cmd/api

run-worker:
	PAYMENT_URL=$(PAYMENT_URL) go run ./cmd/worker

run-payments:
	PAYMENT_PORT=$(PAYMENT_PORT) go run ./cmd/payment-service

logs:
	docker compose logs -f

psql:
	docker compose exec postgres psql -U ticketqueue -d ticketqueue

migrate-up:
	migrate -path migrations -database "$(DATABASE_URL)" up

migrate-down:
	migrate -path migrations -database "$(DATABASE_URL)" down 1

# Only needed if the pgdata volume predates scripts/init-test-db.sql
# (docker-entrypoint-initdb.d runs on first init only).
create-test-db:
	docker compose exec postgres psql -U ticketqueue -d ticketqueue -c "CREATE DATABASE ticketqueue_test;"

migrate-test-up:
	migrate -path migrations -database "$(TEST_DATABASE_URL)" up

migrate-test-down:
	migrate -path migrations -database "$(TEST_DATABASE_URL)" down 1

test:
	go test ./...

# -p 1: integration packages share one test DB and Redis index, so they
# must not run in parallel (one package's TRUNCATE would wipe another's rows).
test-integration:
	go test -tags=integration -p 1 ./...

seed:
	go run ./cmd/seed

# End-to-end reliability demo; needs run, run-worker and run-payments up.
demo:
	bash scripts/demo_reconciliation.sh

# ---- Load testing -----------------------------------------------------------
# TICKETS=100 is the oversell proof; TICKETS=2000 is the latency proof. Never
# quote a p(95) taken from the 100-ticket run: it has only 100 samples.
TICKETS ?= 100
USERS ?= 2000
WINDOWS ?= 50
VUS ?= 2000

load-seed:
	go run ./cmd/loadseed -tickets=$(TICKETS) -users=$(USERS) -windows=$(WINDOWS)

load-storm:
	k6 run -e VUS=$(VUS) scripts/k6/purchase_storm.js

load-journey:
	k6 run -e VUS=$(VUS) scripts/k6/full_journey.js

load-calibrate:
	k6 run scripts/k6/calibrate.js

# Run after every load test. Exits non-zero if anything is wrong.
verify-run:
	bash scripts/verify_run.sh

# Proves the verification suite can actually fail. See scripts/k6/README.md.
sabotage:
	docker compose exec -T postgres psql -U ticketqueue -d ticketqueue -f - < scripts/sabotage.sql

# ---- Chaos ------------------------------------------------------------------
# Each scenario starts and kills the binaries itself, so stop any `make run` /
# `make run-worker` / `make run-payments` first. Results land in docs/chaos/.
chaos-1:
	bash scripts/chaos/chaos1_api_kill.sh

chaos-2:
	bash scripts/chaos/chaos2_provider_outage.sh

chaos-3:
	bash scripts/chaos/chaos3_worker_kill.sh

chaos-all: chaos-1 chaos-2 chaos-3

# ---- Containers -------------------------------------------------------------
# GIT_SHA is baked into each binary and returned by /health. An image tag can be
# re-pushed under the same name; the commit in the binary cannot.
GIT_SHA ?= $(shell git rev-parse --short HEAD)
IMAGE_TAG ?= local

docker-build:
	@for b in api worker payment-service; do \
		echo "building $$b"; \
		docker build -q -f build/$$b.Dockerfile --build-arg GIT_SHA=$(GIT_SHA) \
			-t ticketqueue/$$b:$(IMAGE_TAG) . >/dev/null || exit 1; \
	done
	@echo; docker images --format '{{.Repository}}:{{.Tag}}\t{{.Size}}' | grep '^ticketqueue/'

# Runs the BUILT IMAGES rather than `go run`, through PgBouncer, so what is
# tested is what gets deployed. Everything else (postgres, redis, prometheus,
# grafana, pgbouncer) comes up as usual.
docker-up: docker-build
	docker compose --profile app up -d
	@echo "api      http://localhost:$(PORT)/v1/health"
	@echo "payments http://localhost:$(PAYMENT_PORT)/health"

# `--profile app down` would take the whole project with it — postgres, redis,
# prometheus, the lot — because `down` is project-scoped, not profile-scoped.
# Stop and remove only the three app containers.
docker-down:
	docker compose rm -sf api worker payment-service

# Integration suite through PgBouncer, i.e. against the deployed topology.
test-pgbouncer:
	TEST_DATABASE_URL="postgres://ticketqueue:localdev@localhost:6432/ticketqueue_test?sslmode=disable" \
	PGBOUNCER=true go test -tags=integration -p 1 -count=1 ./...
