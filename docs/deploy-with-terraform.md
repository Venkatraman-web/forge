# The test machines in Terraform

`infra/terraform/` describes the six EC2 test machines (`forge-m7`, `forge-db2`, `cart-db`, `shop`, `loadgen`, `workers`) and their firewall
group `forge-bench`, exactly as they were created by hand, and gives one switch to start or stop all six. Ansible (see
[deploy-with-ansible.md](deploy-with-ansible.md)) puts the software on them; Terraform only owns the machines themselves.

## What it owns
- The six instances (type `m7i-flex.large`, the key pair `forge-key` by name, the subnet, the root disk, the metadata settings, the tags).
- The security group `forge-bench`: SSH from the addresses in `terraform.tfvars` and all traffic between members of the group.
- `aws_ec2_instance_state`: the state the machines are put in, from the variable `fleet_state` (`running` or `stopped`, default `stopped`).

## What it does not own
The key pair itself, the VPC and subnet, the image (Ubuntu) the machines were made from, the disks' contents, and anything outside AWS (the Neon projects).
After import the image, the public-address setting and user data are ignored, so a change to them can never replace a machine.

## Commands (from `infra/terraform/`, with the AWS CLI signed in: `aws login`, region `ap-southeast-2`)
```bash
terraform plan                                  # read-only: must say "No changes" unless someone changed AWS by hand
terraform apply -var fleet_state=running        # start the six machines (this costs credits until they are stopped)
terraform apply -var fleet_state=stopped        # stop them (disks stay, no instance charge; public addresses are released)
```
The public addresses change on every start; `infra/ansible/make-inventory.sh` reads the current ones from AWS.

## Safeguards
- Every instance has `lifecycle { prevent_destroy = true }`: a plan that would delete or replace one makes Terraform stop with an error.
- The state file (`terraform.tfstate`), the provider directory and `terraform.tfvars` (the SSH addresses) are git-ignored. **The state file exists only on the machine that ran Terraform**; if it is lost, the resources can be imported again from `imports.tf` (the instance ids are in `main.tf`).
- Only the start/stop switch is meant to be applied routinely. Read any other plan before applying it.

## What happened when it was set up (2026-11-01)
The configuration was written from what exists in AWS (Terraform's generated import configuration, then cleaned up into one `for_each` over the six machines). The first `plan` said `7 to import, 6 to add, 0 to change, 0 to destroy`; the apply recorded the imports and the six state records without touching a machine; a second `plan` said "No changes". The switch was tested once in both directions: the six machines went from `stopped` to `running` in about two minutes, answered Ansible, and came back with all their services running (enabled at boot); the switch back to `stopped` stopped all six and the plan stayed clean.

## Not done
The optional rule limiting SSH to a single address (SSH is already limited to the four addresses in `terraform.tfvars`), encrypting the EBS volumes (they are not encrypted; turning it on replaces the disks, so it needs a plan of its own), Elastic IPs (not used: the public address changes on every start), and any remote state or locking (a single person uses this).
