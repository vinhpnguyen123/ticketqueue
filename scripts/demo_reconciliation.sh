#!/usr/bin/env bash
#
# End-to-end demo of the headline reliability scenario: a payment whose
# response is lost on the network. The server never guesses whether money
# moved; the reconciliation worker asks the provider and self-heals the
# booking with no user retry and no manual intervention.
#
# Requires the full local stack:
#   make up && make migrate-up && make seed
#   make run-payments    # simulated provider
#   make run             # API
#   make run-worker      # promotion + reconciliation workers
#
# Usage: bash scripts/demo_reconciliation.sh [--happy]
#   --happy also runs the ordinary success path for contrast.

set -euo pipefail

API_URL="${API_URL:-http://localhost:8081}"
PAYMENT_URL="${PAYMENT_URL:-http://localhost:8082}"
PAYMENT_API_KEY="${PAYMENT_API_KEY:-ps_test_localdev}"
PASSWORD="demo1234"
QUANTITY=2

# Recovery window: the worker only considers bookings older than 90s and runs
# every 30s, so a real recovery lands at 90-120s. 150s leaves headroom.
RECOVERY_TIMEOUT=150
ADMISSION_TIMEOUT=60

RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; BOLD=$'\033[1m'; DIM=$'\033[2m'; OFF=$'\033[0m'

failures=0

pass() { printf '%s  PASS%s  %s\n' "$GREEN" "$OFF" "$1"; }
fail() { printf '%s  FAIL%s  %s\n' "$RED" "$OFF" "$1"; failures=$((failures + 1)); }
info() { printf '%s        %s%s\n' "$DIM" "$1" "$OFF"; }
say()  { printf '\n%s%s%s\n' "$BOLD" "$1" "$OFF"; }
note() { printf '%s        %s%s\n' "$YELLOW" "$1" "$OFF"; }

die() { printf '\n%sFATAL%s %s\n' "$RED" "$OFF" "$1" >&2; exit 1; }

# assert_eq <actual> <expected> <description>
assert_eq() {
  if [[ "$1" == "$2" ]]; then pass "$3"; else fail "$3 (got '$1', want '$2')"; fi
}

psql_one() {
  docker compose exec -T postgres psql -U ticketqueue -d ticketqueue -t -A -c "$1" | tr -d '\r'
}

# api <method> <path> <jwt-or-empty> <body-or-empty> [extra curl args...]
# Sets API_STATUS and API_BODY.
api() {
  local method=$1 path=$2 jwt=$3 body=$4; shift 4
  local args=(-s -o /tmp/tq_demo_body -w '%{http_code}' -X "$method" "$API_URL$path")
  if [[ -n "$jwt" ]]; then args+=(-H "Authorization: Bearer $jwt"); fi
  if [[ -n "$body" ]]; then args+=(-H 'Content-Type: application/json' -d "$body"); fi
  API_STATUS=$(curl "${args[@]}" "$@")
  API_BODY=$(cat /tmp/tq_demo_body)
}

preflight() {
  command -v jq >/dev/null || die "jq is required"
  docker compose ps >/dev/null 2>&1 || die "docker compose is not running (make up)"
  curl -sf "$API_URL/v1/health" >/dev/null || die "API not reachable at $API_URL (make run). Override with API_URL=..."
  curl -sf "$PAYMENT_URL/health" >/dev/null || die "payment service not reachable at $PAYMENT_URL (make run-payments). Override with PAYMENT_URL=..."
  [[ -n "$(psql_one "SELECT 1")" ]] || die "cannot query postgres via docker compose"
}

# register_and_admit <label> sets JWT / PURCHASE_TOKEN / TIER_ID / EVENT_ID.
# It sets globals rather than echoing, because a die() inside a command
# substitution would only exit the subshell and the demo would carry on with
# empty values.
register_and_admit() {
  local label=$1
  local email="$label-$(date +%s)-$RANDOM@demo.test"

  api POST /v1/auth/register "" "{\"email\":\"$email\",\"password\":\"$PASSWORD\"}"
  if [[ "$API_STATUS" == "429" ]]; then
    die "registration rate-limited (3 per window per IP). Wait a minute and re-run."
  fi
  [[ "$API_STATUS" == "201" ]] || die "register failed ($API_STATUS): $API_BODY"
  JWT=$(jq -r '.data.token' <<<"$API_BODY")

  # The seeded on_sale event, with its first tier.
  local event_id tier_id
  event_id=$(psql_one "SELECT id FROM events WHERE status='on_sale' ORDER BY created_at LIMIT 1;")
  [[ -n "$event_id" ]] || die "no on_sale event found — run 'make seed'"
  tier_id=$(psql_one "SELECT id FROM ticket_tier WHERE event_id='$event_id' ORDER BY price_cents DESC LIMIT 1;")

  api POST "/v1/events/$event_id/queue/join" "$JWT" ""
  [[ "$API_STATUS" == "201" || "$API_STATUS" == "200" ]] || die "queue join failed ($API_STATUS): $API_BODY"

  # The promotion worker admits in FIFO batches every 5s.
  local waited=0 status purchase_token=""
  while (( waited < ADMISSION_TIMEOUT )); do
    api GET "/v1/events/$event_id/queue/position" "$JWT" ""
    status=$(jq -r '.data.status // empty' <<<"$API_BODY")
    if [[ "$status" == "admitted" ]]; then
      purchase_token=$(jq -r '.data.purchase_token' <<<"$API_BODY")
      break
    fi
    sleep 2; waited=$((waited + 2))
  done
  [[ -n "$purchase_token" ]] || die "not admitted within ${ADMISSION_TIMEOUT}s — is 'make run-worker' running?"

  PURCHASE_TOKEN=$purchase_token
  TIER_ID=$tier_id
  EVENT_ID=$event_id
}

# buy <payment_token> <idempotency_key> — uses the globals set by register_and_admit.
buy() {
  api POST /v1/purchases "$JWT" \
    "{\"event_id\":\"$EVENT_ID\",\"tier_id\":\"$TIER_ID\",\"quantity\":$QUANTITY,\"payment_token\":\"$1\",\"purchase_token\":\"$PURCHASE_TOKEN\"}" \
    -H "Idempotency-Key: $2"
}

# ─────────────────────────────────────────────────────────────────────────
# Scenario: Carl's dropped payment
# ─────────────────────────────────────────────────────────────────────────
run_carl() {
  say "SCENARIO — Carl's payment response is lost on the network"

  register_and_admit carl
  local idem="demo-carl-$(date +%s)-$RANDOM"
  info "idempotency key: $idem"

  # 1. The purchase: the provider charges the card, then the response is
  #    dropped mid-flight (tok_drops_response hijacks the connection).
  buy tok_drops_response "$idem"
  assert_eq "$API_STATUS" "504" "purchase returns 504 (we do not know if money moved)"
  assert_eq "$(jq -r '.error.type' <<<"$API_BODY")" "payment_processing_unclear" "error type is payment_processing_unclear"

  local booking_id; booking_id=$(jq -r '.error.details.booking_id' <<<"$API_BODY")
  [[ "$booking_id" != "null" && -n "$booking_id" ]] || die "no booking_id in the 504 response"

  # 2. Our side: the booking is parked, not guessed at.
  assert_eq "$(psql_one "SELECT status FROM booking WHERE id='$booking_id';")" "held" \
    "booking is HELD, not failed and not confirmed"
  assert_eq "$(psql_one "SELECT status FROM payments WHERE booking_id='$booking_id';")" "unknown" \
    "payment is recorded as 'unknown' (durable evidence of the attempt)"
  note "Booking $booking_id is HELD — the server does not know if money moved."

  # 3. The provider's side: the money DID move. This is the whole point.
  local stored_key charge_status
  stored_key=$(psql_one "SELECT idempotency_key FROM booking WHERE id='$booking_id';")
  charge_status=$(curl -s -H "Authorization: Bearer $PAYMENT_API_KEY" \
    "$PAYMENT_URL/v1/charges/by-idempotency-key/$stored_key" | jq -r '.status')
  assert_eq "$charge_status" "succeeded" "provider has a SUCCEEDED charge under the same idempotency key"
  note "But the provider DID charge. The response was lost on the network."

  # 4. Nobody retries. Nobody intervenes. The worker asks the provider.
  say "Waiting for the reconciliation worker (no user retry, no manual action)…"
  local started elapsed=0 status=""
  started=$(date +%s)
  while (( elapsed < RECOVERY_TIMEOUT )); do
    status=$(psql_one "SELECT status FROM booking WHERE id='$booking_id';")
    if [[ "$status" == "confirmed" ]]; then break; fi
    printf '\r%s        %ss elapsed — booking still %s%s' "$DIM" "$elapsed" "$status" "$OFF"
    sleep 5
    elapsed=$(( $(date +%s) - started ))
  done
  printf '\r%*s\r' 60 ''

  if [[ "$status" == "confirmed" ]]; then
    pass "booking self-healed to CONFIRMED after ${elapsed}s"
  else
    fail "booking did not recover within ${RECOVERY_TIMEOUT}s (still '$status') — is 'make run-worker' running?"
  fi
  RECOVERY_SECONDS=$elapsed

  # 5. The money invariants.
  assert_eq "$(psql_one "SELECT COUNT(*) FROM tickets WHERE booking_id='$booking_id' AND status='sold';")" \
    "$QUANTITY" "exactly $QUANTITY tickets are SOLD"
  assert_eq "$(psql_one "SELECT COUNT(*) FROM payments WHERE booking_id='$booking_id' AND status='succeeded';")" \
    "1" "exactly one SUCCEEDED payment row (charged once, never twice)"

  # 6. The trail: every state change, who caused it, in order.
  say "Audit timeline for $booking_id"
  docker compose exec -T postgres psql -U ticketqueue -d ticketqueue -c \
    "SELECT created_at, action, actor_type FROM audit_log
     WHERE entity_type='booking' AND entity_id='$booking_id' ORDER BY created_at;"

  CARL_BOOKING=$booking_id
}

# ─────────────────────────────────────────────────────────────────────────
# Contrast: the ordinary path, where nothing goes wrong
# ─────────────────────────────────────────────────────────────────────────
run_happy() {
  say "CONTRAST — the same purchase when the network behaves"

  register_and_admit happy
  local idem="demo-happy-$(date +%s)-$RANDOM"

  local started; started=$(date +%s)
  buy tok_visa "$idem"
  HAPPY_SECONDS=$(( $(date +%s) - started ))

  assert_eq "$API_STATUS" "201" "purchase returns 201 immediately"
  assert_eq "$(jq -r '.data.status' <<<"$API_BODY")" "confirmed" "booking is confirmed in the response itself"

  HAPPY_BOOKING=$(jq -r '.data.booking_id' <<<"$API_BODY")
  assert_eq "$(psql_one "SELECT COUNT(*) FROM tickets WHERE booking_id='$HAPPY_BOOKING' AND status='sold';")" \
    "$QUANTITY" "exactly $QUANTITY tickets are SOLD"
}

main() {
  local with_happy=false
  if [[ "${1:-}" == "--happy" ]]; then with_happy=true; fi

  preflight
  run_carl
  if $with_happy; then run_happy; fi

  say "SUMMARY"
  printf '  dropped response  booking %s  recovered in %ss  (worker, no user action)\n' \
    "${CARL_BOOKING:0:8}" "${RECOVERY_SECONDS:-?}"
  if $with_happy; then
    printf '  clean network     booking %s  confirmed in %ss  (synchronous)\n' \
      "${HAPPY_BOOKING:0:8}" "${HAPPY_SECONDS:-?}"
  fi

  if (( failures == 0 )); then
    printf '\n%sALL ASSERTIONS PASSED%s\n' "$GREEN$BOLD" "$OFF"
  else
    printf '\n%s%d ASSERTION(S) FAILED%s\n' "$RED$BOLD" "$failures" "$OFF"
    exit 1
  fi
}

main "$@"
