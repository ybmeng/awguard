#!/usr/bin/env bash
# Kills every running botnet process, rebuilds server and app from the current
# tree, and restarts both — so what's running is never stale relative to the
# source. Run at every checkpoint before the user tests.
#
#   ./dev/checkpoint.sh
#
# Refuses to deploy a red tree: the Go gate must pass before anything restarts.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
repo="$(cd "$here/../.." && pwd)"
service="gui/$(id -u)/com.std.bgservices"

cd "$repo"
go build ./go/... >/dev/null
go test ./go/botnet/... >/dev/null
echo "gate: go build + go test green"

go build -o "$HOME/bin/stdd" ./go/std/stdd
echo "built ~/bin/stdd"

# Stray hand-run daemons hold ports and (pre-lock) could sweep the live DB.
pkill -x botnetd 2>/dev/null && echo "killed stray botnetd" || true

launchctl kickstart -k "$service"
for _ in $(seq 1 50); do
    curl -sf -m 1 "http://127.0.0.1:8730/v1/bots" >/dev/null && break
    sleep 0.2
done
curl -sf -m 2 "http://127.0.0.1:8730/v1/bots" >/dev/null
echo "restarted $service (pid $(pgrep -x stdd))"

osascript -e 'quit app "BotNet"' 2>/dev/null || true
for _ in $(seq 1 25); do
    pgrep -f "BotNet.app/Contents/MacOS/BotNet" >/dev/null || break
    sleep 0.2
done
"$here/dev/install-app.sh"
open -a BotNet
echo "checkpoint: server $(date -r "$HOME/bin/stdd" +%H:%M:%S), app $(date -r /Applications/BotNet.app/Contents/MacOS/BotNet +%H:%M:%S), all fresh"
