---
name: botnet-orchestration
description: Orchestrating botnet work — the two-agent memo→implement→verify loop, checkpoint discipline, and acceptance testing. Use when coordinating any botnet feature that touches go/botnet or swift/botnet.
---

# BotNet orchestration

The botnet app is a Go server (`go/botnet`, serving :8730 off `~/.botnet/net.db`
via the `com.std.bgservices` LaunchAgent) and a SwiftUI Mac client
(`swift/botnet`). Features are delivered by an orchestrator running two
long-lived teammate agents with disjoint territories.

## The loop

1. **Memo.** Spawn (or reuse) one server agent and one UI agent. Each memo
   carries: the task, the fixed API contract, the hard constraints (below), and
   where to write its report. Fix the client↔server contract in the memo
   *before* dispatch — then both sides build in parallel against the contract
   instead of serializing.
2. **Implement.** Agents work their own territory only: server agent never
   touches `swift/`, UI agent never touches `go/`. Companion skills:
   `botnet-server`, `botnet-ui`.
3. **Verify.** Agents verify with their own harnesses (Go: `-race` suite;
   UI: xcodegen + Release build + offscreen snapshots against a scratch
   server). The orchestrator then spot-checks the diff (`git diff --stat` +
   targeted greps) before accepting any "done" — summaries have disagreed with
   the tree before.
4. **Checkpoint.** Run `swift/botnet/dev/checkpoint.sh` — kills, rebuilds, and
   restarts server *and* app, gated on the Go suite. The user never tests stale
   binaries. ONLY the orchestrator installs/launches; an agent replacing the
   app bundle under the running process crashes it (macOS code-signature kill).
5. **Accept.** Orchestrator runs the E2E acceptance at the API level against
   the live server (real model call when the feature involves one). The user
   does the final in-UI acceptance. Then commit: two logical commits (server,
   app), `.claude/` config stays uncommitted, skills ARE committed.

## Hard constraints (put verbatim in every memo)

- Never touch `~/.botnet/net.db` directly — API or `.backup` copies only.
  Scratch DBs via `BOTNET_DB`; never run a hand-launched daemon on the default
  DB (its startup sweep kills the live server's in-flight turn — now also
  guarded by the flock lock).
- No installs, no app launches, no `checkpoint.sh` from agents.
- No commits from agents; the orchestrator commits.
- Reports go to a scratchpad file; replies stay short (messages truncate ~4KB).

## Lessons that cost us

- **Directive changes mid-flight can be missed.** An agent finished the old
  shape after a redirect was sent. On any redirect, require an explicit
  acknowledgment before trusting the next "done"; on any "done", grep the tree
  for the *new* shape before accepting.
- **Restore from git, don't reconstruct.** When un-deleting recently removed
  code, `git show HEAD:file` / `git checkout --` is the source of truth;
  hand-reconstruction drifts.
- **curl is a proxy.** Real verification runs the app's actual Swift code
  (decode-check harness) or a real model call — demo-data E2E misses real-data
  poison and shape mismatches.
- **Verify the RUNNING process is the new binary** (print build timestamps —
  checkpoint.sh does).
- **Two agents, one file = clobber.** Stagger writes to shared spec files
  (schema.go); one installer, ever.

## Skill upkeep

Each iteration, agents update their own skill (`botnet-server`, `botnet-ui`)
with new lessons, and the orchestrator updates this one. A lesson written here
that keeps being re-taught in memos should become tooling instead (the
checkpoint.sh pattern).
