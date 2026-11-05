# One log group per service, one-day retention. Retention is a cost lever that
# is easy to forget: CloudWatch bills for ingestion and storage, and the
# default is "never expire", which keeps charging long after the stack is gone.
resource "aws_cloudwatch_log_group" "service" {
  for_each          = toset(["api", "worker", "payment-service", "migrate", "pgbouncer"])
  name              = "/ecs/${local.name}/${each.value}"
  retention_in_days = 1
}

# --- Alarms ------------------------------------------------------------------
#
# No actions attached. These exist to draw the lines on a graph and to make the
# load-test result explainable, not to page anyone. The question they answer is
# the one the local run could not: the laptop measured Postgres at 749-788% of
# 800% CPU while sharing those cores with six other processes, so "the database
# is the bottleneck" was true but confounded. Here the database has its own
# instance, and these alarms show whether the ceiling followed it.

resource "aws_cloudwatch_metric_alarm" "rds_cpu" {
  alarm_name          = "${local.name}-rds-cpu"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = "CPUUtilization"
  namespace           = "AWS/RDS"
  period              = 60
  statistic           = "Average"
  threshold           = 80
  alarm_description   = "Postgres CPU. The local run pinned this; the point of the deployment is to see whether it still does on dedicated hardware."
  dimensions          = { DBInstanceIdentifier = aws_db_instance.main.identifier }
}

# The one alarm that changes how a number should be READ rather than what to
# fix. A t4g instance throttles to its baseline when credits run out, and a
# throttled database looks exactly like a saturated one. If this fires during a
# load test, the throughput number describes the credit balance, not the app.
resource "aws_cloudwatch_metric_alarm" "rds_cpu_credits" {
  alarm_name          = "${local.name}-rds-cpu-credits"
  comparison_operator = "LessThanThreshold"
  evaluation_periods  = 1
  metric_name         = "CPUCreditBalance"
  namespace           = "AWS/RDS"
  period              = 300
  statistic           = "Average"
  threshold           = 30
  alarm_description   = "Burst credits nearly exhausted. Any load-test number taken while this is in ALARM is a number about throttling. Re-run on db.m7g.large."
  dimensions          = { DBInstanceIdentifier = aws_db_instance.main.identifier }

  # Only meaningful on a burstable class; a non-burstable instance never
  # publishes this metric and the alarm would sit in INSUFFICIENT_DATA.
  count = startswith(var.rds_instance_class, "db.t") ? 1 : 0
}

resource "aws_cloudwatch_metric_alarm" "rds_connections" {
  alarm_name          = "${local.name}-rds-connections"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = "DatabaseConnections"
  namespace           = "AWS/RDS"
  period              = 60
  statistic           = "Maximum"
  threshold           = 60
  alarm_description   = "Connections to Postgres. With a PgBouncer sidecar per API task at default_pool_size 25, this should stay well under the instance limit — if it does not, the pooler is not doing its job."
  dimensions          = { DBInstanceIdentifier = aws_db_instance.main.identifier }
}

resource "aws_cloudwatch_metric_alarm" "redis_cpu" {
  alarm_name          = "${local.name}-redis-cpu"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = "CPUUtilization"
  namespace           = "AWS/ElastiCache"
  period              = 60
  statistic           = "Average"
  threshold           = 80
  alarm_description   = "Redis CPU. Layer 1 of the oversell defense runs here; if it saturates, the fast path stops being fast."
  dimensions          = { CacheClusterId = aws_elasticache_cluster.main.cluster_id }
}

resource "aws_cloudwatch_metric_alarm" "ecs_cpu" {
  for_each = toset(["api", "worker", "payment-service"])

  alarm_name          = "${local.name}-${each.value}-cpu"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = "CPUUtilization"
  namespace           = "AWS/ECS"
  period              = 60
  statistic           = "Average"
  threshold           = 80
  alarm_description   = "Task CPU. If the API saturates before Postgres does, the bottleneck moved and the Week 6 conclusion changes."
  dimensions = {
    ClusterName = aws_ecs_cluster.main.name
    ServiceName = each.value
  }
}

resource "aws_cloudwatch_metric_alarm" "ecs_memory" {
  for_each = toset(["api", "worker", "payment-service"])

  alarm_name          = "${local.name}-${each.value}-memory"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = "MemoryUtilization"
  namespace           = "AWS/ECS"
  period              = 60
  statistic           = "Average"
  threshold           = 85
  alarm_description   = "Task memory. A Fargate task that hits its limit is killed, not throttled."
  dimensions = {
    ClusterName = aws_ecs_cluster.main.name
    ServiceName = each.value
  }
}
