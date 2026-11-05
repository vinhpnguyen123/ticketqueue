# Generated, never written down. A password literal in a .tf file is in git
# forever, and a password in a variable is in terraform.tfstate anyway — but at
# least this one was never typed by a human and never reused anywhere else.
resource "random_password" "db" {
  length = 32
  # Alphanumeric only: the value is embedded unescaped in postgres:// URLs, where
  # characters like #, %, ? and [ break parsing or change what the URL means.
  special = false
}

resource "random_password" "jwt" {
  length  = 48
  special = false
}

resource "random_password" "payment_api_key" {
  length  = 32
  special = false
}

resource "random_password" "webhook" {
  length  = 32
  special = false
}

# recovery_window_in_days = 0 so `terraform destroy` really deletes them. The
# default is a 30-day recovery window, during which the name stays taken and
# the secret keeps costing $0.40/month — so a re-apply the next day fails on a
# name collision and the "destroyed" stack is still billing.
resource "aws_secretsmanager_secret" "db_password" {
  name                    = "${local.name}/db-password"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "db_password" {
  secret_id     = aws_secretsmanager_secret.db_password.id
  secret_string = random_password.db.result
}

resource "aws_secretsmanager_secret" "jwt" {
  name                    = "${local.name}/jwt-secret"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "jwt" {
  secret_id     = aws_secretsmanager_secret.jwt.id
  secret_string = random_password.jwt.result
}

resource "aws_secretsmanager_secret" "payment_api_key" {
  name                    = "${local.name}/payment-api-key"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "payment_api_key" {
  secret_id     = aws_secretsmanager_secret.payment_api_key.id
  secret_string = random_password.payment_api_key.result
}

resource "aws_secretsmanager_secret" "webhook" {
  name                    = "${local.name}/webhook-secret"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "webhook" {
  secret_id     = aws_secretsmanager_secret.webhook.id
  secret_string = random_password.webhook.result
}

# The full connection strings are secrets too, because they contain the
# password. Task definitions reference these by ARN through `secrets`, never
# through `environment` — an environment variable is printed in the task
# definition JSON, visible in the console, and returned by
# `aws ecs describe-task-definition` to anyone with read access.
resource "aws_secretsmanager_secret" "api_database_url" {
  name                    = "${local.name}/api-database-url"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "api_database_url" {
  secret_id     = aws_secretsmanager_secret.api_database_url.id
  secret_string = local.api_database_url
}

resource "aws_secretsmanager_secret" "worker_database_url" {
  name                    = "${local.name}/worker-database-url"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "worker_database_url" {
  secret_id     = aws_secretsmanager_secret.worker_database_url.id
  secret_string = local.worker_database_url
}
