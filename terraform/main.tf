locals {
  name = "ticketqueue"

  # Every resource carries these. Cost Explorer can then attribute the bill per
  # project, and — more importantly — `aws resourcegroupstaggingapi get-resources
  # --tag-filters Key=Project,Values=ticketqueue` finds anything a failed
  # destroy left behind. An RDS instance nobody remembers is how a $20 demo
  # becomes a $200 bill.
  tags = {
    Project     = "ticketqueue"
    Environment = "demo"
    Owner       = var.owner_email
    ManagedBy   = "terraform"
  }

  # Cloud Map gives the tasks DNS names for each other. Without it, Fargate
  # tasks have no way to address one another: they get an ENI and a private IP
  # that changes on every deploy, and PAYMENT_URL has nothing stable to point
  # at. An internal ALB would also work and costs another ~$0.54/day for a job
  # a private DNS namespace does for cents.
  payment_url = "http://payment-service.${local.name}.local:8082"
  webhook_url = "http://api.${local.name}.local:8080/webhooks/payment-service"

  # The API talks to Postgres through the PgBouncer sidecar in its own task, so
  # the host is localhost. The worker has no sidecar and connects to RDS
  # directly — it runs five loops on a 5-to-60-second cadence, which is not the
  # connection churn a pooler exists to absorb.
  api_database_url    = "postgres://${local.db_user}:${random_password.db.result}@localhost:6432/${local.db_name}?sslmode=require"
  worker_database_url = "postgres://${local.db_user}:${random_password.db.result}@${aws_db_instance.main.address}:5432/${local.db_name}?sslmode=require"

  db_user = "ticketqueue"
  db_name = "ticketqueue"
}

data "aws_availability_zones" "available" {
  state = "available"
}

# --- Budget ------------------------------------------------------------------

# Created before anything that can spend money. Terraform has no global apply
# order, so the resources that cost the most depend on this explicitly: see
# depends_on in data.tf and loadgen.tf. An alarm that arrives after the bill is
# not an alarm.
resource "aws_budgets_budget" "monthly" {
  provider = aws.us_east_1

  name         = "${local.name}-demo"
  budget_type  = "COST"
  limit_amount = tostring(var.budget_limit_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  # Forecast first: an alert at 100% actual arrives after the money is gone.
  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 80
    threshold_type             = "PERCENTAGE"
    notification_type          = "FORECASTED"
    subscriber_email_addresses = [var.owner_email]
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_email_addresses = [var.owner_email]
  }
}
