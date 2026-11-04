# --- VPC ---------------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_support   = true
  enable_dns_hostnames = true # Cloud Map private DNS needs both.
  tags                 = { Name = local.name }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = local.name }
}

# TWO public subnets, and no private subnets at all.
#
# Two because an ALB requires subnets in at least two availability zones.
#
# Public because a NAT Gateway costs ~$1/day plus per-GB data processing, which
# is a third of this stack's entire budget, for an environment that exists for
# two days. Tasks therefore run with assign_public_ip = true and are protected
# by security groups instead of by network placement.
#
# IN PRODUCTION THIS WOULD BE WRONG. Tasks belong in private subnets, reaching
# ECR and Secrets Manager through a NAT gateway or VPC endpoints, so that a
# misconfigured security group is not the only thing between the internet and a
# workload. That is a deliberate, costed trade for an ephemeral demo, not a
# pattern to copy.
resource "aws_subnet" "public" {
  count                   = 2
  vpc_id                  = aws_vpc.main.id
  cidr_block              = cidrsubnet(aws_vpc.main.cidr_block, 8, count.index)
  availability_zone       = data.aws_availability_zones.available.names[count.index]
  map_public_ip_on_launch = true
  tags                    = { Name = "${local.name}-public-${count.index}" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }

  tags = { Name = "${local.name}-public" }
}

resource "aws_route_table_association" "public" {
  count          = length(aws_subnet.public)
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# --- Security groups ---------------------------------------------------------
#
# Because the subnets are public, these ARE the perimeter. Each rule below is
# the only thing standing between a component and the internet, so each one is
# justified rather than widened "just in case".

resource "aws_security_group" "alb" {
  name        = "${local.name}-alb"
  description = "Public entry point. The only security group that accepts traffic from the internet."
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "HTTP from anywhere. No HTTPS: a demo on a temporary ALB hostname has no certificate worth issuing."
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "To the API tasks."
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group" "api" {
  name        = "${local.name}-api"
  description = "API tasks. Reachable only through the ALB, never directly."
  vpc_id      = aws_vpc.main.id

  ingress {
    description     = "App port, from the ALB only. A public IP on the task does not make the port public."
    from_port       = 8080
    to_port         = 8080
    protocol        = "tcp"
    security_groups = [aws_security_group.alb.id]
  }

  ingress {
    # This rule keeps direct access internal, but it does NOT protect /metrics
    # from the internet: the ALB is allowed in above and forwards any path. The
    # listener rule in alb.tf is what blocks /metrics publicly (§11a).
    description = "Prometheus scrape of /metrics from inside the VPC"
    from_port   = 8080
    to_port     = 8080
    protocol    = "tcp"
    cidr_blocks = [aws_vpc.main.cidr_block]
  }

  egress {
    description = "RDS, Redis, the payment service, plus ECR and Secrets Manager over the internet gateway."
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group" "worker" {
  name        = "${local.name}-worker"
  description = "Worker tasks. No ingress at all except metrics scraping."
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "Metrics port, VPC-internal only. The worker is not a server and accepts nothing else."
    from_port   = 9091
    to_port     = 9091
    protocol    = "tcp"
    cidr_blocks = [aws_vpc.main.cidr_block]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group" "payment" {
  name        = "${local.name}-payment"
  description = "Simulated provider. In reality this is another company's service and would not be in this VPC."
  vpc_id      = aws_vpc.main.id

  ingress {
    description     = "Charges and lookups, from the API and the worker only."
    from_port       = 8082
    to_port         = 8082
    protocol        = "tcp"
    security_groups = [aws_security_group.api.id, aws_security_group.worker.id]
  }

  ingress {
    description = "Metrics scrape, VPC-internal only."
    from_port   = 8082
    to_port     = 8082
    protocol    = "tcp"
    cidr_blocks = [aws_vpc.main.cidr_block]
  }

  egress {
    description = "Webhook delivery back to the API, and ECR."
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group" "rds" {
  name        = "${local.name}-rds"
  description = "Postgres. Never publicly accessible."
  vpc_id      = aws_vpc.main.id

  ingress {
    description     = "5432 from the API (via its PgBouncer sidecar, which shares the task's ENI and therefore this group), the worker, and the one-shot migration task."
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.api.id, aws_security_group.worker.id, aws_security_group.migrate.id]
  }
}

resource "aws_security_group" "redis" {
  name        = "${local.name}-redis"
  description = "ElastiCache. Never publicly accessible."
  vpc_id      = aws_vpc.main.id

  ingress {
    description     = "6379 from the API and the worker only."
    from_port       = 6379
    to_port         = 6379
    protocol        = "tcp"
    security_groups = [aws_security_group.api.id, aws_security_group.worker.id]
  }
}

resource "aws_security_group" "migrate" {
  name        = "${local.name}-migrate"
  description = "One-shot migration task. Exists as its own group so RDS can admit it without widening any long-lived rule."
  vpc_id      = aws_vpc.main.id

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group" "loadgen" {
  count       = var.enable_loadgen ? 1 : 0
  name        = "${local.name}-loadgen"
  description = "k6 host. Also runs Prometheus and Grafana, which is why it needs to reach the metrics ports."
  vpc_id      = aws_vpc.main.id

  dynamic "ingress" {
    for_each = var.loadgen_ssh_cidr == "" ? [] : [var.loadgen_ssh_cidr]
    content {
      # Never 0.0.0.0/0: this box holds the load-test credentials.
      description = "SSH from one address"
      from_port   = 22
      to_port     = 22
      protocol    = "tcp"
      cidr_blocks = [ingress.value]
    }
  }

  dynamic "ingress" {
    for_each = var.loadgen_ssh_cidr == "" ? [] : [var.loadgen_ssh_cidr]
    content {
      description = "Grafana, from the same one address."
      from_port   = 3000
      to_port     = 3000
      protocol    = "tcp"
      cidr_blocks = [ingress.value]
    }
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# The load generator reaches the ALB like any other client, but explicitly
# rather than by sharing the public rule, so the intent is visible.
resource "aws_security_group_rule" "alb_from_loadgen" {
  count                    = var.enable_loadgen ? 1 : 0
  type                     = "ingress"
  from_port                = 80
  to_port                  = 80
  protocol                 = "tcp"
  security_group_id        = aws_security_group.alb.id
  source_security_group_id = aws_security_group.loadgen[0].id
  description              = "Load generator to the ALB."
}
