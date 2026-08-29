#!/usr/bin/env bash
# Compiles decode-check.swift against the app's real Models.swift and
# APIClient.swift (not copies) and runs it, so a decoder regression fails here
# before it fails in the app. Points at the live daemon by default; set
# BOTNET_API to check another server.
#
#   ./dev/decode-check.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bin="$here/build/decode-check"
export BOTNET_API="${BOTNET_API:-http://127.0.0.1:8730}"

mkdir -p "$here/build"
swiftc -o "$bin" \
    "$here/Sources/Models.swift" \
    "$here/Sources/APIClient.swift" \
    "$here/dev/decode-check.swift"

exec "$bin"
