#!/usr/bin/env bash
#
# verify_run.sh gates load-test evidence, so it must never pass when a check could
# not run. A fake `docker` first on PATH simulates each dependency failing.
#
# Usage: bash scripts/verify_run_selftest.sh

set -uo pipefail

stubs=$(mktemp -d)
trap 'rm -rf "$stubs"' EXIT
cat > "$stubs/docker" <<'STUB'
#!/usr/bin/env bash
# Called as: docker compose exec -T <service> <cmd...>
service=$4
case "$service:${FAKE_MODE}" in
  postgres:psql_down) echo "service postgres is not running" >&2; exit 1 ;;
  postgres:no_rows)   exit 0 ;;
  postgres:tiers_down)
    [[ "$*" == *"-f -"* ]] && { cat >/dev/null; for _ in $(seq 1 "$EXPECTED"); do echo " PASS   |     0"; done; exit 0; }
    exit 1 ;;
  postgres:redis_down)
    [[ "$*" == *"-f -"* ]] && { cat >/dev/null; for _ in $(seq 1 "$EXPECTED"); do echo " PASS   |     0"; done; exit 0; }
    echo "11111111-1111-1111-1111-111111111111 5"; exit 0 ;;
  redis:redis_down)   exit 1 ;;
esac
exit 0
STUB
chmod +x "$stubs/docker"

export EXPECTED
EXPECTED=$(grep -c 'AS result' scripts/verify_run.sql)

failed=0
for mode in psql_down no_rows tiers_down redis_down; do
  out=$(FAKE_MODE=$mode PATH="$stubs:$PATH" bash scripts/verify_run.sh 2>&1)
  code=$?
  if (( code == 0 )) || grep -q 'ALL CHECKS PASSED' <<<"$out"; then
    echo "FAIL  $mode: exit $code"
    failed=1
  else
    echo "ok    $mode: exit $code"
  fi
done
exit "$failed"
