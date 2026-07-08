DATABASE_URL ?= postgres://ticketqueue:localdev@localhost:5434/ticketqueue?sslmode=disable

.PHONY: up down run logs psql migrate-up migrate-down test seed

up:
	docker compose up -d

down:
	docker compose down

run:
	go run ./cmd/api

logs:
	docker compose logs -f

psql:
	docker compose exec postgres psql -U ticketqueue -d ticketqueue

migrate-up:
	migrate -path migrations -database "$(DATABASE_URL)" up

migrate-down:
	migrate -path migrations -database "$(DATABASE_URL)" down 1

test:
	go test ./...

seed:
	go run ./cmd/seed
