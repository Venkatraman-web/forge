#!/usr/bin/env bash
# Entry point of the ForgeCart/Forge load-testing suite. See README.md.
#   ./run.sh list | preflight | dry-run [flags] | run [flags]
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
cd "$repo"
exec go run ./forgecart/test_files "$@"
