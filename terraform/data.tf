resource "aws_db_subnet_group" "main" {
  name       = local.name
  subnet_ids = aws_subnet.public[*].id
}

resource "aws_db_instance" "main" {
  identifier     = local.name
  engine         = "postgres"
  engine_version = "16"
  instance_class = var.rds_instance_class

  allocated_storage = var.rds_allocated_storage
  storage_type      = "gp3"
  storage_encrypted = true

  db_name  = local.db_name
  username = local.db_user
  password = random_password.db.result

  db_subnet_group_name   = aws_db_subnet_group.main.name
  vpc_security_group_ids = [aws_security_group.rds.id]

  # The subnets are public, so this flag is the thing keeping Postgres off the
  # internet. With it false, RDS gets no public IP and is reachable only from
  # inside the VPC, which is what the security group then narrows further.
  publicly_accessible = false

  multi_az = false # Single AZ: this is a demo, and Multi-AZ doubles the cost.

  # Ephemeral by design. Backups on a stack that lives two days would only
  # produce snapshots that outlive it and keep billing.
  backup_retention_period = 0
  skip_final_snapshot     = true
  deletion_protection     = false
  apply_immediately       = true

  # --- The instrumentation this deployment exists for -----------------------
  #
  # The local run measured Postgres at 749-788% CPU out of 800% on an 8-core
  # laptop that was also running k6, the three app processes, Redis, Prometheus
  # and Grafana. The whole point of deploying is to re-measure that with the
  # database alone on its own instance. Without these two settings the run
  # produces a throughput number and no explanation for it, which is the exact
  # failure §11a was written to prevent.
  #
  # Performance Insights is the managed equivalent of pg_stat_statements: it
  # answers "which statement, and what was it waiting on" directly.
  performance_insights_enabled          = true
  performance_insights_retention_period = 7 # Free tier. 7 days outlives the stack.

  # 15-second OS metrics. The 60-second default is too coarse to see a load
  # test that runs for twenty seconds.
  monitoring_interval = 15
  monitoring_role_arn = aws_iam_role.rds_monitoring.arn

  # Postgres logs into CloudWatch so a slow query or a connection error is
  # visible without shelling anywhere.
  enabled_cloudwatch_logs_exports = ["postgresql"]

  # Do not create the database before the budget alarm exists. Terraform has no
  # global ordering, and this is the resource most likely to still be running
  # next month if something goes wrong.
  depends_on = [aws_budgets_budget.monthly]
}

resource "aws_iam_role" "rds_monitoring" {
  name = "${local.name}-rds-monitoring"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "monitoring.rds.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "rds_monitoring" {
  role       = aws_iam_role.rds_monitoring.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonRDSEnhancedMonitoringRole"
}

# --- Redis -------------------------------------------------------------------

resource "aws_elasticache_subnet_group" "main" {
  name       = local.name
  subnet_ids = aws_subnet.public[*].id
}

# Single node, no replica, no cluster mode. The application treats Redis as a
# cache and a fast-path counter, never as the source of truth — Postgres is
# (Principle #1), and the drift checker repairs Redis from Postgres when they
# disagree. Losing this node costs a slow first purchase, not correctness, so
# paying for a replica in a two-day demo would buy nothing.
resource "aws_elasticache_cluster" "main" {
  cluster_id           = local.name
  engine               = "redis"
  engine_version       = "7.1"
  node_type            = "cache.t4g.micro"
  num_cache_nodes      = 1
  parameter_group_name = "default.redis7"
  port                 = 6379

  subnet_group_name  = aws_elasticache_subnet_group.main.name
  security_group_ids = [aws_security_group.redis.id]

  depends_on = [aws_budgets_budget.monthly]
}
