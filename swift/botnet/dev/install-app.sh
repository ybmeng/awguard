#!/usr/bin/env bash
# Builds BotNet.app in Release and installs it to /Applications, so it survives
# a `clean build` and is launchable from Spotlight instead of only from
# DerivedData.
#
#   ./dev/install-app.sh
#
# Re-running replaces the installed copy in place. Quit the app first; a running
# binary cannot be overwritten.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dest="${INSTALL_DIR:-/Applications}"

cd "$here"
xcodegen generate >/dev/null

xcodebuild -project BotNet.xcodeproj -scheme BotNet -configuration Release build 2>&1 \
    | grep -E "error:|BUILD (SUCCEEDED|FAILED)"

built="$(xcodebuild -project BotNet.xcodeproj -scheme BotNet -configuration Release \
    -showBuildSettings 2>/dev/null | awk '/ BUILT_PRODUCTS_DIR =/{print $3}')/BotNet.app"

if [ ! -d "$built" ]; then
    echo "install-app: no app at $built" >&2
    exit 1
fi

if pgrep -f "BotNet.app/Contents/MacOS/BotNet" >/dev/null; then
    echo "install-app: BotNet is running — quit it first" >&2
    exit 1
fi

rm -rf "$dest/BotNet.app"
cp -R "$built" "$dest/BotNet.app"
echo "installed $dest/BotNet.app"
echo "launch it with: open -a BotNet"
