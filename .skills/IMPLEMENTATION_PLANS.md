TICKETQUEUE — IMPLEMENTATION PLAN (SOURCE OF TRUTH)

Last updated: end of Week 4 (rev. 3 — see §18 for what changed). This document overrides any older summary, ERD image, or chat memory. When anything conflicts, THIS + the ./migrations files win.

═══════════════════════════════════════════

1. WHAT THIS IS

═══════════════════════════════════════════ A fintech-grade event ticket booking backend in Go. Guarantees zero overselling under heavy concurrency and zero double-charges under network failure. Portfolio flagship targeting backend/fintech roles. Solo, ~8 weeks.

The hard parts are NOT the queue. They are: overselling prevention under concurrency, idempotent retries, and recovery from network failures via reconciliation.

═══════════════════════════════════════════

2. STACK (do not substitute)

═══════════════════════════════════════════ Go 1.22+ · chi · pgx/v5 (pgxpool) · go-redis/v9 · golang-jwt/v5 · alexedwards/argon2id · prometheus/client_golang PostgreSQL 16 · Redis 7 · Docker Compose · golang-migrate · Prometheus + Grafana (Week 5) · k6 (Week 6) · AWS+Terraform (Week 7) Binaries: cmd/api, cmd/worker, cmd/payment-service, cmd/seed Raw SQL via pgx. No ORM.

═══════════════════════════════════════════

3. THREE LOAD-BEARING PRINCIPLES (never violate)

═══════════════════════════════════════════

Postgres is truth, Redis is speed. Redis is rebuildable; on disagreement Postgres wins.
Never hold a DB transaction (or row lock) open across a network call. Two-transaction split, payment/provider call in between. Applies to workers too.
Every state-creating / money operation needs an idempotency key.

Corollary of 1+3 (money rule): never release a booking's tickets on a guess about money. A held booking is only failed after the provider gives a verdict ("no charge" or "charge failed"). Frozen inventory is recoverable; charged-without-ticket is a customer harm.

═══════════════════════════════════════════

4. LOCKED DECISIONS (settled — do not re-open)

═══════════════════════════════════════════

Go, not Quarkus/Node.
Simulated payment service, NOT real Stripe. Reason: real providers can't inject dropped-response/timeout scenarios reconciliation exists to handle. Sim mirrors Stripe's contract → real swap is an adapter change.
Merged BOOKING table (holds+orders combined). No holds table, no order_items table. Tickets reference booking_id directly.
Single tier per booking (booking.ticket_tier_id + quantity). No carts, no multi-tier purchases. The system has NO cart concept — it's a queue-gated single-shot purchase.
Pessimistic locking primary (FOR UPDATE / SKIP LOCKED) for claims that complete inside one transaction. booking.version exists for optimistic guard on state transitions.
Pre-created ticket ROWS (one row per ticket at tier creation), never a counter. Enables SKIP LOCKED + per-ticket audit + Layer-3 structural guarantee.
Option A window counting: purchase window = admitted+unexpired queue_entries row. booking.hold_expires_at = ticket-release deadline (10 min), NOT the shopping window.
Read Committed isolation. Correctness from row locks + CHECK constraints + guarded UPDATEs, not isolation escalation.

If I (human) try to re-open one of these at a hard boundary, remind me it's settled and that the real risk is not finishing.

═══════════════════════════════════════════

5. THE OVERSELLING DEFENSE (3 layers)

═══════════════════════════════════════════ L1 — Redis Lua atomic check-and-decrement on inventory:{tier_id}. Fast reject (~9,900 of 10,000). L2 — SELECT FOR UPDATE SKIP LOCKED on individual available ticket rows. Disjoint parallel claims. L3 — Structural: each ticket holds one booking_id; guarded UPDATE (WHERE status='available' AND booking_id IS NULL, assert RowsAffected==qty) + tickets CHECK constraint. Double-assignment impossible. Partial index idx_tickets_claim (WHERE status='available') keeps L2 on index scans as events sell out.

L1 is an optimization, not a guarantee. If Redis is wrong (lost compensation, crash), L2+L3 still prevent overselling; the drift checker heals Redis afterwards.

═══════════════════════════════════════════

6. DATABASE — 9 TABLES (migrations = source of truth)

═══════════════════════════════════════════ users(id, email, password_hash, is_admin, created_at) events(id, name, venue, event_at, status, max_concurrent_windows, purchase_window_seconds, created_at) ticket_tier(id, event_id FK, name, price_cents, currency, quantity_total, sale_start, sale_end, created_at) booking(id, user_id FK, event_id FK, ticket_tier_id FK, quantity, idempotency_key, status, total_cents, currency, hold_expires_at, version, confirmed_at, failed_at, created_at) tickets(id, ticket_tier_id FK, booking_id FK-nullable, status, created_at) queue_entries(id, user_id FK, event_id FK, joined_at, status, admitted_at, left_at) idempotency_keys(key, user_id, request_hash, booking_id FK, response_status, response_body, locked_at, completed_at, expires_at; PK (key,user_id)) payments(id, booking_id FK, provider, provider_txn_id, status, amount_cents, currency, card_brand, card_last4, provider_response, created_at) audit_log(id, entity_type, entity_id, action, actor_type, actor_user_id, before_state, after_state, metadata, request_id, created_at) — NO foreign keys by design

Index note (migration 000010): idx_booking_reconciliation is on hold_expires_at, so it CANNOT serve the stuck-booking scans, which filter and order by created_at. idx_booking_stuck ON booking(created_at) WHERE status='held' exists for those — used by the reconciliation worker's hot-path scan and the admin stuck list.

payments reading rule: a booking that went through an ambiguous outcome and was later resolved legitimately keeps TWO payments rows — the 'unknown' row TX2 wrote as evidence of the attempt, and the 'succeeded'/'failed' row the resolver wrote. Current payment state = the most recent row with status <> 'unknown'; fall back to 'unknown' only when no resolved row exists. The 'unknown' row is audit history, never current state. Every admin view and reconciliation query must follow this rule.

Rules:

Money = BIGINT cents + CHAR(3) currency (regex CHECK). Never floats.
Lifecycle CHECK constraints on booking and tickets make illegal states unrepresentable.
Partial indexes on hot paths: idx_tickets_claim (WHERE status='available'), idx_booking_reconciliation (WHERE status='held'). Keep them PARTIAL.
audit_log polymorphic via entity_type+entity_id; must outlive entities; high-throughput inserts.
No reference column, no metadata column on booking. Human reference = derived (TQ- + first 8 UUID chars). Edge-case notes go to audit_log.
No new columns without a migration AND an update to this section. "Needs review" / "needs refund" flags are expressed via audit_log actions, not new columns.

Status enums:

events: draft, on_sale, sold_out, closed, cancelled
booking: held, confirmed, failed, cancelled
tickets: available, held, sold
queue_entries: waiting, admitted, expired, left, completed
payments: initiated, succeeded, failed, unknown, refunded
audit actor_type: user, worker, admin, system

Audit action names used for cross-cutting flags (keep consistent):

reconciliation_needed — money moved but booking could not be confirmed (Outcome D). Surfaced in the admin stuck-bookings view.
expiry_deferred_provider_unreachable — hold deadline passed but no provider verdict (logged once per booking, not per cycle).
inventory_drift_corrected — drift checker rewrote a Redis inventory key.
invariant_violation — tickets count != quantity_total, or ticket/booking status mismatch. Never auto-fixed.
queue_entry_abandoned — waiting entry expired by event lifecycle or safety sweep.
Standard audit metadata keys (keep consistent):

- source — which finalize path resolved the booking. Values: hold_expiry | reconciliation |
  sync_retry | admin. Carried on booking.Actor.Source and written into the audit row's
  metadata by the shared ConfirmBooking / FailBooking. Required on every audit row produced
  by ReconcileBooking, because after the move to the shared resolver the action name alone
  (booking_failed) no longer distinguishes "hold deadline passed" from "reconciliation swept
  it up". Same value set feeds the reconciliation_resolution_total{source} metric in §11a, so
  audit trail and metrics agree by construction.
- reason — why a booking was failed. Values: no_charge_found | card_declined |
  provider_unreachable (deferral only).
- tickets_released — integer count of tickets returned to 'available' by this action. Used to
  verify Redis compensation matches what the DB actually released.

═══════════════════════════════════════════

7. TWO IDEMPOTENCY MECHANISMS (don't confuse)

═══════════════════════════════════════════

booking.idempotency_key = the cross-system BRIDGE to the payment provider (charge time + reconciliation lookup). The only ID existing on both sides at every crash point.
idempotency_keys TABLE = HTTP-level dedup: request_hash (422 on mismatch), cached response replay, locked_at (serialize concurrent dupes), expires_at (cleanup). PK (key,user_id).

═══════════════════════════════════════════

8. THE PURCHASE FLOW (canonical)

═══════════════════════════════════════════ POST /v1/purchases (auth + idempotency middleware). Body: event_id, ticket_tier_id, quantity(1-10), payment_token, purchase_token(JWT).

PHASE 1 — TX1: validate purchase_token → duplicate-charge guard (WHERE user_id AND status='held' → 410 active_booking_exists) → Lua decrement (→409 sold_out) → SELECT FOR UPDATE SKIP LOCKED claim (rows<qty → rollback + Redis compensate + 409) → INSERT booking 'held' (hold_expires_at NOW()+10min) → guarded ticket assignment (assert RowsAffected==qty) → audit → COMMIT (locks released).

PHASE 2 — payment call, NO DB TX OPEN, 3s timeout, using booking.idempotency_key as the provider idempotency key.

PHASE 3 — TX2, four outcomes: A. succeeded → booking confirmed, tickets sold, payments succeeded, audit → 201 B. declined (402) → booking failed, tickets released, Redis INCRBY after commit, payments failed → 402 C. ambiguous (timeout/dropped/500) → LEAVE booking 'held', payments 'unknown', → 504 payment_processing_unclear. Never guess. D. money-moved-but-booking-no-longer-held race (guarded UPDATE RowsAffected 0) → record succeeded payment + reconciliation_needed audit → 409 booking_expired_after_charge. Resolution is admin-driven (see §17 gap "Charged after booking failed").

Redis compensation (INCRBY back) always AFTER commit/rollback, never inside a DB tx. Lost compensation is healed by the drift checker (accepted window).

All finalize paths (purchase TX2, reconciliation, hold-expiration, sync reconcile on retry, admin manual reconcile) go through the SAME shared functions: ConfirmBooking / FailBooking / RecordAmbiguous. No path has its own copy of finalize SQL.

═══════════════════════════════════════════

9. THE 5 WORKERS (shared Runner: recover-from-panic, log cycle, continue)

═══════════════════════════════════════════ Locking rule for workers:

Workers that finish their work INSIDE one transaction (promotion, drift checker, idempotency cleanup) claim rows with FOR UPDATE SKIP LOCKED.
Workers that call the payment provider (reconciliation, hold-expiration) scan with a PLAIN READ (no FOR UPDATE), call the provider with no transaction open, then finalize with guarded UPDATEs (WHERE status='held', assert RowsAffected). Holding a lock across the provider call would violate Principle #2. If two workers race on the same booking, the loser gets RowsAffected 0 and skips — safe because provider lookup is side-effect-free.
ALL workers: guarded UPDATEs only, batch LIMIT per cycle, audit every state change with actor_type 'worker'.
Promotion (5s) — Option-A capacity count (admitted+unexpired queue_entries) → ZPOPMIN batch → mark admitted + issue purchase_token, jitter 100-400ms → expired-window sweep.
Hold-expiration (5s) — BUILT (internal/worker/holdexpiry.go, via booking.ReconcileBooking; §12 tests #7-#12). Held bookings past hold_expires_at → ask the provider by idempotency_key (shared ReconcileBooking, per-call 3s timeout, batch LIMIT so one cycle stays bounded):
no charge / charge failed → FailBooking (release tickets, never touch 'sold') → Redis INCRBY after commit.
charge succeeded → ConfirmBooking (tickets are still held by THIS booking, so confirming is safe).
provider unreachable → defer: leave 'held', tickets stay held, audit expiry_deferred_provider_unreachable once. Never release on a guess (money rule, §3). (Changed in rev. 3 — previously released tickets without asking the provider. See §17.)
Reconciliation (30s) — held bookings older than 90s → lookup provider by idempotency_key → confirm / fail / defer. Never guess when provider unreachable. Uses shared ReconcileBooking → ConfirmBooking/FailBooking. BUILT: the resolver lives in internal/booking/reconcile.go (ReconcileBooking + LoadStuck; ConfirmBooking/FailBooking in finalize.go), and is called today by the reconciliation worker, the hold-expiration worker, and the admin manual-reconcile endpoint. Only sync-reconcile-on-retry still has to be wired to it. Every caller passes Actor.Source ('purchase' | 'reconciliation' | 'hold_expiry' | 'admin_manual'), which lands in the audit row's metadata — actor_type alone cannot tell two workers apart. Candidate scan uses idx_booking_stuck (migration 000010). In practice reconciliation resolves most stuck bookings at ~90s; hold-expiration is the hard-deadline backstop.
Idempotency cleanup (10min) — delete expired keys (batched DELETE ... WHERE ctid IN (SELECT ... LIMIT n) to avoid long locks).
Drift checker (60s, Week 5) — three jobs, see §17 for details: a. Redis-vs-Postgres inventory: for each on_sale tier, compare inventory:{tier_id} to COUNT(tickets WHERE status='available'). Only correct Redis when the SAME drift is seen on two consecutive cycles (tolerates in-flight TX1s between Lua decrement and commit). Postgres value wins. Audit inventory_drift_corrected, increment inventory_drift_detected_total. b. Invariants (detect + alert, NEVER auto-fix): COUNT(tickets per tier) == quantity_total; no 'sold' ticket attached to a non-confirmed booking; no 'held' ticket attached to a failed/cancelled booking. Violation → audit invariant_violation + ERROR log. c. Abandoned-queue-entry safety sweep (§17).

═══════════════════════════════════════════

10. SIMULATED PAYMENT SERVICE

═══════════════════════════════════════════ Separate Go binary, own in-memory store, mirrors Stripe contract. Bearer key auth. Provider-side idempotency (same key → stored record). Endpoints: POST /v1/charges, GET /v1/charges/by-idempotency-key/{key} (reconciliation bridge), GET /health, GET /metrics. Failure-injection tokens: tok_visa, tok_mastercard, tok_declined, tok_insufficient_funds, tok_slow_5s, tok_slow_30s, tok_drops_response (charges then hijacks connection — the headline), tok_500_then_succeed, unknown→422. Chaos switch (for testing the money rule): an env/admin toggle that makes the service unreachable for N seconds, used to test hold-expiration deferral.

Note: in-memory store means restarting the payment service forgets charges → reconciliation would see "never charged". Tests and demos must not restart it mid-scenario. Documented limitation of the sim, not of the design.

═══════════════════════════════════════════

11. CODING RULES

═══════════════════════════════════════════

Shared response envelope: {"data",...,"request_id"} / {"error":{"type","message","request_id"}}. Standard error type codes.
Every state change → audit_log row with correct actor_type.
Guarded UPDATEs everywhere: expected status in WHERE, assert RowsAffected, never blind-update.
Money-as-object in API: {"amount": cents, "currency": "USD"}. ISO 8601 UTC timestamps. UUIDs in responses.
Never log: passwords, hashes, payment tokens, full bodies on payment endpoints, Authorization headers.
user_id always from JWT/context, never from URL (prevents IDOR). Resource lookups by id are always scoped: WHERE id=$1 AND user_id=$2 → 404 (not 403) when not owned.
Admin endpoints: require users.is_admin from JWT; admin actions audit with actor_type 'admin' + actor_user_id.
Rate limiting: fixed-window counter (INCR+EXPIRE), fail-open, coarse abuse-deterrence. Known 2x boundary burst accepted (documented).
Every outbound network call has an explicit timeout (context.WithTimeout). No call without a deadline.

═══════════════════════════════════════════

11a. OBSERVABILITY (Week 5)

═══════════════════════════════════════════ Purpose: the Week 6 load test needs visibility into WHERE latency goes, not just the final k6 number. Without metrics, a failed 10k run tells you "it broke" but not "the connection pool saturated at 6k VU".

Stack: prometheus/client_golang in all three binaries; Prometheus + Grafana added to docker-compose (ports 9090 / 3000). Scrape interval 5s locally. Grafana dashboard JSON committed to the repo (provisioned, not hand-built in the UI) so it's reproducible. Each binary exposes GET /metrics on its own port. Never expose /metrics publicly in the AWS deploy — bind to the VPC only.

Metrics to instrument (minimum viable set)

API (cmd/api):

http_request_duration_seconds — histogram, labels: route (chi route pattern, never raw path), method, status.
purchase_phase_duration_seconds — histogram, labels: phase (tx1 | payment_call | tx2). THE key metric: shows whether latency is DB contention or provider latency.
purchase_outcome_total — counter, labels: outcome (confirmed | declined | sold_out | ambiguous | duplicate_blocked | expired_after_charge).
ticket_claim_rows_affected — histogram. Detects partial claims / Lua-vs-truth races.
redis_lua_reject_total — counter. Proves Layer 1 filters ~99% before the DB.
idempotency_outcome_total — counter, labels: outcome (new | replayed | in_progress | hash_mismatch).
db_pool_in_use / db_pool_idle / db_pool_wait_duration_seconds — from pgxpool.Stat(), sampled by a background goroutine. Connection saturation is the most likely first bottleneck at 10k VU.

Workers (cmd/worker):

worker_cycle_duration_seconds — histogram, label: worker (promotion | hold_expiry | reconciliation | drift | idem_cleanup).
worker_items_processed_total — counter, same label.
worker_errors_total — counter, same label.
bookings_held_current — gauge. Rising = reconciliation falling behind or provider down.
reconciliation_resolution_total — counter, labels: source (reconciliation | hold_expiry | sync_retry | admin), outcome (confirmed | failed | no_charge | deferred). Deferred spikes = provider trouble.
inventory_drift_detected_total — counter. Should be ~0; non-zero is a real finding.
invariant_violation_total — counter. Must be 0. Any non-zero value is a bug.

Payment service (cmd/payment-service):

charge_duration_seconds, charge_outcome_total (labels: token type, outcome).
Grafana dashboard (one dashboard, 4 rows)

Row 1 — Throughput & outcomes: purchases/sec by outcome, sold-out rate. Row 2 — Latency breakdown: P95 of tx1 / payment_call / tx2 stacked. This is the panel screenshotted for the README. Row 3 — Contention: db_pool_in_use vs max, pool wait duration, ticket claim rows_affected distribution. Row 4 — Recovery: bookings_held_current, reconciliation resolutions by source/outcome, drift detections, invariant violations.

Rules
Cheap counters/histograms only. NO high-cardinality labels (never booking_id, user_id, idempotency_key, event_id, raw URL path as a label).
Instrumentation must never change behavior: record timings AFTER commit/rollback, never add work while holding a row lock.
slog stays the record for individual events; Prometheus is for aggregate shape. Deferred reconciliation logs to slog, counts to Prometheus.
Why this earns its place

Week 6's deliverable is not just "10k concurrent, 0 oversells" — it's being able to explain WHERE the time went and WHAT saturated first. It also produces the Grafana screenshot that gives a backend-only project something visual for the README and portfolio.

═══════════════════════════════════════════

12. TESTING RULES (non-negotiable)

═══════════════════════════════════════════

A feature is done when its test passes, not when written. Test in the same session.
Integration tests (//go:build integration) hit real Dockerized PG/Redis, ticketqueue_test DB (truncate per test), Redis DB index 1 + FLUSHDB.
Concurrency code MUST have a goroutine test (N claim M → exactly M succeed, zero double-assignment). Run flaky/concurrency tests 3x before "green". Use -race on concurrency tests.
make test (unit) + make test-integration both green before every commit. Never commit red.
Full suite at each week's gate to catch regressions.
Human reads/understands every line of concurrency-critical code (TX1, TX2, Lua, workers, idempotency middleware, ReconcileBooking).

Required scenario tests (Week 5 integration suite):

Happy path → confirmed, tickets sold, one payments row succeeded.
Decline → failed, tickets available, Redis inventory restored.
Timeout (tok_slow_5s) → 504, booking held, payment unknown → reconciliation confirms.
Dropped response (tok_drops_response) → reconciliation confirms, customer charged exactly once.
Retry with same key → replayed response; same key + different body → 422.
Concurrent duplicate requests → exactly one booking.
Hold expiry with no charge → failed, tickets released.
Hold expiry with provider UNREACHABLE → booking stays held, tickets stay held; provider back → resolved correctly.
Hold expiry where provider says charged → confirmed (not released).
Payment-vs-expiry race → 409 booking_expired_after_charge + reconciliation_needed audit.
Drift checker: corrupt Redis inventory → corrected after two cycles; in-flight TX1 does not cause false correction.
Invariant check: manually corrupt a ticket row → invariant_violation audited, nothing auto-fixed.

═══════════════════════════════════════════

13. PROGRESS TRACKER

═══════════════════════════════════════════ DONE:

Week 0: skeleton, docker-compose, 9-table migrations (up/down verified), seed (100 VIP + 400 General).
Week 1: test infra, auth (register/login/me, argon2id, JWT, enumeration-safe), events (list/detail, cursor pagination, money-as-object).
Week 2: Redis queue (join/position/leave + rebuild), rate limiting (fixed-window, fail-open), promotion worker (Option-A, ZPOPMIN+jitter, expired sweep).
Week 3: payment service (failure tokens), idempotency middleware (ON CONFLICT claim, replay, in-progress serialization, cleanup), TX1 (Lua + SKIP LOCKED + Layer-3 guard), hold-expiration worker.
Week 4: TX2 (4-way outcome), ambiguous-504-leaves-held, payment-vs-expiry race, reconciliation worker (lookup by key, never-guess), Carl demo (self-heals to confirmed + audit timeline; scripts/demo_reconciliation.sh + internal/booking/demo_reconciliation_test.go). CORRECTED: sync reconciliation on retry was NOT built — it moves to Week 5 with the orders API. Real recovery time is 90-120s (measured 105s), not 60s: the worker's threshold is 90s and its cycle is 30s. Tightening those to 45s/15s and centralizing the timing constants is an open item.

WEEK 5 (hardening + admin + observability + webhook) — status as of 2026-09-17:

DONE — Correctness fix (1): reconciliation scan is a plain read, no FOR UPDATE, no lock held across the provider call. Proven by TestReconcileHoldsNoLockAcrossProviderCall (a slow provider mid-cycle does not block a concurrent purchase on the same tier).
DONE — Shared resolver extracted: booking.ReconcileBooking (internal/booking/reconcile.go), used by the reconciliation worker AND the admin manual-reconcile endpoint. The money path exists once.
DONE — Admin: stuck-bookings list (GET /v1/admin/bookings/stuck), manual reconcile (POST /v1/admin/bookings/{id}/reconcile, actor 'admin', 503 + booking left held when the provider is unreachable), audit-trail query (GET /v1/admin/audit by entity_id or request_id). auth.RequireAdmin fails closed. 7 integration tests.
DONE — Correctness fix (2), the money-rule gap (§17): hold-expiration now calls ReconcileBooking before releasing anything and DEFERS when the provider is unreachable (booking stays 'held', tickets stay held, one 'expiry_deferred_provider_unreachable' audit row per booking, slog.Warn every cycle). Its scan is a plain read on idx_booking_reconciliation. §12 tests #7-#12 all present and green with -race.
DONE (extra, not originally planned) — end-to-end demo: scripts/demo_reconciliation.sh + docs/demo.md + `make demo`; migration 000010 idx_booking_stuck; local ports pinned (API 8081, payment service 8082).

OUTSTANDING — in priority order:

 Admin gap: the stuck list only covers held bookings older than N seconds. The plan also requires bookings carrying a 'reconciliation_needed' audit row (Outcome D: money moved, hold already expired). Those are status='failed', so the current query misses them entirely — and they are the ones that actually need a human, because they need a refund.
 GET /v1/orders (list, cursor, user-scoped), GET /v1/orders/{id} (with sync reconcile if held — wire it to ReconcileBooking), GET /v1/purchases/{id}/status
 Prometheus + Grafana: add to docker-compose, instrument all three binaries per §11a, build the 4-row dashboard ← do BEFORE the Week 6 load test
 Drift checker worker (§9 #5 + §17); plus event-status-change path expires waiting entries on sold_out/closed/cancelled
 Tighten reconciliation timing to 45s threshold / 15s cycle and centralize the timing constants in one place (they currently live in 7: worker const, worker SQL, demo script, docs/demo.md, this plan, and two test files — that drift is the actual bug)
 Integration suite covering §12 — the purchase/expiry/reconciliation scenarios are all covered now. Still missing the drift-checker and invariant scenarios, blocked on that worker.
 Webhook POST /webhooks/payment-service (HMAC + timestamp replay protection + event dedup) ← CUT FIRST if behind

Mid-week checkpoint: if correctness fixes + orders API + admin + metrics are not done by mid-week, cut webhook and dashboard polish immediately (§14 #1 and #2). Don't wait for end of week.

LATER:

Week 6: k6 load test (10k concurrent @ 100 tickets → exactly 100 sold, 0 oversells, measure P95); capture Grafana panels during the run (latency breakdown, pool saturation, recovery); chaos (kill API mid-load, reconciliation recovers; make provider unreachable, verify no tickets released on a guess). After the run, verify in SQL: sold tickets == confirmed booking quantities == succeeded payment amounts. NOTE: do NOT claim load numbers on resume until this runs.
Week 7: Dockerfiles, Terraform (VPC/RDS/ElastiCache/ECS/ALB/Secrets), deploy, demo video, tear down (~$15-30). Add PgBouncer (transaction mode; set pgx simple protocol to avoid prepared-statement breakage).
Week 8: README (pitch, headline load result, Grafana screenshots, 3 headline diagrams, 13 problems, schema, privacy section, honest production-hardening section incl. §17 documented gaps), interview prep.

═══════════════════════════════════════════

14. CUT LINE (cut from TOP when behind; never extend timeline)

═══════════════════════════════════════════

Webhook endpoint (reconciliation alone is correct without it)
Grafana dashboard polish (keep the Prometheus metrics + /metrics endpoints; cut only the dashboard styling)
Drift checker worker (keep invariant check (b) as a SQL script run after the load test if the worker is cut)
Admin inventory endpoint
Queue leave endpoint
Sync reconciliation on retry (fall back to cache-and-replay)
AWS deployment (local Docker demo + video instead) NEVER cut: purchase flow, idempotency, reconciliation worker, hold expiration (incl. the ask-provider-before-release fix), the load test, and purchase_phase_duration_seconds (Week 6 is uninterpretable without it).

═══════════════════════════════════════════

15. HOW TO USE THIS FOR FUTURE PROMPTS

═══════════════════════════════════════════ When requesting prompts for a new phase:

Prompts are gated: each ends with a Verification + Commit step; verify manually before the next.
Each prompt follows house style: exact table/column/status names from §6, shared envelope, guarded UPDATEs, audit on every state change, tests in same prompt (run concurrency tests 3x).
New workers reuse the shared Runner and follow the §9 locking rule. New finalize paths reuse ReconcileBooking / ConfirmBooking / FailBooking (DRY). New metrics follow §11a naming and label rules.
If a prompt references something not in §6, it's stale — flag it, derive or use audit_log, no silent schema invention.
If a prompt would hold a lock across a network call, it violates §3 — flag it before building.
If a prompt would release tickets or fail a booking without a provider verdict, it violates the money rule (§3) — flag it before building.
Delegate freely: skeleton, config, Dockerfiles, boilerplate, metric wiring, dashboard JSON. Keep on a short leash: TX1/TX2, Lua, workers, idempotency middleware, ReconcileBooking — human writes or fully understands.

═══════════════════════════════════════════

16. UNDER-THE-HOOD EXPLANATIONS (my own notes — the "why", for interviews)

═══════════════════════════════════════════ These are my own words explaining how each mechanism actually works. Kept as the reasoning layer behind the spec above. If I can't re-explain these from memory, I don't understand my own project yet.

Controlling how many people enter a purchase window, and for how long

EVENTS.max_concurrent_windows controls how many people are admitted into active purchase windows at the same time. EVENTS.purchase_window_seconds sets how long a window lasts. (Note: the window lives on the admitted queue_entries row — Option A. booking.hold_expires_at is a separate, longer clock: the ticket-release deadline for reconciliation, not the shopping window.)

How the 3 layers actually work (pre-created rows approach)

The correct approach is "pre-created" rows: the moment a tier is created with quantity_total = 100, I insert 100 rows into TICKETS immediately. Each row is one real ticket — its own id, status = 'available', booking_id = NULL (belongs to no one yet). All 100 tickets already exist in the database before anyone buys anything.

Claim statement: UPDATE tickets SET booking_id = <booking>, status = 'held' WHERE id = <this ticket> AND status = 'available' AND booking_id IS NULL

Layer 2 — divide and claim, no shoving. Because 100 real ticket rows exist, 10,000 people arriving at once can each "lock and grab" a DIFFERENT row using SELECT FOR UPDATE SKIP LOCKED. Whoever locks a row owns it; everyone else skips it and looks for another free row — nobody waits on anybody. If instead there were only a single number "100 left", everyone would have to queue up to edit the same cell — slow and error-prone.

Layer 3 — the database blocks double-selling even if the code has bugs. Because each ticket is a row that can hold only one booking_id, structurally a ticket cannot belong to two bookings at once. This is the final safety net: even if all the application code is wrong, the database still won't let one ticket appear in two bookings. With the counter approach (just one number), you don't have this net — if the decrement logic has a bug, you oversell without noticing.

How the BOOKING table works

The 'held' status = a durable waiting state (Problem 3 — reconciliation). Recall the two-transaction model: TX1 creates the booking as 'held' and commits → call the provider → TX2 finalizes the result. If the server crashes BETWEEN TX1 and TX2, the booking row is STILL THERE in 'held'. That "in-limbo" state is exactly what lets the reconciliation worker find the stuck booking and rescue it. Without this durable intermediate state, a crash would lose the trail completely.

The two-transaction model = don't hold locks during a network call (Problem 4).

Why QUEUE_ENTRIES instead of relying on Redis HA

Because Postgres writes durably — it only says OK after the data is safely on disk. When Postgres says "saved", the data is truly persisted — there's no "lag by a beat" window. Nobody gets lost.

The correct approach: write to Postgres FIRST (durable), then write to Redis (for speed). Then:

Redis handles speed: fast answers to "what's my position?", "who's admitted next?".
Postgres handles truth: the record that never loses anyone.
If Redis dies, I rebuild the queue from QUEUE_ENTRIES in Postgres, ordered by joined_at. No position is lost, because the data was durably written to Postgres beforehand. (This is about authority, separate from Redis HA which is about availability — both matter, they solve different problems.)
How reconciliation works

The problem it solves: the server calls the provider to charge. The provider actually charges (customer's card is debited), but the RESPONSE is lost (network drop) or the server CRASHES right then. Now the server does NOT know whether the money was taken. The booking is stuck in 'held'. The customer may be charged without getting a ticket.

Why it's rescuable — thanks to the two-transaction model: TX1 creates the booking 'held' (with idempotency_key) and commits → call the provider (holding no transaction) → TX2 finalizes. If it crashes between TX1 and TX2, the booking is still there in 'held' with the key already written. That durable 'held' state is what makes reconciliation possible.

How the worker runs (every ~30s):

Step 1 — scan for stuck bookings: find bookings status='held' older than 90s. PLAIN READ — no FOR UPDATE — because the next step is a network call, and holding a row lock across a network call is exactly what Principle #2 forbids.
Step 2 — ask the provider using the bridge: for each stuck booking, the worker takes the booking's idempotency_key and asks the provider: "Has this key been charged?" This is why the key must be sent to the provider AS ITS idempotency key at call time — so now both sides recognize it.
Step 3 — the provider answers, 4 possibilities:
"Never saw this key" → never charged (crash BEFORE the call landed). Worker sets booking failed, returns tickets to 'available'.
"Yes, charged successfully" → money really moved. Worker completes the unfinished TX2: write provider_txn_id into PAYMENTS, move booking to 'confirmed', tickets to 'sold'.
"Called but it failed" → worker sets booking failed, returns tickets to stock.
Provider UNREACHABLE → do nothing, leave 'held', retry next cycle. Never guess.
Step 4 — finalize with a guarded UPDATE (WHERE status='held'). If another worker (or the purchase handler, or hold-expiration) already finalized it, RowsAffected = 0 and this worker just moves on. So two workers racing is harmless: the lookup has no side effects, and only one UPDATE can win.

Interview line: "Why doesn't reconciliation use SKIP LOCKED like the other workers? Because it calls the provider between reading and writing. Locking would hold a row lock across a network call. Instead it reads without a lock, and the guarded UPDATE is the concurrency control — first writer wins, everyone else sees zero rows affected."

Why hold-expiration asks the provider before releasing tickets

Hold-expiration used to be simple: held + past deadline → fail, release tickets. The problem: if the provider is down for more than 10 minutes, reconciliation keeps deferring (correctly), but hold-expiration would release the tickets anyway — even though the customer's card may already be charged. Then someone else buys those tickets, and when the provider comes back, the first customer paid for nothing.

So hold-expiration now uses the same ReconcileBooking logic: it only releases tickets after the provider says "no charge" or "charge failed". If the provider says "charged", it confirms (the tickets are still held by that booking, so it's safe). If the provider is unreachable, it waits.

The trade-off, stated honestly: during a long provider outage, some tickets stay frozen in 'held' longer than 10 minutes. I accept that. Frozen inventory costs a few unsold seats for a while and heals itself; charging someone without giving them a ticket is a real customer harm and a support/dispute problem. In fintech you pick the failure mode that is recoverable.

Why a separate idempotency_keys table instead of only booking.idempotency_key

I use BOTH, each for a different job:

BOOKING.idempotency_key = the BRIDGE: written early in TX1, sent to the payment provider, used by the reconciliation worker for lookup. It needs to sit directly on the booking for convenience.
IDEMPOTENCY_KEYS table = HTTP idempotency semantics: detect different content (422), replay the old response, order concurrent requests, manage key expiry.
request_hash — detect "same key but different content". Imagine the user sends key abc-123 to buy 2 VIP. For some reason (client bug, reused old key) they send abc-123 AGAIN, but this time to buy 5 General. If only the key column on booking existed: the system sees "this key exists" and returns the old booking — the user thinks they bought 5 General but actually gets back the 2-VIP order. Totally wrong and silent. The separate table stores request_hash (a fingerprint of the original request body). Same key + same content → return old result. Same key + DIFFERENT content → 422. A plain column on booking has nowhere to store the original content to compare against.
response_body — replay the exact first answer. When the client retries, it expects the EXACT same answer as the first time. If only the key column existed: on retry you catch the duplicate, then have to look up the booking and GUESS what to return — but the booking may already have moved from 'held' to 'confirmed', so a resent request gets back something different from the first response. The separate table stores the verbatim first response_body, so the retry replays it identically — true HTTP idempotency.
locked_at — handle two duplicate requests arriving AT THE SAME TIME. The hardest case. The user double-clicks Pay, or the browser resends almost simultaneously — BEFORE the first finishes creating the booking. If relying only on a booking UNIQUE constraint: both rush to create a booking, one wins, the loser hits a UNIQUE error — then the loser has to read the winner, which may not have committed yet. Very messy. The separate table with locked_at: the first request plants a flag, the second sees it and yields. It ORDERS the two concurrent requests cleanly, up front. (booking.version does a different job — preventing lost updates — it does NOT solve this create-race.)
expires_at — allow key reuse. Idempotency keys usually expire (Stripe ~24h). If the key were bound to the booking row, it would live as long as the booking — essentially forever, and the key space would never be cleaned up. The separate table's expires_at manages key lifecycle independently.
Audit log

AUDIT_LOG — append-only history, no foreign keys on purpose so it outlives deleted entities. Every state change writes who/what/when/before/after plus request_id. Role: auditability and dispute resolution (Problem 11) — one indexed query by entity_id answers "everything that happened to this booking" weeks later.

═══════════════════════════════════════════

17. KNOWN GAPS & THEIR RESOLUTIONS

═══════════════════════════════════════════ Gaps identified in design review, with the decided treatment. "Fix" = build it; "Document" = deliberately out of scope, note the boundary.

GAP: Hold-expiration could release tickets for a charged booking → FIXED (Week 5)

Problem: hold-expiration failed every held booking past hold_expires_at without asking the provider. If the provider was unreachable for longer than 10 minutes (or the server crashed between TX1 and TX2 and the charge actually landed), tickets were released and resold while the original customer may have been charged. Reconciliation could not repair it afterwards because the booking was no longer 'held'. Note: a payments row is NOT a reliable signal here — in the crash-between-TX1-and-TX2 case no payments row exists at all. Only the provider knows. Resolution: hold-expiration calls the shared ReconcileBooking before any release (§9 #2). Release only on "no charge" / "charge failed"; confirm on "charged"; defer on unreachable. Tests §12 #7–#9. Residual race (accepted): provider says "no charge", hold-expiration releases, then an in-flight charge lands. Requires a charge still in flight at the 10-minute mark despite a 3s client timeout — extremely rare. Caught by Outcome D / reconciliation_needed (next gap).
Interview line: "Hold expiry asks the provider before releasing, and defers when it can't
reach them — releasing inventory for a booking that may have been charged is the one case
where being fast is worse than being slow."


GAP: Charged after booking failed (Outcome D) → PARTIAL FIX + DOCUMENT

Problem: money moved, but the booking is already failed and its tickets may be resold. Cannot confirm. Built: payments row recorded as succeeded, audit reconciliation_needed, booking shown in the admin stuck-bookings list, 409 booking_expired_after_charge to the client. Documented, not built: automatic refund. Production would call the provider's refund API (idempotent, keyed by booking.idempotency_key + ":refund") and move payments to 'refunded'. The sim service has no refund endpoint; adding one is a Phase-2 item. Interview line: "The system never silently loses the money — every such case is recorded, audited, and surfaced for refund. Automatic refunds are the next step, and the key design is already there."

GAP: Abandoned queue entries never cleaned → FIX (Week 5, in drift checker)

Problem: users who join the queue (status='waiting') but are never admitted — event sells out before they reach the front, or they close the tab — leave 'waiting' rows forever. Over many events, queue_entries grows unbounded with stale rows for events that are long over. Note: admitted-then-vanished users are ALREADY handled by the promotion worker's expired-window sweep. The unhandled case is specifically never-admitted 'waiting' rows.

Resolution — two guarded UPDATEs: (A) Event-lifecycle cleanup (correct trigger): when an event transitions to sold_out / closed / cancelled, in the SAME transaction expire its remaining waiting entries: UPDATE queue_entries SET status='expired', left_at=NOW() WHERE event_id=$1 AND status='waiting'; (B) Time-based safety sweep (net for stuck events): in the drift-checker cycle, UPDATE queue_entries qe SET status='expired', left_at=NOW() FROM events e WHERE qe.event_id=e.id AND qe.status='waiting' AND e.status IN ('sold_out','closed','cancelled'); Both are guarded UPDATEs; audit each as 'queue_entry_abandoned' actor 'worker'/'system'. After the Postgres update commits, remove the same users from the Redis sorted set (Postgres first, Redis second — Principle #1).

Phase 2 (document, don't build): terminal-state rows (expired/left/completed) still accumulate across years. Production would PARTITION queue_entries by month and drop old partitions. Not built — portfolio load never reaches that scale.

Interview line: "Waiting entries are expired when their event closes, plus a time-based safety sweep in the drift checker. For long-term growth of terminal rows, production would partition by month and drop old partitions — documented rather than built, since portfolio load never reaches that scale."

GAP: Redis inventory drift → FIX (Week 5, drift checker)

Problem: Redis INCRBY compensation happens after commit; if the process dies between commit and INCRBY, Redis under-counts (sells out early in L1 while Postgres still has tickets). The reverse (Redis over-counts) is harmless for correctness because L2+L3 still hold, but causes extra DB load. Resolution: drift checker (§9 #5a) rewrites Redis from Postgres, but only after the same drift is seen on two consecutive cycles, so in-flight TX1s are not "corrected" into an oversell-looking state. Postgres wins.

GAP: Fraud / anti-scalping → DOCUMENT (deliberately out of scope)

The system implements coarse rate limiting only. It does NOT implement the anti-scalping layer a production ticketing platform needs: device fingerprinting, cross-account velocity checks, CAPTCHA/proof-of-work, ML fraud scoring, stolen-card BIN validation, per-identity purchase limits. In production this is a dedicated service or third party (Stripe Radar, hCaptcha, bot-management vendor) sitting in front of the queue and purchase endpoints. The architecture accommodates it: an abuse-scoring check slots in at queue admission and at purchase submission without touching the concurrency core. Optional cheap primitive if ever wanted: per-user-per-event purchase limit via one guarded COUNT query at purchase time.

GAP: Simulated provider is in-memory → DOCUMENT

Restarting cmd/payment-service loses all charges, so reconciliation would see "never charged" for real charges. This is a property of the simulator, not the design: a real provider's ledger is durable. Tests and demos never restart the sim mid-scenario.

═══════════════════════════════════════════

18. REVISION NOTES

═══════════════════════════════════════════ rev. 2 (end of Week 4):

Added Prometheus + Grafana (§2, §11a, Week 5/6/8 tasks, cut line #2).
Reconciliation scan changed to plain read (no FOR UPDATE across the provider call).
Added fraud/anti-scalping gap (document only).

rev. 3 (end of Week 4, design review):

§9 locking rule rewritten: SKIP LOCKED only for workers that finish inside one transaction; provider-calling workers use plain read + guarded UPDATE. Removes the old contradiction ("all workers use FOR UPDATE SKIP LOCKED").
Hold-expiration now asks the provider before releasing tickets (money rule added to §3). New gap in §17. Added as the FIRST Week 5 task.
Shared ReconcileBooking used by all five finalize paths (§8).
Drift checker promoted to worker #5 with interval, two-cycle confirmation, and never-auto-fix invariants.
Outcome D gap made explicit: recorded + surfaced to admin; automatic refund documented as Phase 2.
Standard audit action names (§6), 12 required integration scenarios (§12), mid-week cut checkpoint (§13).
Fixed formatting error in the Week 5 checklist; hold-expiration fix added to NEVER-cut list.