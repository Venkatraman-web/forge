variable "fleet_state" {
  description = "running or stopped: the state the six machines are put in by aws_ec2_instance_state"
  type        = string
  default     = "stopped"
  validation {
    condition     = contains(["running", "stopped"], var.fleet_state)
    error_message = "fleet_state must be running or stopped."
  }
}

variable "ssh_cidrs" {
  description = "The addresses (CIDR) allowed to reach SSH on the machines. Kept in terraform.tfvars, which is not committed."
  type        = list(string)
}

variable "ami" {
  description = "The image the six machines were created from (Ubuntu). Changing it would replace the machines, so it is ignored after import."
  type        = string
  default     = "ami-01e91e94accc42eec"
}

variable "subnet_id" {
  type    = string
  default = "subnet-05264423f80c77139"
}

variable "vpc_id" {
  type    = string
  default = "vpc-0e0ee9f45587751e3"
}
