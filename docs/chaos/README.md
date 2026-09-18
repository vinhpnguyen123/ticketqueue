# Chaos scenarios

Three failures injected **while a load test is in flight**. Recovery under idle
conditions proves nothing: a system with no traffic has nothing to strand.

```bash
make chaos-1   # kill the API mid-load
make chaos-2   # payment provider unreachable (the money rule)
make chaos-3   # kill the worker, not the API
make chaos-all
```

Each scenario starts and kills the binaries itself, so stop any `make run` /
`make run-worker` / `make run-payments` first — it refuses to start otherwise.
Each writes a timestamped transcript here, exits non-zero on any failed check,
and prints a Grafana link with that run's exact window pinned.

All three run at **500 VUs**: half the *latency* ceiling from
[`scripts/k6/README.md`](../../scripts/k6/README.md), not half the error-rate
ceiling. At 4,000 VUs p(95) is already ~15s and a booking left `held` could be
the chaos or just congestion. At 500 the system is healthy, so every stranded
booking is the fault that was injected.

## Results, 2026-09-18

### CHAOS 1 — API killed mid-load

SIGKILL, not SIGTERM: a graceful shutdown drains in-flight requests, which is
the opposite of what is being simulated. The process must vanish between TX1
and TX2.

**270 bookings stranded by the crash, all 270 resolved 271s after it, zero money
discrepancies.** All nine `verify_run.sql` checks green.

Recovery is **batch-limited, not threshold-limited**. `booking.StuckAfter` is 90s
and `ReconcileInterval` is 30s, so nothing is even eligible for 90s — but
`ReconcileBatch` is 50 per cycle, so a backlog drains at ~100 bookings/minute
and 270 of them take ~4.5 minutes. Quote the rate, not a fixed number.

Two numbers in that transcript are **not** application failures: `http_req_failed`
near 70% and a wildly inflated iteration count. A refused connection returns in
microseconds, so while the API is dead 500 VUs spin far faster than they ever
could against a live server and the whole rate is dominated by the seconds the
process did not exist. The same load with no kill measures **0.00% failed over
6,988 iterations**.

### CHAOS 2 — payment provider unreachable (§3, the money rule)

The strongest correctness demo in the project, and only possible after the
Week-5 fix that made hold-expiration ask the provider before releasing anything.

**Provider down for 90s. 500 bookings held, 0 tickets released on a guess, all of
them resolved within 37s of recovery.** All nine checks green.

| | |
|---|---|
| Tickets available at outage start | 36,337 |
| Highest it reached during the outage | 36,337 — **it never rose** |
| Bookings frozen in `held` | 500 |
| `expiry_deferred_provider_unreachable` rows | 59 |
| Distinct bookings behind those rows | 59 — **exactly one each**, across ~18 hold-expiry cycles |

The held count pins at exactly the VU count because the purchase endpoint allows
one held booking per user: each user contributes one frozen booking and is then
turned away. Inventory froze and healed itself; nobody's paid seat was resold.

Two things this deliberately does **not** do, both for reasons in the script:

1. It never restarts `cmd/payment-service`. The sim's ledger is in memory (§17),
   so a restart makes the provider *forget* charges rather than *fail to answer*
   them — a different scenario, and one that would make hold-expiry correctly
   conclude `no_charge` for bookings that really were paid. It uses the §10
   chaos switch, which hangs without answering so the client's own 3s timeout
   fires.
2. It does not wait ten real minutes. `hold_expires_at` is `created_at + 10
   minutes`, so a 90s outage would produce **zero** deferral rows and the headline
   assertion would silently test nothing. The script backdates `hold_expires_at`
   instead. That is stage dressing for the clock only: the worker still has to
   ask the provider, still gets no answer, and still has to decide not to release.

### CHAOS 3 — worker killed, API left alive

`purchase_storm`, not `full_journey`: all five workers are one binary, so killing
it also kills the **promotion** worker. A journey test would leave every VU
stuck in the queue awaiting an admission that can never come — the load would
stop rather than continue, and the scenario would be measuring its own harness.

Assertions read Postgres, not Prometheus. The gauge sampler lives in the same
binary, so `bookings_held_current` freezes at a comforting stale value while the
worker is dead, and counters reset to zero on restart, making any before/after
delta meaningless.

**Worker dead 60s under load: unresolved bookings peaked at 347, the API never
stopped completing purchases (5,924 → 11,288 confirmed across the outage), the 80
left over drained 93s after restart, zero money discrepancies.** All nine checks
green.

**The run disproved its own premise, which is the interesting part.** Held
bookings do not climb monotonically while the worker is dead — they rose and fell
repeatedly, because the **webhook** resolves dropped-response bookings through the
API in about a second and needs no worker at all. The worker is the backstop for
what the webhook misses, not the first line of recovery. Two independent recovery
paths exist, and killing one leaves the other working.

An earlier version of this scenario failed on a readiness probe that timed out
at 3s while the API was demonstrably confirming ~100 purchases a second. "Kept
serving" is now asserted on work *completed* at every sample; the probe is kept,
with a realistic deadline, purely as a saturation signal.

## Still outstanding

**Grafana screenshots.** Each run prints a link with its `from`/`to` pinned to
the exact seconds it covered, which is more reproducible than a screenshot
instruction and still works next week — but capturing the images is manual and
has not been done. The two worth capturing:

- **Row 2, Latency breakdown** during CHAOS 1 — where time went while the API was dead.
- **Row 4, Money & recovery** during CHAOS 2 — `bookings_held_current` rising then
  draining with `admin_stuck_bookings_current{category="charged_not_delivered"}`
  flat at zero. That is the single most persuasive image this project produces.

Save them to `docs/screenshots/`.
