variable "region" {
  description = "Deployment region. us-east-1 is the cheapest for this set of services."
  type        = string
  default     = "us-east-1"
}

variable "owner_email" {
  description = "Where the budget alarm sends its warning. Required — a stack that can outspend its budget silently is the thing this file exists to prevent."
  type        = string
}

variable "budget_limit_usd" {
  description = "Monthly budget. Alarms at 80% forecast and 100% actual."
  type        = number
  default     = 20
}

variable "image_tag" {
  description = "Tag to deploy. Set to the git SHA by `make tf-push`, which is also what /v1/health reports, so the running code can be identified from outside."
  type        = string
  default     = "latest"
}

# --- Sizing ------------------------------------------------------------------

variable "rds_instance_class" {
  description = <<-EOT
    db.t4g.micro is enough to deploy and smoke-test, and it is what keeps this
    inside budget while the stack idles.

    IT IS BURSTABLE, AND THAT MATTERS FOR THE LOAD TEST. A t4g instance earns
    CPU credits while idle and spends them under load; when the balance hits
    zero it is throttled to its baseline (10% of 2 vCPU). The symptom — Postgres
    pinned at its ceiling, throughput flat — looks identical to the local
    finding that the database was the bottleneck, but for an entirely different
    reason. Measuring on a throttled instance would produce a number that says
    nothing about the application.

    So: bump this to db.m7g.large (non-burstable) for the load-test run, or
    watch the CPUCreditBalance alarm and record in the results which class each
    number came from. Do not skip both.
  EOT
  type        = string
  default     = "db.t4g.micro"
}

variable "rds_allocated_storage" {
  type    = number
  default = 20
}

variable "api_desired_count" {
  type    = number
  default = 2
}

# --- Load generator ----------------------------------------------------------

variable "enable_loadgen" {
  description = <<-EOT
    Off by default, because it is the single largest cost in this stack: a
    c6i.2xlarge is ~$8.16/day against a total budget of $15-30. Left running
    over a weekend it alone exceeds the budget.

    It exists because a k6 run from a laptop over the internet measures home
    upload bandwidth, not the system. Turn it on, run the test, turn it off the
    same day. `make tf-destroy` verifies nothing is left.
  EOT
  type        = bool
  default     = false
}

variable "loadgen_instance_type" {
  type    = string
  default = "c6i.2xlarge"
}

variable "loadgen_ssh_cidr" {
  description = "Your IP in CIDR form, e.g. 203.0.113.4/32, for SSH to the load generator. Never 0.0.0.0/0."
  type        = string
  default     = ""
}

variable "loadgen_key_name" {
  description = "Existing EC2 key pair name for SSH into the load generator. Empty means no SSH — use SSM Session Manager instead."
  type        = string
  default     = ""
}
