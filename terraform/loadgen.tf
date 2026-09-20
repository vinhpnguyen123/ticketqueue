# Optional, and off by default, because it is the most expensive thing here:
# ~$8.16/day against a $15-30 total. Turn it on, run the test, destroy it.
#
# It exists because a k6 run from a laptop over the internet measures home
# upload bandwidth and a round trip through an ISP, not the system. The Week 6
# numbers were also distorted by k6 competing for the same eight cores as
# everything it was loading; this box separates the two for the first time.
#
# It also carries Prometheus and Grafana. CloudWatch and Performance Insights
# cover the infrastructure, but neither can produce
# purchase_phase_duration_seconds{phase} or db_pool_in_use — the application
# metrics the whole Week 6 analysis was built on. Without them the AWS run
# yields a throughput number with no breakdown of where the time went, which
# is the failure §11a exists to prevent. Running them here costs nothing extra
# and they die with the instance.

data "aws_ami" "al2023_arm" {
  count       = var.enable_loadgen ? 1 : 0
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["al2023-ami-2023.*-arm64"]
  }
}

data "aws_ami" "al2023_x86" {
  count       = var.enable_loadgen ? 1 : 0
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["al2023-ami-2023.*-x86_64"]
  }
}

locals {
  # c6i is x86; c7g/c6g are ARM. Pick the AMI that matches whatever instance
  # type was asked for, rather than assuming.
  loadgen_is_arm = var.enable_loadgen ? can(regex("^c[0-9]+g", var.loadgen_instance_type)) : false
  loadgen_ami = var.enable_loadgen ? (
    local.loadgen_is_arm ? data.aws_ami.al2023_arm[0].id : data.aws_ami.al2023_x86[0].id
  ) : ""
}

resource "aws_instance" "loadgen" {
  count = var.enable_loadgen ? 1 : 0

  ami                         = local.loadgen_ami
  instance_type               = var.loadgen_instance_type
  subnet_id                   = aws_subnet.public[0].id
  vpc_security_group_ids      = [aws_security_group.loadgen[0].id]
  associate_public_ip_address = true
  key_name                    = var.loadgen_key_name == "" ? null : var.loadgen_key_name
  iam_instance_profile        = aws_iam_instance_profile.loadgen[0].name

  user_data = <<-EOT
    #!/bin/bash
    set -euxo pipefail

    # THE KERNEL LIMIT THAT COST 16% OF CONNECTIONS LOCALLY.
    # macOS ships kern.ipc.somaxconn=128, and at 10,000 VUs the accept queue
    # overflowed: k6 sent 10,000 requests and the API logged 8,351. Linux
    # defaults to 4096, which is already better, but it is set explicitly here
    # so the ceiling this run finds belongs to the application and not to a
    # default nobody checked.
    sysctl -w net.core.somaxconn=65535
    sysctl -w net.ipv4.tcp_max_syn_backlog=65535
    # A load generator opens tens of thousands of short-lived outbound
    # connections and will exhaust ephemeral ports and file descriptors long
    # before the app is stressed.
    sysctl -w net.ipv4.ip_local_port_range="1024 65535"
    sysctl -w net.ipv4.tcp_tw_reuse=1
    echo '* soft nofile 1048576' >> /etc/security/limits.conf
    echo '* hard nofile 1048576' >> /etc/security/limits.conf
    cat >> /etc/sysctl.d/99-loadgen.conf <<'SYSCTL'
    net.core.somaxconn = 65535
    net.ipv4.tcp_max_syn_backlog = 65535
    net.ipv4.ip_local_port_range = 1024 65535
    net.ipv4.tcp_tw_reuse = 1
    SYSCTL

    dnf install -y docker git
    systemctl enable --now docker
    usermod -aG docker ec2-user

    # k6 from the official repo rather than a vendored binary.
    cat > /etc/yum.repos.d/k6.repo <<'REPO'
    [k6]
    name=k6
    baseurl=https://dl.k6.io/rpm
    enabled=1
    gpgcheck=0
    REPO
    dnf install -y k6 || echo "k6 install failed; scp the binary instead"

    # The scripts are copied up by `make tf-loadgen-sync` rather than baked in,
    # so a change to scripts/k6 does not require replacing the instance.
    mkdir -p /opt/ticketqueue && chown ec2-user:ec2-user /opt/ticketqueue
  EOT

  user_data_replace_on_change = true

  tags = { Name = "${local.name}-loadgen" }

  depends_on = [aws_budgets_budget.monthly]
}

# SSM Session Manager, so the box is reachable without opening SSH at all when
# loadgen_ssh_cidr is left empty.
resource "aws_iam_role" "loadgen" {
  count = var.enable_loadgen ? 1 : 0
  name  = "${local.name}-loadgen"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "loadgen_ssm" {
  count      = var.enable_loadgen ? 1 : 0
  role       = aws_iam_role.loadgen[0].name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_instance_profile" "loadgen" {
  count = var.enable_loadgen ? 1 : 0
  name  = "${local.name}-loadgen"
  role  = aws_iam_role.loadgen[0].name
}
