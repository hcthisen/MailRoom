#!/usr/bin/env bash
# Runs the test suite. On Linux/macOS this is plain `go test`. On Windows
# (Git Bash) it cross-compiles tests for Linux and runs them in WSL, because
# some managed Windows machines block unsigned test executables.
# Usage: scripts/test.sh [go test args...]   e.g. scripts/test.sh ./internal/db/ -run TestX -v
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ "${OS:-}" == "Windows_NT" ]]; then
  export GOOS=linux GOARCH=amd64 CGO_ENABLED=0
  exec go test -exec "bash $(pwd)/scripts/wsl-exec.sh" "${@:-./...}"
fi
exec go test "${@:-./...}"
