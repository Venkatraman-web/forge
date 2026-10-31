# Changelog

## v0.2.0 (unreleased; written 2026-11-01)

Everything here was checked by scripted runs and by hand on AWS test machines and Neon free-tier databases.
The repository has no automated tests. Numbers are in [docs/benchmarks-aws.md](docs/benchmarks-aws.md)
(sections 31 to 71); this is not a production-readiness claim.

### Added

- **Per-type queue limit** (`max_pending` in the config file). The most jobs of one type that may be queued at once;
  a refusal is `503 queue_full` with `job_type` and `limit` in the error. Section 70, ADR 0004.
- **Type-aware claim** and a partial index `jobs_due_type_idx`: the claim query costs 3.4 to 5 times less in the database
  (section 57); the end-to-end rate did not change.
- **ForgeCart checkout saga** with its own inventory database (stock reserved first, order created, then payment;
  reservations released on decline or expiry, orphans cleaned by a reaper), and a bulk release.
- **Chaos tests** (sections 62 to 69) and security tests ([docs/security-tests.md](docs/security-tests.md)).
- **Infrastructure as code:** Ansible playbooks (audit, drift fix, deploy, verify, smoke), Terraform description of the six
  test machines with a start/stop switch, GitHub Actions CI and release workflows, Dependabot
  ([docs/deploy-with-ansible.md](docs/deploy-with-ansible.md), [docs/deploy-with-terraform.md](docs/deploy-with-terraform.md),
  [docs/releasing.md](docs/releasing.md)). The workflows have not run on GitHub yet.

### Changed

- ForgeCart sends **two** background jobs per paid order (`inventory.reconcile`, `order.process`) instead of four.
- `GET /v1/jobs/{id}` answers `404` at once for an id that is not of the form `job_` plus 16 hex digits.
- Minimum Go version is **1.25** (go.mod, Dockerfile builder image `golang:1.26-alpine`).
- ForgeCart under database outages answers `503` with a JSON error instead of failing in other ways, and unknown routes
  answer a JSON `404`.

### Fixed

- A checkout that failed after reserving stock could leave the reservation held (found by the first chaos test; fixed and rerun).
- Reservation cleanup after a shop-database outage (section 69).

### Upgrade notes (read before upgrading)

- **Two new Forge migrations** (`20261030002930_jobs_due_type_index`, `20261030120000_type_max_pending`). The second replaces
  the SQL functions `forge_submit_job` and `forge_submit_jobs`. Old Forge binaries do not work against the new functions and
  the reverse; rolling back needs the old functions restored. Apply with `atlas migrate apply` before starting the new binary.
- **ForgeCart needs an inventory database** (`inventory_migrations/`, `FORGECART_INVENTORY_DATABASE_URL`); the shop migration that
  drops `products` from the orders database is not reversible without a backup.
- New config field `max_pending` and new optional error fields `job_type` and `limit` are additive.

### Known limitations

- Peak end-to-end rate about 1,000 orders/s was measured on Neon free-tier databases (two runs per variant, one session);
  it varied by about 25% between fresh Neon projects for reasons not isolated. Neon free quotas ran out several times.
- One stranded payment was seen once and not explained (the sweeper clears it); about 68 HTTP 500 per outage test from in-flight resets.
- The per-type limit counts with a bounded query, not a counter table; real Neon pause/resume, webhook and TLS paths are untested.
- Five Go standard-library vulnerabilities reported by govulncheck are accepted unfixed (older Go toolchain on purpose).
- No TLS, no tenant isolation, one Forge process validated, EBS volumes unencrypted, Terraform state only on the author's machine.

## v0.1.0 (2026-10-25)

First published version.
