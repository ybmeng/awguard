---
name: botnet-server
description: Working on the botnet Go server — any change under go/botnet (store, API, sync, tools). Read before touching schema.go, store.go, server.go, openrouter.go, or tools.go.
---

# botnet server work

## Territory and hard rules

- Scope is `go/` only. Never create, edit, or delete anything under `swift/`.
- Never touch `~/.botnet/net.db` — no sqlite3 against it, no writes. Scratch databases only, via the `BOTNET_DB` env var; never run a daemon against the default DB.
- No installs, no `install-app.sh`/`checkpoint.sh`, no `git commit`. Leave the tree dirty; the orchestrator deploys and commits.

## Schema-as-spec

`schema.go` is the human-authored spec; storage, API, and sync derive from its structs. It carries `DECISION` (settled — implement, don't re-decide), `OPEN` (yours or the user's to settle), and `DEFERRED` markers. When your change settles a marker, rewrite it as a DECISION recording the call and its reason; when it creates a new open question (e.g. last-write-wins on a new field), add an OPEN. The markers ARE the design record — a change that skips them is incomplete.

## Sync invariants that must survive any change

- Every write to bots/messages/segments is captured into `change_log` by SQLite AFTER INSERT/UPDATE/DELETE triggers. Capture is a schema property, not a Go convention — never add a write path that expects Go code to remember to log it. Note: SQLite row triggers fire on any UPDATE of the row, so field-only updates (e.g. memory) emit for free; prove it with a test anyway.
- The call-site table in DESIGN-sync.md's appendix is a TEST ORACLE, enforced by `TestEveryMutatingCallSiteEmitsItsChangeRows`. A new write path means a table row AND an oracle-test case in the same change, plus a short DESIGN-sync.md section if the path is genuinely new.
- `Bot.Version` hashes authored fields only (DisplayName, SystemPrompt, Model). Chat traffic — list metadata, memory writes, anything the model does mid-turn — must never move it, or chat 412s the user's in-flight edits.

## House patterns

- Migrations: add columns via the guarded `added` list in `migrate` (idempotent `addColumn`, `DEFAULT ''`); each backfill step is guarded by the state it writes. Test against the `seedLegacyDB` fixture and across a close/reopen.
- Model tools: one flat tool per surface, a command enum plus optional fields, no oneOf — mid-tier models need flat schemas with prose descriptions. Commands live in a table (`memoryCommands` in tools.go): name, requirements, description line, handler; enum, description, and dispatch all derive from it, so a new command is one entry. Malformed calls return instructive `error: ...` tool RESULTS that consume a loop iteration; only real store failures fail the turn.
- Multiple top-level tools dispatch through a name→handler registry (`toolHandlers`), not a single-name gate — a new tool is one entry. A tool that makes a network call needs `context.Context` threaded through `Run` (memory ignores it); make it fail-soft (a backend/network error returns an instructive `error:` tool result, not a failed turn).
- **Two entry points — wire env config into BOTH.** The server is embedded as `bg_services/botnetsvc` (the PRODUCTION path `stdd` runs, what the app talks to on :8730) and also built standalone as `cmd/botnetd` (what `seed-demo.sh` and e2e use). Anything constructed from the environment — a search `Router`, provider keys — must be wired in BOTH. The web-search router shipped wired only into `cmd/botnetd` and was invisible in the real app until `botnetsvc.go` got the same `NewRouterFromEnv()` + `ConfigureSearch()` call. Grep both entry points whenever you add env-driven wiring. Keep the env-built value out of `NewServer` (default it nil) so ambient keys can't make the `-race` suite non-deterministic.
- Heterogeneous wire lists: when one array holds differently-shaped objects (a `{type:"function",function:{…}}` tool plus a bare `{type:"openrouter:web_search"}` server tool), build it as `[]any` of distinct types — never bolt an empty `function:{}` onto a struct, it leaks into the request and `/v1/tools`. A differently-shaped entry the app decodes (e.g. `ToolDefinition`) must tolerate the new shape in the SAME change (make `function` optional), or the client's whole `/v1/tools` decode throws.
- Pluggable external providers get a keyless `mock` backend. When a tool routes to key-gated providers (search: Exa/Brave/Tavily), add a first-class deterministic mock selected by env (`SEARCH_BACKEND=mock`), off by default, same interface, no special-casing in dispatch. It makes the whole pipe verifiable with no paid key and lets the user try before buying one — a real backend is then one config line. Prove the loop end-to-end through it (model actually calls the tool → results persisted → re-served), not just unit tests.
- New provider keys follow the OpenRouter key path exactly: env var (`EXA_API_KEY`) else `~/.config/botnet/<provider>.txt`, resolved by a shared helper. A file drop plus a service restart is the whole setup — no code change.
- Test seams: `fakeLLM` for server-level behavior (what Prompt the turn carried), and a scripted httptest upstream behind `rewriteHost` for wire-level OpenRouter behavior (tool loops, request shape). Script the model's responses; assert both the requests sent and the store afterwards.
- Read-only derived endpoints (`/v1/tools` is the model): serve the exact value the wire path marshals — `writeJSON`'s Encoder output is `json.Marshal` plus a trailing newline — and pin the no-drift guarantee by byte-comparing the endpoint body against the raw sub-value (`json.RawMessage`) pulled from a scripted upstream's recorded request. The method-pattern mux (`"GET /v1/..."`) 405s other methods for free; test it anyway.
- Done means `go vet ./go/botnet/...` clean and `go test ./go/botnet/... -race -count=1` green. Never report before running that.

## Working with the orchestrator

- Your task memo is a fixed contract: DECISIONs in it are implemented, not re-litigated. Acknowledge a directive change explicitly the moment it arrives, and check your inbox again before the final report — this session a three-tool report crossed mid-flight with a directive switching to a single tool, and only a re-check caught it.
- Write the full report to a scratchpad file; reply with a few lines pointing at it. When accused of missing a change you already made, verify on disk (grep, rerun tests) and answer with evidence, not memory.
