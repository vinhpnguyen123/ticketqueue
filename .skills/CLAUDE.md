# CLAUDE.md — TicketQueue

Read this fully at the start of every session before writing code.

## What this project is

A fintech-grade event ticket booking backend in Go. It guarantees **zero overselling** under heavy concurrency and **zero double-charges** under network failure. It exists to demonstrate payment-correctness engineering for backend/fintech roles. Solo portfolio project, ~8 weeks.

The hard parts are NOT the queue. They are: overselling prevention under concurrency, idempotent retries, and graceful recovery from network failures via reconciliation.

## Stack (do not substitute without explicit instruction)

- Go 1.22+, chi router, pgx/v5 (pgxpool), go-redis/v9, golang-jwt/v5, alexedwards/argon2id
- PostgreSQL 18, Redis 7, Docker Compose, golang-migrate
- Three binaries: cmd/api, cmd/worker, cmd/payment-service. Plus cmd/seed.
- k6 for load testing (Week 6). AWS/Terraform for deploy (Week 7).

## The three load-bearing principles (never violate)

1. **Postgres is truth, Redis is speed.** Anything in Redis must be rebuildable from Postgres. On disagreement, Postgres wins.
2. **Never hold a DB transaction open across a network call.** Use the two-transaction split with the payment call in between.
3. **Every state-creating / money operation needs an idempotency key.**

## Locked architecture decisions (DO NOT re-open these — they are settled)

- **Go, not Quarkus/Node.** Settled. Do not suggest switching languages.
- **Simulated payment service, NOT real Stripe.** Deliberate: real providers can't inject dropped-response/timeout scenarios the reconciliation worker exists to handle. The sim mirrors Stripe's contract so a real swap would be an adapter change. Do not suggest integrating real Stripe.
- **Merged BOOKING table** (holds + orders combined). No separate holds table, no order_items table. Tickets reference booking_id directly.
- **Single tier per booking** (booking has ticket_tier_id + quantity). Multi-tier carts are out of scope by design.
- **Pessimistic locking** (SELECT FOR UPDATE / SKIP LOCKED) is the primary strategy. Bookings also carry a `version` column for optimistic guarding on state transitions.
- **Pre-created ticket rows** (one row per ticket, created at tier creation). Tickets are rows, never a counter. This is what enables SKIP LOCKED + per-ticket audit + the Layer-3 structural guarantee.
- **Option A window counting:** a purchase window = an admitted, unexpired queue_entries row. `booking.hold_expires_at` is the ticket-release deadline (10 min, longer than the 30s reconciliation cycle), NOT the shopping window.
- **Read Committed isolation.** Correctness comes from row locks + CHECK constraints, not isolation escalation.

If I ask to re-open one of these, remind me it's settled and ask if I'm sure — I have a pattern of re-litigating foundations to avoid hard implementation work.

## The overselling defense (three layers — know which is which)

- Layer 1: Redis Lua atomic check-and-decrement on `inventory:{tier_id}` (fast reject).
- Layer 2: `SELECT ... FOR UPDATE SKIP LOCKED` on individual available ticket rows (transactional claim).
- Layer 3: structural — each ticket row holds one booking_id; guarded UPDATE (`WHERE status='available' AND booking_id IS NULL`, assert RowsAffected == qty) + the tickets CHECK constraint. Double-assignment is impossible under concurrency.

## Database: 9 tables

users, events, ticket_tier, booking, tickets, queue_entries, idempotency_keys, payments, audit_log.

- Money is always BIGINT cents + CHAR(3) currency. Never floats.
- Lifecycle CHECK constraints on booking and tickets make illegal states unrepresentable.
- Partial indexes on the hot paths: idx_tickets_claim (WHERE status='available'), idx_booking_reconciliation (WHERE status='held'). These MUST stay partial — they keep hot queries on index scans as events sell out.
- audit_log has NO foreign keys by design (must outlive entities, high-throughput inserts). Polymorphic via entity_type + entity_id.
- The full schema is in ./migrations. Migrations are the single source of truth — the Notion ERD and any summary can be stale; trust the migration files.

## Two idempotency mechanisms (don't confuse them)

- `booking.idempotency_key`: the cross-system bridge to the payment provider (used at charge time and by reconciliation lookup).
- `idempotency_keys` table: HTTP-level dedup — request_hash, cached response, locked_at for concurrency serialization, expires_at for cleanup. PK is (key, user_id).

## Coding rules

- Every new endpoint uses the shared response envelope ({"data",...,"request_id"} / {"error":{"type","message","request_id"}}) and the standard error `type` codes.
- Every state change writes an audit_log row with the correct actor_type (user | worker | admin | system).
- Guarded UPDATEs everywhere: include the expected current status in WHERE; assert RowsAffected; never blind-update state.
- Redis compensation (INCRBY back) happens AFTER commit/rollback, never inside the DB transaction. If a compensation is lost to a crash, the drift checker heals it — that window is accepted.
- Workers use the shared Runner (recover from panic, log cycle duration, continue). All worker queries use FOR UPDATE SKIP LOCKED.
- Never log passwords, password hashes, payment tokens, full request bodies on payment endpoints, or Authorization headers.
- Prefer raw SQL via pgx. No ORM.
- After each prompt, give me what files you added, what did you do with that files, what function you made.
- After that, gimme ways to test out API Endpoints with edge cases and easy cases both write out so that i can test manually.


## Testing rules (non-negotiable)

- A feature is done when its test passes, not when the code is written. Write tests in the same session as the feature.
- Integration tests (//go:build integration) run against real Dockerized Postgres/Redis using the ticketqueue_test DB (truncate per test via testutil). Redis tests use DB index 1 + FLUSHDB.
- Concurrency-sensitive code MUST have a goroutine-based test (e.g., N goroutines claiming M tickets → exactly M succeed, zero double-assignment). Run flaky/concurrency tests 3× before calling them green.
- `make test` (unit) and `make test-integration` must both be green before every commit. Never commit red.
- Run the FULL suite at each week's gate to catch regressions.

## Commit / workflow rules

- Small commits, commit when something works. Descriptive messages.
- Verify manually (curl the real flow) in addition to tests before committing an endpoint.
- I (the human) must read and understand every line of the concurrency-critical code (TX1, TX2, Lua scripts, workers, idempotency middleware) — do not let me accept it blindly. Explain non-obvious lines.

## PROGRESS TRACKER (update this at the end of each session)

DONE:

- Week 0: skeleton, docker-compose (PG+Redis), 9-table schema + migrations (up/down verified), seed (100 VIP + 400 General tickets)
- Week 1: test infra (test DB, testutil, response envelope), auth (register/login/me, argon2id, JWT, enumeration-safe login), events endpoints (list + detail, cursor pagination, money-as-object)
- Week 2: Redis sorted-set queue (join/position/leave + crash rebuild), rate limiting (fail-open), promotion worker (Option-A counting, ZPOPMIN FIFO + jitter, expired-window sweep)
-

NEXT Week 3:

- [x] Simulated payment service (failure-injection tokens incl. tok_drops_response)
- [x] Idempotency middleware (atomic ON CONFLICT claim, replay, in-progress serialization, cleanup worker),
- [x] TX1 purchase (Lua + SKIP LOCKED claim + Layer-3 guarded assignment), hold-expiration worker
      — Week 4 (the payoff week; highest risk):
- [x] TX2: replace the temporary seam in POST /v1/purchases — finalize booking to confirmed/failed based on payment result; write payments row; guarded UPDATE tickets held→sold; audit
- [x] Handle the ambiguous 504 case: payment timeout/dropped response → leave booking 'held', return 504 payment_processing_unclear with booking_id
- [x] Synchronous reconciliation on retry (retry with same key + still-held booking → query provider, resolve, respond with real outcome)
- [x] Reconciliation worker: find held bookings past a short threshold, query provider by idempotency key, resolve confirmed/failed, never guess if provider unreachable, audit every resolution
- [x] Payment-vs-expiration race: both paths lock booking row FOR UPDATE; deterministic winner; verify with a test
- [x] THE CARL DEMO: purchase with tok_drops_response → 504 → reconciliation worker resolves booking to confirmed within ~60s, visible in audit log. This is the headline artifact.

LATER:

- Week 5: order/booking history endpoints, admin endpoints (stuck bookings, manual reconcile, audit trail), webhook + HMAC (cut first if behind), drift checker (incl. tickets-count-vs-quantity_total invariant)
- Week 6: k6 load test — 10k concurrent purchases at 100 tickets → exactly 100 sold, 0 oversells, measure P95; chaos test (kill API mid-load)
- Week 7: Dockerfiles, Terraform (VPC/RDS/ElastiCache/ECS/ALB), deploy, demo video, tear down. Add PgBouncer here (transaction mode; set pgx to simple protocol to avoid prepared-statement breakage).
- Week 8: README (pitch, headline load-test result, 3 headline diagrams, 13 problems, schema, privacy section, honest production-hardening section), interview prep.

## Cut line (cut from the TOP when behind schedule; never extend timeline)

1. Webhook endpoint (reconciliation alone is correct without it)
2. Drift checker worker
3. Admin inventory endpoint
4. Queue leave endpoint
5. Synchronous reconciliation on retry (fall back to cache-and-replay)
6. AWS deployment (local Docker demo + video instead)
   NEVER cut: purchase flow, idempotency, reconciliation worker, hold expiration, the load test.
