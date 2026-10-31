#!/bin/bash
# Builds the Linux binaries that the playbooks deploy, into infra/ansible/build/ (git-ignored), and writes
# build/SHA256SUMS. The playbooks compare the hash of the binary a service is running with the hash of the file built here.
set -euo pipefail
cd "$(dirname "$0")/../.."          # the repository root
out=infra/ansible/build
mkdir -p "$out"
build() { GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$out/$2" "$1"; }
build ./cmd/forge forge
build ./forgecart/cmd/forgecart forgecart
build ./forgecart/cmd/forgecart-payments forgecart-payments
build ./forgecart/cmd/forgecart-dispatcher forgecart-dispatcher
build ./forgecart/cmd/forgecart-inventory-worker forgecart-inventory-worker
build ./forgecart/cmd/forgecart-order-worker forgecart-order-worker
( cd "$out" && sha256sum forge forgecart forgecart-payments forgecart-dispatcher forgecart-inventory-worker forgecart-order-worker > SHA256SUMS )
echo "built into $out:"; cat "$out/SHA256SUMS" | awk '{print substr($1,1,12), $2}'
