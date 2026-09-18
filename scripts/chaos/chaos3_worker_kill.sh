#!/usr/bin/env bash
#
# CHAOS 3 — kill the worker, not the API.
#
# What it proves: the worker is a RECOVERY mechanism, not a correctness
# dependency. With it dead, ambiguous bookings pile up and nothing resolves —
# but nothing is lost either, the API keeps serving, and the moment the worker
# comes back the whole backlog drains and the money still adds up.
#
# WHY purchase_storm AND NOT full_journey: all five workers are one binary
# (cmd/worker/main.go), so killing it also kills the PROMOTION worker. A journey
# test would then have every VU stuck in the queue waiting for an admission that
# can never come — the load would stop rather than continue, and the scenario
# would be measuring its own harness. The storm uses pre-minted purchase tokens
# and needs no promotion, so load really does keep flowing.
#
# WHY THE ASSERTIONS READ POSTGRES AND NOT PROMETHEUS: the gauge sampler lives
# in the same binary. With the worker dead, bookings_held_current and
# admin_stuck_bookings_current freeze at their last value and would report a
# comforting, stale number. And Prometheus counters reset to zero when a process
# restarts, so any before/after delta across this kill is meaningless.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

KILL_AT="${KILL_AT:-25}"
DOWN_FOR="${DOWN_FOR:-60}"
DURATION="${DURATION:-120s}"
TICKETS="${TICKETS:-40000}"
DROP_PCT="${DROP_PCT:-10}"   # share of VUs paying with tok_drops_response

chaos_begin "chaos3-worker-kill"
preflight
build_binaries
trap cleanup EXIT

step "Start the world"
start_proc payments
start_proc worker
wait_http "$PAYMENT_URL/health" || exit 1
seed_world "$TICKETS" "$CHAOS_VUS"
start_proc api
wait_http "$API_URL/v1/health/ready" 30 || exit 1

step "Load: $CHAOS_VUS VUs, $DURATION, ${DROP_PCT}% paying with tok_drops_response"
info "the dropped-response token is what produces ambiguous bookings continuously"
RUN_ID="chaos3-$(date +%s)" \
  k6 run --quiet --no-thresholds \
    -e VUS="$CHAOS_VUS" -e DURATION="$DURATION" -e DROP_PCT="$DROP_PCT" \
    "$REPO/scripts/k6/purchase_storm.js" > "$BUILD_DIR/k6.log" 2>&1 &
K6_PID=$!
PIDS+=("$K6_PID")

sleep "$KILL_AT"
HELD_AT_KILL="$(held_count)"

step "KILL the worker for ${DOWN_FOR}s"
kill_proc worker
info "held at the moment of the kill: $HELD_AT_KILL"

# "Kept serving" is asserted on work COMPLETED, not on a probe. A readiness
# probe under 500 VUs of sustained load competes with that load for the same
# connection pool, and an early version of this script failed the run because a
# 3s probe timed out while the API was demonstrably confirming ~100 purchases a
# second. Confirmed bookings rising across every sample is the unambiguous
# evidence; the probe is kept, with a realistic deadline, as a slowness signal.
HEALTH_TIMEOUTS=0
CONFIRMED_ALWAYS_RISING=1
HELD_PEAK="$(held_count)"
prev_confirmed="$(confirmed_count)"
outage_end=$((SECONDS + DOWN_FOR))
while (( SECONDS < outage_end )); do
  sleep 12
  h="$(held_count)"; c="$(confirmed_count)"
  (( h > HELD_PEAK )) && HELD_PEAK="$h"
  (( c > prev_confirmed )) || CONFIRMED_ALWAYS_RISING=0
  prev_confirmed="$c"
  curl -sf -o /dev/null --max-time 10 "$API_URL/v1/health/ready" || HEALTH_TIMEOUTS=$((HEALTH_TIMEOUTS + 1))
  printf '  %st+%-3ss  held=%-6s confirmed=%-6s%s\n' "$DIM" "$((SECONDS - KILL_AT))" "$h" "$c" "$OFF"
done

HELD_AT_END="$(held_count)"
(( HELD_AT_END > HELD_PEAK )) && HELD_PEAK="$HELD_AT_END"
CONFIRMED_DURING="$(confirmed_count)"

step "Assertions DURING the worker outage"
assert_eq "$CONFIRMED_ALWAYS_RISING" "1" "the API kept completing purchases at every sample, with no worker alive"
assert_ge "$CONFIRMED_DURING" "$((HELD_AT_KILL + 1))" "purchases still completed — the happy path needs no worker"
assert_ge "$HELD_PEAK" "$HELD_AT_KILL" "unresolved bookings accumulated while nothing could resolve them"
info "readiness probe timed out $HEALTH_TIMEOUTS time(s) — a saturation signal, not an outage"

# WHAT THIS RUN ACTUALLY SHOWED, and it is not what the scenario assumed:
# held does not climb monotonically while the worker is dead. It rose to
# $HELD_PEAK and fell again, because the WEBHOOK resolves dropped-response
# bookings through the API in about a second and needs no worker at all. The
# worker is the backstop for the cases the webhook misses, not the first line.
# That is a stronger result than "the backlog grows": two independent recovery
# paths exist, and killing one leaves the other working.
# Nothing may be lost while the only thing that could repair it is dead.
assert_eq "$(pg1 "SELECT count(*) FROM booking b WHERE b.status='held' AND NOT EXISTS (SELECT 1 FROM tickets t WHERE t.booking_id=b.id);")" \
  "0" "every held booking still owns its tickets — nothing was lost"

step "Restart the worker"
RECOVERY_T0=$SECONDS   # the drain is measured from the restart, not from the
                       # end of the load run
start_proc worker
wait_http "http://localhost:${WORKER_METRICS_PORT}/metrics" 30 || true
wait "$K6_PID" 2>/dev/null

step "Drain"
info "backlog of $HELD_AT_END held bookings (peak during the outage: $HELD_PEAK)"
info "StuckAfter=90s, then ReconcileBatch=50 per 30s cycle"
if wait_until_settled 300; then
  pass "backlog of $HELD_AT_END drained ${RECOVERY_SECONDS}s after the worker came back"
fi

step "Assertions AFTER restart"
assert_eq "$(held_count)" "0" "the backlog drained completely"
assert_eq "$(pg1 "SELECT count(*) FROM booking WHERE status='held' AND created_at < NOW() - INTERVAL '5 minutes';")" \
  "0" "zero bookings held longer than 5 minutes"

step "Money correctness (scripts/verify_run.sql)"
if bash "$REPO/scripts/verify_run.sh" > "$BUILD_DIR/verify.log" 2>&1; then
  pass "all nine checks green"
  grep -E '^ (PASS|FAIL)' "$BUILD_DIR/verify.log" | sed 's/^/    /'
else
  fail "verify_run.sh reported a failure"
  cat "$BUILD_DIR/verify.log"
fi

step "RESULT"
say "  ${BOLD}Worker dead ${DOWN_FOR}s under load: unresolved bookings peaked at ${HELD_PEAK}, the API never stopped completing purchases, the ${HELD_AT_END} left over drained ${RECOVERY_SECONDS}s after restart, zero money discrepancies.${OFF}"

chaos_end "The worker is a recovery mechanism, not a correctness dependency: the data was durable without it."
