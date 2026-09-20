# The prompt did not mention a registry, but Fargate cannot pull from a laptop.
# Four repositories: the three services plus a one-shot migration image.
locals {
  images = toset(["api", "worker", "payment-service", "migrate"])
}

resource "aws_ecr_repository" "app" {
  for_each = local.images

  name = "${local.name}/${each.value}"

  # force_delete so `terraform destroy` removes repositories that still hold
  # images. Without it, destroy fails and leaves four repositories billing for
  # storage indefinitely.
  force_delete = true

  image_scanning_configuration {
    scan_on_push = true
  }
}

# One untagged image is kept for rollback; the rest are expired. Storage is
# cheap but not free, and nothing here should outlive the stack.
resource "aws_ecr_lifecycle_policy" "app" {
  for_each   = aws_ecr_repository.app
  repository = each.value.name

  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep the 3 most recent images"
      selection    = { tagStatus = "any", countType = "imageCountMoreThan", countNumber = 3 }
      action       = { type = "expire" }
    }]
  })
}
