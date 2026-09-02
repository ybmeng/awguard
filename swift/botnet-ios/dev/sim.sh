#!/usr/bin/env bash
# Builds BotNetMobile, installs it on a simulator and launches it against a
# chosen botnetd. This is the phone's answer to the Mac's dev/snapshot.sh:
# `screencapture` is blocked and there is no offscreen renderer for iOS, so the
# only honest way to look at a screen is to run the app on a real simulator and
# `xcrun simctl io booted screenshot`.
#
#   ./dev/sim.sh                                   # build, install, launch on 8730
#   BOTNET_API=http://127.0.0.1:8813 ./dev/sim.sh  # …against a scratch daemon
#   SIM="BotNet-iPhone" ./dev/sim.sh
#
# The base URL is written into the app's own defaults under the SAME key its
# @AppStorage reads (see BaseURL.key), because the app has no launch
# environment to inherit one from.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
sim="${SIM:-BotNet-iPhone}"
api="${BOTNET_API:-http://127.0.0.1:8730}"
bundle="com.anywatch.botnet.ios"
derived="$here/build/dd"

cd "$here"
xcodegen generate
xcodebuild -project BotNetMobile.xcodeproj -scheme BotNetMobile \
  -destination "platform=iOS Simulator,name=$sim" \
  -configuration Debug -derivedDataPath "$derived" build

app="$derived/Build/Products/Debug-iphonesimulator/BotNetMobile.app"
[ -d "$app" ] || { echo "no app at $app" >&2; exit 1; }

udid="$(xcrun simctl list devices | awk -v s="$sim" '$0 ~ s {match($0, /[0-9A-F-]{36}/); print substr($0, RSTART, RLENGTH); exit}')"
[ -n "$udid" ] || { echo "no simulator named $sim" >&2; exit 1; }

xcrun simctl boot "$udid" 2>/dev/null || true
xcrun simctl bootstatus "$udid" -b

xcrun simctl install "$udid" "$app"
xcrun simctl spawn "$udid" defaults write "$bundle" botnetBaseURL "$api"
xcrun simctl terminate "$udid" "$bundle" 2>/dev/null || true
xcrun simctl launch "$udid" "$bundle"

echo "running against $api on $sim ($udid)"
echo "screenshot with: xcrun simctl io $udid screenshot <path>.png"
