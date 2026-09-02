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

if ! xcrun swiftc \
  -sdk "$sdk" \
  -target "${arch}-apple-ios17.0-simulator" \
  -parse-as-library \
  -emit-library -o /dev/null \
  -Xclang-linker -isysroot -Xclang-linker "$sdk" \
  "${paths[@]}"
then
    # A tree that predates the iOS platform fork fails here for a reason that
    # has nothing to do with the change under test, and an adopter who cannot
    # tell that apart from a real AppKit leak will rationally start ignoring
    # this gate. Say which one it is.
    if ! grep -q "canImport(AppKit)" "$shared/DesignSystem.swift"; then
        echo >&2
        echo "shared-check: this tree has no iOS platform fork in DesignSystem.swift," >&2
        echo "shared-check: so the failure above is EXPECTED and is not caused by your change." >&2
        echo "shared-check: the gate is only meaningful once that fork has landed here." >&2
    fi
    exit 1
fi

echo "shared layer compiles for iOS (${#paths[@]} files)"
