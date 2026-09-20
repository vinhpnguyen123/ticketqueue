terraform {
  required_version = ">= 1.6"

  required_providers {
    aws    = { source = "hashicorp/aws", version = "~> 5.80" }
    random = { source = "hashicorp/random", version = "~> 3.6" }
  }

  # Local state on purpose. Remote state exists to coordinate a team and to
  # survive a lost laptop; this stack lives for a day or two and is then
  # destroyed. An S3 bucket plus a DynamoDB lock table would be two more
  # resources to create, pay for, and forget to delete — which is the exact
  # failure this whole configuration is shaped to avoid.
  #
  # terraform.tfstate is gitignored. If it is lost before `destroy`, see the
  # DESTROY CHECKLIST in README.md: the Project=ticketqueue tag is how every
  # resource is found again.
}

provider "aws" {
  region = var.region

  default_tags {
    tags = local.tags
  }
}

# AWS Budgets is a global service whose endpoint lives in us-east-1. Without
# this alias, a deployment in any other region cannot create the budget alarm
# at all — and the budget alarm is the one resource that must not be optional.
provider "aws" {
  alias  = "us_east_1"
  region = "us-east-1"

  default_tags {
    tags = local.tags
  }
}
