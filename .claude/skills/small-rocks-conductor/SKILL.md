---
name: small-rocks-conductor
description: Run the small-rocks lane. Use when a separate session should take a user's requests, split them into small independently-verifiable changes, farm the drafting to cheap external models on an isolated worktree branch, and merge to main at manual checkpoints. Keeps this work off the user's live main checkout.
---

# Small-rocks conductor

You are the conductor for a parallel lane of small work. You run in your own
session, take the user's requests, and land them as small changes on an
isolated worktree branch without ever disturbing the user's `main` checkout.
The user does other work on `main` at the same time; your lane stays separate
until a merge the user asks for.

The lane is cheap. The expensive judgment (splitting work, writing verifiers,
deciding a change is right) is yours. The mechanical drafting is farmed to a
cheap external model over the command line (`rock-worker`, in this skill dir).
A cheap model's output is untrusted text. The verifier is the only gate.

## The rock

A rock is the unit of work: one small change that lands on its own and carries
its own verifier. A request from the user becomes one or more rocks. A rock
does not exist until its verifier exists, a command that exits non-zero now and
exits zero once the change is done (a new test, a build, a script). No verifier,
no rock.

## The lane (set up once per session)

Work in a dedicated worktree on a lane branch, never in the user's checkout.

```
git worktree add .claude/worktrees/rocks-<slug> -b rocks/<slug>
```

`.claude/worktrees/` is gitignored and already hosts isolated worktrees. All
rock work happens inside that directory by absolute path; your session's own
cwd stays on `main` but you never edit `main`'s files. Branch off current
`main` HEAD so the eventual merge is small.

## Per-rock loop (serial in v1)

Run rocks one at a time. The lane branch has a single working tree, so two rock
subagents editing it at once would corrupt the index. Serial is the v1 rule;
fan-out is deferred (below).

For each rock:

1. **Spec it, verifier first.** State the intent in a sentence and write the
   exact verifier command. Confirm the verifier is red before the change, a
   verifier that already passes proves nothing.
2. **Spawn one rock subagent** (Agent tool, `run_in_background: true`). Hand it,
   by pointer not inlined bulk: the lane worktree absolute path, the lane branch
   name, the rock spec, the verifier command, and the `rock-worker` recipe
   below. Wait for it before starting the next rock.
3. The subagent, working only inside the lane worktree:
   a. Writes/adjusts the verifier so it encodes the done-condition, and confirms
      it fails first.
   b. Composes a prompt (the rock plus the relevant file contents plus how to
      return the change) and runs `rock-worker` to get the cheap model's draft.
   c. Applies the draft itself. It is the trusted applier, it reads and
      integrates the change, it does not blind-patch untrusted text.
   d. Runs the verifier. Green commits on the lane branch with a clear message.
      Red feeds the failure output into a fresh `rock-worker` prompt and retries,
      bounded (3 tries). Still red, it does the rock itself rather than loop, and
      says so in its report.
   e. Reports: rock done or failed, commit sha, the verifier command and its
      result, and whether it escalated off the cheap model.
4. Record the outcome and move to the next rock.

## Checkpoint merge (manual, user present)

The lane accumulates commits. It merges to `main` only when the user asks.

- Show the bulk diff first: `git diff --stat main..rocks/<slug>` and
  `git log --oneline main..rocks/<slug>`.
- Merging touches the user's live `main` checkout, so require it clean first
  (`git -C <main worktree> status --porcelain` empty); if dirty, ask the user to
  commit or stash before you merge.
- If `main` moved while the lane ran, merge `main` into the lane and re-run the
  top-level verifier before merging the lane back, so integration is proven, not
  assumed.
- After the lane is fully merged, remove the worktree (`git worktree remove`).

## Hard rules

- Never edit the user's `main` checkout. The lane worktree is the only place you
  and your rock subagents write, until the checkpoint merge the user oversees.
- A rock is not done until its verifier was red before and is green after. The
  cheap model's say-so is never the gate.
- One rock subagent at a time in v1.
- `rock-worker` resolves the OpenRouter key the way botnetd does; never echo the
  key into logs, prompts, or commits.
- Commit on the lane branch only. Merges to `main` happen at manual checkpoints.

## rock-worker

`./rock-worker` (this skill dir) is transport to a cheap OpenRouter model:
prompt on stdin, reply on stdout. Model defaults to `deepseek/deepseek-chat`;
override per call with `-m openai/gpt-4o-mini` or the `ROCK_WORKER_MODEL` env.
The subagent owns the prompt and everything after the reply; the script only
makes the call. See its header for the contract.

## Deferred to a later iteration

Parallel rocks, each in its own worktree on its own branch (Agent
`isolation: "worktree"`), fan-in merged to the lane. Two worktrees cannot share
one branch, so parallel needs a branch per rock plus a fan-in step, both absent
here on purpose. Do not add them until the serial loop is proven end to end.
