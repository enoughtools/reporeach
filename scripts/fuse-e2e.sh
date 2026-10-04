#!/bin/bash
# Disposable Linux FUSE validation; never installs a driver on the Mac host.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export REPOREACH_FUSE_TEST_PATTERN="${REPOREACH_FUSE_TEST_PATTERN:-^TestE2E}"
export REPOREACH_FUSE_TEST_TIMEOUT="${REPOREACH_FUSE_TEST_TIMEOUT:-20m}"
export REPOREACH_FUSE_TEST_COUNT="${REPOREACH_FUSE_TEST_COUNT:-1}"
if [ "$(uname -s)" = Linux ] && [ -c /dev/fuse ]; then
  cd "$ROOT"
  AFS_RUN_E2E_TESTS=1 go test -count=1 -run '^TestFUSEMountSmoke$' -timeout=2m -v .
  AFS_RUN_E2E_TESTS=1 go test -count="$REPOREACH_FUSE_TEST_COUNT" -run "$REPOREACH_FUSE_TEST_PATTERN" -timeout="$REPOREACH_FUSE_TEST_TIMEOUT" -v .
else
  command -v docker >/dev/null || { echo "FUSE tests require a Linux /dev/fuse device or Docker." >&2; exit 1; }
  docker run --rm --device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor=unconfined \
    -e REPOREACH_FUSE_TEST_PATTERN -e REPOREACH_FUSE_TEST_TIMEOUT -e REPOREACH_FUSE_TEST_COUNT \
    -v "$ROOT:/source:ro" -w /workspace \
    golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d bash -euc '
      apt-get update -qq
      apt-get install -y --no-install-recommends fuse3 git ca-certificates
      test -c /dev/fuse
      cp -a /source/internal /source/cmd /source/go.mod /source/go.sum /workspace/
      cp /source/*.go /workspace/
      export AFS_RUN_E2E_TESTS=1
      go test -count=1 -run "^TestFUSEMountSmoke$" -timeout=2m -v .
      go test -count="$REPOREACH_FUSE_TEST_COUNT" -run "$REPOREACH_FUSE_TEST_PATTERN" -timeout="$REPOREACH_FUSE_TEST_TIMEOUT" -v .
    '
fi
