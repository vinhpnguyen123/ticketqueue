#!/usr/bin/env bash
#
# Shared harness for the chaos scenarios.
#
# WHY THIS OWNS THE PROCESSES: every scenario kills one of them and has to bring
# it back. It cannot do that to a binary somebody else started in another
# terminal, so it starts all three itself and refuses to run if the ports are
# already taken. That also makes a run reproducible: same build, same flags,
# every time.
#
# WHY LOAD IS ALWAYS IN FLIGHT: recovery under idle conditions proves nothing.
# A system with no traffic has nothing to strand.

set -uo pipefail

RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; BOLD=$'\033[1m'; DIM=$'\033[2m'; OFF=$'\033[0m'

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
export DATABASE_URL="${DATABASE_URL:-postgres://ticketqueue:localdev@localhost:5434/ticketqueue?sslmode=disable}"
API_PORT="${API_PORT:-8081}"
PAYMENT_PORT="${PAYMENT_PORT:-8082}"
WORKER_METRICS_PORT="${WORKER_METRICS_PORT:-9091}"
API_URL="http://localhost:${API_PORT}"
PAYMENT_URL="http://localhost:${PAYMENT_PORT}"
PAYMENT_API_KEY="${PAYMENT_API_KEY:-ps_test_localdev}"
GRAFANA_URL="${GRAFANA_URL:-http://localhost:3000}"

# The chaos VU level. 500 is half the latency ceiling measured in
# scripts/k6/README.md, not half the error-rate ceiling: at 4,000 VUs p(95) is
# already ~15s and a booking left 'held' could be the crash OR just congestion.
# At 500 the system is healthy, so every stranded booking is the chaos.
CHAOS_VUS="${CHAOS_VUS:-500}"

RUN_DIR="$REPO/docs/chaos"
BUILD_DIR="${TMPDIR:-/tmp}/ticketqueue-chaos-$$"
FAILURES=0
declare -a PIDS=()

# ------------------------------------------------------------------ plumbing

pg() { docker compose -f "$REPO/docker-compose.yml" exec -T postgres psql -U ticketqueue -d ticketqueue "$@"; }
pg1() { pg -t -A -c "$1" | tr -d '\r' | head -1; }

say()  { printf '%s\n' "$*"; }
step() { printf '\n%s▸ %s%s\n' "$BOLD" "$*" "$OFF"; }
info() { printf '  %s%s%s\n' "$DIM" "$*" "$OFF"; }

pass() { printf '  %sPASS%s  %s\n' "$GREEN" "$OFF" "$*"; }
fail() { printf '  %sFAIL%s  %s\n' "$RED" "$OFF" "$*"; FAILURES=$((FAILURES + 1)); }

# assert_eq <actual> <expected> <description>
assert_eq() {
  if [[ "$1" == "$2" ]]; then pass "$3 ($1)"; else fail "$3 — expected $2, got $1"; fi
}

# assert_le <actual> <max> <description>
assert_le() {
  if (( $1 <= $2 )); then pass "$3 ($1 <= $2)"; else fail "$3 — expected <= $2, got $1"; fi
}

# assert_ge <actual> <min> <description>
assert_ge() {
  if (( $1 >= $2 )); then pass "$3 ($1 >= $2)"; else fail "$3 — expected >= $2, got $1"; fi
}

# ------------------------------------------------------- process lifecycle

port_busy() { lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1; }

build_binaries() {
  mkdir -p "$BUILD_DIR"
  info "building into $BUILD_DIR"
  for c in api worker payment-service loadseed; do
    go build -o "$BUILD_DIR/$c" "$REPO/cmd/$c" || { fail "build $c"; exit 1; }
  done
}

# start_proc <name> — starts one binary with the right env, records its pid.
start_proc() {
  case "$1" in
    api)     PORT="$API_PORT" PAYMENT_URL="$PAYMENT_URL" nohup "$BUILD_DIR/api" > "$BUILD_DIR/api.log" 2>&1 & ;;
    worker)  PAYMENT_URL="$PAYMENT_URL" METRICS_PORT="$WORKER_METRICS_PORT" nohup "$BUILD_DIR/worker" > "$BUILD_DIR/worker.log" 2>&1 & ;;
    payments) PAYMENT_PORT="$PAYMENT_PORT" nohup "$BUILD_DIR/payment-service" > "$BUILD_DIR/payments.log" 2>&1 & ;;
    *) fail "unknown process $1"; return 1 ;;
  esac
  local pid=$!
  eval "PID_$1=$pid"
  PIDS+=("$pid")
  info "$1 started (pid $pid)"
}

pid_of() { local v="PID_$1"; echo "${!v:-}"; }

# kill_proc <name> — SIGKILL, not SIGTERM. A graceful shutdown drains in-flight
# requests, which is the opposite of the failure being simulated: we want
# requests cut off mid-flight, between TX1 and TX2.
kill_proc() {
  local pid; pid="$(pid_of "$1")"
  [[ -n "$pid" ]] && kill -9 "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
  info "$1 KILLED (pid $pid)"
}

wait_http() {
  local url="$1" deadline=$((SECONDS + ${2:-30}))
  until curl -sf -o /dev/null "$url"; do
    (( SECONDS > deadline )) && { fail "timed out waiting for $url"; return 1; }
    sleep 0.3
  done
}

cleanup() {
  local p
  # Keep the k6 summary, not the spew. CHAOS 1 produced a 17MB log once: while
  # the API is dead every refused connection writes a line, and 100k of them
  # bury the only part anyone reads. The summary is printed last, so the tail is
  # the evidence; 200KB is far more than it needs and bounds the file.
  if [[ -f "$BUILD_DIR/k6.log" && -n "${K6_LOG_OUT:-}" ]]; then
    tail -c 200000 "$BUILD_DIR/k6.log" > "$K6_LOG_OUT"
  fi
  for p in "${PIDS[@]:-}"; do [[ -n "$p" ]] && kill -9 "$p" 2>/dev/null; done
  rm -rf "$BUILD_DIR"
}

# ------------------------------------------------------------------ metrics

# worker_metric <name> <label-filter-substring|""> — sums matching samples off
# the worker's /metrics. Returns 0 when the worker is dead, which is a real
# answer for CHAOS 3 rather than an error.
worker_metric() {
  local name="$1" filter="${2:-}"
  curl -sf --max-time 2 "http://localhost:${WORKER_METRICS_PORT}/metrics" 2>/dev/null \
    | grep "^${name}" | { [[ -n "$filter" ]] && grep -F "$filter" || cat; } \
    | awk '{s+=$NF} END {printf "%d", s+0}'
}

# ------------------------------------------------------------------- domain

TIER_ID=""; EVENT_ID=""
load_world() {
  TIER_ID="$(python3 -c "import json;print(json.load(open('$REPO/scripts/k6/seed.json'))['tier_id'])")"
  EVENT_ID="$(python3 -c "import json;print(json.load(open('$REPO/scripts/k6/seed.json'))['event_id'])")"
  info "tier $TIER_ID"
}

held_count()      { pg1 "SELECT count(*) FROM booking WHERE status='held' AND ticket_tier_id='$TIER_ID';"; }
available_count() { pg1 "SELECT count(*) FROM tickets WHERE status='available' AND ticket_tier_id='$TIER_ID';"; }
confirmed_count() { pg1 "SELECT count(*) FROM booking WHERE status='confirmed' AND ticket_tier_id='$TIER_ID';"; }
deferral_rows()   { pg1 "SELECT count(*) FROM audit_log WHERE action='expiry_deferred_provider_unreachable';"; }
deferral_bookings() { pg1 "SELECT count(DISTINCT entity_id) FROM audit_log WHERE action='expiry_deferred_provider_unreachable';"; }

# wait_until_settled <deadline-seconds> — polls until no booking on this tier is
# still 'held', and prints how long that took.
#
# The deadline is not arbitrary: booking.StuckAfter is 90s and ReconcileInterval
# is 30s, so a booking stranded at t=0 is not even ELIGIBLE for reconciliation
# until t=90s and may wait another 30s for a cycle. "Two cycles" would be 60s
# and would recover nothing. Real recovery is 90-120s by construction.
# RECOVERY_T0 must be set to the moment the fault was injected or lifted,
# BEFORE any waiting on the load generator. Starting the clock after `wait` on
# k6 measured the drain as 5s in an early chaos2 run, because the workers had
# already been draining for the 30s the script spent waiting for the run to end.
# The number a reader cares about is "how long from the event", not "how long
# after I happened to start looking".
wait_until_settled() {
  local deadline=$((SECONDS + ${1:-240})) start=${RECOVERY_T0:-$SECONDS} held
  while :; do
    held="$(held_count)"
    if [[ "$held" == "0" ]]; then
      RECOVERY_SECONDS=$((SECONDS - start))
      return 0
    fi
    if (( SECONDS > deadline )); then
      RECOVERY_SECONDS=$((SECONDS - start))
      fail "still $held booking(s) held after ${RECOVERY_SECONDS}s"
      return 1
    fi
    sleep 5
  done
}

# ------------------------------------------------------------------ reporting

CHAOS_NAME=""; T_START=""
chaos_begin() {
  CHAOS_NAME="$1"
  T_START="$(date +%s)"
  mkdir -p "$RUN_DIR"
  OUT="$RUN_DIR/${CHAOS_NAME}-$(date +%Y%m%dT%H%M%S).txt"
  # tee everything: a chaos result that only ever existed in a terminal that
  # scrolled away is not evidence.
  exec > >(tee "$OUT") 2>&1
  K6_LOG_OUT="${OUT%.txt}-k6.txt"
  printf '%s%s%s  %s\n' "$BOLD" "$CHAOS_NAME" "$OFF" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  info "VUs=$CHAOS_VUS  output=$OUT"
}

# chaos_end prints the verdict plus a Grafana link with this run's exact window
# pinned. A link beats a screenshot instruction: the panel can be opened and
# captured afterwards over precisely the seconds the scenario covered, and it
# still works next week.
chaos_end() {
  local t_end=$(($(date +%s) + 30))
  [[ -n "${K6_LOG_OUT:-}" ]] && info "k6 detail: $K6_LOG_OUT"
  step "Grafana, this run's window"
  say "  ${GRAFANA_URL}/d/ticketqueue/ticketqueue?from=$((T_START - 30))000&to=${t_end}000"
  info "$1"

  step "VERDICT"
  if (( FAILURES == 0 )); then
    printf '  %s%s PASSED%s — %s\n' "$GREEN$BOLD" "$CHAOS_NAME" "$OFF" "$OUT"
    return 0
  fi
  printf '  %s%s FAILED: %d check(s)%s — %s\n' "$RED$BOLD" "$CHAOS_NAME" "$FAILURES" "$OFF" "$OUT"
  return 1
}

# preflight refuses to run rather than fight whatever is already on the ports.
preflight() {
  local p
  for p in "$API_PORT" "$PAYMENT_PORT" "$WORKER_METRICS_PORT"; do
    if port_busy "$p"; then
      say "${RED}Port $p is already in use.${OFF}"
      say "Chaos scenarios start and kill the binaries themselves, so they cannot"
      say "share them with a 'make run' in another terminal. Stop those first."
      exit 1
    fi
  done
  docker compose -f "$REPO/docker-compose.yml" exec -T postgres pg_isready -U ticketqueue >/dev/null 2>&1 \
    || { say "${RED}Postgres is not up. Run: make up${OFF}"; exit 1; }
}

# seed_world <tickets> <users> — a fresh event every run. A tier somebody has
# already touched makes every count afterwards meaningless. The tier is
# deliberately larger than the VU count so sustained load keeps making real
# bookings instead of measuring fast-path rejections.
seed_world() {
  info "seeding: $1 tickets, $2 users"
  "$BUILD_DIR/loadseed" -tickets="$1" -users="$2" -windows=500 >/dev/null 2>&1 \
    || { fail "loadseed"; exit 1; }
  load_world
}
