#!/usr/bin/env bash
# Quick verification gate for every module under go/.
# Each module's tests are required to be fast (sub-second) so this stays quick.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "==> go vet ./go/..."
go vet ./go/...

echo "==> go test ./go/..."
go test ./go/... "$@"

echo "==> build stdd"
go build -o /dev/null ./go/std/stdd

echo "OK: all go/ modules verified"
