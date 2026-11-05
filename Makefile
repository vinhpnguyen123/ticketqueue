DATABASE_URL ?= postgres://ticketqueue:localdev@localhost:5434/ticketqueue?sslmode=disable
TEST_DATABASE_URL ?= postgres://ticketqueue:localdev@localhost:5434/ticketqueue_test?sslmode=disable

# 8080 is taken by a local Quarkus app, so the API sits on 8081 and the
# simulated provider on 8082. Override any of these on the command line.
PORT ?= 8081
PAYMENT_PORT ?= 8082
PAYMENT_URL ?= http://localhost:8082

# Real deployments get theirs from Secrets Manager. Locally, a random secret is
# generated once into a git-ignored file, so the API and the worker share it and
# no signing key is ever published in this repo. The binaries refuse to start
# without one.
ifndef JWT_SECRET
JWT_SECRET := $(shell test -s .env.jwt-secret || (umask 077; openssl rand -hex 32 > .env.jwt-secret); cat .env.jwt-secret)
endif
export JWT_SECRET

.PHONY: up down run run-worker run-payments logs psql migrate-up migrate-down create-test-db migrate-test-up migrate-test-down test test-integration seed demo load-seed load-storm load-journey load-calibrate verify-run sabotage chaos-1 chaos-2 chaos-3 chaos-all docker-build docker-up docker-down test-pgbouncer docker-push tf-init tf-plan tf-apply tf-migrate tf-seed tf-destroy tf-cost

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
	@for b in api worker payment-service migrate; do \
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

# ---- AWS ---------------------------------------------------------------------
# Terraform lives in terraform/ with LOCAL state. Read terraform/README.md
# before the first apply — the destroy checklist is at the top of it on purpose.
TF      := terraform -chdir=terraform
REGION  ?= us-east-1
CLUSTER ?= ticketqueue

tf-init:
	$(TF) init

tf-plan:
	$(TF) plan -var image_tag=$(GIT_SHA)

tf-apply:
	$(TF) apply -var image_tag=$(GIT_SHA)

# Build for ARM64 and push to ECR. The images must exist before `tf-apply`
# creates services that pull them, and the tag is the git SHA so /v1/health can
# be matched against a commit.
docker-push: docker-build
	@set -e; \
	acct=$$(aws sts get-caller-identity --query Account --output text); \
	reg=$$acct.dkr.ecr.$(REGION).amazonaws.com; \
	aws ecr get-login-password --region $(REGION) | docker login --username AWS --password-stdin $$reg; \
	for b in api worker payment-service migrate; do \
		docker tag ticketqueue/$$b:$(IMAGE_TAG) $$reg/ticketqueue/$$b:$(GIT_SHA); \
		docker push $$reg/ticketqueue/$$b:$(GIT_SHA); \
	done; \
	echo; echo "pushed at tag $(GIT_SHA)"

# Schema, as a one-shot task. Never from API startup: two API tasks booting at
# once would race on the migration table. Waits and fails if the migration does.
# CMD overrides `up`, for recovery: make tf-migrate CMD="force 14"
tf-migrate:
	@CLUSTER=$(CLUSTER) REGION=$(REGION) bash scripts/tf_migrate.sh $(CMD)

tf-seed:
	@echo "Seeding runs from the load generator, which is inside the VPC."
	@echo "RDS is not publicly accessible, so there is nowhere else to run it from."
	@echo "  make tf-loadgen-sync && ssh ec2-user@<loadgen-ip> './loadseed -tickets=... -users=...'"

# Prints what is still billing. Run it after destroy, every time.
tf-cost:
	@aws resourcegroupstaggingapi get-resources \
	  --tag-filters Key=Project,Values=ticketqueue --region $(REGION) \
	  --query 'ResourceTagMappingList[].ResourceARN' --output table

tf-destroy:
	@echo "This destroys the ENTIRE ticketqueue AWS stack: RDS (no final snapshot),"
	@echo "ElastiCache, ECS services, ALB, secrets, and the load generator."
	@printf 'Type the word destroy to continue: '; read ans; [ "$$ans" = destroy ] || { echo aborted; exit 1; }
	$(TF) destroy
	@echo
	@echo "=== Anything listed below is STILL RUNNING and still billing ==="
	@$(MAKE) tf-cost
	@echo "=== An empty table above is what you want. ==="
