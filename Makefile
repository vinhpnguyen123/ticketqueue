DATABASE_URL ?= postgres://ticketqueue:localdev@localhost:5434/ticketqueue?sslmode=disable
TEST_DATABASE_URL ?= postgres://ticketqueue:localdev@localhost:5434/ticketqueue_test?sslmode=disable

.PHONY: up down run run-worker logs psql migrate-up migrate-down create-test-db migrate-test-up migrate-test-down test test-integration seed

up:
	docker compose up -d

down:
	docker compose down

run:
	go run ./cmd/api

run-worker:
	go run ./cmd/worker

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
