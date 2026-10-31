# Putting Forge on GitHub and publishing the Docker image

The repository is `github.com/Venkatraman-web/forge`. Nothing here has run yet: the release workflow has never
executed, and the first push is done by hand. This page records what was prepared and what to do.

## 1. Already done before the first push

1. **What is public.** Everything tracked by git. The published copy is a single fresh commit, so the
   development history (older commits, their author address, and files removed earlier) is not part of it.
   `CLAUDE.md` and `docs/history/` are kept private (listed in `.gitignore`, not tracked).
2. **Secrets.** `.env`, keys, certificates and cloud credentials are git-ignored, and no key, password or token
   is in the tracked files (only placeholders such as `CHANGE_ME_before_first_start`).
3. **Licence.** MIT. The `LICENSE` file lives on GitHub (it is listed in `.gitignore` here, so it is never
   tracked locally; a `git pull` brings it in).
4. **Go module path.** `go.mod` says `module github.com/Venkatraman-web/forge`, so other developers can
   `go get github.com/Venkatraman-web/forge/sdk/workerruntime`. All imports were updated and the code builds and vets.

## 2. Push

The `LICENSE` file was created on GitHub first, so the remote is not empty. In the prepared folder
(a fresh git repository holding the whole project, not yet committed), set your identity, commit, put your
commit on top of GitHub's licence commit, and push:

```bash
git config user.name "Your Name"
git config user.email "ID+Venkatraman-web@users.noreply.github.com"   # GitHub, Settings, Emails
git add -A
git commit -m "Initial commit"
git remote add origin https://github.com/Venkatraman-web/forge.git
git pull --rebase origin main     # brings in GitHub's LICENSE commit under yours
git push -u origin main
```

GitHub then runs `.github/workflows/ci.yml` (formatting, vet and build). It has never run before, so expect to
fix a small surprise or two.

## 3. Publish the image to GitHub's container registry

`.github/workflows/release.yml` builds the image for amd64 and arm64 and pushes it to
`ghcr.io/venkatraman-web/forge` when you push a version tag:

```bash
git tag v0.1.0
git push origin v0.1.0
```

After the **first** push, open your profile on GitHub, Packages, `forge`, Package settings, and set the
visibility to **Public** (new packages start private). Then anyone can run:

```bash
docker pull ghcr.io/venkatraman-web/forge:0.1.0
```

To make `compose.yaml` use the published image instead of building from source, change the `forge:` service
to `image: ghcr.io/venkatraman-web/forge:0.1.0` and remove `build: .`.

### Why this registry

GitHub Container Registry (ghcr.io) is free for public images, lives next to the code and the CI that builds
it, needs no separate account or secret (the workflow uses its own token), and links the image to the
repository. Docker Hub works equally well for pulling but needs its own account and a stored access token.
Either way, **customers also need a database**: the image alone is not a complete setup, so ship
`compose.yaml` and `.env.example` with it (they are in the repository) and keep
[deploy-on-aws.md](deploy-on-aws.md) as the guide.

## 4. What the workflows check (added 2026-11-01; none of this has run on GitHub yet)

- **`ci.yml`, on every push and pull request:** formatting, `go vet` and build; a **migrations** job (`atlas migrate validate`, and `atlas migrate diff` must produce no new file, so the GORM model and the migration directory agree); an **image** job (the Docker image builds and, started without a database, stops with a message about `FORGE_DATABASE_URL`); an **infra** job (Terraform `fmt` and `validate`, a syntax check of every Ansible playbook, and `ansible-lint`; it touches no machine and needs no AWS credentials), and a **vulnerabilities** job (`govulncheck`) that reports but never fails the build, because a finding in the Go standard library is fixed by a newer Go patch release (the `Dockerfile` builds with `golang:1.26-alpine`, which tracks the latest patch).
- **`release.yml`, on a `v*` tag:** the same formatting, vet and build checks first; then the multi-architecture image with provenance and an SBOM; then a GitHub release with notes generated from the commits.
- **`dependabot.yml`:** weekly update proposals for the Go modules, the Actions, the Docker base images and the Terraform provider.

Things a release must say, because they break an upgrade: a database migration that replaces functions (the new Forge needs the new migration, an old Forge cannot run against the migrated database, and a rollback needs the old functions restored), and the minimum Go version for people who use the SDK (`go.mod`).
