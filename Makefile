DATABASE_URL ?= postgres://ticketqueue:localdev@localhost:5434/ticketqueue?sslmode=disable
TEST_DATABASE_URL ?= postgres://ticketqueue:localdev@localhost:5434/ticketqueue_test?sslmode=disable

# 8080 is taken by a local Quarkus app, so the API sits on 8081 and the
# simulated provider on 8082. Override any of these on the command line.
PORT ?= 8081
PAYMENT_PORT ?= 8082
PAYMENT_URL ?= http://localhost:8082

.PHONY: up down run run-worker run-payments logs psql migrate-up migrate-down create-test-db migrate-test-up migrate-test-down test test-integration seed demo

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
