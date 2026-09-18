#!/usr/bin/env bash
#
# CHAOS 1 — kill the API mid-load.
#
# What it proves: a process dying between TX1 and TX2 strands bookings in
# 'held' with tickets locked to them, and NOTHING is lost — the reconciliation
# worker asks the provider what really happened and finishes every one of them,
# without the API that created them ever being consulted.
#
# SIGKILL, not SIGTERM: a graceful shutdown drains in-flight requests, which is
# precisely the case we do not want. We want the process to vanish between the
# two transactions.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

KILL_AT="${KILL_AT:-30}"     # seconds into the run
DOWN_FOR="${DOWN_FOR:-5}"    # seconds the API stays dead
DURATION="${DURATION:-90s}"
TICKETS="${TICKETS:-40000}"  # bigger than the run can sell, so load stays real

chaos_begin "chaos1-api-kill"
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
info "api up on $API_PORT"

step "Load: $CHAOS_VUS VUs, $DURATION, sustained"
RUN_ID="chaos1-$(date +%s)" \
  k6 run --quiet --no-thresholds \
    -e VUS="$CHAOS_VUS" -e DURATION="$DURATION" \
    "$REPO/scripts/k6/purchase_storm.js" > "$BUILD_DIR/k6.log" 2>&1 &
K6_PID=$!
PIDS+=("$K6_PID")

sleep "$KILL_AT"
CONFIRMED_BEFORE="$(confirmed_count)"
RECON_BEFORE="$(worker_metric reconciliation_resolution_total 'source="reconciliation"')"

step "KILL the API ${KILL_AT}s in"
RECOVERY_T0=$SECONDS   # every recovery number below is measured from HERE
kill_proc api

# Count AFTER the kill: with the API dead nothing new can reach 'held', so
# everything still held is stranded by the crash rather than merely in flight.
sleep 1
STRANDED="$(held_count)"
say "  ${YEL}${STRANDED} bookings stranded in 'held' with tickets locked to them${OFF}"
info "confirmed before the crash: $CONFIRMED_BEFORE"

sleep "$DOWN_FOR"
step "Restart the API"
start_proc api
wait_http "$API_URL/v1/health/ready" 30 || exit 1

info "waiting for the load to finish"
wait "$K6_PID" 2>/dev/null
grep -E "iterations\.\.\.|http_req_failed\.|purchase_outcomes\.\." "$BUILD_DIR/k6.log" | sed 's/^/  /'

# HOW TO READ THOSE TWO NUMBERS — neither is an application failure:
#
# http_req_failed looks enormous (~70%). A refused connection comes back in
# microseconds, so while the API is dead 500 VUs spin far faster than they ever
# could against a live server: a ~5s outage contributes ~100k instant failures
# on top of the ~200/s of real work. The rate is dominated by the seconds the
# process did not exist, which is what the scenario asked for. The same run with
# no kill measures 0.00% failed over 6,988 iterations.
#
# iterations is inflated by the same effect and is not comparable to a clean run.

# NOTE, worth reading in the output: every stranded user now holds a booking,
# and the purchase endpoint allows one held booking per user, so for the rest of
# the run those VUs get 410 active_booking_exists. Load does not stop, it turns
# into rejections. That is the duplicate-charge guard working, not a failure.

step "Recovery"
info "booking.StuckAfter=90s, ReconcileInterval=30s — nothing is even eligible"
info "for 90s, so recovery is 90-120s by construction, not 'two cycles'"
# ...and that floor only applies to a SMALL backlog. ReconcileBatch is 50 per
# 30s cycle, so a real crash recovers at 100 bookings/minute and the wall-clock
# time is set by the backlog, not by the threshold. Measured: 365 stranded took
# 266s, which is 8 cycles. Worth knowing before quoting a recovery number.
info "ReconcileBatch=50/cycle → a backlog drains at ~100 bookings/minute"
if wait_until_settled 300; then
  pass "all $STRANDED stranded bookings resolved ${RECOVERY_SECONDS}s after the kill"
else
  say "  ${RED}bookings still held after ${RECOVERY_SECONDS}s${OFF}"
fi

RECON_AFTER="$(worker_metric reconciliation_resolution_total 'source="reconciliation"')"
RESOLVED=$((RECON_AFTER - RECON_BEFORE))

step "Assertions"
assert_eq "$(held_count)" "0" "zero bookings left 'held' on this tier"
assert_eq "$(pg1 "SELECT count(*) FROM booking WHERE status='held' AND created_at < NOW() - INTERVAL '5 minutes';")" \
  "0" "zero bookings held longer than 5 minutes (verify_run check 8)"

# "Roughly": the sync-retry path and hold-expiry can also resolve a booking, and
# a stranded booking whose charge never happened resolves as no_charge. What
# must hold is that reconciliation did the bulk of this work.
if (( STRANDED > 0 )); then
  assert_ge "$RESOLVED" "$((STRANDED / 2))" "reconciliation_resolution_total{source=reconciliation} rose with the stranded count"
else
  fail "no bookings were stranded — the kill did not land mid-purchase, so this run proves nothing"
fi

step "Money correctness (scripts/verify_run.sql)"
if bash "$REPO/scripts/verify_run.sh" > "$BUILD_DIR/verify.log" 2>&1; then
  pass "all nine checks green"
  grep -E '^ (PASS|FAIL)' "$BUILD_DIR/verify.log" | sed 's/^/    /'
else
  fail "verify_run.sh reported a failure"
  cat "$BUILD_DIR/verify.log"
fi

step "RESULT"
say "  ${BOLD}${STRANDED} bookings stranded by the crash, all ${STRANDED} resolved ${RECOVERY_SECONDS}s after it, zero money discrepancies.${OFF}"

chaos_end "Row 2 (Latency breakdown) shows where time went while the API was dead."
