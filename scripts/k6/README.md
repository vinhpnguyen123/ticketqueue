# Load testing

Two scenarios, because the system is queue-gated and one number cannot describe both
things that matter.

## Why two scenarios

`max_concurrent_windows` caps how many users hold a purchase token at once. That is the
whole point of the queue: a realistic end-to-end journey **cannot** produce 10,000
simultaneous purchase attempts, because the queue throttles them by design.

So a journey test measures the queue, and a contention test measures the claim. They are
different claims and must be reported separately.

| | Scenario A — `purchase_storm.js` | Scenario B — `full_journey.js` |
|---|---|---|
| What it does | every VU hits `POST /v1/purchases` at the same instant | join queue → poll → purchase |
| Tokens | minted by `cmd/loadseed` | earned from the promotion worker |
| What it proves | the three-layer defense under maximum contention | queue throughput and admission pacing |
| Backs the claim | "N concurrent buyers, M tickets, exactly M sold" | "the full stack works as a user experiences it" |

The minted tokens in Scenario A are a **load-test affordance, not a production path**. They
exist so the queue does not throttle the thing under test.

## Why the setup is a Go binary

`cmd/loadseed` creates the world instead of `lib/setup.js`, for two reasons that JavaScript
cannot work around:

1. **The API rate-limits registration to 3/minute/IP and login to 5/minute/IP.** Creating
   even a thousand identities over HTTP from one machine would take hours. The seeder
   inserts them directly.
2. **Purchase tokens are signed by `auth.GeneratePurchaseToken`** — the same function the
   promotion worker uses. Reimplementing HS256 signing in JS would create a second
   definition of a security token that can silently drift from the real one.

A consequence worth stating: **Scenario B does not measure register/login.** It starts at
"join the queue" with a pre-issued session JWT. That is honest — those endpoints are
rate-limited by design and cannot be load-tested from a single IP.

## Running

```bash
make up && make migrate-up          # infrastructure
make run-payments                   # terminal 1
make run                            # terminal 2
make run-worker                     # terminal 3   (required for Scenario B)

make load-seed TICKETS=100 USERS=2000     # fresh event + tier + identities
make load-storm VUS=2000                  # Scenario A
make verify-run                           # ALWAYS
```

`make load-seed` creates a **new event and tier every time**. Counts only mean something
against a tier nobody has touched.

## Reading the output

**The latency number is `purchase_completed_duration` p(95).** Not `http_req_duration`.

In a contention run ~99% of requests are rejected by the Redis fast path in a couple of
milliseconds. A p(95) over all requests is therefore a p(95) **of rejections** — fast,
flattering, meaningless. The trends are kept apart on purpose:

| Metric | What it holds |
|---|---|
| `purchase_completed_duration` | HTTP 201 only — **the headline number** |
| `purchase_rejected_duration` | HTTP 409 sold_out — Layer 1 doing its job |
| `purchase_ambiguous_duration` | HTTP 504 — the system refusing to guess |
| `purchase_outcomes` | counter, tagged by outcome |

`http_req_failed` is scoped to 5xx via `setResponseCallback`. Without that, a healthy
sell-out reports as a 50% failure rate, because k6 counts every 409 as a failure.

## The sample-size problem

With a 100-ticket tier there are only 100 successful purchases per run, so p(95) sits on
the 95th of 100 samples. It wobbles between runs and is not a stable statistic.

Run two configurations and report them as **separate claims**:

```bash
make load-seed TICKETS=100  USERS=2000 && make load-storm VUS=2000   # oversell proof, 3x
make load-seed TICKETS=2000 USERS=2000 && make load-storm VUS=2000   # latency proof
```

- **(a) oversell proof** — 100-ticket tier, three consecutive runs, exactly 100 sold every time.
- **(b) latency proof** — 2,000-ticket tier, ~2,000 completed samples, stable p(95).

**Never quote a p(95) taken from the 100-ticket run.**

Results for both, with raw output, live in [`docs/load-test-results.md`](../../docs/load-test-results.md) §2 and §3.

## Calibration — find the machine's ceiling, not the app's

```bash
make load-seed TICKETS=20000 USERS=10000 && make load-calibrate
```

Ramps 500 → 1k → 2k → 5k → 10k, 30s per step. At each step watch completed p(95), the
error rate, `rate(db_pool_acquire_wait_seconds_total)` in Grafana, and whether k6 itself
is saturating (CPU, `ulimit -n`).

**Honest reporting rule: claim the VU count actually sustained with a clean error rate.**
If this laptop tops out at 4k, the number is 4k. It can be raised after the Week 7 AWS
run, not before.

### Measured ceiling on this machine (2026-09-18)

MacBook, 8 cores / 8 GB, running k6 **and** all four processes **and** Postgres, Redis,
Prometheus and Grafana. Client and server compete for the same cores, so a dedicated load
generator would go higher. Every step below sold a ticket on every request (tier sized at
2x the VU count), so this measures the full purchase path, not the fast-path rejection.

Produced by a fixed-VU `purchase_storm` run per step with a re-seed between them, **not** by
`calibrate.js`: k6 prints one aggregate summary for a whole ramp, so a ramping run cannot
tell you what happened at 2k versus 8k — which is the only thing calibration is for.

```bash
for V in 500 1000 2000 4000 8000; do
  make load-seed TICKETS=$((V*2)) USERS=$V && make load-storm VUS=$V
done
```

| VUs | completed p(95) | throughput | 5xx | connection errors |
|---:|---:|---:|---:|---:|
| 500 | 1.31 s | 360/s | 0 | 0 |
| 1,000 | 2.55 s | 373/s | 0 | 0 |
| 2,000 | 5.82 s | 322/s | 0 | 0 |
| 4,000 | 15.81 s | 244/s | 0 | 0 |
| 8,000 | 36.45 s | 199/s | 0 | 0 |
| 10,000 | 17.89 s | 444/s | 0 | **1,649 (16%)** |

**Throughput plateaus at ~350 completed purchases/s and never rises.** Past that, extra VUs
buy queue time and nothing else — p(95) tracks Little's law almost exactly (2,000 / 350 ≈
5.7 s). So the honest claims are:

- **~500 VUs** is the load this machine serves with a p(95) under the 2 s threshold.
- **8,000 VUs** is the load it survives with a clean error rate — slow, but zero 5xx and
  zero dropped connections.
- **~350 purchases/s** is the number that actually describes the machine.

(The 10,000 row looks *faster* only because 16% of its requests never arrived and the tier
was 100 tickets, so most of what did arrive was a fast-path rejection. It is not comparable
to the rows above it.)

**What saturated first: the connection pool, then the kernel — never the app.**
`pgxpool MaxConns = 10`, and `db_pool_in_use` sits pinned at 10 from 1,000 VUs upward
while `payment_call` stays at 5 ms; the 2,000-VU run accumulated ~15,000 s of
`db_pool_acquire_wait_seconds_total`. That is the application's ceiling here, and it is a
configuration value. Second, at 10,000 VUs, `kern.ipc.somaxconn` is **128** on macOS: with
an app taking ~15 s per request the accept queue overflows and the kernel drops connections
before the process sees them. Proof: k6 sent 10,000 requests, the API logged exactly
**8,351** (100 x 201 + 8,251 x 409) — and the 1,649 missing ones equal k6's connection-error
count exactly. The API process held only 148 file descriptors at peak, so fd limits were
never the constraint. Full breakdown: `docs/load-test-results.md` §4.

**And it still sold exactly 100 of 100 tickets.** Correctness did not depend on the machine
coping: `make verify-run` passed all nine checks across 18,200 sold tickets and $9.1M of
charges accumulated over the whole session.

Raise `somaxconn` before quoting a higher VU number, or better, wait for the Week 7 AWS run
where the load generator is not fighting the server for cores.

## Correctness verification

```bash
make verify-run
```

Nine checks; exits non-zero if any fails. A load-test number is worthless without them —
"10k concurrent" only matters if the tickets and the money still add up afterwards.

Checks 1–8 are SQL (`scripts/verify_run.sql`). Check 9 compares Redis to Postgres and needs
`redis-cli`, so it lives in the shell wrapper. A **missing** Redis key is reported as SKIP,
not FAIL: the counter is lazily initialised, and only a key that *disagrees* is drift —
the same distinction the drift checker makes.

Run the worker for two drift cycles before trusting check 9, or in-flight work reads as drift.

## Proving the verification suite can fail

A verification suite that has never failed is not evidence. Two experiments, both run:

### 1. Corrupt the data directly

```bash
make sabotage && make verify-run     # expect failures
make load-seed                       # back to clean
```

**Result:** 6 of 9 checks failed. Two invariants were broken; the extra sold ticket cascaded
into checks 1, 3, 6 and 7a as well. The suite catches what it claims to catch.

### 2. Remove Layer 2 and try to force an oversell

The obvious sabotage — inflating the Redis counter above real capacity — **does not work**,
and finding that out is the most interesting result here.

| What was broken | Result |
|---|---|
| Layer 2's short-claim check removed | 100 sold, 0 errors, **all checks pass** |
| Layer 2 removed **and** Redis counter set to 300 against a 100-ticket tier | 100 sold, 200 × HTTP 500, **all checks pass, zero oversells** |

The 200 failures were all `layer-3 guard: assigned 0 of 1`. With Layer 2 gone and Layer 1
actively lying, **Layer 3 still made overselling structurally impossible** — it turned a
broken system into a loud failure instead of a silent one.

The layers are not redundant decoration. To actually produce an oversell you would have to
dismantle the row-level claim itself, not just the checks around it.

(The Layer-2 edit was made locally, run, and reverted with `git checkout`. Nothing in the
repo is modified by these experiments except the DEV database, which `make load-seed`
recreates.)
