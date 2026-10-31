# Deploying and checking the test machines with Ansible

The files are in `infra/ansible/`. They describe what the six test machines (Forge, Forge's database, the shop, the workers,
the load generator and the helper machine `cart-db`) should look like, and check that they do. This page says what exists and what
has been tried. **All of it has been run against the machines (2026-11-01): the audit, fix-drift, a dry run and a real deploy of release `v0.2.0-rc1`, verify and smoke.** The first dry runs failed three times because of mistakes in the files (see the end of this page), so expect a small surprise or two on the next change as well.

## Prerequisites
- `ansible-core` (the project used it from an isolated environment, `~/.forge-aws/venv`) and the AWS CLI signed in (`aws login`), region `ap-southeast-2`.
- The machines started (`terraform apply -var fleet_state=running` in `infra/terraform/`, see [deploy-with-terraform.md](deploy-with-terraform.md)). Their public addresses change on every start, so the inventory is built from AWS each time.

## Everyday commands (from `infra/ansible/`)
```bash
./make-inventory.sh                        # reads the running machines from AWS (read-only) into inventory/hosts.ini (git-ignored)
ansible-playbook playbooks/audit.yml       # read-only: disk, services, the hash of each running binary, file modes, listening ports
ansible-playbook playbooks/fix-drift.yml --check --diff   # dry run of the desired service state, then without --check
```

## Deploying a version (run once, 2026-11-01)
```bash
./build.sh                                  # Linux binaries and SHA256SUMS into build/ (git-ignored)
./make-vars.sh                              # Neon host names for the /etc/hosts pins, from ~/.forge-aws/secrets (git-ignored result: inventory/group_vars/all/neon.local.yml)
ansible-playbook playbooks/deploy.yml -e release_label=v0.2.0 --check --diff
ansible-playbook playbooks/deploy.yml -e release_label=v0.2.0
ansible-playbook playbooks/verify.yml       # checks only
ansible-playbook playbooks/smoke.yml        # a few read-only requests
```
A deploy installs each binary as `<name>-<release_label>` and points the path the service starts at it, so the previous release stays on disk
for a rollback. After installing, every role **fails the run** if the binary a service is really running (its hash) is not the one built here, or if
its health check does not answer.

## What is deliberately not managed
- `/etc/forge.env`, `/etc/cart.env`, `/etc/payments.env`, `/etc/cartw.env`: they hold database URLs and keys. Only their file mode (0600) is enforced.
  Nothing secret is in the repository; the generated `neon.local.yml` holds host names only.
- The systemd unit files of the shop, the payment service, the dispatchers and the workers (only Forge's `ExecStart` line is set). They are as they were on the machines; bringing them under Ansible needs them copied from the machines first.
- The databases and their schemas (Atlas migrations), the Neon projects, the load and chaos test scripts, and the machines themselves (the plan for Terraform is not started).

## What was found and fixed with it (2026-11-01)
The first audit showed, on machines that had been restarted: the order worker and the second worker set not running, two of the four dispatchers not running (not enabled at boot), disks 86% and 87% full (4 to 6 GB of plain-text syslog written by the services during the test runs), and an unused PostgreSQL listening on the Forge machine. `fix-drift.yml` enables and starts the services, caps the journal at 200 MB, and, only with tags, empties the oversized syslog files and stops the unused PostgreSQL. All of that was applied and the audit repeated: disks at 29% to 41%, all dispatchers and workers running.

## Things that went wrong while building it
Ansible's shared SSH connections stalled several runs (now off); a task that writes into a folder failed because the folder did not exist (a dry run does not catch that); a task name with an unquoted colon made the file unreadable. A dry run (`--check`) shows what would change; it does not prove the change will work.

## The first real deploy (2026-11-01, release `v0.2.0-rc1`)
A dry run and then a real run of `deploy.yml` succeeded on all six machines; every role's check passed (each service runs exactly the binary built here and answers its health check), then `verify.yml` and `smoke.yml` passed with no changes. A 60 s load run on the deployed build (400 shoppers, no disturbance) gave 1,204 orders/s, 73,426 orders, 0 failed, 26 of 26 integrity checks, drained, and 132,296 Forge jobs succeeded with none failed. It took about 11 minutes for the deploy (each task opens a new SSH connection, because shared connections stalled earlier runs).

Three mistakes found only by running against the machines (the syntax check and the linter passed all of them): the variable files (`group_vars`) must sit next to the inventory (`inventory/group_vars/`), not next to the playbook folder's parent, or nothing in them is loaded; making task names start with a capital letter also changed the service names inside tasks (`forge` became `Forge`, and Linux service names are case-sensitive); and a dry run skips shell commands, so the step that reads the hash of the running binary has to be told to run anyway, and the comparison with the built binary only makes sense in a real run.
