#!/usr/bin/env bash
#
# CHAOS 2 — the payment provider goes unreachable mid-load (§3, the money rule).
#
# The strongest correctness demo in the project, and the one that only became
# possible after the Week-5 fix: hold-expiration now ASKS the provider before
# releasing anything, and defers when it cannot.
#
# The claim: during a provider outage the system freezes inventory rather than
# guess. Tickets stay locked to bookings that might have been charged. Frozen
# seats cost a little money for a little while and heal themselves; releasing a
# paid customer's seat to someone else is a dispute that never fully heals.
#
# TWO THINGS THIS DELIBERATELY DOES NOT DO:
#
#  1. It does not restart cmd/payment-service. The sim's ledger is in memory
#     (§17), so a restart makes the provider FORGET charges rather than fail to
#     answer them. That is a different scenario — "the provider lost the
#     charge" — and it would make hold-expiry correctly conclude no_charge for
#     bookings that really were paid. It uses the §10 chaos switch, which hangs
#     without answering, so the client's own 3s timeout is what fires.
#
#  2. It does not wait 10 real minutes for holds to expire on their own.
#     hold_expires_at is created_at + 10 minutes, so a 90s outage would produce
#     ZERO 'expiry_deferred_provider_unreachable' rows and the headline
#     assertion would silently test nothing. The script backdates
#     hold_expires_at for this tier's bookings instead, which is stage dressing
#     for the clock and nothing else: the worker still has to ask the provider,
#     still has to be refused, and still has to decide not to release.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

OUTAGE_AT="${OUTAGE_AT:-25}"
OUTAGE_FOR="${OUTAGE_FOR:-90}"
DURATION="${DURATION:-150s}"
TICKETS="${TICKETS:-40000}"

chaos_begin "chaos2-provider-outage"
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

step "Load: $CHAOS_VUS VUs, $DURATION, sustained"
RUN_ID="chaos2-$(date +%s)" \
  k6 run --quiet --no-thresholds \
    -e VUS="$CHAOS_VUS" -e DURATION="$DURATION" \
    "$REPO/scripts/k6/purchase_storm.js" > "$BUILD_DIR/k6.log" 2>&1 &
K6_PID=$!
PIDS+=("$K6_PID")

sleep "$OUTAGE_AT"

AVAILABLE_AT_OUTAGE="$(available_count)"
CONFIRMED_AT_OUTAGE="$(confirmed_count)"

step "Provider UNREACHABLE for ${OUTAGE_FOR}s"
curl -sf -X POST "$PAYMENT_URL/chaos/unreachable" \
  -H "Authorization: Bearer $PAYMENT_API_KEY" -H 'Content-Type: application/json' \
  -d "{\"seconds\":$OUTAGE_FOR}" | sed 's/^/  /'
say ""
info "tickets available at outage start: $AVAILABLE_AT_OUTAGE  (this number must never RISE)"

# Backdate so hold-expiry engages inside the outage instead of in ten minutes.
backdate() {
  pg -q -c "UPDATE booking SET hold_expires_at = NOW() - INTERVAL '30 seconds'
            WHERE status='held' AND ticket_tier_id='$TIER_ID';" >/dev/null 2>&1
}

AVAILABLE_PEAK="$AVAILABLE_AT_OUTAGE"
HELD_PEAK=0
outage_end=$((SECONDS + OUTAGE_FOR))
while (( SECONDS < outage_end )); do
  backdate
  sleep 12
  h="$(held_count)"; a="$(available_count)"; d="$(worker_metric reconciliation_resolution_total 'outcome="deferred"')"
  (( a > AVAILABLE_PEAK )) && AVAILABLE_PEAK="$a"
  (( h > HELD_PEAK )) && HELD_PEAK="$h"
  printf '  %st+%-3ss  held=%-6s available=%-6s deferred_total=%-5s%s\n' \
    "$DIM" "$((SECONDS - OUTAGE_AT))" "$h" "$a" "$d" "$OFF"
done

HELD_AT_END="$(held_count)"
(( HELD_AT_END > HELD_PEAK )) && HELD_PEAK="$HELD_AT_END"
DEFERRED_DURING="$(worker_metric reconciliation_resolution_total 'outcome="deferred"')"
DEFERRAL_ROWS="$(deferral_rows)"
DEFERRAL_BOOKINGS="$(deferral_bookings)"

step "Assertions DURING the outage"
assert_ge "$HELD_PEAK" "1" "bookings piled up in 'held' while the provider was away"
assert_ge "$DEFERRED_DURING" "1" "reconciliation_resolution_total{outcome=deferred} incremented"
assert_le "$AVAILABLE_PEAK" "$AVAILABLE_AT_OUTAGE" \
  "ZERO tickets returned to 'available' on a guess"
assert_ge "$DEFERRAL_BOOKINGS" "1" "hold-expiry deferred at least one booking"
# The three-cadence rule: hold-expiry runs every 5s, so a 90s outage is ~18
# cycles. One row per booking, not eighteen.
assert_eq "$DEFERRAL_ROWS" "$DEFERRAL_BOOKINGS" \
  "exactly one 'expiry_deferred_provider_unreachable' row per affected booking"

step "Provider BACK"
curl -sf -X POST "$PAYMENT_URL/chaos/unreachable" \
  -H "Authorization: Bearer $PAYMENT_API_KEY" -H 'Content-Type: application/json' \
  -d '{"seconds":0}' | sed 's/^/  /'
RECOVERY_T0=$SECONDS   # recovery is measured from the provider answering again,
                       # not from whenever the load generator happens to stop
say ""
wait "$K6_PID" 2>/dev/null

step "Recovery"
if wait_until_settled 300; then
  pass "every stranded booking resolved in ${RECOVERY_SECONDS}s"
fi

step "Assertions AFTER recovery"
assert_eq "$(held_count)" "0" "zero bookings left 'held'"
assert_eq "$DEFERRAL_ROWS" "$(deferral_rows)" \
  "no further deferral rows were written once the provider answered"

step "Money correctness (scripts/verify_run.sql)"
if bash "$REPO/scripts/verify_run.sh" > "$BUILD_DIR/verify.log" 2>&1; then
  pass "all nine checks green"
  grep -E '^ (PASS|FAIL)' "$BUILD_DIR/verify.log" | sed 's/^/    /'
else
  fail "verify_run.sh reported a failure"
  cat "$BUILD_DIR/verify.log"
fi

step "RESULT"
say "  ${BOLD}Provider down for ${OUTAGE_FOR}s. ${HELD_PEAK} bookings held, 0 tickets released on a guess, all of them resolved within ${RECOVERY_SECONDS}s of recovery.${OFF}"

chaos_end "Row 4 (Money & recovery): bookings_held_current rising then draining, with admin_stuck_bookings_current{category=charged_not_delivered} flat at 0."
