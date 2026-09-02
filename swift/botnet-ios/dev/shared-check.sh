#!/usr/bin/env bash
# Proves the files swift/botnet-ios shares with the Mac app still compile for
# iOS. This is the gate that keeps a shared file honest: the Mac target builds
# them against AppKit, so an unguarded NSColor/NSWorkspace only surfaces here.
#
#   ./dev/shared-check.sh
#
# It compiles the shared list ALONE — no iOS-only sources — so a failure is
# always the shared layer's, never the phone UI's. Keep the list in step with
# the `sources:` entries project.yml points at ../botnet/Sources.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
shared="$here/../botnet/Sources"

sdk="$(xcrun --sdk iphonesimulator --show-sdk-path)"
# The simulator triple follows the host arch: arm64 on Apple silicon.
arch="$(uname -m)"

xcrun swiftc \
  -sdk "$sdk" \
  -target "${arch}-apple-ios17.0-simulator" \
  -parse-as-library \
  -emit-library -o /dev/null \
  -Xclang-linker -isysroot -Xclang-linker "$sdk" \
  "$shared/Models.swift" \
  "$shared/APIClient.swift" \
  "$shared/Store.swift" \
  "$shared/Transcript.swift" \
  "$shared/DesignSystem.swift"

echo "shared layer compiles for iOS"
