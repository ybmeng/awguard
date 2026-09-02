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
#
# Whoever edits swift/botnet/Sources should run this alongside the two Release
# builds, so it deliberately depends on NOTHING in swift/botnet-ios but its own
# location: it needs no xcodeproj, no simulator, and no iOS target. Copying this
# one file into a tree is enough to have the gate.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
shared="$here/../botnet/Sources"

# The files the iOS app compiles from the Mac's tree.
files=(Models.swift APIClient.swift Store.swift Transcript.swift DesignSystem.swift)

paths=()
missing=()
for file in "${files[@]}"; do
    if [ -f "$shared/$file" ]; then
        paths+=("$shared/$file")
    else
        missing+=("$file")
    fi
done
# A renamed or moved shared file has to say so in one line. Left to swiftc it
# reads as a compiler error about a file it cannot open, which looks like a
# broken script rather than a shared file that walked away from the iOS target.
if [ ${#missing[@]} -ne 0 ]; then
    echo "shared-check: missing from $shared: ${missing[*]}" >&2
    echo "shared-check: update the list here and the sources: entries in swift/botnet-ios/project.yml" >&2
    exit 1
fi

sdk="$(xcrun --sdk iphonesimulator --show-sdk-path)"
# The simulator triple follows the host arch: arm64 on Apple silicon.
arch="$(uname -m)"

xcrun swiftc \
  -sdk "$sdk" \
  -target "${arch}-apple-ios17.0-simulator" \
  -parse-as-library \
  -emit-library -o /dev/null \
  -Xclang-linker -isysroot -Xclang-linker "$sdk" \
  "${paths[@]}"

echo "shared layer compiles for iOS (${#paths[@]} files)"
