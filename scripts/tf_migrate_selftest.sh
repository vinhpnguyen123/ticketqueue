#!/usr/bin/env bash
#
# tf_migrate.sh gates a deploy, so it must only succeed when the migration did.
# A fake `aws` first on PATH simulates each way the one-shot task can go wrong.
#
# Usage: bash scripts/tf_migrate_selftest.sh

set -uo pipefail

stubs=$(mktemp -d)
trap 'rm -rf "$stubs"' EXIT
cat > "$stubs/aws" <<'STUB'
#!/usr/bin/env bash
case "$1 $2" in
  "ec2 describe-subnets")        echo "subnet-1,subnet-2" ;;
  "ec2 describe-security-groups") echo "sg-1" ;;
  "ecs run-task")
    echo "$*" >> "$AWS_CALLS"
    [[ "$FAKE_MODE" == no_task ]] && echo None || echo "arn:task/1" ;;
  "ecs wait")
    [[ "$FAKE_MODE" == wait_fails ]] && exit 255 ; exit 0 ;;
  "ecs describe-tasks")
    case "$FAKE_MODE" in
      failed)       echo 1 ;;
      never_ran)    echo None ;;
      *)            echo 0 ;;
    esac ;;
esac
STUB
chmod +x "$stubs/aws"

export CLUSTER=test REGION=us-east-1 AWS_CALLS="$stubs/calls"
failed=0
expect() {
  local mode=$1 want=$2 code
  shift 2
  FAKE_MODE=$mode PATH="$stubs:$PATH" bash scripts/tf_migrate.sh "$@" >/dev/null 2>&1
  code=$?
  if { [[ "$want" == ok ]] && (( code == 0 )); } || { [[ "$want" == fail ]] && (( code != 0 )); }; then
    echo "ok    $mode: exit $code"
  else
    echo "FAIL  $mode: exit $code, want $want"
    failed=1
  fi
}

expect success ok
expect failed fail
expect never_ran fail
expect no_task fail
expect wait_fails fail

: > "$AWS_CALLS"
expect success ok force 14
if ! grep -q '"force","14"' "$AWS_CALLS"; then
  echo "FAIL  command override was not passed to the task"
  failed=1
fi
exit "$failed"
