#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

export BEADS_TEST_ENV_RUN_DOLT=1

source "$REPO_ROOT/.buildflags"
source "$REPO_ROOT/scripts/ci/lib/timing.sh"
source "$REPO_ROOT/scripts/ci/lib/test-env.sh"

cd "$REPO_ROOT"

beads_test_env_enter

GO_TEST_PKG_PARALLEL="${GO_TEST_PKG_PARALLEL:-4}"
GO_TEST_PARALLEL="${GO_TEST_PARALLEL:-4}"

ci_time "pr-core go test" -- \
    go test -p "$GO_TEST_PKG_PARALLEL" -parallel "$GO_TEST_PARALLEL" -race -timeout 20m ./...
