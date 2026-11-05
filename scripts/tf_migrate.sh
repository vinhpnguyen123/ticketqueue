#!/usr/bin/env bash
#
# Runs the one-shot migrate task on ECS and exits with its result. Starting the
# task proves nothing: a failed migration leaves the app running without the
# unique indexes that enforce the money rules, so this waits for the verdict.
#
# Usage: CLUSTER=ticketqueue REGION=us-east-1 bash scripts/tf_migrate.sh [command...]
#        With no command the task runs `up`. Example: ... tf_migrate.sh force 14

set -euo pipefail

die() {
  echo "MIGRATION FAILED: $1" >&2
  echo "logs: aws logs tail /ecs/ticketqueue/migrate --since 15m --region $REGION" >&2
  echo "recovery: see 'If a migration fails' in terraform/README.md" >&2
  exit 1
}

subnet=$(aws ec2 describe-subnets --region "$REGION" \
  --filters Name=tag:Project,Values=ticketqueue --query 'Subnets[0].SubnetId' --output text)
group=$(aws ec2 describe-security-groups --region "$REGION" \
  --filters Name=group-name,Values=ticketqueue-migrate --query 'SecurityGroups[0].GroupId' --output text)

overrides=()
if (( $# > 0 )); then
  command=$(printf '"%s",' "$@")
  overrides=(--overrides "{\"containerOverrides\":[{\"name\":\"migrate\",\"command\":[${command%,}]}]}")
fi

task=$(aws ecs run-task --cluster "$CLUSTER" --launch-type FARGATE \
  --task-definition ticketqueue-migrate \
  --network-configuration "awsvpcConfiguration={subnets=[$subnet],securityGroups=[$group],assignPublicIp=ENABLED}" \
  --region "$REGION" --query 'tasks[0].taskArn' --output text "${overrides[@]+"${overrides[@]}"}")
[[ -n "$task" && "$task" != None ]] || die "ECS did not start the task"
echo "started $task, waiting for it to finish"

aws ecs wait tasks-stopped --cluster "$CLUSTER" --tasks "$task" --region "$REGION" \
  || die "the task did not stop in time"

code=$(aws ecs describe-tasks --cluster "$CLUSTER" --tasks "$task" --region "$REGION" \
  --query 'tasks[0].containers[?name==`migrate`].exitCode | [0]' --output text)
# None means the container never ran, which an image pull failure looks like.
[[ "$code" == 0 ]] || die "the container exited with '$code'"
echo "migration finished"
