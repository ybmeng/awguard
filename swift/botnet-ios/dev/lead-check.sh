#!/usr/bin/env bash
# Runs dev/lead-check.swift: what the Add Fact sheet's lead row says, asserted
# against Projects decoded by the real APIClient from real wire shapes.
#
#   ./dev/lead-check.sh
#
# Compiled for the HOST, not the simulator — FactLead.swift and the shared
# models are Foundation-only, so this needs no device and runs in seconds. It
# covers the one value a screenshot cannot reach: a draft of 0, which the server
# rewrites to the project's own lead on create.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
shared="$here/../botnet/Sources"
out="$(mktemp -d)"
trap 'rm -rf "$out"' EXIT

xcrun swiftc -o "$out/lead-check" \
  "$shared/Models.swift" \
  "$shared/APIClient.swift" \
  "$here/Sources/FactLead.swift" \
  "$here/dev/lead-check.swift"

"$out/lead-check"
