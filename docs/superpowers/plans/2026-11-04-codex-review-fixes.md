# Codex Review Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the 23 actionable findings from the Codex review (finding #8 is documented, not fixed), one commit per task, before the Week 7 AWS deploy.

**Architecture:** Postgres becomes the only input to queue promotion; Redis is repaired from it and never decides who is admitted. Provider keys become per-booking UUIDs, so they cannot collide across users. Everything else is a local fix at the root cause, in the shared function where one exists.

**Tech Stack:** Go, chi, pgx/v5, go-redis/v9, PostgreSQL 18 (compose), Redis 7, Terraform, GitHub Actions.

**Spec:** the Codex findings (numbered per task below) plus `.skills/IMPLEMENTATION_PLANS.md` and `.skills/CLAUDE.md`. Every finding was verified against the code on 2026-11-04.

## Global Constraints

- Work on branch `fix/codex-review`, never on `main`.
- Postgres is truth, Redis is speed. Postgres is written first, Redis second, and Redis compensation happens after commit.
- Never hold a DB transaction or row lock across a network call or a sleep.
- Money rule: never release tickets or fail a booking without a provider verdict.
- Every state change writes an audit_log row. Guarded UPDATEs only; assert RowsAffected.
- All finalize paths go through `ReconcileBooking` / `ConfirmBooking` / `FailBooking`.
- `make test` and `make test-integration` are both green before every commit. Concurrency tests run with `-race -count=3`.
- New columns or indexes need a migration and a §6 update. No high-cardinality metric labels.
- The human reads and understands all TX1, worker, Lua and idempotency code before it is committed.

Run a single integration test with:
`go test -tags=integration -p 1 -count=1 -run <TestName> ./<package>/`

## Review Focus

Each of these has its test added to the owning task.

1. A client retries with the same Idempotency-Key after the provider key change: it must replay the first response and create exactly one charge (Task 2).
2. Worker receives SIGTERM mid-batch: users not yet admitted stay `waiting` and are admitted after restart (Task 8).
3. Existing rows violate a new unique index: the migration must fail loudly, with a documented pre-check query (Tasks 2, 3).
4. Admin reconciles a fresh booking the provider DID charge: it must still confirm; only "not found" is deferred (Task 4).
5. Redis is down while someone views an event: the page still returns 200 and does not report a false size of 0 (Task 21).

---

### Task 1: CI in place before any fix

**Why:** no `.github/` exists, and the global rule requires CI from Phase 0. 23 fixes with no regression gate is the wrong order.

**Files:** Create `.github/workflows/ci.yml`.

- [ ] **Step 1:** Create branch `fix/codex-review`. Run `make up`, `make test`, `make test-integration`. Expected: green baseline. If red, stop and report; do not fix on top of red.
- [ ] **Step 2:** Write `ci.yml`, triggered on `pull_request` and push to `main`. Job `test` with services `postgres:18-alpine` and `redis:7-alpine`; `actions/setup-go` using `go-version-file: go.mod`; steps: `gofmt -l .` must print nothing, `go vet ./...`, `go build ./...`, `make test`, migrate the test DB, `make test-integration`. Job `terraform`: `terraform fmt -check`, `terraform init -backend=false`, `terraform validate` in `terraform/`.
- [ ] **Step 3:** Commit: `CI: lint, build, unit and integration tests, terraform validate on every PR and push to main`.
- [ ] **Step 4 (needs user approval, outward-facing):** Push the branch (this also publishes the unpushed Terraform commit), open a PR, confirm CI is green, then enable branch protection requiring it via `gh api`. Deploys are manual `make tf-apply`, so the rule for them is: only apply a commit with green CI.

---

### Task 2: Finding #1 — provider keys collide across users

**Why:** HTTP dedup is scoped by `(key, user_id)`, but the raw client key goes to the provider, whose ledger is global. `idx_booking_idem` is not unique.

**Files:**
- Modify: `internal/http/purchase.go` (around lines 150, 178)
- Create: `migrations/000015_booking_idem_unique.{up,down}.sql`
- Modify: `internal/http/purchase_integration_test.go:449` helper, `scripts/demo_reconciliation.sh` if it uses the client key (line 149 already reads `stored_key`, confirm its source)
- Test: `internal/http/purchase_integration_test.go`

**Interfaces:** Produces: `booking.idempotency_key` is a server-generated UUID, globally unique. `HoldTickets` and `Charge` signatures do not change.

- [ ] **Step 1: Write failing tests.**
  `TestPurchaseSameKeyTwoUsersChargedSeparately`: users A and B, tiers priced 1000 and 50000, same `Idempotency-Key`, `tok_visa`. Assert both 201, two distinct `booking.idempotency_key` values, two distinct provider charge ids, and each charge's amount equals that booking's `total_cents`.
  `TestPurchaseRetrySameKeyOneCharge`: same user, same key and body, twice. Assert second response has `Idempotency-Replayed: true`, one booking row, one succeeded payments row.
- [ ] **Step 2:** Run them. Expected: first FAILS (same charge id, B's amount wrong).
- [ ] **Step 3: Implement.** In `Create`, replace `idemKey := r.Header.Get("Idempotency-Key")` with `providerKey := uuid.NewString()` and pass it to `HoldTickets` and `Charge`. A retry never reaches this line, because the middleware replays or rejects it, so one booking always has one key.
- [ ] **Step 4: Migration.** Up: `DROP INDEX idx_booking_idem; CREATE UNIQUE INDEX idx_booking_idem ON booking(idempotency_key);`. Down restores the non-unique index. Pre-check for existing data: `SELECT idempotency_key FROM booking GROUP BY 1 HAVING COUNT(*) > 1;`. Dev data that fails this can be reset; do not silently delete rows. This also makes `LoadStuckByKey` single-row, which it previously only assumed.
- [ ] **Step 5:** Fix helpers and tests that looked a charge up by the client key: read `booking.idempotency_key` instead. Tests calling `HoldTickets` directly with a repeated key must use distinct keys.
- [ ] **Step 6:** Run `make test test-integration`. Expected: PASS. Run `make demo`. Expected: PASS.
- [ ] **Step 7:** Commit: `Generate a per-booking provider key: a client key is only unique per user`.

---

### Task 3: Finding #2 — concurrent purchases bypass the duplicate-charge guard

**Why:** `ActiveBookingID` is read-then-insert with nothing enforcing the result. `idx_booking_user_active` is not unique.

**Files:**
- Create: `migrations/000016_booking_user_active_unique.{up,down}.sql`
- Modify: `internal/booking/booking.go`, `internal/http/purchase.go`
- Test: `internal/booking` integration test (new file `active_booking_integration_test.go`)

**Interfaces:** Produces `var ErrActiveBooking = errors.New("user already has a held booking")` in package `booking`.

- [ ] **Step 1: Write failing test** `TestHoldTicketsOneHeldPerUser`: one user, 10 goroutines call `HoldTickets` with distinct keys on a 50-ticket tier, released by one barrier. Assert exactly 1 success, 9 `ErrActiveBooking`, one booking row, exactly `qty` tickets held. Test at this level so a fast first purchase finishing cannot legitimately let a second through.
- [ ] **Step 2:** Run with `-race -count=3`. Expected: FAIL (10 bookings).
- [ ] **Step 3: Migration.** Up: drop and recreate as `CREATE UNIQUE INDEX idx_booking_user_active ON booking(user_id) WHERE status = 'held';`. Pre-check: `SELECT user_id FROM booking WHERE status='held' GROUP BY 1 HAVING COUNT(*) > 1;`.
- [ ] **Step 4: Implement.** In `HoldTickets`, when the INSERT fails with `*pgconn.PgError` where `Code == "23505"` and `ConstraintName == "idx_booking_user_active"`, return `ErrActiveBooking`. In `Create`, the existing error branch already compensates Redis; add an `errors.Is(err, booking.ErrActiveBooking)` case that answers the same 410 `active_booking_exists` and increments `OutcomeDuplicateBlocked`. Keep the `ActiveBookingID` pre-check as the cheap path.
- [ ] **Step 5:** Add `TestPurchaseActiveBookingCompensatesInventory`: after the 410 path, the Redis counter equals its starting value minus only the winner's quantity.
- [ ] **Step 6:** `make test test-integration`, plus Step 1's test with `-race -count=3`. Expected: PASS. Fix any test that created two held bookings for one user by using separate users.
- [ ] **Step 7:** Commit: `Enforce one held booking per user with a unique partial index`.

---

### Task 4: Finding #6 — "not found" treated as "never charged" while a payment is in flight

**Why:** `ReconcileBooking` treats a 404 as no money moved. That is only true once the charge can no longer land. The workers are protected by their scan age; admin and webhook callers are not. The fix belongs in the shared function.

**Files:**
- Modify: `internal/booking/reconcile.go`, `internal/http/admin.go`
- Test: `internal/http/admin_integration_test.go`

**Interfaces:** Produces `Stuck.CreatedAt time.Time` (set by `LoadStuck` and `LoadStuckByKey` through `scanStuck`), and `const ProviderInFlight = "in_flight"`. Task 16 consumes `ProviderInFlight`.

- [ ] **Step 1: Write failing tests.**
  `TestAdminReconcileFreshBookingNotFoundDefers`: fresh held booking, provider has no record. Assert 409 `payment_may_be_in_flight`, booking still `held`, tickets still `held`, no Redis change.
  `TestAdminReconcileFreshBookingChargedConfirms`: fresh booking, provider has a charge. Assert 200 and `confirmed`.
  `TestAdminReconcileOldBookingNotFoundFails`: `created_at` backdated past `StuckAfter`, no charge. Assert `failed`, tickets released.
- [ ] **Step 2:** Run. Expected: first FAILS.
- [ ] **Step 3: Implement.** In `case NotFound:`, before opening a transaction: `if time.Since(s.CreatedAt) < StuckAfter`, increment the `deferred` metric and return `Resolution{Provider: ProviderInFlight, Status: "held"}, nil`. Workers leave `CreatedAt` zero, and `time.Since` of the zero time is far above `StuckAfter`, so they behave as today; they already filter by age in SQL. Update the `Resolution.Provider` doc comment. In `admin.go`, map `ProviderInFlight` to the 409 above.
- [ ] **Step 4:** `make test test-integration`. Expected: PASS. Backdate any existing admin test that relied on failing a fresh not-found booking.
- [ ] **Step 5:** Commit: `Defer a not-found verdict until a charge can no longer be in flight`.

---

### Task 5: Finding #7 — provider idempotency is not atomic

**Why:** the existence check and the write take separate locks, and `record` overwrites. Only reachable when one key is sent concurrently, so this is simulator fidelity rather than a live money bug. It is cheap and the sim claims to mirror Stripe.

**Files:** Modify `internal/paymentsim/paymentsim.go`. Test: `internal/paymentsim/paymentsim_test.go`.

- [ ] **Step 1: Write failing test** `TestConcurrentSameKeyOneOutcome`: 20 goroutines POST one key, half `tok_visa`, half `tok_declined`, released by a barrier. Assert every response carries the same charge id and same status, the HTTP status matches that status (200 or 402), and lookup returns that record.
- [ ] **Step 2:** Run with `-race -count=3`. Expected: FAIL.
- [ ] **Step 3: Implement.** In `record`, under one write lock: if `charges[key]` exists return it, else store and return the new charge. First writer wins. `succeed` and `fail` answer from the returned charge's `Status`, so a late decline cannot answer 402 for a stored success, and skip `notify` when they did not create the record. `record` therefore also returns `created bool`.
- [ ] **Step 4:** `go test -race -count=3 ./internal/paymentsim/`. Expected: PASS.
- [ ] **Step 5:** Commit: `Make the simulated ledger first-writer-wins per idempotency key`.

---

### Task 6: Finding #9 — purchases ignore event status and tier sale dates

**Why:** queue join checks `on_sale`, purchase does not, and nothing checks `sale_start` / `sale_end`. Sale dates are bypassable today. The cancellation path is latent because nothing transitions event status yet.

**Files:** Modify `internal/booking/booking.go`, `internal/http/purchase.go`. Test: `internal/http/purchase_integration_test.go`.

**Interfaces:** `booking.Tier` gains `OnSale bool`, computed in SQL from the database clock.

- [ ] **Step 1: Write failing table test** `TestPurchaseRejectedOutsideSale`: event `cancelled`; event `closed`; `sale_start` 1 minute ahead; `sale_end` 1 minute ago. Each with a valid purchase token. Assert 410 `sales_closed`, no booking row, Redis counter unchanged, zero provider calls. Control cases: NULL dates and an open window both return 201.
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3: Implement.** `LoadTier` joins `events` and selects `e.status = 'on_sale' AND (t.sale_start IS NULL OR NOW() >= t.sale_start) AND (t.sale_end IS NULL OR NOW() < t.sale_end)`. In `Create`, after the event-ownership check and before Layer 1, reject with 410 `sales_closed` (same code as queue join). Seeds leave the dates NULL, so existing flows are unaffected.
- [ ] **Step 4:** `make test test-integration`. Expected: PASS.
- [ ] **Step 5:** Commit: `Reject purchases outside the event's sale state and the tier's sale window`.

---

### Task 7: Finding #22 — total price can overflow

**Why:** `int64(qty) * tier.PriceCents` is unchecked. Not reachable through the API (no tier-creation endpoint), only through bad data. One guard closes it.

**Files:** Modify `internal/http/purchase.go`. Test: `internal/http/purchase_integration_test.go`.

- [ ] **Step 1: Write failing test** `TestPurchaseRejectsTotalOverflow`: tier priced 4611686018427387904, quantity 4. Assert 422 `validation_failed`, no booking, zero provider calls, Redis unchanged.
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3: Implement.** Before Layer 1: `if tier.PriceCents > math.MaxInt64/int64(req.Quantity)`, reject.
- [ ] **Step 4:** Run the test and `make test-integration`. Expected: PASS.
- [ ] **Step 5:** Commit: `Reject a purchase whose total would overflow int64`.

---

### Task 8: Findings #3 and #5 — promotion loses users and can exceed capacity

**Why:** `ZPOPMIN` removes users before admission commits, so a crash, DB error, or SIGTERM during the jitter sleep strands them. The capacity count is not serialized between workers. Both come from Redis deciding who is next. **Needs decision D1: this replaces §9's `ZPOPMIN` step.**

**Files:** Modify `internal/worker/promotion.go`. Test: `internal/worker/promotion_integration_test.go`.

**Interfaces:** Produces `func admitNext(ctx context.Context, pool *pgxpool.Pool, eventID string, maxWindows, windowSeconds int) (userID string, admitted bool, err error)`. Removes `admitUser`.

- [ ] **Step 1: Write failing tests.**
  `TestPromoteDoesNotLoseUsersOnFailure`: three waiting users, promotion context cancelled after the first admission. Assert 1 admitted, 2 still `waiting`, and a fresh cycle admits them with no rejoin.
  `TestPromoteWithRedisEmpty`: waiting rows, `FLUSHDB`, run a cycle. Assert admitted in `joined_at` order.
  `TestConcurrentPromotionRespectsCapacity`: one free slot, five waiting users, 8 goroutines run `PromoteCycle` at once. Assert exactly 1 admitted.
- [ ] **Step 2:** Run with `-race -count=3`. Expected: FAIL.
- [ ] **Step 3: Implement `admitNext`** as one transaction:
  1. `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))` on the event id. Use this rather than `events ... FOR UPDATE`, which would conflict with the FK `FOR KEY SHARE` lock TX1 takes on `events`. Transaction-scoped, so it is safe under PgBouncer transaction mode.
  2. Count active windows (same query as today). If `>= maxWindows`, return `admitted=false`.
  3. `UPDATE queue_entries SET status='admitted', admitted_at=NOW() WHERE id = (SELECT id FROM queue_entries WHERE event_id=$1 AND status='waiting' ORDER BY joined_at, id LIMIT 1 FOR UPDATE SKIP LOCKED) RETURNING user_id`. Served by `idx_queue_recovery`. No row means `admitted=false`.
  4. Write the `queue_admitted` audit row and commit.
- [ ] **Step 4: Rewrite `promoteEvent`:** loop `admitNext` until `admitted=false`, an error, or `ctx.Err() != nil`. After each commit call `rdb.ZRem`; on failure log and continue, because Task 9's sync removes the leftover member. Call `jitter()` between admissions, outside any transaction. Increment `WorkerItems` per admission.
- [ ] **Step 5:** Adapt `TestPromoteFIFOUpToCapacity` and `TestPromoteSkipsUserWhoLeft` if they depended on Redis-only ordering. Remove `ZPopMin` usage.
- [ ] **Step 6:** `make test test-integration`, plus Step 1's tests with `-race -count=3`. Expected: PASS.
- [ ] **Step 7:** Human reads the whole file. Commit: `Promote from Postgres under a per-event lock; Redis no longer decides admission`.

---

### Task 9: Findings #4 and #15 — Redis queue recovery never runs and is destructive

**Why:** `RebuildFromPostgres` has no production caller. It also deletes the live key before reading Postgres, so a failed query empties the queue. After Task 8 Redis loss no longer blocks admission, but positions stay wrong until something repairs it.

**Files:** Modify `internal/queue/queue.go`, `internal/worker/drift.go`, the comment on `queue.Join`. Test: `internal/queue/queue_integration_test.go`, `internal/worker/drift_integration_test.go`.

**Interfaces:** Replaces `RebuildFromPostgres` with `func SyncFromPostgres(ctx context.Context, rdb *redis.Client, db *pgxpool.Pool, eventID string) error`.

- [ ] **Step 1: Write failing tests.**
  `TestSyncRestoresOrderAfterFlush`: three waiting rows, flush, sync. Assert original `joined_at` order.
  `TestSyncQueryFailureKeepsMembers`: members in Redis, closed DB pool, sync returns an error, `ZCARD` unchanged.
  `TestSyncRemovesDepartedMember`: Redis member whose row is `left`. Assert removed.
  `TestSyncKeepsMemberWhoJoinedAfterSnapshot`: member present in Redis with a `waiting` row not in the initial snapshot. Assert kept.
  `TestDriftCycleRepairsQueue`: flush Redis, run one drift cycle on an `on_sale` event. Assert members restored. `Runner` runs a cycle at startup, so this also covers recovery on restart.
- [ ] **Step 2:** Run. Expected: FAIL (`SyncFromPostgres` undefined).
- [ ] **Step 3: Implement `SyncFromPostgres`.** Never `DEL`. Order matters:
  1. Read waiting rows; on error return, Redis untouched.
  2. `ZADD NX` them with the `joined_at` micro score.
  3. `ZRANGE` all members.
  4. Re-check those members in Postgres: `WHERE event_id=$1 AND user_id = ANY($2) AND status='waiting'`.
  5. `ZREM` members not returned.

  Step 4 runs after step 2, so a leave during the sync is caught. A join writes Postgres first, so a concurrent join is seen as waiting and kept.
- [ ] **Step 4:** Add a fourth job to `DriftChecker.Cycle`, with the same per-job recovery as jobs A–C, that calls `SyncFromPostgres` for every `on_sale` event. Add `// ponytail: full ZRANGE per event per 60s; fine at 10k waiting, switch to ZSCAN if queues grow past that`. Update the `Join` comment and the old rebuild test.
- [ ] **Step 5:** `make test test-integration`. Expected: PASS.
- [ ] **Step 6:** Commit: `Repair the Redis queue from Postgres every drift cycle, without deleting first`.

---

### Task 10: Finding #13 — rejoining hides an admitted user's purchase token

**Why:** idempotent join also matches `admitted` rows and re-adds them to the waiting set, so Position reports `waiting`. When capacity is full, promotion never pops them, so the window can run out with the token hidden. After Task 8 nothing pops them at all, so this must follow it.

**Files:** Modify `internal/queue/queue.go`, `internal/http/queue.go`. Test: `internal/http/queue_integration_test.go`.

**Interfaces:** Produces `var ErrAlreadyAdmitted = errors.New("already admitted")`; `Join` returns it and skips `ZADD`. Handler helper `func (h *QueueHandler) respondAdmitted(w http.ResponseWriter, r *http.Request, e event.EventWithTiers, userID string)`, extracted from the admitted branch of `Position`.

- [ ] **Step 1: Write failing test** `TestRejoinWhileAdmittedKeepsToken`: admit a user, POST join, then GET position. Assert both answer 200 with `status: admitted`, a `purchase_token`, an unchanged `window_expires_at`, and `ZSCORE` is nil.
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3: Implement.** In `Join`'s conflict branch, select `status` too; if `admitted`, return `ErrAlreadyAdmitted` before `ZADD`. In the handler, on that error call `respondAdmitted`. `Position` calls the same helper.
- [ ] **Step 4:** `make test test-integration`. Expected: PASS.
- [ ] **Step 5:** Commit: `Rejoin returns the admitted window instead of re-queueing the user`.

---

### Task 11: Finding #14 — leave removes Redis before Postgres

**Why:** `Leave` does `ZREM` first. If the UPDATE fails, the durable row stays `waiting`. After Task 8 that user would still be admitted, and Task 9 would put them back.

**Files:** Modify `internal/queue/queue.go`. Test: `internal/queue/queue_integration_test.go`.

- [ ] **Step 1: Write failing test** `TestLeaveDBFailureKeepsRedisMember`: closed DB pool, `Leave` returns an error, `ZSCORE` still present. Then with a live pool a retry succeeds: row `left`, member gone.
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3: Implement.** UPDATE first and return its error or `ErrNotInQueue`. Then `ZREM`; if it fails, log and return nil, because the leave is durable and the next sync removes the member. Audit as today.
- [ ] **Step 4:** `make test test-integration`. Expected: PASS.
- [ ] **Step 5:** Commit: `Leave writes Postgres first, then Redis`.

---

### Task 12: Finding #16 — inventory repair overwrites concurrent changes

**Why:** the drift checker does a plain `SET` from an earlier read, so a reservation or compensation landing in between is lost. Lazy init also uses `SET`, while `ReserveInventory` already uses `SetNX` for the same reason.

**Files:** Modify `internal/worker/drift.go`. Test: `internal/worker/drift_integration_test.go`.

**Interfaces:** Produces `func casInventory(ctx context.Context, rdb *redis.Client, key string, expected, value int64) (bool, error)`, a Lua compare-and-set: `if redis.call('GET',KEYS[1]) == ARGV[1] then redis.call('SET',KEYS[1],ARGV[2]) return 1 end return 0`.

- [ ] **Step 1: Write failing tests.**
  `TestCasInventoryKeepsInterveningDelta`: key changed after the expected value was read. Assert returns false and the value is preserved.
  `TestCasInventoryAppliesWhenUnchanged`: asserts true and the new value.
- [ ] **Step 2:** Run. Expected: FAIL (undefined).
- [ ] **Step 3: Implement.** Lazy init becomes `SetNX`. Correction uses `casInventory(key, cached, t.truth)`. On false: delete the pending entry, no metric, no audit, re-evaluate next cycle.
- [ ] **Step 4:** `make test test-integration`, and the existing two-cycle tests with `-race -count=3`. Expected: PASS.
- [ ] **Step 5:** Human reads the Lua. Commit: `Inventory repair uses compare-and-set so it cannot overwrite in-flight changes`.

---

### Task 13: Finding #10 — forged X-Forwarded-For bypasses login rate limits

**Why:** `ByIP` trusts the leftmost entry, which the client controls. An ALB appends the real client IP on the right. The number of trusted proxies is deployment configuration.

**Files:** Modify `internal/ratelimit/ratelimit.go`, `internal/http/auth.go`, `internal/config/config.go`, `cmd/api/main.go`, `terraform/ecs.tf`. Tests: `internal/ratelimit/ratelimit_test.go`, `internal/http/ratelimit_integration_test.go`.

**Interfaces:** Replaces `ByIP` with `func ByClientIP(trustedHops int) func(*http.Request) string`. `Config.TrustedProxyHops int` from `TRUSTED_PROXY_HOPS`, default 0, invalid values fail `Load`. `NewAuthHandler` gains a `trustedHops int` parameter.

- [ ] **Step 1: Write failing tests.**
  `TestByClientIPNoProxyIgnoresHeader`: hops 0, forged header, returns the RemoteAddr host.
  `TestByClientIPOneHopTakesRightmost`: hops 1, `203.0.113.7, 198.51.100.9`, returns `198.51.100.9`.
  `TestByClientIPMalformedFallsBack`: entry not an IP, or fewer entries than hops, returns RemoteAddr.
  `TestForgedForwardedForSharesBucket` (integration): hops 1, rotating left entries, fixed right entry. Assert 429 after the limit.
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3: Implement.** Join `r.Header.Values("X-Forwarded-For")`, split on commas, take index `len - trustedHops`, validate with `net.ParseIP`. Set `TRUSTED_PROXY_HOPS = "1"` on the API task in `ecs.tf`. Update tests that rotated the header to get fresh buckets.
- [ ] **Step 4:** `make test test-integration`. Expected: PASS.
- [ ] **Step 5:** Commit: `Rate limit on the proxy-appended client IP, not a client-supplied header`.

---

### Task 14: Finding #11 — unbounded request bodies

**Why:** the idempotency middleware calls `io.ReadAll` with no limit, and the JSON decoders have none either. The server also has no header timeout, which the review did not mention; one line closes it.

**Files:** Modify `cmd/api/main.go`, `internal/idempotency/idempotency.go`, `internal/http/response/response.go`, and every `json.NewDecoder(r.Body)` site in `internal/http/`. Test: `internal/http/auth_integration_test.go`, `internal/http/purchase_integration_test.go`.

**Interfaces:** Produces `const MaxBodyBytes = 64 << 10` and `func LimitBody(h http.Handler) http.Handler` in package `api`, wrapping `http.MaxBytesHandler`. Produces `func response.BodyTooLarge(w http.ResponseWriter, r *http.Request, err error) bool`, which writes 413 `request_too_large` when `errors.As` finds `*http.MaxBytesError`.

- [ ] **Step 1: Write failing test** `TestOversizedBodyRejected`: table over purchase, register and login with a body of `MaxBodyBytes + 1`. Assert 413, and for purchase that no `idempotency_keys` row was created.
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3: Implement.** Wrap the server handler with `LimitBody`. Call `BodyTooLarge` first in each read or decode error branch. Set `ReadHeaderTimeout: 5 * time.Second` on `http.Server`. The webhook's own `LimitReader` stays.
- [ ] **Step 4:** `make test test-integration`. Expected: PASS.
- [ ] **Step 5:** Commit: `Bound request bodies at 64 KiB and answer 413; add a header read timeout`.

---

### Task 15: Findings #12 and #23 — secrets default to known values

**Why:** an unset `JWT_SECRET` starts with a value published in the repo, so anyone can mint an admin token. An explicitly empty `WEBHOOK_SECRET` cannot disable the receiver as documented, because `getenv` treats empty as unset. `Load` is shared, so one change covers both.

**Files:** Modify `internal/config/config.go`, `cmd/payment-service/main.go`, `Makefile`, `docker-compose.yml`, `.env.example`, `scripts/chaos/lib.sh`, `scripts/demo_reconciliation.sh`. Test: create `internal/config/config_test.go`.

- [ ] **Step 1: Write failing tests** (`t.Setenv`).
  `TestLoadRejectsMissingJWTSecret`, `TestLoadRejectsPlaceholderJWTSecret` (`change-me-in-real-env`), `TestLoadRejectsShortJWTSecret` (31 chars).
  `TestLoadAcceptsStrongJWTSecret`.
  `TestWebhookSecretHasNoDefault`: empty and unset both give `""`.
- [ ] **Step 2:** Run `go test ./internal/config/`. Expected: FAIL.
- [ ] **Step 3: Implement.** `JWT_SECRET` is required, not the placeholder, at least 32 bytes; otherwise `Load` returns an error, which every `main` already turns into exit 1. `WebhookSecret: os.Getenv("WEBHOOK_SECRET")`; `verify` already rejects an empty secret. In `cmd/payment-service`, exit with an error when `WEBHOOK_URL` is set and the secret is empty.
- [ ] **Step 4:** Supply dev values: `export JWT_SECRET ?= localdev-jwt-secret-not-for-production-0000` in the Makefile, the same literal for `api` and `worker` in compose, and an export in the two scripts if they start binaries directly. Keep `WEBHOOK_SECRET` where it is already set, and check any local flow that needs the webhook still gets it.
- [ ] **Step 5:** `make test test-integration`, `make demo`. Start the API with `JWT_SECRET` unset. Expected: exits 1 with a clear error.
- [ ] **Step 6:** Commit: `Fail fast on a missing or known JWT secret; no default webhook secret`.

---

### Task 16: Finding #17 — a failed webhook is never retried

**Why:** the dedup row commits before processing, so a retry after a failure gets `duplicate_ignored`. Related: a provider-unreachable result is marked processed and answered 200.

**Files:** Modify `internal/http/webhook.go`. Test: `internal/http/webhook_integration_test.go`.

**Interfaces:** Consumes `booking.ProviderInFlight` from Task 4.

- [ ] **Step 1: Write failing test** `TestWebhookRetryAfterFailureProcesses`: provider unreachable, first delivery answers 503 and `processed_at` is NULL. Provider restored, identical delivery resolves the booking. Third delivery answers `duplicate_ignored`.
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3: Implement.** Replace the claim with `INSERT INTO webhook_events_seen (event_id) VALUES ($1) ON CONFLICT (event_id) DO UPDATE SET event_id = EXCLUDED.event_id RETURNING processed_at`. Non-NULL means `duplicate_ignored`; NULL means process. Two concurrent deliveries are safe because `ReconcileBooking`'s guarded UPDATE lets only one apply. In `resolve`, when `res.Provider` is `ProviderUnreachable` or `ProviderInFlight`, answer 503 `provider_unreachable` without `markProcessed`. Update the comments that say everything answers 200.
- [ ] **Step 4:** `make test test-integration`. Expected: PASS.
- [ ] **Step 5:** Commit: `Dedupe webhooks on processed_at so a failed delivery can be retried`.

---

### Task 17: Finding #18 — webhook timestamp is not signed

**Why:** the HMAC covers only the body, so the timestamp can be replaced. Real impact is small, because a replay only makes the resolver ask the provider again, but the freshness check currently proves nothing.

**Files:** Modify `internal/paymentsim/webhook.go`, `internal/http/webhook.go`. Test: `internal/http/webhook_integration_test.go` (call sites at lines 55 and 200).

**Interfaces:** Changes to `func Sign(ts string, body []byte, secret string) string`, an HMAC over `ts + "." + body`.

- [ ] **Step 1: Write failing test** `TestWebhookRejectsRetimestampedReplay`: sign with a 1-hour-old timestamp, send with a current `X-Webhook-Timestamp`. Assert 401 `invalid_signature`.
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3: Implement.** Change `Sign`, `deliver` and `verify(raw, ts, sig)`, and update test helpers.
- [ ] **Step 4:** `make test test-integration`. Expected: PASS.
- [ ] **Step 5:** Commit: `Sign the webhook timestamp together with the body`.

---

### Task 18: Finding #19 — generated DB password breaks the connection URL

**Why:** `override_special` includes `#`, `%`, `?`, `[`, `]` and more, interpolated unescaped into `postgres://` URLs. With 32 characters a breaking character is close to certain, so this would very likely fail the first deploy.

**Files:** Modify `terraform/secrets.tf`.

- [ ] **Step 1:** Install terraform (not on PATH). Set `special = false` on `random_password.db` and remove `override_special` and its comment, matching the other generated secrets. 32 alphanumeric characters is ample entropy.
- [ ] **Step 2:** `terraform fmt -check` and `terraform validate` in `terraform/`. Expected: pass.
- [ ] **Step 3:** Commit: `Generate a URL-safe database password`.

---

### Task 19: Finding #20 — ALB exposes /metrics publicly

**Why:** `/metrics` shares the API listener and the ALB forwards every path. The comment at `terraform/network.tf:97` claims the security group protects it, which is false: the ALB is allowed through that group. Prometheus scrapes the task directly on 8080, so blocking at the ALB leaves scraping working.

**Files:** Modify `terraform/alb.tf`, `terraform/network.tf:97`, `terraform/README.md`.

- [ ] **Step 1:** Add `aws_lb_listener_rule.block_metrics`: priority 1, `path_pattern` values `["/metrics", "/metrics/*"]`, action `fixed-response` 403 `text/plain`.
- [ ] **Step 2:** Correct the comment in `network.tf`. Add a post-deploy check to the README: `curl -i http://$ALB/metrics` returns 403, and the Prometheus API target is `up`.
- [ ] **Step 3:** `terraform fmt -check`, `terraform validate`. Expected: pass.
- [ ] **Step 4:** Commit: `Block /metrics at the ALB`.

---

### Task 20: Finding #21 — verify_run can report success when nothing ran

**Why:** failures are ignored and success means "no FAIL row found", so with Postgres down it prints ALL CHECKS PASSED. Past evidence is unaffected: `docs/screenshots/verify-run-all-checks-pass.txt` contains real PASS rows.

**Files:** Modify `scripts/verify_run.sh`. Create `scripts/verify_run_selftest.sh`. Add the selftest to `ci.yml`.

- [ ] **Step 1: Write failing selftest.** Put a fake `docker` first on PATH and run `verify_run.sh` for three cases: psql exits 1; psql exits 0 with no check rows; `redis-cli` exits 1. Assert each exits nonzero and never prints `ALL CHECKS PASSED`.
- [ ] **Step 2:** Run `bash scripts/verify_run_selftest.sh`. Expected: FAIL.
- [ ] **Step 3: Implement.** Add a `die()` that exits 2. Run psql with `-v ON_ERROR_STOP=1` and die on nonzero. Require the number of `^ (PASS|FAIL)` rows to equal `grep -c 'AS result' scripts/verify_run.sql`. Die if the tier query fails. Check `redis-cli` exit status so a failure is not read as an empty key.
- [ ] **Step 4:** With the stack up, `make verify-run` passes on real data. `docker compose stop postgres`, then `make verify-run` exits nonzero. Restart it.
- [ ] **Step 5:** Commit: `verify_run fails when a check could not run`.

---

### Task 21: Finding #24 — event details always report queue size 0

**Why:** leftover `TODO(week 2)` placeholder.

**Files:** Modify `internal/http/events.go`, `internal/queue/queue.go`, `cmd/api/main.go`, `internal/http/events_integration_test.go:27`.

**Interfaces:** `NewEventsHandler(pool, rdb)`. Produces `func queue.Size(ctx context.Context, rdb *redis.Client, eventID string) (int64, error)` using `ZCARD`. The response field becomes `map[string]*int64`.

- [ ] **Step 1: Write failing tests.**
  `TestEventDetailReportsQueueSize`: three joined users, assert `queue.size == 3`.
  `TestEventDetailRedisDown`: closed Redis client, assert 200 and `queue.size` is null. It must not be 0, which would be false.
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3: Implement** and delete the TODO. On a Redis error, log and return nil size.
- [ ] **Step 4:** `make test test-integration`. Expected: PASS.
- [ ] **Step 5:** Commit: `Report the real queue size on event details`.

---

### Task 22: Finding #8, spec update, and full regression gate

**Why:** #8 is a known simulator limit already recorded in §10 and §17, so the fix is an honest note, not code (decision D2). Tasks 2, 3 and 8 change hot paths, so the Week 6 evidence must be re-proved before the AWS run.

**Files:** Modify `.skills/IMPLEMENTATION_PLANS.md`, `terraform/README.md`, `docs/load-test-results.md`.

- [ ] **Step 1:** Add to `terraform/README.md`: never redeploy or restart the payment-service task during a run, because reconciliation would see real charges as "never charged".
- [ ] **Step 2:** Update the plan: §6 (indexes, migrations 15 and 16), §7 (provider key is a per-booking UUID), §9 (promotion, drift job D), §13 (mark this work, correct line 304 on Terraform, remove stale outstanding items at 285–286, repair the truncated CHAOS 2 line at 296 from `docs/load-test-results.md`), §17 (new gaps and #8 note), §18 (rev. 6), and the header. This commit also carries the existing whitespace changes.
- [ ] **Step 3:** Run in order: `make test`, `make test-integration`, the concurrency tests with `-race -count=3`, `make test-pgbouncer`, `make demo`. Expected: all green.
- [ ] **Step 4:** Re-run one 2,000-VU oversell storm, `make verify-run`, and `make chaos-all`. Expected: exactly 100 sold, 0 oversells, every check passing. Record throughput next to the old numbers. If it moved, report it rather than hide it.
- [ ] **Step 5:** Commit: `Record the review fixes: spec, gaps, regression evidence`.
- [ ] **Step 6:** Use superpowers:finishing-a-development-branch.
