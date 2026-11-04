resource "aws_ecs_cluster" "main" {
  name = local.name

  setting {
    name  = "containerInsights"
    value = "disabled" # Costs per metric; the alarms in observability.tf cover what is needed.
  }

  depends_on = [aws_budgets_budget.monthly]
}

# Fargate tasks get an ENI and a private IP that changes on every deploy, and
# nothing resolves another task by name. Cloud Map gives them a private DNS
# namespace so PAYMENT_URL and WEBHOOK_URL have something stable to point at.
resource "aws_service_discovery_private_dns_namespace" "main" {
  name = "${local.name}.local"
  vpc  = aws_vpc.main.id
}

resource "aws_service_discovery_service" "api" {
  name = "api"

  dns_config {
    namespace_id   = aws_service_discovery_private_dns_namespace.main.id
    routing_policy = "MULTIVALUE"
    dns_records {
      type = "A"
      ttl  = 10
    }
  }

  health_check_custom_config { failure_threshold = 1 }
}

resource "aws_service_discovery_service" "payment" {
  name = "payment-service"

  dns_config {
    namespace_id   = aws_service_discovery_private_dns_namespace.main.id
    routing_policy = "MULTIVALUE"
    dns_records {
      type = "A"
      ttl  = 10
    }
  }

  health_check_custom_config { failure_threshold = 1 }
}

# --- IAM ---------------------------------------------------------------------

# The EXECUTION role is used by the ECS agent before the container starts: pull
# the image, fetch the secrets, write the logs. The TASK role is what the
# application itself gets. Keeping them apart means the application cannot read
# the secrets of other services even though the agent can read its own.
resource "aws_iam_role" "execution" {
  name = "${local.name}-execution"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# The managed policy covers ECR and CloudWatch but NOT Secrets Manager, and the
# failure mode is a task that will not start with a message about the secret
# rather than about permissions.
resource "aws_iam_role_policy" "execution_secrets" {
  name = "read-secrets"
  role = aws_iam_role.execution.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = ["secretsmanager:GetSecretValue"]
      Resource = [for s in [
        aws_secretsmanager_secret.db_password,
        aws_secretsmanager_secret.jwt,
        aws_secretsmanager_secret.payment_api_key,
        aws_secretsmanager_secret.webhook,
        aws_secretsmanager_secret.api_database_url,
        aws_secretsmanager_secret.worker_database_url,
      ] : s.arn]
    }]
  })
}

resource "aws_iam_role" "task" {
  name = "${local.name}-task"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

# --- Shared task-definition pieces -------------------------------------------

locals {
  registry = "${data.aws_caller_identity.current.account_id}.dkr.ecr.${var.region}.amazonaws.com"

  # ARM64, not the X86_64 default. The images are built on Apple Silicon and
  # are arm64; a task definition that does not say so fails at startup with an
  # exec format error that points nowhere useful. ARM Fargate is also about 20%
  # cheaper, so the laptop's architecture happens to be the right choice here.
  runtime = {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }

  log_config = { for svc in ["api", "worker", "payment-service", "migrate", "pgbouncer"] : svc => {
    logDriver = "awslogs"
    options = {
      awslogs-group         = aws_cloudwatch_log_group.service[svc].name
      awslogs-region        = var.region
      awslogs-stream-prefix = "ecs"
    }
  } }
}

data "aws_caller_identity" "current" {}

# --- API task: app + PgBouncer sidecar ---------------------------------------

resource "aws_ecs_task_definition" "api" {
  family                   = "${local.name}-api"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 512
  memory                   = 1024
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.task.arn

  runtime_platform {
    operating_system_family = local.runtime.operating_system_family
    cpu_architecture        = local.runtime.cpu_architecture
  }

  container_definitions = jsonencode([
    {
      name         = "api"
      image        = "${local.registry}/${local.name}/api:${var.image_tag}"
      essential    = true
      portMappings = [{ containerPort = 8080, protocol = "tcp" }]

      environment = [
        { name = "PORT", value = "8080" },
        # localhost:6432 is the sidecar. Containers in one Fargate task share a
        # network namespace, so this never leaves the task.
        { name = "PGBOUNCER", value = "true" },
        { name = "POOL_MAX_CONNS", value = "25" },
        { name = "REDIS_ADDR", value = "${aws_elasticache_cluster.main.cache_nodes[0].address}:6379" },
        { name = "PAYMENT_URL", value = local.payment_url },
        # The ALB is the one proxy in front of the API.
        { name = "TRUSTED_PROXY_HOPS", value = "1" },
      ]

      secrets = [
        { name = "DATABASE_URL", valueFrom = aws_secretsmanager_secret.api_database_url.arn },
        { name = "JWT_SECRET", valueFrom = aws_secretsmanager_secret.jwt.arn },
        { name = "PAYMENT_API_KEY", valueFrom = aws_secretsmanager_secret.payment_api_key.arn },
        { name = "WEBHOOK_SECRET", valueFrom = aws_secretsmanager_secret.webhook.arn },
      ]

      # The image is distroless and has no shell, so the healthcheck is the
      # probe compiled into the binary itself (internal/version.Healthcheck).
      healthCheck = {
        command     = ["CMD", "/api", "-healthcheck", "http://127.0.0.1:8080/v1/health"]
        interval    = 15
        timeout     = 5
        retries     = 3
        startPeriod = 20
      }

      # Do not accept traffic until the pooler is actually listening.
      dependsOn = [{ containerName = "pgbouncer", condition = "START" }]

      logConfiguration = local.log_config["api"]
    },
    {
      # A pooler per task rather than one shared instance: it dies with its
      # API task, needs no separate service or load balancer, and its pool size
      # scales with the number of API tasks instead of being a fixed ceiling
      # everything contends for.
      name         = "pgbouncer"
      image        = "edoburu/pgbouncer:latest"
      essential    = true
      portMappings = [{ containerPort = 6432, protocol = "tcp" }]

      environment = [
        { name = "DB_HOST", value = aws_db_instance.main.address },
        { name = "DB_PORT", value = "5432" },
        { name = "DB_USER", value = local.db_user },
        { name = "DB_NAME", value = local.db_name },
        { name = "LISTEN_PORT", value = "6432" },
        { name = "AUTH_TYPE", value = "scram-sha-256" },

        # Measured locally, not guessed. See docs/load-test-results.md §7-§8.
        { name = "POOL_MODE", value = "transaction" },
        { name = "DEFAULT_POOL_SIZE", value = "25" },
        { name = "MAX_CLIENT_CONN", value = "1000" },

        # PgBouncer >= 1.21 tracks prepared statements and re-prepares them on
        # whichever server connection it hands out. Measured: at 0 this setting
        # produced 107 "prepared statement does not exist" errors in 4,000
        # transactions; at 200, zero.
        #
        # The application STILL runs pgx in simple protocol (PGBOUNCER=true
        # above) rather than relying on this. RDS Proxy and older PgBouncer do
        # not re-prepare, and an application must not depend on a pooler
        # feature it might be moved off. Simple protocol is why every JSONB
        # parameter goes through db.JSONB with an explicit ::jsonb cast — do
        # not revert that pattern.
        { name = "MAX_PREPARED_STATEMENTS", value = "200" },

        # pgx sends these at startup; PgBouncer rejects unknown ones by default
        # with an error that reads nothing like a pooling problem.
        { name = "IGNORE_STARTUP_PARAMETERS", value = "extra_float_digits,options,search_path" },
      ]

      secrets = [
        { name = "DB_PASSWORD", valueFrom = aws_secretsmanager_secret.db_password.arn },
      ]

      logConfiguration = local.log_config["pgbouncer"]
    },
  ])
}

resource "aws_ecs_service" "api" {
  name            = "api"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.api.arn
  desired_count   = var.api_desired_count
  launch_type     = "FARGATE"

  network_configuration {
    subnets          = aws_subnet.public[*].id
    security_groups  = [aws_security_group.api.id]
    assign_public_ip = true # No NAT gateway; this is how the task reaches ECR.
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.api.arn
    container_name   = "api"
    container_port   = 8080
  }

  service_registries {
    registry_arn = aws_service_discovery_service.api.arn
  }

  # Without this the service can settle into a loop of starting tasks the ALB
  # kills before they finish warming.
  health_check_grace_period_seconds = 60

  depends_on = [aws_lb_listener.http]
}

# --- Worker ------------------------------------------------------------------

resource "aws_ecs_task_definition" "worker" {
  family                   = "${local.name}-worker"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.task.arn

  runtime_platform {
    operating_system_family = local.runtime.operating_system_family
    cpu_architecture        = local.runtime.cpu_architecture
  }

  container_definitions = jsonencode([{
    name         = "worker"
    image        = "${local.registry}/${local.name}/worker:${var.image_tag}"
    essential    = true
    portMappings = [{ containerPort = 9091, protocol = "tcp" }]

    environment = [
      { name = "METRICS_PORT", value = "9091" },
      { name = "REDIS_ADDR", value = "${aws_elasticache_cluster.main.cache_nodes[0].address}:6379" },
      { name = "PAYMENT_URL", value = local.payment_url },
      # No PGBOUNCER: the worker connects to RDS directly. Its five loops run
      # on 5-to-60-second cadences and hold a handful of connections — that is
      # not the churn a pooler exists to absorb, and a sidecar per worker would
      # add a hop for no benefit.
    ]

    secrets = [
      { name = "DATABASE_URL", valueFrom = aws_secretsmanager_secret.worker_database_url.arn },
      { name = "JWT_SECRET", valueFrom = aws_secretsmanager_secret.jwt.arn },
      { name = "PAYMENT_API_KEY", valueFrom = aws_secretsmanager_secret.payment_api_key.arn },
    ]

    healthCheck = {
      command     = ["CMD", "/worker", "-healthcheck", "http://127.0.0.1:9091/health"]
      interval    = 15
      timeout     = 5
      retries     = 3
      startPeriod = 20
    }

    logConfiguration = local.log_config["worker"]
  }])
}

resource "aws_ecs_service" "worker" {
  name            = "worker"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.worker.arn
  desired_count   = 1
  launch_type     = "FARGATE"

  # Exactly one. The workers are safe to run concurrently — every one of them
  # uses guarded UPDATEs and the reconciliation path is idempotent — but a
  # second replica would double the provider lookups during recovery for no
  # gain in recovery time, which is bounded by ReconcileBatch.
  network_configuration {
    subnets          = aws_subnet.public[*].id
    security_groups  = [aws_security_group.worker.id]
    assign_public_ip = true
  }
}

# --- Payment service ---------------------------------------------------------

resource "aws_ecs_task_definition" "payment" {
  family                   = "${local.name}-payment-service"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.task.arn

  runtime_platform {
    operating_system_family = local.runtime.operating_system_family
    cpu_architecture        = local.runtime.cpu_architecture
  }

  container_definitions = jsonencode([{
    name         = "payment-service"
    image        = "${local.registry}/${local.name}/payment-service:${var.image_tag}"
    essential    = true
    portMappings = [{ containerPort = 8082, protocol = "tcp" }]

    environment = [
      { name = "PAYMENT_PORT", value = "8082" },
      # Resolves through Cloud Map to whichever API tasks are alive.
      { name = "WEBHOOK_URL", value = local.webhook_url },
    ]

    secrets = [
      { name = "PAYMENT_API_KEY", valueFrom = aws_secretsmanager_secret.payment_api_key.arn },
      { name = "WEBHOOK_SECRET", valueFrom = aws_secretsmanager_secret.webhook.arn },
    ]

    healthCheck = {
      command     = ["CMD", "/payment-service", "-healthcheck", "http://127.0.0.1:8082/health"]
      interval    = 15
      timeout     = 5
      retries     = 3
      startPeriod = 20
    }

    logConfiguration = local.log_config["payment-service"]
  }])
}

resource "aws_ecs_service" "payment" {
  name            = "payment-service"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.payment.arn
  desired_count   = 1
  launch_type     = "FARGATE"

  # EXACTLY ONE, and this one is not a cost decision. The simulated provider
  # keeps its charge ledger in memory (§17). A second replica would hold a
  # different ledger, so a reconciliation lookup could land on the task that
  # never saw the charge and answer "no charge" for money that moved — which
  # would make hold-expiry release a paid customer's tickets. A real provider
  # has a durable ledger and no such constraint.
  network_configuration {
    subnets          = aws_subnet.public[*].id
    security_groups  = [aws_security_group.payment.id]
    assign_public_ip = true
  }

  service_registries {
    registry_arn = aws_service_discovery_service.payment.arn
  }
}

# --- Migrations: a task definition, deliberately not a service ---------------

# Run once with `make tf-migrate`, never on API startup. Two API tasks booting
# at the same time would both try to migrate, and golang-migrate's version
# table would be the only thing standing between them and a half-applied
# schema. A one-shot task makes the ordering explicit and the failure visible.
resource "aws_ecs_task_definition" "migrate" {
  family                   = "${local.name}-migrate"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.task.arn

  runtime_platform {
    operating_system_family = local.runtime.operating_system_family
    cpu_architecture        = local.runtime.cpu_architecture
  }

  container_definitions = jsonencode([{
    name      = "migrate"
    image     = "${local.registry}/${local.name}/migrate:${var.image_tag}"
    essential = true

    # Straight to RDS, not through a pooler: DDL in a transaction is exactly
    # the workload transaction pooling is worst at, and this runs once.
    secrets = [
      { name = "DATABASE_URL", valueFrom = aws_secretsmanager_secret.worker_database_url.arn },
    ]

    logConfiguration = local.log_config["migrate"]
  }])
}
