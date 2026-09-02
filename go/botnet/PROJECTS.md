# Projects

The second service, and the counterpart to automations: an automation is HOW work happens, a
project is what work is ABOUT. A goal plus typed, dated facts, with health derived from the
facts rather than stored beside them.

The cases it exists for are the ones that quietly go wrong: a passport or China visa expiring,
a Shanghai company formation whose next step is waiting on a human, a Singapore entity's annual
return / AGM / tax filing coming round again, a visa's overall standing. Each is one project
whose facts say what is true and when it comes due.

State lives in the botnet database beside bots and events, so a bot writes it through a tool,
the single-writer lock and the change feed cover it for free, and passport numbers never land
in a git-tracked directory.

## The fact model

A project is a name, a goal and its facts. Facts are the only authored state; everything the UI
shows about a project's condition is computed from them on read.

| Kind | Carries | Means |
|---|---|---|
| `deadline` | `due`, `leadDays`, tickable | one dated obligation |
| `recurring` | `due` (the FIRST occurrence), `rrule`, `tz`, `leadDays` | a dated obligation that repeats |
| `milestone` | `blocker` (optional), tickable | a step; a blocker names the human action it waits on |
| `note` | `body` | undated context; changes nothing |

Which fields each kind requires and which it may never carry is one table (`factRules` in
`store.go`), not a run of `if`s — so a fifth kind is one entry, and an illegal state (a note with
a recurrence, a recurring obligation someone can tick off once and forget) is rejected at the
write boundary rather than stored and rendered.

Two cross-field rules ride the same table: a `milestone` cannot be both blocked and done (a step
waiting on someone is not finished), and no two facts of one project may share a title
case-insensitively (bots address facts BY title, so a twin makes both copies unaddressable).

## Health

Derived on every read by `projectHealth(facts, now)`, and by nothing else. There is no health
column, no scheduled job to keep one true, and no health in the change feed — the fact write
that moved it is what the feed announced.

| Health | When |
|---|---|
| `overdue` | an undone `deadline` whose `due` has arrived or passed |
| `blocked` | an undone `milestone` with a non-empty `blocker` |
| `due_soon` | an undone dated fact inside its lead window |
| `ok` | facts exist, none of the above |
| `unknown` | zero facts — nothing to be well or ill about |

Precedence is strict, in that order. Two boundaries are worth knowing:

- The lead window is half-open at both ends. `[due - leadDays, due)` is `due_soon`; `[due, ∞)`
  is `overdue`. A deadline is overdue the instant it arrives, not the day after.
- A `recurring` fact is never overdue. Its health comes from its NEXT occurrence, expanded
  through the same rrule engine the calendar uses, so a filing made every March is not overdue
  in April — it is due next March.

`nextDue` is the nearest OUTSTANDING due instant across the undone dated facts. A passed
deadline is still outstanding, which is what the sidebar's "overdue 3d" renders from. A done
fact is invisible to both answers.

## Calendar projection

Every undone `deadline` or `recurring` fact has exactly one event on the calendar named
"Projects", maintained in the same transaction as the fact write by one function
(`projectFact`). That is what puts a passport expiry in the month grid without a second
scheduler.

The projection converges: writing the same fact twice yields one event, marking it done deletes
the event, renaming the project rewrites its events' titles, and a pointer left dangling by
someone deleting the event in the Calendar panel repairs itself on the next fact write. The fact
is the truth; the event is its shadow.

## How a bot is meant to use it

The tool description in `projectToolDef()` (`tools.go`) is the source of truth. It is quoted
here verbatim, and if the two ever disagree, the tool description is right:

```
How to record something — take the FIRST rule that fits:
1. A date in the future you must act by → kind=deadline with "due" and "lead_days" (passport renewals 180, visa renewals 90, company filings 60; anything else 30).
2. An obligation that repeats → kind=recurring with "due" (the FIRST occurrence), "rrule" and "tz".
3. A step someone must complete → kind=milestone. If a HUMAN must act, set "blocker" to exactly what they must do, and clear it ("blocker": "") once they have. A blocked step cannot also be done.
4. Only "what happened" or "what I learned" → kind=note. A note NEVER changes health, so if you are about to write a date into one, it is a deadline: go back to 1.
5. Before "create" or "add_fact", run "show" on the project. Update the existing fact with update_fact rather than adding a twin — a duplicate title is refused.
6. When a deadline is renewed (a new passport arrives), set "due" to the new date. Mark it done ONLY when the obligation itself no longer exists.
7. Never mark anything done without evidence, and record that evidence as a note in the same turn.
```

Bots address projects and facts BY NAME; there is no id anywhere in the tool surface. A model
that has to carry a `prj_` id between turns loses it, and a project name is what the user said
out loud. The cost is the ambiguity error, which is cheap and instructive.

There are no delete commands. Deletion is UI-only, behind a confirmation — the same call already
made for `delete_calendar`. A cheap model must not be able to drop a project's history, or to
tick a fact off by removing it.

## Guards

Advice in a description is a hope; these make the four known misuses answer with a correction.
Two live in the store, so REST enforces them too; two live at the tool boundary, because what
they catch is a model misfiling something rather than a human meaning what they typed.

| Guard | Where | Answer |
|---|---|---|
| a twin fact title | store | `ErrDuplicateName` → 409 on REST, `fact "x" already exists in P; use update_fact` on the tool |
| blocked and done together | store (`factRules.conflicts`) | `ErrInvalid` → 400 / instructive error, naming "clear the blocker first" |
| a date inside a note | tool only | refusal quoting the date and the `add_fact`/`deadline` call to resend |
| done well before `due` | tool only | ALLOWED, with `marked done N days before due; if this was a renewal, set due to the new date instead` |

Every mutating command's result ends with the project's health line, re-read after the write:

```
Passports: due_soon, next due 2027-03-14 (in 193d)
```

Health is derived, so the copy a handler held before its write is already stale. Re-reading is
how the model sees what its own write actually did, and it is the feedback that makes the ladder
learnable rather than merely stated. A project with nothing health-bearing ends instead with
`health unknown: add a deadline, recurring or milestone fact`.

## A system prompt for a project-operator bot

Paste this into a bot's system prompt to make it the one that keeps projects true:

> You keep the user's projects accurate. A project is a goal plus typed, dated facts; its health
> is derived from those facts, so the only way to change what the user sees is to record a fact
> correctly.
>
> Start every project-related turn by calling the `project` tool with `list`, and `show` any
> project you are about to change. Follow the tool's numbered ladder exactly when deciding which
> kind a thing is — the common mistake is filing a dated obligation as a note, which surfaces
> nowhere.
>
> When the user tells you something happened, ask yourself what it changes: a renewed passport
> moves a deadline's `due`, it does not mark it done. A step someone else must take is a
> milestone with a `blocker` naming exactly what they must do. Clear the blocker the moment they
> have done it.
>
> Never mark anything done without evidence, and record that evidence as a note in the same
> turn. If you are unsure which project something belongs to, ask rather than guessing — a fact
> in the wrong project is worse than no fact.
>
> After every write, read the health line the tool returns back to you and tell the user what
> changed, in one sentence.
