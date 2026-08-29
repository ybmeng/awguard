#!/usr/bin/env bash
# Renders the real UI to PNGs so a change can be looked at, diffed, and re-run.
# Requires the demo server from dev/seed-demo.sh to be up on $BOTNET_API.
#
#   ./dev/seed-demo.sh &            # once
#   ./dev/snapshot.sh               # build/snapshots/{light,dark}.png
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${SNAPSHOT_DIR:-$here/build/snapshots}"
export BOTNET_API="${BOTNET_API:-http://127.0.0.1:8731}"

if ! curl -sf -m 3 "$BOTNET_API/v1/bots" >/dev/null; then
    echo "snapshot: no server at $BOTNET_API — run ./dev/seed-demo.sh first" >&2
    exit 1
fi

mkdir -p "$out"
cd "$here"
xcodegen generate >/dev/null
xcodebuild -project BotNet.xcodeproj -scheme Snapshot -configuration Debug build 2>&1 \
    | grep -E "error:|BUILD (SUCCEEDED|FAILED)"

bin="$(xcodebuild -project BotNet.xcodeproj -scheme Snapshot -configuration Debug \
    -showBuildSettings 2>/dev/null | awk '/ BUILT_PRODUCTS_DIR =/{print $3}')/snapshot"

"$bin" --out "$out/light.png"
"$bin" --out "$out/dark.png" --dark
