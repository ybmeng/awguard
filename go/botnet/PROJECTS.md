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

## Severity

Every project also carries a `severity`: the same verdict collapsed into the three bands a
person reads as colours, derived from the ROLLED-UP health by one table (`severityOf` in
`projects.go`).

| Severity | Health | Means |
|---|---|---|
| `S0` | `overdue` | act now |
| `S1` | `blocked`, `due_soon` | should be doing |
| `S2` | `ok`, `unknown` | tracked, not pressing |

It is on the wire rather than computed by each client, because every client that renders a dot
would otherwise carry its own copy of the mapping and disagree the moment a sixth health lands.
A band a client does not recognise renders as the quiet one, never as an error.

## Hierarchy

A project may sit under one other, through `parentId` — the only authored part of the shape.
Names stay GLOBALLY unique, so the name a user says out loud still addresses exactly one
project and there is no path syntax for a model to get wrong.

Three refusals at the write boundary, enforced once for both the REST face and the tool:

| Refusal | Answer |
|---|---|
| the parent does not exist | `ErrNotFound` → 404 / an instructive error listing the projects that do |
| a project under itself | `ErrInvalid` → 400 |
| a project under its own descendant | `ErrInvalid` → 400, `moving "X" under "Y" would create a cycle` |

A cycle is invalid rather than missing: every row named exists, it is the RELATION that is
impossible — a ring has no root, so neither project would appear in any tree again.

`health`, `severity` and `nextDue` ROLL UP: a parent's condition is the worst thing anywhere in
its subtree and the nearest outstanding date in it, so the sidebar's dot means "something under
here needs me". `factCount` does not roll up — it counts what was authored on that project —
and `childCount` counts DIRECT children, which is what a disclosure chevron needs.

The whole listing is two queries whatever the tree's shape: projects once, facts once, then one
post-order pass. `GET /v1/projects` stays a flat array and the client builds the tree from
`parentId`; `GET /v1/projects/{id}` adds a `children` array of the direct children, hydrated and
ordered exactly like the list (`[]`, never `null`, when there are none).

Deleting a project deletes its WHOLE subtree, every fact under it and every projected event, as
per-row deletes — so the change feed carries a real tombstone for each, and a sub-project is
never orphaned into a top-level row the user never made.

## The lead threshold

How early a date starts mattering is a property of the FOLDER of work, not of each fact: a
passport is worth six months of warning, an invoice a fortnight. So a project carries
`defaultLeadDays`, and every dated fact under it with no lead of its own is judged by it.

`effectiveLeadDays` is the derived answer — own `defaultLeadDays` when set, else the nearest
ancestor that set one, else the global 30 — computed in the same forest pass that rolls health
up. The two directions travel together in one walk: health folds UP out of a subtree, the
threshold flows DOWN into it.

| Where | Value |
|---|---|
| `Document Expirations`, `defaultLeadDays: 180` | 180 |
| `Passport` under it, unset | 180, inherited |
| `China Q2 Visa` under it, `defaultLeadDays: 90` | 90, its own overrides |
| `Singapore Co`, top level, unset | 30, the global default |

It applies at READ time, not at create time. A fact's `leadDays` is its OWN window and `0` means
UNSET, so a dated fact stores exactly what it was given and `effectiveLeadDays` — `leadDays` when
set, else the project's answer above — is what health, `due_soon` and `nextDue` are judged by.
Raising a project's default therefore changes what every fact still borrowing it means, on the
next read, with no migration and no write to any fact row. A fact that named its own window is
untouched by that.

Both write paths obey the one rule. Create once substituted the project's lead into the row while
a patch to 0 stored a literal 0, so the same number meant two different things depending on which
route wrote it and a fact could not round-trip through create; neither path substitutes now.

`0` in `defaultLeadDays` means UNSET the same way, so patching it to 0 clears the project's own
threshold and the ancestor's applies again. The accepted cost of spending 0 on "unset" at both
levels is that a zero-day window is UNREPRESENTABLE: a date with no early warning at all cannot
be expressed, and a lead of 1 is the nearest thing.

## Owner and nudges

A project nobody is answerable for is a list, not a responsibility. `ownerBot` names the bot
answerable for one, and `effectiveOwner` inherits it exactly as the lead threshold does — own
owner, else the nearest ancestor's, else none. Naming an owner once on "Document Expirations"
makes every document under it somebody's.

An owner whose bot has been deleted READS as unset, in both fields, so no client renders a thread
that is gone. `DeleteBot` clears the stored pointer to match, one project row at a time, so the
change feed carries a real update for each.

`POST /v1/projects/tick` is the whole nudge, and the std `ping` service POSTs it hourly. It
derives the forest once, then per project compares the rolled-up health against `lastHealth` —
the health the last tick observed, stored on the project and deliberately NOT on the wire, since
health is derived and refetched and a second, older copy would only invite a client to trust the
wrong one.

| Case | What happens |
|---|---|
| worse than `lastHealth`, owner free | the owner is told, `lastHealth` moves — in ONE transaction |
| worse, no effective owner | skipped with a reason, `lastHealth` untouched |
| worse, the owner has a turn in flight | skipped with a reason, `lastHealth` untouched |
| the same or better | `lastHealth` moves silently; improvements are not news |

An empty `lastHealth` counts as `ok`, so a project that is already overdue the first time the
tick meets it nudges immediately rather than being adopted as the new normal. The skipped cases
write nothing at all, which is what makes them a deferral rather than a lost message: the next
tick re-decides from the same comparison. Two projects owned by one bot therefore take two ticks,
because a bot answers one turn at a time.

The response is `{"checked": N, "nudged": [...], "skipped": [...]}`, both lists always arrays.
The tick is idempotent by construction — running it twice over the same state nudges nothing the
second time — so `ping` needs no backoff and a caller may run it as often as it likes.

The message is an ordinary user-role append to the owner's thread, through the same store path
`POST /v1/bots/{id}/messages` uses, so the model turn starts, the reply lands in the transcript
and no client needs a new rendering. It is recognisable by its opening and by nothing structural:

```
Project nudge — Document Expirations is now S1 due_soon (was S2 ok). Facts driving it:
- US passport expires: due 2027-03-14 (in 190d, lead 180d)
- Photos taken: blocked — Studio has not sent the digital copies
Act on it or update the facts with the project tool; reply with what you did.
```

The facts listed are the undone ones anywhere in the project's SUBTREE that are as loud as the
project now is — subtree, because health rolled up, so a parent's nudge has to name the child's
fact that caused it. At most five, most urgent first.

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
1. A date in the future you must act by → kind=deadline with "due". The lead window comes from the PROJECT, so set it once with "default_lead_days" on the project that holds this kind of date (passport renewals 180, visa renewals 90, company filings 60; anything else 30) rather than typing "lead_days" into every fact; "lead_days" is for the one fact that differs, and "0" means "use the project's". Widening a project's default widens every fact still borrowing it, so fix the window in one place rather than editing facts.
2. An obligation that repeats → kind=recurring with "due" (the FIRST occurrence), "rrule" and "tz".
3. A step someone must complete → kind=milestone. If a HUMAN must act, set "blocker" to exactly what they must do, and clear it ("blocker": "") once they have. A blocked step cannot also be done.
4. Only "what happened" or "what I learned" → kind=note. A note NEVER changes health, so if you are about to write a date into one, it is a deadline: go back to 1.
5. Before "create" or "add_fact", run "show" on the project. Update the existing fact with update_fact rather than adding a twin — a duplicate title is refused.
6. When a deadline is renewed (a new passport arrives), set "due" to the new date. Mark it done ONLY when the obligation itself no longer exists.
7. Never mark anything done without evidence, and record that evidence as a note in the same turn.
8. A document or entity with its own dates and notes under a bigger goal (Passport under Document Expirations) → a sub-project: "create" with "parent". A single date under a project → a fact.
```

Bots address projects and facts BY NAME; there is no id anywhere in the tool surface. A model
that has to carry a `prj_` id between turns loses it, and a project name is what the user said
out loud. The cost is the ambiguity error, which is cheap and instructive.

There are no delete commands. Deletion is UI-only, behind a confirmation — the same call already
made for `delete_calendar`. A cheap model must not be able to drop a project's history, or to
tick a fact off by removing it. `update` (rename, re-goal, re-parent) is the exception that
proves the rule: every one of those is reversible, which dropping a subtree is not.

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
Passports: S1 due_soon, next due 2027-03-14 (in 193d), lead 180d
```

The severity band leads, so a model knows how loud the answer is without holding a table of five
healths, and the effective lead closes it — that is both why the project is amber rather than
green and the window the next fact filed here will take.

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
> changed, in one sentence. It ends with the project's lead window; when you notice yourself
> setting the same `lead_days` on fact after fact, set the project's `default_lead_days` once
> instead and let the sub-projects inherit it.
>
> A message beginning `Project nudge — ` is not from the user: it is the server telling you a
> project you own has got worse, with the facts that drove it. Act on it — chase the blocker,
> renew the document, correct a fact that is no longer true — and reply saying what you did. If
> there is nothing you can do without the user, say exactly what you need from them. You will be
> told once per deterioration, so nothing is repeated at you and nothing is said twice.
