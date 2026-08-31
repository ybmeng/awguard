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
   add a decoded field, add a fixture case (present AND absent). Keep it
   READ-ONLY: it defaults to the user's live daemon, so a write probe there
   would mutate real data. To prove a POST/PATCH/DELETE, compile a throwaway
   `swiftc Sources/Models.swift Sources/APIClient.swift scratch.swift` in your
   scratchpad and point it at the demo server — real client, no tree residue.
6. Other agents run demo servers too, and 8731 is the port they all reach for.
   Take an odd one (`DEMO_ADDR=127.0.0.1:8793`), and if a request suddenly
   fails oddly — a 501 with an HTML body was a Python `http.server` that had
   grabbed the port after botnetd died — re-check `lsof -nP -iTCP:<port>` before
   believing the failure. Stop your own daemon by that pid, never `pkill -f
   botnetd`: another agent's server is usually running too.

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
- `SidebarView.swift` — the Services section, bot list, search, delete context
  menu. Owns `SidebarSelection` (`.bot(id)` | `.service(kind)`) and
  `ServiceKind`; ContentView switches the detail pane on that enum, so a new
  service is a case plus a pane, never a sentinel id.
- `CalendarView.swift` — the Calendar service's pane: events grouped by day
  (upcoming, then "Earlier"), each row showing its author. Grouping lives in
  `EventGroups`/`EventDay` in the same file, not in the view body.
- `Store.swift` — AppStore, thin @MainActor client over botnetd; caches server
  responses, owns no durable state. `awaitReply` polls a sent turn until it
  settles, then `refreshBotList()` — that refetch is what live-updates
  sidebar previews AND the memory panel; don't drop it.
- `APIClient.swift` — HTTP + tolerant date decoding; `isUnimplemented` maps
  404s from older servers to silence or a "too old" message.
- `Models.swift` — Swift mirror of go/botnet/schema.go.
- `DesignSystem.swift` — Palette / TypeScale / Metric + BotAvatar. Views never
  hardcode a color, font, or magic dimension: add a token if none fits.
- `Transcript.swift`, `Sheets.swift` — turn/bubble grouping; new-bot, settings
  and event sheets (`EventSheet` + `EventTarget`). New sheets go here, not into
  the pane that presents them.

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
- Snapshot renders one pane beside the sidebar; pick it with a flag
  (`--calendar`, `--event-sheet [--new-event]`) — a rendering mode is not the
  banned `--seed-*` flag. A sheet has to be drawn flat, at its own
  `.frame(width:height:)`, and its NavigationStack toolbar (Cancel/Save) will
  NOT appear — the toolbar needs a real window, same as `.inspector`. Verify
  those buttons by reading the code, not the PNG.
- Seed snapshots via the demo scratch DB with `sqlite3`, NEVER via source
  hacks. To render a state that needs data (citations, tool_calls), `UPDATE`
  the demo DB's column directly — botnetd migrates new columns in on serve, so
  a plain `sqlite3 build/demo.db "UPDATE messages SET tool_calls=… WHERE id=…"`
  then a snapshot renders the real APIClient decoding real persisted data. Do
  NOT add debug seed methods to `Store.swift` or `--seed-*` flags to
  `Snapshot.swift` — that residue was left in the tree three times this session
  after "tree clean" reports. If you must touch source to seed, grep for your
  residue before reporting; the orchestrator greps and rejects it.
- The snapshot captures the BOTTOM of the transcript: the ScrollView opens
  pinned to the newest turn (`.defaultScrollAnchor(.bottom)`) and the offscreen
  render honors that. To snapshot a specific message's UI, seed it onto a LATE
  message (the last bot reply) or use a short-history bot that fits one
  viewport — an early message scrolled off the top won't be in frame. The tool
  renders `store.bots.first`, ordered by `bots.last_message_at` — a
  denormalized column, so editing a demo message's `sent_at` alone doesn't
  reorder; `UPDATE bots SET last_message_at=...` picks which bot renders.
- A `/v1/tools` entry can be a bare server tool (`{type:"openrouter:web_search"}`,
  no `function`). `ToolDefinition` must decode `function` as optional and render
  an unknown/functionless type gracefully (humanize the `type`), or the whole
  `[ToolDefinition]` decode throws and the Tools inspector breaks.
- A macOS `Form` puts a TextField's *placeholder* in the label slot, so
  `Section("Title") { TextField("e.g. Lunch…") }` prints the field's name twice
  the moment it has a value ("e.g. Lunch with Alex" | "Chase sign-in call").
  Label the field (`TextField("Title", …)`) and keep a section header only for
  a multiline field whose label would sit oddly beside a tall box.
- A pane that fills the window pushes a row's trailing column (an author, a
  stamp) to the far edge, a hand's width from the content it belongs to. Cap
  the list's own width the way `Metric.bubbleWidthFraction` caps a bubble —
  `Metric.calendarListWidth` is the calendar's version — and left-align it.
- Markdown in bot bubbles: `Text(String)` renders literally — parse with
  `AttributedString(markdown:options:)` at `.inlineOnlyPreservingWhitespace`
  (keeps single newlines, leaves block syntax literal), raw-text fallback on
  throw. Bot bubbles only; user bubbles stay literal (a typed `*x*` is verbatim).

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
