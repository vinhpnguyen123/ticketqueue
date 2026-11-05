output "alb_dns_name" {
  description = "Public entry point. http://<this>/v1/health should answer with the deployed git SHA."
  value       = aws_lb.main.dns_name
}

output "api_url" {
  value = "http://${aws_lb.main.dns_name}"
}

output "rds_endpoint" {
  description = "Reachable only from inside the VPC."
  value       = aws_db_instance.main.address
}

output "redis_endpoint" {
  value = aws_elasticache_cluster.main.cache_nodes[0].address
}

output "ecs_cluster_name" {
  value = aws_ecs_cluster.main.name
}

output "ecr_registry" {
  description = "Pass to `make tf-push`."
  value       = local.registry
}

output "loadgen_public_ip" {
  value = var.enable_loadgen ? aws_instance.loadgen[0].public_ip : null
}

# NO `aws ecs execute-command` LINE, ON PURPOSE.
#
# The runtime images are distroless/static: there is no /bin/sh, so ECS Exec
# has nothing to run and the command fails with an error about the shell rather
# than about the thing being debugged. That was the trade made when the images
# were built — a 25MB image with no package manager and no shell.
#
# What to use instead, and it covers the cases exec would have:
output "debugging" {
  description = "How to inspect a running deployment without a shell in the container."
  value = {
    logs_api      = "aws logs tail /ecs/${local.name}/api --follow --region ${var.region}"
    logs_worker   = "aws logs tail /ecs/${local.name}/worker --follow --region ${var.region}"
    logs_pooler   = "aws logs tail /ecs/${local.name}/pgbouncer --follow --region ${var.region}"
    version       = "curl -s http://${aws_lb.main.dns_name}/v1/health   # returns the git SHA that is running"
    readiness     = "curl -s http://${aws_lb.main.dns_name}/v1/health/ready   # pings Postgres and Redis"
    task_status   = "aws ecs describe-services --cluster ${aws_ecs_cluster.main.name} --services api worker payment-service --region ${var.region} --query 'services[].{name:serviceName,running:runningCount,desired:desiredCount}'"
    stopped_tasks = "aws ecs list-tasks --cluster ${aws_ecs_cluster.main.name} --desired-status STOPPED --region ${var.region}"
    db_internals  = "Performance Insights in the RDS console — the managed equivalent of pg_stat_statements"
  }
}

output "cost_watch" {
  description = "Run this before walking away. Anything listed is still billing."
  value       = "aws resourcegroupstaggingapi get-resources --tag-filters Key=Project,Values=ticketqueue --region ${var.region} --query 'ResourceTagMappingList[].ResourceARN' --output table"
}
