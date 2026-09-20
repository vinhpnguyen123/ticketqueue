resource "aws_lb" "main" {
  name               = local.name
  load_balancer_type = "application"
  internal           = false
  security_groups    = [aws_security_group.alb.id]
  subnets            = aws_subnet.public[*].id

  # A demo should not survive a forgotten destroy by being hard to delete.
  enable_deletion_protection = false

  depends_on = [aws_budgets_budget.monthly]
}

resource "aws_lb_target_group" "api" {
  name        = "${local.name}-api"
  port        = 8080
  protocol    = "HTTP"
  vpc_id      = aws_vpc.main.id
  target_type = "ip" # Fargate awsvpc tasks register by IP, not instance.

  health_check {
    path = "/v1/health"
    # LIVENESS, not readiness, and the distinction matters operationally.
    # /v1/health/ready pings Postgres and Redis. Pointing the target group at
    # it means a database blip pulls every task out of the load balancer, the
    # ALB returns 503 for everything, and the logs you need in order to
    # diagnose the database are behind a service that is now also down.
    # Liveness answers "is this process alive"; that is what decides whether to
    # replace a task.
    matcher             = "200"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  # Purchases are short. 30 seconds is long enough to finish one in flight
  # during a deploy and short enough that a rolling update is not slow.
  deregistration_delay = 30
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.main.arn
  port              = 80
  protocol          = "HTTP"

  # Plain HTTP. HTTPS would need ACM, which needs a domain you control — and
  # this ALB's hostname is generated and lives for two days. Production would
  # obviously terminate TLS here and redirect :80 to :443; the README says so.
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.api.arn
  }
}
