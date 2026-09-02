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
- `SidebarView.swift` — search pinned on top, then an explorer tree of
  collapsible sections (Services, Automations, Bots) with chevron headers,
  per-section @AppStorage expansion, and a `collapsedOverride` init param the
  Snapshot tool uses instead of writing UserDefaults. Owns `SidebarSelection`
  (`.bot(id)` | `.service(kind)` | `.automation(name)`), `ServiceKind` and
  `SidebarSection`; ContentView switches the detail pane on that enum, so a
  new section is a SidebarSection case + rows + (if selectable) a selection
  case plus a pane, never a sentinel id. A non-empty search force-reveals the
  Bots section (chevron turns with it) without touching the persisted choice.
- `AutomationView.swift` — one automation's pane (freshness badge, schedule
  summary, scheduleError, Run now with poll-until-finished, runs list with
  inline-disclosed RunDetail) plus `FolderOpener`, the open-in-Cursor /
  reveal-in-Finder seam: its `launch`/`reveal` are static closure vars so a
  scratch harness proves the exact `open -a Cursor <path>` argv and the
  Finder fallback without launching anything.
- `CalendarView.swift` — the Calendar service's pane: instances grouped by day
  (upcoming, then "Earlier"), each row showing its author. Grouping lives in
  `EventGroups`/`EventDay` in the same file, not in the view body. The pane
  renders `EventInstance` (from /v1/instances over the Store's bounded window),
  NEVER `Event`: a recurring event is one row server-side but many instances,
  and clicking any of them opens the MASTER event
  (`store.event(id: instance.eventId)`). On a 404 the Store synthesizes
  instances one-to-one from the wholesale events list, so old servers render
  exactly as before — new view code must keep working off that synthesis.
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
  (`--calendar [--month] [--filter-calendar <name>] [--search <text>]`,
  `--event-sheet [--new-event]`, `--manage-calendars`) — a rendering mode is
  not the banned `--seed-*` flag. A mode that resolves a name (like
  `--filter-calendar`) should fail loudly on no-match, or a typo'd run passes
  review as the unfiltered pane. A sheet has to be drawn flat, at its own
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
  a multiline field whose label would sit oddly beside a tall box. The same
  label slot bites inside a custom HStack row: a grouped Form still prints the
  TextField's label ahead of the row and right-aligns the field ("Name |
  Personal | …" on every ManageCalendarsSheet row). For an inline editable
  field in a composed row, add `.labelsHidden()` +
  `.multilineTextAlignment(.leading)`.
- Don't shadow a Foundation type with a model name: the calendar entity is
  `EventCalendar`, not `Calendar`, because `Calendar.current` runs all through
  the date math and shadowing it would force `Foundation.Calendar` onto every
  call site. Check what the obvious name collides with before mirroring a
  server entity.
- A pane that fills the window pushes a row's trailing column (an author, a
  stamp) to the far edge, a hand's width from the content it belongs to. Cap
  the list's own width the way `Metric.bubbleWidthFraction` caps a bubble —
  `Metric.calendarListWidth` is the calendar's version — and left-align it.
- When the contract endpoint hasn't landed in go/ yet (parallel server agent),
  don't stall and don't fake data into Store: stand up a contract-shaped stub
  HTTP server in the SCRATCHPAD (static JSON per the contract, python
  http.server on an odd port) and point BOTNET_API at it — the real APIClient
  decodes real HTTP and the snapshot proves the rendering. Label those PNGs as
  stub-verified in the report and re-run against the real botnetd once the
  route lands. The stub never enters the tree.
- Seeding times for grid review: the grid groups by LOCAL day, so pick UTC
  instants that stay on the intended local date (a 17:05Z event is next-day in
  UTC+8, and your "4th Tuesday" chip renders on a Wednesday). Check `date +%z`
  before choosing seed times.
- A multi-month grid check (a recurring series on several correct days across
  two months) doesn't fit 900pt: run `snapshot --calendar --month --height
  1250` so both month sections are in frame.
- Automations wire shapes: a run's `started`/`finished` are RFC3339 STRINGS,
  never Dates — `finished` is `""` while queued/running and would blow up any
  date decoder. Decode as String with `startedAt`/`finishedAt` computed
  helpers. The run envelope is lenient at the RunDetail layer (`try?` around
  the typed decode) so an unknown future shape degrades to nil, while inside
  RunEnvelope a present-but-wrong-typed key still throws — half an alien
  envelope rendered as truth is worse than none.
- Verifying against a scratch `stdd run` stack: the unix sockets fail with
  `bind: invalid argument` when the root path exceeds sockaddr_un's ~104
  bytes — the deep scratchpad path does. Use a short root (e.g.
  /tmp/<task>-<port>). The trap: botnet's TCP routes still work (the
  automations handler is mounted in-process), so the crash-looping runner is
  invisible until a POST run never executes — grep the stdd log for
  "invalid argument" before trusting the stack.
- Seeding every sidebar freshness state needs only manual runs, thanks to
  the service's precedence (unscheduled > never > pending > stale > failed >
  ok): scheduled manifest + ok manual run → "ok"; scheduled + failed run →
  "failed"; a scheduleError manifest → schedule nil → "unscheduled" even
  with an error run; scheduled + no runs → "never".
- A view state gated on THIS machine (a FileManager.fileExists check, like
  the artifact open-in-Cursor links) cannot be proven against a seeded demo —
  the demo DB's paths don't exist, so the affordance silently renders as its
  degraded form and the PNG passes review while showing nothing. Point the
  snapshot at the live daemon instead (`BOTNET_API=http://127.0.0.1:8730`,
  READ-ONLY panes only — every fetch behind the rendered pane must be a GET,
  same discipline as decode-check's live probes), where the machine-state is
  actually true. Choose the snapshot backend by where the state you're
  proving lives, not by habit.
- Snapshot must not write UserDefaults to pose @AppStorage state (defaults
  litter, cross-run pollution): give the view an override init param
  (`collapsedOverride`) that wins over the stored value, nil in the app.
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

## Shared files and the iOS app (added 2026-09-02)

- `swift/botnet-ios/` is a second xcodegen target (BotNetMobile) that compiles
  Models, APIClient, Store, Transcript and DesignSystem from `../botnet/Sources`
  BY PATH (plus FactLead). Any AppKit call added to a shared file breaks the phone
  build silently until someone builds it. Run `swift/botnet-ios/dev/shared-check.sh`
  (compiles the shared files against the iphonesimulator SDK alone) as a third gate next
  to the two Release builds whenever you touch a shared file; wrap AppKit in
  `#if canImport(AppKit)` with a UIKit twin, as DesignSystem's Palette bridge does.
  Keep shared-file edits additive and source-compatible: no renames, no signature
  changes, so the other target keeps building.
- The Form label-slot trap has an exact inverse on iOS: a TextField carrying a
  `prompt:` DROPS its label there, so the field renders as a bare value with
  nothing naming it. Mac wants the label slot; iOS wants the name carried in the
  row. The same line cannot serve both; branch it. Also `prompt:` must precede
  `axis:` or it does not compile.
- Screenshots on iOS come from a real booted simulator (`dev/sim.sh`:
  simctl boot/install/launch + `simctl io booted screenshot`), never from
  reasoning about layout. Six real defects were caught only that way.

## More house lessons (2026-09-02)

- `PIPESTATUS` is empty in this shell. `cmd | grep …; echo $PIPESTATUS[0]`
  prints nothing and proves nothing. Redirect to a log and read `$?` on the
  next line.
- A verdict label must be computed from the result, never printed beside
  it. `git diff --stat; echo "[empty above = no churn]"` asserts a conclusion
  the output can contradict, and an agent reported a stale pass that way.
  Same family as the PIPESTATUS trap: branch on the value, then print.
- After any commit that touches files your work lives in, re-run your gate
  before calling your result current; a green from before the commit is not
  evidence about HEAD.
- The orchestrator commits by explicit file list, never `git add -A`, while
  any agent may be writing: a sweep once captured half of an agent's
  in-flight edit and left decode-check red on HEAD with both app builds green.
- A scratch swiftc harness must not be named `main.swift`: it turns the file
  into top-level code and any `@main` in the shared sources stops compiling.
- When the Go tree itself will not build (another agent mid-edit),
  `git archive HEAD | tar -x -C <scratch>` gives a buildable backend to verify
  against without touching the checkout; the stub-proxy trick assumes a
  buildable tree.
- Counterfactual derivations belong client-side in the tree value, pinned by a
  live check. The sheets need "what would this project inherit if it cleared
  its own value", which is NOT the wire's `effectiveLeadDays`; `ProjectTree`
  derives it and a decode-check asserts the walk equals the server's derivation
  on every project that sets nothing.
- A done fact shows its absolute date in the muted colour with no relative and
  no lead; a struck-through row that still reads "overdue 49d" is a defect.
- `seed-demo.sh` deletes and rebuilds the demo DB, so re-running it wipes
  anything you seeded through the routes; seed after the last restart.
