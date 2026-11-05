# Load-test results — Week 6

Measured 2026-09-18. Every number below comes from a run whose raw output is
committed next to it: k6 summaries and `verify_run.sql` transcripts in
[`docs/screenshots/`](screenshots/), chaos transcripts in [`docs/chaos/`](chaos/).
The harness is described in [`scripts/k6/README.md`](../scripts/k6/README.md).

## 1. Environment

Everything ran on one laptop, client and server sharing the same cores.

| | |
|---|---|
| Machine | MacBook, Apple M1, 8 cores, 8 GB RAM, macOS 26.5.2 |
| Docker | Docker Desktop 29.5.3; VM given **8 CPUs / 4.1 GB**; no per-container limits in `docker-compose.yml` |
| Postgres | 18.4 (`postgres:18-alpine`), default config |
| Redis | 7.4.9 (`redis:7-alpine`) |
| Observability | Prometheus 3.1.0 (5s scrape, 15d retention), Grafana 11.5.1 |
| App | Go 1.26.3; `cmd/api`, `cmd/worker`, `cmd/payment-service` as host processes (not containers) |
| DB pool | **pgxpool `MaxConns = 10`** (`internal/db/pool.go`), overridable with `POOL_MAX_CONNS` — see §7 |
| PgBouncer | Not present for §2–§6. Added and measured in §7; `pgbouncer/pgbouncer.ini`, transaction mode, `default_pool_size=25` |
| Load generator | k6 v2.2.0 on the same machine |
| Kernel | `kern.ipc.somaxconn = 128`, `kern.maxfilesperproc = 10240` |

The load generator, all three application processes, Postgres, Redis, Prometheus
and Grafana compete for the same eight cores. A dedicated load generator would
push every number here higher.

## 2. Oversell proof

Configuration: 2,000 VUs, each with a pre-minted purchase token, all firing
`POST /v1/purchases` at once against a **fresh 100-ticket tier**. Three
consecutive runs, re-seeded between each.

| Run | VUs | Tier capacity | Sold | Held after | Oversells | 5xx | Run duration | verify_run |
|---:|---:|---:|---:|---:|---:|---:|---:|---|
| 1 | 2,000 | 100 | **100** | 0 | **0** | 0 | 5.2 s | 9/9 PASS |
| 2 | 2,000 | 100 | **100** | 0 | **0** | 0 | 4.1 s | 9/9 PASS |
| 3 | 2,000 | 100 | **100** | 0 | **0** | 0 | 3.5 s | 9/9 PASS |

Raw output per run:
[`k6-oversell-run1-2000vu-100tickets.txt`](screenshots/k6-oversell-run1-2000vu-100tickets.txt) /
[`verify-run-oversell-run1.txt`](screenshots/verify-run-oversell-run1.txt), and
likewise `run2`, `run3`.

Every correctness threshold passed on every run: `http_req_failed == 0`,
`server_error == 0`, `unexpected == 0`, checks 100%. The one threshold that fails
at 2,000 VUs is `purchase_completed_duration p(95) < 2000ms` — a latency
threshold, and §3 explains why 2,000 VUs is past where this hardware can meet
it. A 500-VU run against the same 100-ticket tier passes **every** threshold
(p95 815 ms, 100/100 sold):
[`k6-oversell-500vu-100tickets-all-thresholds-pass.txt`](screenshots/k6-oversell-500vu-100tickets-all-thresholds-pass.txt).

What `verify_run.sql` checks after each run: no booking holds more tickets than
it paid for; sold count never exceeds tier capacity; sold tickets == confirmed
booking quantities; no confirmed booking without a succeeded payment; no
succeeded payment without a confirmed booking; confirmed revenue == charged
amount; three structural invariants from the drift checker; nothing still held
after five minutes; Redis inventory == Postgres per tier.

## 3. Latency

Configuration: 2,000 VUs against a **2,000-ticket tier**, so every request
completes a real purchase and there are 2,000 latency samples instead of 100.

| Metric | Value |
|---|---:|
| `purchase_completed_duration` p50 | **6.26 s** |
| `purchase_completed_duration` p95 | **6.48 s** |
| `purchase_completed_duration` p99 | **6.52 s** |
| min / max | 1.69 s / 6.61 s |
| Throughput | 289 completed purchases/s |
| Sold | 2,000 / 2,000 |
| 5xx / connection errors | 0 / 0 |
| verify_run | 9/9 PASS |

Raw: [`k6-latency-2000vu-2000tickets.txt`](screenshots/k6-latency-2000vu-2000tickets.txt),
[`verify-run-latency-2000tickets.txt`](screenshots/verify-run-latency-2000tickets.txt).

### Where the time went — `purchase_phase_duration_seconds`, same window

| Phase | p50 | p95 | p99 | Share of total time |
|---|---:|---:|---:|---:|
| `tx1` — Redis fast path + `SKIP LOCKED` ticket claim | 1.55 s | 2.85 s | 4.57 s | 42% |
| `payment_call` — HTTP to the payment provider | **3 ms** | **5 ms** | 5 ms | **0.01%** |
| `tx2` — record payment, confirm booking, mark tickets sold | 1.77 s | 3.49 s | 4.70 s | 58% |

**The database phases dominate; the payment call is noise.** `tx2` is the
largest single phase. The provider answered in 5 ms at p95 on its own
histogram (`charge_duration_seconds`), so none of the 6.5 s is the simulated
network — it is time queued in front of a database that is already at 100% CPU
(see §4 and the pool experiment in §7).

### Why p95 is measured on completed purchases only

In a contention run against a 100-ticket tier, ~95% of requests are rejected
by the Redis fast path in a couple of milliseconds. A p95 over *all* requests is
therefore a p95 **of rejections**: fast, flattering, and meaningless as a
statement about how long it takes to buy a ticket. `purchase_completed_duration`
holds HTTP 201 responses only; rejections, ambiguous 504s and declines go to
their own trends. The headline number is the slow one, on purpose.

That is also why the 2,000-ticket configuration exists. A p95 over 100 samples
sits on the 95th value and wobbles run to run — the three oversell runs above
measured 5.01 s, 2.22 s and 2.52 s on the same configuration. Never quote a
latency from a 100-ticket run.

## 4. Ceiling and bottleneck

Fixed-VU runs, one per step, re-seeded between them, tier sized at 2× the VU
count so every request does full purchase work.

| VUs | completed p(95) | throughput | 5xx | connection errors |
|---:|---:|---:|---:|---:|
| 500 | 1.31 s | 360/s | 0 | 0 |
| 1,000 | 2.55 s | 373/s | 0 | 0 |
| 2,000 | 5.82 s | 322/s | 0 | 0 |
| 4,000 | 15.81 s | 244/s | 0 | 0 |
| 8,000 | 36.45 s | 199/s | 0 | **0** |
| 10,000 (100-ticket tier) | 17.89 s | 444/s | 0 | **1,649 (16.5%)** |

Throughput plateaus at **~350 completed purchases/s** from 500 VUs onward and
never rises. Past that point extra VUs buy queue time and nothing else: p95
tracks Little's law (2,000 VUs ÷ ~350/s ≈ 5.7 s; measured 5.82 s).

**Error rate left zero at 10,000 VUs.** 8,000 is the highest level sustained
with zero 5xx and zero dropped connections.

### What saturated first, in order

> **Corrected 2026-09-18 (Week 7).** This section originally said the connection
> pool was the ceiling and that it "was set by a configuration value, not by the
> design". That was wrong as a causal claim, and §7 shows the experiment that
> disproved it: raising `MaxConns` from 10 to 50 does not raise throughput.
> A full pool in front of a saturated database is a queue, not a cause.

1. **Postgres CPU, from ~1,000 VUs.** During a 2,000-VU run the Postgres
   container sits at **749–788% CPU out of the 800% its VM has**, with host idle
   at **0.0%**. The service rate is fixed by how fast the database can execute
   `tx1` and `tx2`, and no client-side setting moves it.
2. **The connection pool is where that backpressure becomes visible, not where
   it originates.** `db_pool_in_use` pins at `db_pool_max` and the API
   accumulates ~15,000 s of `db_pool_acquire_wait_seconds_total` per 15 s
   window — about one empty-pool wait per request. Widen the pool and the same
   waiting happens inside Postgres instead (§7).
3. **The kernel accept queue, at 10,000 VUs.** `kern.ipc.somaxconn = 128`. With
   the app taking ~15 s per request, the listen backlog overflowed and the
   kernel dropped connections before the process saw them. Proof: k6 sent 10,000
   requests, the API logged exactly **8,351** (100 × 201 + 8,251 × 409), and the
   1,649 missing equal k6's connection-error count exactly. The API held 148 file
   descriptors at peak, so `maxfilesperproc` was never the constraint.

The application itself never produced a 5xx at any level, and the 10,000-VU
run still sold exactly 100 of 100 tickets.

**Was the test limited by the load generator?** Partly. k6 ran on the same
eight cores as everything it was loading, so from ~4,000 VUs the client and the
server were starving each other. The pool saturation at 1,000 VUs is real and
happened well before that; the 8,000 and 10,000 numbers should be re-measured
from a separate load generator after the Week 7 deploy before being quoted as
the system's ceiling.

## 5. Chaos results

All three inject the fault **while 500 VUs of load are in flight**. Transcripts
in [`docs/chaos/`](chaos/), method in [`docs/chaos/README.md`](chaos/README.md).

### CHAOS 1 — API SIGKILLed 30 s into the run, restarted 5 s later

> 270 bookings stranded by the crash, all 270 resolved 271s after it, zero money discrepancies.

| | |
|---|---|
| Stranded in `held` at the kill | 270 |
| Resolved by | reconciliation worker (`reconciliation_resolution_total{source="reconciliation"}` +270) |
| Recovery, measured from the kill | **271 s** |
| verify_run | 9/9 PASS |

Recovery is **batch-limited**: `StuckAfter` is 90 s and `ReconcileInterval` 30 s,
but `ReconcileBatch` is 50 per cycle, so a backlog drains at ~100 bookings/min.
A single stranded booking recovers in 90–120 s (measured 105 s in the Week-4
demo); 270 of them take ~4.5 min. The `http_req_failed` figure in that
transcript (~70%) is the seconds the process did not exist — refused
connections return in microseconds and dominate the count. The same load with
no kill: 0.00% over 6,988 iterations.

### CHAOS 2 — payment provider unreachable for 90 s

> Provider down for 90s. 500 bookings held, 0 tickets released on a guess, all of them resolved within 37s of recovery.

| | |
|---|---|
| Bookings frozen in `held` | 500 (pinned at the VU count: one held booking per user) |
| Tickets available at outage start → highest during outage | 36,337 → 36,337 — **never rose** |
| `expiry_deferred_provider_unreachable` audit rows / distinct bookings | 59 / 59 — **exactly one each** across ~18 hold-expiry cycles |
| `reconciliation_resolution_total{outcome="deferred"}` during outage | 30 |
| Recovery, measured from the provider answering again | **37 s** |
| verify_run | 9/9 PASS |

Inventory froze and healed itself; no paid customer's seat was resold. The
outage used the §10 chaos switch (the provider *hangs*, so the client's own 3 s
timeout fires) rather than a restart, because the simulator's ledger is
in-memory and a restart would have made it *forget* charges — a different
failure.

### CHAOS 3 — worker SIGKILLed for 60 s, API left alive, 10% `tok_drops_response`

> Worker dead 60s under load: unresolved bookings peaked at 347, the API never stopped completing purchases, the 80 left over drained 93s after restart, zero money discrepancies.

| | |
|---|---|
| Unresolved bookings, peak during outage | 347 |
| Confirmed purchases across the outage | 5,924 → 11,288 — rising at every sample |
| Left over at worker restart | 80 |
| Drain, measured from restart | **93 s** |
| verify_run | 9/9 PASS |

The held count did **not** climb monotonically while the worker was dead, because
most of it is healthy purchases between TX1 and TX2. **Correction (2026-11-04):**
this section used to credit the webhook. That was wrong: the scenario never sets
`WEBHOOK_URL`, and the Week 6 database shows the webhook resolved one booking in
total. The peak of 347 above is held bookings of any kind, not stuck ones.

## 6. Known limitations of this benchmark

- **Single machine, shared cores.** Load generator and every server process
  ran on the same eight-core laptop. Numbers above 4,000 VUs measure the
  contest between them as much as the application.
- **Docker VM smaller than the host.** Postgres and Redis ran inside a 4.1 GB /
  8-CPU VM, not with the machine's full 8 GB.
- **Simulated payment provider on localhost.** `payment_call` was 5 ms at p95.
  A real provider is 100–500 ms away and would become the dominant phase at low
  concurrency; the pool contention would then be hidden behind it rather than
  absent.
- **No PgBouncer, pool of 10** in the Week 6 runs. PgBouncer was added and
  measured afterwards (§7); it did not move throughput on this machine, because
  the limit is the database's CPU, not connection handling. On RDS, where the
  database has its own cores and connections are expensive, the answer may
  differ — re-run the curve there.
- **VU count limited by the load generator** at the top end, and by the macOS
  listen backlog at 10,000. Neither is the application.
- **100-ticket runs have thin latency samples** (100 values). That is why the
  2,000-ticket configuration exists and why every latency quoted here comes
  from it.
- **`purchase_phase_duration_seconds` uses Prometheus `DefBuckets`, whose top
  bucket is 10 s.** At 4,000 VUs the phase p95 reported exactly 10.00 s — i.e.
  censored. The phase breakdown is trustworthy at ≤ 2,000 VUs and not above.
- **Database size grew across the day.** By the final runs the `booking` table
  held ~90,000 rows from earlier sessions; the 2,000-ticket p95 moved from
  5.25 s (morning, fresh DB) to 6.48 s (evening). Both are reported; the
  evening figure is the one above because its raw output is committed.
- **One day, one operator.** No repeated-day variance measured.

## 7. PgBouncer and pool sizing

Run before the AWS deploy, because the answer changes what is worth paying for.
Same script, same 2,000 VUs against a 3,000-ticket tier, three passes of each
configuration; the median is quoted because one pass of (b) produced an outlier
(109.9/s) that did not reproduce.

| | Config | Throughput (3 passes) | Median | p(95) | `db_pool_in_use` peak | 5xx | prepared-stmt errors |
|---|---|---|---:|---:|---:|---:|---:|
| (a) | Direct to Postgres, `MaxConns=10` — the Week 6 baseline | 235.2 / 212.2 / 257.2 | **235.2/s** | 8.25 s | 10 | 0 | 0 |
| (b) | Direct to Postgres, `MaxConns=50` | 109.9 / 222.6 / 211.0 | **211.0/s** | 8.56 s | 50 | 0 | 0 |
| (c) | Through PgBouncer, `MaxConns=50`, `default_pool_size=25` | 201.1 / 225.4 / 210.0 | **210.0/s** | 9.20 s | 50 | 0 | 0 |

**(b) does not beat (a).** Five times the connections buys nothing, so the Week 6
plateau was never pool starvation and the Week 6 number does not understate the
system. Grafana Row 3 points straight at the reason, and `docker stats`
confirms it: **Postgres runs at 749–788% CPU out of 800%, with the host at 0.0%
idle.** The database is the service rate. A wider pool just moves the queue from
`pgxpool` into Postgres, which is why (b) is, if anything, slightly worse.

**(c) costs about 10% latency and buys nothing here, which is the expected
result and not an argument against it.** PgBouncer earns its place when
connections are expensive and numerous — an RDS instance with `max_connections`
in the low hundreds and several ECS tasks each holding a pool. On one laptop
with one API process there is nothing for it to multiplex. It is in the stack
because Week 7 needs it, and because running it locally is how the two
incompatibilities in §8 were found before they could cost ECS time.

Correction to §4 follows from this: the pool was a symptom, not the cause.

## 8. Two incompatibilities PgBouncer exposed

Both were found locally, in minutes, and both would have been expensive in ECS.

**1. The documented trap — prepared statements — is real but no longer
automatic.** pgx prepares and caches statements per connection; transaction-mode
PgBouncer hands a different server connection to each transaction. Reproduced
deliberately at 200 goroutines × 20 transactions:

| PgBouncer setting | Result |
|---|---|
| `max_prepared_statements = 0` | 3,891 ok, **107 `prepared statement ... does not exist`** (2.7%) |
| `max_prepared_statements = 200` (1.25 default) | 4,000 ok, 0 errors |
| any, with pgx simple protocol | 4,000 ok, 0 errors |

PgBouncer ≥ 1.21 tracks prepared statements and re-prepares them on whichever
server connection it hands out, so on 1.25.2 the trap does not fire by itself.
The value is pinned in `pgbouncer/pgbouncer.ini` rather than inherited, and the
app still switches to simple protocol under `PGBOUNCER=true`, because this is a
property of whatever pooler is actually in front of the database — an older
PgBouncer, or RDS Proxy, will not do it for you.

Note the failure shape: the full integration suite passed against PgBouncer with
prepared statements enabled and the fix switched off. Only the concurrency
reproduction showed it. That is exactly the pattern the plan warned about.

**2. The undocumented trap — JSONB parameters — broke everything.** Turning on
simple protocol turned the suite red across five packages:

```
unable to encode map[string]interface{}{...} into text format
for unknown type (OID 0): cannot find encode plan
invalid input syntax for type json (SQLSTATE 22P02)
```

Under the extended protocol pgx asks the server to describe each parameter,
learns the target column is `jsonb`, and picks an encoder. Simple protocol has
no describe step, so a bare Go map has no target type and a `[]byte` encodes as
a `bytea` literal. Every `audit_log` write was affected — the audit trail is not
an optional feature. Fixed in one place (`db.JSONB`) with an explicit `::jsonb`
cast at all eight call sites, which behaves identically under both protocols, so
there is no PgBouncer-only code path to keep in sync.

## 9. Index and audit tuning (2026-09-19)

Three fixes found with `pg_stat_statements` and `EXPLAIN (ANALYZE, BUFFERS)`,
committed separately so each carries its own evidence.

### Query-level results — deterministic, measured per fix

| Fix | Query | Before | After |
|---|---|---:|---:|
| 1 | TX1 claim (`SKIP LOCKED`), on the 258k-ticket tier | 48.4 ms / 41,667 buffers | **0.07 ms / 5 buffers** |
| 1 | Tier availability `COUNT(*)`, warm | 18.5 ms / 1,508 buffers | **11.1 ms / 1,681 buffers** |
| 2 | Stuck gauge, `charged_not_delivered` branch | Parallel Seq Scan, 11,418 buffers | **1 buffer** |
| 2 | Stuck gauge, whole query | 11,452 buffers | **32 buffers** |

Fix 1 replaced `idx_tickets_claim(ticket_tier_id, status)` with
`(ticket_tier_id, id)`: `status` was dead weight inside a partial index that
already filters on it, and nothing in the key matched `ORDER BY id`, so there
was no good plan — only two bad ones the planner alternated between (index +
Sort at 471 ms, or `tickets_pkey` with a filter at 48 ms). Costs 6.6× index size
(1,848 kB → 12 MB): a unique `id` per entry defeats B-tree deduplication, which
had been compressing the old repeated keys.

Fix 2 added an 8 KB partial index on `audit_log(entity_id) WHERE
action='reconciliation_needed'`, so the planner drives the gauge's `EXISTS` from
the tiny table instead of seq-scanning `booking` for the unindexable
`status <> 'confirmed'`. No SQL changed; the planner picked it up on its own.

Fix 3 dropped `idx_audit_actor` (20 MB, nothing filters on `actor_user_id`),
merged `booking_created` + `tickets_held` into one row, and removed the
redundant `tier_id` from metadata.

### Profile shift — `pg_stat_statements`, 10,000 purchases

| | Before | After |
|---|---:|---:|
| TX1 claim query | **#1, 19.2%**, 3.35 ms mean | **#10, 3.9%**, 0.19 ms mean |
| `audit_log` inserts | 24.5%, 4.15 rows/purchase | **15.8%**, 3 rows/purchase |
| New top entry | — | `INSERT INTO booking`, 20.4% |

The claim query left the top of the profile, which was the goal.

### End-to-end throughput — no measurable change, and that is the finding

Five passes, 2,000 VUs against a 3,000-ticket tier, same configuration as §7(a):

| | Passes | Median |
|---|---|---:|
| Before (§7a) | 235.2 / 212.2 / 257.2 | **235.2/s** |
| After | 208.2 / 254.4 / 252.9 / 159.4 / 191.5 | **208.2/s** |

The ranges overlap almost entirely, so the honest reading is **no measurable
change**, not a regression. Two reasons, both worth stating:

1. **The benchmark tier is small.** The 48 ms → 0.07 ms win was measured on a
   258k-ticket tier accumulated over the project's life. The storm runs against
   3,000 tickets, where even the bad plan only sorted ~3,000 rows. Fix 1 matters
   most as a tier grows, and the benchmark does not exercise that.
2. **§4 still holds.** Postgres was at 749–788% of 800% CPU before any of this,
   and making individual queries cheaper does not change a service rate set by
   total CPU across ~10 indexed writes per purchase. Cheaper queries mean more
   headroom, not more throughput, until something else gives.

**So the Week 6 conclusion does not change.** The bottleneck is still database
CPU, not any single query. That was worth confirming rather than assuming: these
fixes were exactly the kind that *could* have overturned it, and did not.

Verification: integration suite green 3× through PgBouncer and once direct,
`make demo` PASS (recovered in 116 s), `verify_run.sh` 9/9.

## Numbers to carry forward

> These are the Week 6 numbers. Section 10 has the current ones and takes precedence.

The resume and README should carry these, with the qualifier attached:

| Claim | Measured value | Say it as |
|---|---|---|
| Concurrent buyers | **8,000** VUs sustained with 0 errors, 0 oversells; **10,000** still sold exactly 100/100 with the kernel dropping 16% of connections | "8,000 concurrent buyers, zero errors, zero oversells — ~1,000 under 2 s p95 on a single laptop" |
| P95 purchase latency | **6.48 s** at 2,000 VUs on a pool of 10; **815 ms** at 500 VUs; **1.31 s** at 500 VUs full-purchase | Quote the 500-VU figure with the VU level; never the 2,000-VU figure without "pool of 10, no PgBouncer" |
| Recovery after a crash | **90–120 s** for one stranded booking (measured 105 s); backlog drains at **~100 bookings/min** (270 in 271 s, CHAOS 1) | Two numbers, always together |
| Provider outage | **0** tickets released on a guess across 500 frozen bookings and 90 s; resolved 37 s after recovery | The money-rule sentence |
| Worker outage | API kept confirming; 347-booking peak drained 93 s after restart | "The worker is a recovery path, not a dependency" |

## 9. Regression after the review fixes (2026-11-04)

Re-run after the Codex review fixes, which changed the hot path (provider key, a
unique index on held bookings, promotion moved to Postgres). Same laptop, same
harness, one run of each, so differences this small are within noise.

| | Week 6 | After the fixes |
|---|---|---|
| Oversell, 2,000 VUs vs 100 tickets | 100 sold, 0 oversells | **100 sold, 0 oversells** |
| Latency, 2,000 VUs vs 2,000 tickets, p50 / p95 | 6.26s / 6.48s | **6.49s / 6.62s** |
| Throughput in that run | 289 purchases/s | **280 purchases/s** |
| CHAOS 1, API killed | 270 stranded, 271s | 59 stranded, 151s |
| CHAOS 2, provider down 90s | 500 frozen, 0 released, 37s | **500 frozen, 0 released, 24s** |
| verify_run | all pass | **all pass** |

The p95 < 2s k6 threshold fails at 2,000 VUs in both columns; it is written for
500 VUs. CHAOS 1 stranded fewer bookings because a kill lands on whatever happens
to be in flight, so the two rows are not a like-for-like speedup.

CHAOS 3 failed its first run on one check, `HELD_PEAK >= HELD_AT_KILL`. That was
a harness flaw, not a regression: at this load held bookings are mostly purchases
between TX1 and TX2, so the check compared two random samples of traffic (426 at
the kill, 286 at the peak). It now counts held bookings with an `unknown` payment,
and the re-run passed with 23 of them waiting and a 207-booking backlog drained in
122s. Both runs are kept in `docs/chaos/`.

### Final run, after the branch review fixes

The review added Redis timeouts, transactional queue audit rows, durable Position
reads and disconnect handling. Everything was re-run on that final code: 100 of
100 and 2,000 of 2,000 sold, verify_run passing after both, PgBouncer suite green,
demo recovered in 112s, and all three chaos scenarios passed (CHAOS 1: 8 stranded,
resolved in 117s. CHAOS 2: 500 frozen, 0 released, resolved within 39s. CHAOS 3:
79 left over, drained). Use these numbers rather than the table above where they
differ; a kill lands on whatever is in flight, so CHAOS 1 varies by run.

**Throughput is unresolved.** That run measured 147 purchases/s, so old and new
binaries were compared on the same 2,000-VU, 2,000-ticket storm, interleaved and
then with the order reversed:

| | Purchases/s, sorted | Median |
|---|---|---|
| Before the review fixes (dff2639) | 211, 222, 260, 264, 321 | 260 |
| After them | 141, 166, 230, 238, 268 | 230 |

Five runs each on a busy laptop do not separate these. Two of the five new runs
were slow, and none of the old ones were. No errors were logged in those runs and
the purchase path gained no query, so no mechanism was found. Treat a possible
slowdown of up to about 10% as open, and settle it on the AWS run, where the
database has its own hardware and the load generator does not share it.

## 10. Re-measured on the current code (2026-11-05)

The ceiling and the pool comparison were repeated with the same method, because
sections 4 and 7 predated the review fixes. Raw results:
[`remeasure-2026-11-05.jsonl`](screenshots/remeasure-2026-11-05.jsonl). verify_run
passed afterwards. One run per step, so read the shape, not the exact values.

| VUs | Week 6 p95 | Now p95 | Week 6 rate | Now rate | 5xx | Sold |
|---:|---:|---:|---:|---:|---:|---:|
| 500 | 1.31 s | 2.48 s | 360/s | 197/s | 0 | 500 of 500 |
| 1,000 | 2.55 s | 3.78 s | 373/s | 255/s | 0 | 1,000 of 1,000 |
| 2,000 | 5.82 s | 6.38 s | 322/s | 298/s | 0 | 2,000 of 2,000 |
| 4,000 | 15.81 s | 15.01 s | 244/s | 260/s | 0 | 4,000 of 4,000 |
| 8,000 | 36.45 s | 33.88 s | 199/s | 214/s | 0 | 8,000 of 8,000 |
| 10,000, 100 tickets | 17.89 s | 18.49 s | 444/s | 519/s | 0 | 100 of 100 |

- **Zero 5xx and no failed purchases up to 8,000 VUs.** That finding holds.
- **At 10,000, 275 connections (2.75%) failed**, against 1,649 (16.5%) in Week 6,
  and exactly 100 tickets still sold. The kernel backlog is still the limit.
- **From 2,000 VUs up the numbers match Week 6. At 500 and 1,000 they are clearly
  lower.** This run did not explain why. The 500 step was the first after a cold
  start and lasted about 2.5s, so it is weak evidence, but the difference is not
  dismissed. Quote about 200 to 300 purchases/s on this laptop, not 360.

**Pool comparison**, 2,000 VUs, three passes, interleaved rather than run in blocks:

| Config | Throughput | Median | Week 7 median |
|---|---|---:|---:|
| Direct, `MaxConns=10` | 283 / 203 / 156 | 203/s | 235/s |
| Direct, `MaxConns=50` | 195 / 273 / 218 | 218/s | 211/s |
| PgBouncer, `MaxConns=50` | 217 / 72 / 201 | 201/s | 210/s |

The conclusion stands: five times the connections buys nothing measurable. The
spread inside each row (72 to 283) is far larger than the gap between rows.

**This re-measurement found a real regression.** Its first attempt, on what was then
`main`, failed up to 7,014 of 8,000 requests with 500s. The disconnect fix had set a
15s limit on finishing a purchase and 5s on storing its outcome, both shorter than
the wait for a connection under load, and a 500ms Redis read limit failed under a
saturated host. No money was at risk, because the bookings stayed held for the
worker. Earlier regressions stopped at 2,000 VUs, so they never reached it. Those
limits are now 2 minutes, and Redis read timeouts are back at the library default.

