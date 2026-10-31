# The six test machines and their firewall group, described from what exists in AWS (imported, see imports.tf).
# `terraform plan` must show NO changes before anything is applied. Nothing here may replace or destroy a machine:
# every instance has prevent_destroy, and the image, the public-address setting and user data are ignored after import.
locals {
  machines = {
    forge    = { name = "forge-m7", instance_id = "i-048998a9b6bfb9f7a", private_ip = "172.31.3.197", disk_gb = 12 }
    forge_db = { name = "forge-db2", instance_id = "i-0c933f1eba94356d4", private_ip = "172.31.12.41", disk_gb = 12 }
    cart_db  = { name = "cart-db", instance_id = "i-0a58aa816e28ce229", private_ip = "172.31.1.149", disk_gb = 20 }
    shop     = { name = "shop", instance_id = "i-05f253ef3485d226f", private_ip = "172.31.1.0", disk_gb = 12 }
    loadgen  = { name = "loadgen", instance_id = "i-0b15c95c03e7b0c1d", private_ip = "172.31.2.64", disk_gb = 12 }
    workers  = { name = "workers", instance_id = "i-0ee5a8f53b58e59e5", private_ip = "172.31.4.91", disk_gb = 12 }
  }
  tags = { project = "forge-bench" }
}

resource "aws_security_group" "forge_bench" {
  name        = "forge-bench"
  description = "Forge benchmark"
  vpc_id      = var.vpc_id

  ingress {
    description = ""
    protocol    = "tcp"
    from_port   = 22
    to_port     = 22
    cidr_blocks = var.ssh_cidrs
  }
  ingress {
    description = ""
    protocol    = "-1"
    from_port   = 0
    to_port     = 0
    self        = true
  }
  egress {
    description = ""
    protocol    = "-1"
    from_port   = 0
    to_port     = 0
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.tags, { Name = "forge-bench" })
}

resource "aws_instance" "machine" {
  for_each = local.machines

  ami                    = var.ami
  instance_type          = "m7i-flex.large"
  key_name               = "forge-key"
  subnet_id              = var.subnet_id
  private_ip             = each.value.private_ip
  vpc_security_group_ids = [aws_security_group.forge_bench.id]

  metadata_options {
    http_endpoint               = "enabled"
    http_protocol_ipv6          = "disabled"
    http_put_response_hop_limit = 2
    http_tokens                 = "required"
    instance_metadata_tags      = "disabled"
  }

  root_block_device {
    delete_on_termination = true
    encrypted             = false
    iops                  = 3000
    throughput            = 125
    volume_size           = each.value.disk_gb
    volume_type           = "gp3"
    tags                  = merge(local.tags, { Name = each.value.name })
  }

  tags = merge(local.tags, { Name = each.value.name })

  lifecycle {
    prevent_destroy = true
    ignore_changes  = [ami, associate_public_ip_address, user_data, user_data_replace_on_change]
  }
}

# Starts or stops the whole fleet: terraform apply -var fleet_state=running  (or stopped).
resource "aws_ec2_instance_state" "fleet" {
  for_each    = aws_instance.machine
  instance_id = each.value.id
  state       = var.fleet_state
}

output "machines" {
  description = "Name, instance id and private address of each machine (the public address changes on every start; read it from AWS)"
  value       = { for k, m in aws_instance.machine : k => { id = m.id, private_ip = m.private_ip, state = var.fleet_state } }
}
