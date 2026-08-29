---
name: botnet-ui
description: Use for any work under swift/botnet — the BotNet Mac app UI. Hard rules, the build/snapshot verify loop, codebase map, and house patterns.
---

# BotNet UI work

You are the UI agent for the SwiftUI Mac app in `swift/botnet/`. The Go server
under `go/` belongs to another agent — read it if you must, never edit it.

## Hard rules (violations have crashed the user's app before)

- NEVER run `dev/install-app.sh` or `dev/checkpoint.sh`, never copy anything
  into /Applications, never quit or launch the BotNet app. Replacing the app
  bundle under the running process kills it. The orchestrator owns installs
  and restarts; you verify by building and offscreen rendering only.
- Never touch `~/.botnet/net.db` (the real DB). For a live server use
  `dev/seed-demo.sh` — it seeds a throwaway DB and serves on 127.0.0.1:8731,
  rebuilding botnetd from the current Go tree. `pkill -f botnetd` when done.
- Do not `git commit` — the orchestrator commits. Do report exactly what
  changed.

## Verify loop

From `swift/botnet/`:

1. `xcodegen generate` (project.yml is the source of truth; regenerate after
   adding files or changing targets).
2. `xcodebuild -project BotNet.xcodeproj -scheme BotNet -configuration Release
   build` — this is the gate. SourceKit/single-file diagnostics are noise
   (files import each other; only the target build sees them all).
3. Also build the `Snapshot` scheme — it compiles Sources/ minus BotNetApp
   plus `dev/Snapshot/Snapshot.swift`, so a ChatView/BotDetails signature
   change breaks it separately. Fix `Snapshot.swift` when signatures change.
4. Screenshots without launching anything: `./dev/seed-demo.sh &`, then
   `./dev/snapshot.sh` (light+dark PNGs of Sidebar+Chat), or run the built
   `snapshot` binary directly with `--details` to render the inspector panel
   as a third column, `--dark`, `--out <path>`. `screencapture` is blocked;
   ImageRenderer can't draw ScrollView — the tool hosts an offscreen NSWindow
   instead. Read the PNGs yourself and look at them critically.
5. Wire-contract proof: `BOTNET_API=http://127.0.0.1:8731 ./dev/decode-check.sh`
   compiles Models+APIClient against captured fixtures and live GETs. When you
   add a decoded field, add a fixture case (present AND absent).

## Codebase map (Sources/, ~1600 lines total — read what you touch)

- `BotNetApp.swift` — @main + ContentView: NavigationSplitView, selection,
  sheet/inspector presentation state. Window-chrome state lives here.
- `ChatView.swift` — chat pane (header, transcript, composer) and BotDetails
  (the right-hand inspector). New inspector sections go through
  `InspectorSection` (same file) — title + expanded binding + content, with an
  optional accessory slot; never hand-roll the chevron/hairline header. Its
  hairline rule is neighbor-blind (header and open body each draw one bottom
  line); verify mixed open/closed states with `snapshot --details
  --collapse-memory`. State that must survive collapse (drafts) lives on
  BotDetails, not inside the content closure — collapse destroys content.
- `SidebarView.swift` — bot list, search, delete context menu.
- `Store.swift` — AppStore, thin @MainActor client over botnetd; caches server
  responses, owns no durable state. `awaitReply` polls a sent turn until it
  settles, then `refreshBotList()` — that refetch is what live-updates
  sidebar previews AND the memory panel; don't drop it.
- `APIClient.swift` — HTTP + tolerant date decoding; `isUnimplemented` maps
  404s from older servers to silence or a "too old" message.
- `Models.swift` — Swift mirror of go/botnet/schema.go.
- `DesignSystem.swift` — Palette / TypeScale / Metric + BotAvatar. Views never
  hardcode a color, font, or magic dimension: add a token if none fits.
- `Transcript.swift`, `Sheets.swift` — turn/bubble grouping; new-bot and
  settings sheets.

## House patterns

- Fields newer than the first server release decode as Optional with nil
  meaning "old server", never as a real value (see Models.swift header).
- Editors: explicit Save only (no autosave — the server's model can also
  write, last-write-wins). On a failed save keep the editor open with the
  draft intact; errors surface via `store.lastError` (one shared alert).
- Reset transient per-bot view state on bot switch (`.onChange(of: bot.id)`)
  so a draft never crosses bots.
- Resolve bots from `store.bots` inside `body` (as ContentView does) — never
  hold a captured Bot copy, or refetches won't propagate.
- Restoring deleted code: it's in git — `git checkout -- <file>` /
  `git show HEAD:<file>`. Never reconstruct from memory; re-apply your delta
  on top so unchanged code stays byte-identical.
- Match the existing comment voice: comments state constraints and reasons,
  not narration.
- Opaque server JSON (e.g. a tool's parameters schema from /v1/tools): decode
  through `JSONValue` in Models.swift and show its `prettyPrinted` text —
  never type out an evolving schema's fields. Probe Bool before Double there
  or true/false bridge to 1/0.
- Snapshot.swift's capture window gives `.task` fetches no time to land; any
  store data a view loads in `.task` must also be awaited explicitly in
  Snapshot.main() before render (as refresh/loadConversation/loadTools are).

## Working with the orchestrator

- Tasks arrive as memos with a fixed contract (exact JSON fields, routes,
  semantics). Build against the contract; don't wait for the server agent —
  but check the Go tree, the change often lands mid-task and you can verify
  against the real server via seed-demo. Re-check right before verifying:
  seed-demo rebuilds botnetd from the current tree, so a route that 404'd at
  task start can be live by snapshot time (happened with /v1/tools) and the
  real server beats any stub.
- Directives get corrected mid-task. Check your inbox BEFORE writing the
  final report and before ending your turn — a correction was nearly missed
  this session. Acknowledge course changes explicitly in your reply.
- Deliverable: a report file in your scratchpad (what changed, where each
  capability lives, build proof, snapshot paths) plus a short SendMessage
  reply pointing at it.

Update this skill when you learn something the next agent would otherwise
rediscover the hard way.
