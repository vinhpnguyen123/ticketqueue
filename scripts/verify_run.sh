#!/usr/bin/env bash
#
# Correctness verification after a load test. Runs the SQL checks, then the one
# check SQL cannot do: comparing Redis against Postgres.
#
# Exits non-zero if ANY check fails, so it can gate a run.
#
# Usage: make verify-run       (or: bash scripts/verify_run.sh)

set -uo pipefail

RED=$'\033[31m'; GREEN=$'\033[32m'; BOLD=$'\033[1m'; DIM=$'\033[2m'; OFF=$'\033[0m'

psql_run() {
  docker compose exec -T postgres psql -U ticketqueue -d ticketqueue "$@"
}

# A check that could not run is not a check that passed. Exit 2 keeps "could not
# verify" distinct from exit 1, "verified and found a problem".
die() {
  printf '%sCOULD NOT VERIFY:%s %s\n' "$RED$BOLD" "$OFF" "$1" >&2
  exit 2
}

echo "${BOLD}Correctness verification${OFF}"

sql_output=$(psql_run -v ON_ERROR_STOP=1 -f - < scripts/verify_run.sql 2>&1) \
  || { echo "$sql_output"; die "the SQL checks did not finish"; }
echo "$sql_output"

expected=$(grep -c 'AS result' scripts/verify_run.sql)
reported=$(grep -cE '^ (PASS|FAIL)' <<<"$sql_output" || true)
(( reported == expected )) || die "expected $expected SQL check results, got $reported"

failures=$(grep -c '^ FAIL' <<<"$sql_output" || true)

# --- Check 9: Redis vs Postgres, which psql cannot do on its own -------------
echo ""
echo "=== 9. Redis inventory matches Postgres per tier ==="
echo "${DIM}    (run the drift checker for two cycles first, or in-flight work reads as drift)${OFF}"

tiers=$(psql_run -t -A -F' ' -c \
  "SELECT t.id, COUNT(tk.id) FILTER (WHERE tk.status='available')
   FROM ticket_tier t
   JOIN events e ON e.id = t.event_id
   LEFT JOIN tickets tk ON tk.ticket_tier_id = t.id
   WHERE e.status = 'on_sale'
   GROUP BY t.id" | tr -d '\r') || die "could not list the on-sale tiers"

redis_failures=0
checked=0
skipped=0
while read -r tier_id truth; do
  [[ -z "$tier_id" ]] && continue
  checked=$((checked + 1))
  cached=$(docker compose exec -T redis redis-cli GET "inventory:${tier_id}" | tr -d '\r') \
    || die "could not read Redis for tier ${tier_id:0:8}"
  if [[ -z "$cached" ]]; then
    # A missing key is not drift: the counter is lazily initialised on the first
    # purchase or by the drift checker. Only a key that DISAGREES is a finding.
    printf '  %sSKIP%s  tier %s  no redis key yet (truth=%s, never purchased)\n' "$DIM" "$OFF" "${tier_id:0:8}" "$truth"
    skipped=$((skipped + 1))
  elif [[ "$cached" != "$truth" ]]; then
    printf '  %sFAIL%s  tier %s  redis=%s postgres=%s\n' "$RED" "$OFF" "${tier_id:0:8}" "$cached" "$truth"
    redis_failures=$((redis_failures + 1))
  fi
done <<<"$tiers"

if (( redis_failures == 0 )); then
  printf '  %sPASS%s  %d of %d on-sale tier(s) agree with Postgres (%d not yet initialised)\n' \
    "$GREEN" "$OFF" "$((checked - skipped))" "$checked" "$skipped"
fi
failures=$((failures + redis_failures))

echo ""
if (( failures == 0 )); then
  printf '%sALL CHECKS PASSED%s\n' "$GREEN$BOLD" "$OFF"
else
  printf '%s%d CHECK(S) FAILED%s\n' "$RED$BOLD" "$failures" "$OFF"
  exit 1
fi
