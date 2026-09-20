# AWS deployment — ephemeral, budgeted, disposable

This stack exists to answer one question the laptop could not: **is the
database still the bottleneck when it has its own hardware?** The local runs
measured Postgres at 749–788% CPU out of 800%, on a machine also running k6,
three app processes, Redis, Prometheus and Grafana. That finding was real but
confounded. Here the database is alone on its own instance.

It is built to live for **a day or two** and then be destroyed. Budget: **$15–30
total**. Every resource choice below follows from that.

---

# ☠️ DESTROY FIRST

This section is above the deploy instructions deliberately. Read it before you
apply anything.

```bash
make tf-destroy     # prompts for confirmation, then verifies
```

## DESTROY CHECKLIST

Work down this list. Do not stop at the first green tick.

- [ ] `make tf-destroy` completed **without errors**. A partial destroy is the
      normal way this goes wrong: one resource fails, Terraform stops, and
      everything after it in the graph stays running.
- [ ] The tag sweep returns an **empty table**:
      ```bash
      make tf-cost
      ```
      This queries by `Project=ticketqueue`, which every resource carries. It
      is the backstop for a lost or corrupted state file.
- [ ] **RDS is really gone.** It is the most expensive thing that can be left
      behind and the easiest to miss, because a failed delete leaves it
      `available` and billing:
      ```bash
      aws rds describe-db-instances --region us-east-1 \
        --query 'DBInstances[?starts_with(DBInstanceIdentifier,`ticketqueue`)].[DBInstanceIdentifier,DBInstanceStatus]' --output table
      ```
- [ ] **No EC2 load generator.** At ~$8.16/day this alone outruns the budget in
      four days:
      ```bash
      aws ec2 describe-instances --region us-east-1 \
        --filters Name=tag:Project,Values=ticketqueue Name=instance-state-name,Values=running \
        --query 'Reservations[].Instances[].[InstanceId,InstanceType]' --output table
      ```
- [ ] **No ElastiCache cluster**, **no ALB**, **no NAT gateway** (there should
      never have been one):
      ```bash
      aws elasticache describe-cache-clusters --region us-east-1 --query 'CacheClusters[].CacheClusterId'
      aws elbv2 describe-load-balancers --region us-east-1 --query 'LoadBalancers[].LoadBalancerName'
      aws ec2 describe-nat-gateways --region us-east-1 --query 'NatGateways[?State==`available`].NatGatewayId'
      ```
- [ ] Check the actual bill a day later. Cost Explorer, filtered to
      `Project=ticketqueue`. Tags are why that filter works.

**If the state file is lost**, Terraform cannot destroy anything. The tag sweep
above is the recovery path: it lists every ARN, and each one can be deleted by
hand. This is the reason every resource is tagged.

---

## What it costs

Measured against on-demand us-east-1 pricing, ARM where available.

| Resource | Per day | Notes |
|---|---:|---|
| ALB | $0.54 + LCU | Fixed hourly charge; the LCU part is negligible at this scale |
| RDS `db.t4g.micro` + 20 GB gp3 | $0.46 | Single-AZ, no backups |
| ElastiCache `cache.t4g.micro` | $0.38 | Single node |
| Fargate, 1.5 vCPU + 3 GB (ARM) | $1.42 | api ×2, worker ×1, payments ×1 |
| Secrets Manager (6), logs, ECR | ~$0.15 | 1-day log retention |
| **Idle subtotal** | **~$3.00/day** | |
| **EC2 load generator `c6i.2xlarge`** | **$8.16/day** | **Off by default. The budget risk.** |
| RDS `db.m7g.large` (if bumped) | $3.70/day | Only for the load-test run |

Two days idle plus a few hours of load testing lands around **$8–12**. The way
this becomes a $200 bill is leaving the load generator or RDS running — hence
the checklist above.

A budget alarm at $20 emails you at 80% forecast and 100% actual. It is created
before anything that spends money: the expensive resources carry an explicit
`depends_on` pointing at it, because Terraform has no global apply order and an
alarm that arrives after the bill is not an alarm.

## Apply order

```bash
make tf-init                      # 1. providers
make docker-push                  # 2. build ARM64 images, push to ECR
                                  #    MUST be before apply: services pull on start
make tf-plan                      # 3. read it
make tf-apply                     # 4. ~12 min, RDS is the slow part
make tf-migrate                   # 5. schema, as a one-shot ECS task
curl http://$(terraform -chdir=terraform output -raw alb_dns_name)/v1/health
                                  # 6. should return the git SHA you pushed
```

Step 2 before step 4 is not optional. ECR repositories are created by
`tf-apply`, so the very first run is a chicken-and-egg: apply once (services
will fail to pull), then push, then apply again. After that, push first.

## Deliberate choices, and what production would do instead

**No NAT gateway.** ~$1/day plus per-GB processing — a third of the budget for
two days of life. ECS tasks run in **public subnets** with
`assign_public_ip = true`, locked down by security groups: the API accepts 8080
only from the ALB's security group, the worker accepts nothing, Postgres and
Redis accept only the app security groups. *Production would put tasks in
private subnets reaching ECR and Secrets Manager through VPC endpoints, so a
misconfigured security group is not the only thing between a workload and the
internet.*

**HTTP only, no ACM certificate.** The ALB hostname is generated and lives two
days; there is no domain to validate against. *Production terminates TLS at the
ALB and redirects :80 to :443.*

**Single-AZ RDS, no backups, `skip_final_snapshot`.** Backups of a two-day demo
would outlive the demo and keep billing. *Production is Multi-AZ with PITR.*

**One payment-service task, and this one is not about cost.** The simulated
provider keeps its charge ledger in memory (§17). A second replica would hold a
different ledger, so a reconciliation lookup could land on the task that never
saw a charge and answer "no charge" for money that moved — which would make
hold-expiry release a paid customer's tickets. A real provider has a durable
ledger and no such constraint.

**Local Terraform state.** Remote state exists to coordinate a team and survive
a lost laptop. This stack lives two days; an S3 bucket and a DynamoDB lock table
would be two more resources to create, pay for, and forget to delete. State is
gitignored — **it contains the generated database password in plaintext**.

**No `aws ecs execute-command` output.** The runtime images are
`distroless/static`: no `/bin/sh`, so ECS Exec has nothing to run. That was the
trade made to get a 25 MB image with no shell and no package manager. Use the
`debugging` output instead — `aws logs tail`, `/v1/health` for the running SHA,
`/v1/health/ready` for dependency state, and Performance Insights for the
database internals.

## PgBouncer runs as a sidecar

One pooler per API task, on `localhost:6432`, rather than a shared instance: it
dies with its task, needs no service or load balancer of its own, and its pool
size scales with the number of API tasks.

Settings come from the local measurements in `docs/load-test-results.md` §7–§8:
`pool_mode=transaction`, `default_pool_size=25`, `max_client_conn=1000`,
`max_prepared_statements=200`.

That last one has a measured history. PgBouncer ≥ 1.21 re-prepares statements
on whichever server connection it hands out; at `0` a reproduction produced
**107 "prepared statement does not exist" errors in 4,000 transactions**, at
`200`, zero. **The application still runs pgx in simple protocol
(`PGBOUNCER=true`)** rather than depending on that, because RDS Proxy and older
PgBouncer do not re-prepare and an application should not rely on a pooler
feature it might be moved off. Simple protocol is why every JSONB parameter goes
through `db.JSONB` with an explicit `::jsonb` cast — **do not revert that
pattern**, it is load-bearing here.

The worker connects to RDS directly, with no sidecar: its five loops run on
5-to-60-second cadences and hold a handful of connections, which is not the
churn a pooler exists to absorb.

## Reading the load-test result

The **RDS instance class decides whether the number means anything.**
`db.t4g.micro` is burstable: it earns CPU credits while idle and spends them
under load, and when the balance hits zero it is throttled to 10% of 2 vCPU. A
throttled database looks exactly like a saturated one — flat throughput,
Postgres pinned — but for an entirely different reason.

So either bump it for the run:

```bash
terraform -chdir=terraform apply -var rds_instance_class=db.m7g.large
```

or watch the `ticketqueue-rds-cpu-credits` alarm and **record in the results
which class each number came from**. Do not skip both.

Application metrics (`purchase_phase_duration_seconds{phase}`, `db_pool_in_use`,
`db_pool_acquire_wait_seconds_total`) come from Prometheus and Grafana running
on the load generator, inside the VPC. CloudWatch and Performance Insights cover
the infrastructure; neither can produce the tx1 / payment_call / tx2 breakdown
that the whole Week 6 analysis was built on, and without it the run yields a
throughput number with no explanation.

## Files

| File | Contents |
|---|---|
| `versions.tf` | Provider pins, the `us-east-1` alias Budgets requires, why state is local |
| `variables.tf` | Sizing, the burstable-RDS warning, the load-generator cost warning |
| `main.tf` | Tags, Cloud Map URLs, connection strings, the budget alarm |
| `network.tf` | VPC, two public subnets, and the security groups that are the perimeter |
| `data.tf` | RDS with Performance Insights and Enhanced Monitoring, ElastiCache |
| `secrets.tf` | `random_password`, Secrets Manager, `recovery_window_in_days = 0` |
| `ecr.tf` | Four repositories, `force_delete`, lifecycle expiry |
| `ecs.tf` | Cluster, Cloud Map, three services, the PgBouncer sidecar, the migration task |
| `alb.tf` | ALB, target group health-checking `/v1/health` (liveness, not readiness) |
| `observability.tf` | Log groups at 1-day retention, alarms including CPU credits |
| `loadgen.tf` | Optional EC2, kernel tuning in user_data, SSM access |
| `outputs.tf` | Endpoints, debugging commands, the cost sweep |
