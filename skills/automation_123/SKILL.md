---
name: automation-123
description: Canonical structure for automations that mature from AI discovery (form 1) into cheap-driver recipes (form 2) into pure scripts (form 3), with failures cascading back up the same ladder
---

# automation-123

One structure read in two directions. Downward (1→2→3) is the maturity ladder: how an automation
hardens over its lifetime. Upward (3→2→1) is the fallback chain at invocation time. The same
artifacts serve both readings.

The hinge between forms is the verifier. Demotion from 1 to 2 requires a recorded step sequence
plus a check that says "this run worked." Demotion from 2 to 3 requires a happy path needing zero
judgment. Escalation back up is triggered by nothing but a red verifier. Without a fast verifier,
form 3 does not fail loudly — it silently rots — so the verifier is not optional per form, it is
the rung material.

## The three forms

**Form 1 — discovery and repair.** A frontier model with full tools (browser, subagents, human on
call) figures the task out by trial and error and records everything it learns in the automation's
SKILL.md. Form 1 is also the top of the fallback chain: when a lower form breaks, form 1 repairs
it from the SKILL.md manual plus the archived raw evidence, instead of rediscovering from scratch.
A form-1 escalation is not done until the lower forms are fixed, verified, and committed.
Escalation ends in a commit, or the ladder decays into "expensive model does it by hand every time."

**Form 2 — frozen steps, cheap driver.** The step list is frozen. The driver (a small model, or a
human for gated steps like captchas) may observe, verify, retry, and select among KNOWN branches.
It never discovers new steps — anything needing discovery is form 1 by definition. Write recipes
as numbered steps with the expected observation after each step, so any driver can follow them and
tell success from failure. Some steps are permanently capped at form 2 (a captcha needs a human);
mark those in the manifest.

**Form 3 — pure script.** Deterministic code or API call, no intelligence anywhere. When the goal
is reachable through a free API, form 3 is the ONLY form to build — no recipes, no browser paths,
no form-2 machinery to maintain for slices the API already covers. Forms 2 and 1 then exist only
for slices the API cannot reach, and for repair. A paid API becomes a dependency only with the
human's explicit sign-off. Requirements:

- Fails loudly: nonzero exit on any anomaly, and never overwrites good state with bad.
- Archives raw responses per run, so an escalating model has evidence, not just a stack trace.
- Emits the result envelope (below) as the last stdout line and to `data/last_result.json`.
- Has an offline verifier: tests against committed snapshots of real responses. Red offline tests
  mean the code broke; a live run failing with green offline tests means the source changed —
  that distinction routes the escalation.

## Directory shape

Each automation is one directory:

```
<automation>/
  README.md            # manifest (below) — the front door for humans, models, and the service
  SKILL.md             # form-1 knowledge: source maps, endpoint specs, gotchas, repair manual
  scripts/             # form-3 entrypoint + helpers
  recipes/             # form-2 step lists (only if the automation has form-2 steps)
  scripts/tests/       # offline verifier + snapshots/
  data/                # outputs (CSVs), raw/ archives (gitignored), last_result.json
```

## README.md manifest

YAML frontmatter, then free text for current state and caveats:

```markdown
---
name: <kebab-case>
goal: <one line>
forms:
  "3": <command, run from the automation dir>
  "2": <recipe path, or absent>
  "1": SKILL.md
verify: <offline test command>
cadence: <when to run / when new data lands — prose for humans>
schedule:
  rrule: "FREQ=MONTHLY;BYDAY=4TU"   # botnet-calendar RRULE subset (RFC 5545 slice)
  at: "13:05"                        # wall-clock HH:MM in tz
  tz: "America/New_York"             # IANA id
  retry_every: 2h                    # fire-time pacing: Go duration between attempts in a window
  retry_for: 30h                     # window length — becomes the calendar event's duration
human_gates: <steps permanently capped at form 2, or none>
---
<current state, placeholders, anything a driver should know before invoking>

## Reporting
<how THIS automation's runs report>

## Data spec
<one subsection per artifact>
```

`schedule:` is the machine-readable twin of the `cadence:` prose, and it is a **provisioning
template, not the schedule itself**. The botnet calendar is the single source of truth for what
fires: the automations service's registration-ensure reads the template once and, only if no
calendar event anywhere names this automation, creates one recurring event on the executable
"Automations" calendar (rrule + at + tz seed the recurrence, `retry_for` becomes the event
duration — the retry window). From then on the calendar is authoritative — moving, editing, or
deleting that event (by the user or a bot) changes when the automation fires, and the template
never overwrites those edits. `retry_every` is the one fire-time field: while an event instance
is active, fires are paced to one attempt per `retry_every` until a run's envelope both says
`ok` and advances past the pre-window baseline (a new artifact path, or a newer `newest` at a
known path — `rows` is deliberately ignored, since sources restate history at constant row
counts). No `schedule:` block means the automation is registered, listed, and manually runnable,
but never auto-registered on the calendar; an event naming it by hand still fires it.

The Reporting section makes the envelope contract concrete for this automation: where the envelope
lands, which statuses this automation can actually emit and what each means here (which artifacts
move fast vs slow, what degraded typically looks like), what a needs_human envelope will instruct
if the automation has human gates, and — when form 3 rides a free API — the statement that form 3
is the only form and why. A server reading only the README must know every way a run can end.

The data spec makes each artifact consumable and validatable by a server that has never read the
code: for every artifact, the format (e.g. CSV with header), the grain (what one row is, and the
key columns that identify it), every column with its type, unit, and meaning (units matter —
thousand USD vs USD has burned real analyses), sort order if guaranteed, and provenance: the
source institution, the exact endpoint or recipe the values came from, any transformation applied
on the way (renames, filters, derived fields), and the revision behavior (does the source restate
history, or only append?). A validator should be able to check an artifact against this section
alone — column names, parseability of each column's type, key uniqueness — without knowing the
domain.

## Result envelope

Every invocation, whichever form ran, reports:

```json
{"automation": "<name>", "status": "ok|degraded|failed|needs_human", "form_used": 3,
 "artifacts": [{"path": "data/monthly.csv", "rows": 49, "newest": "202607"}],
 "escalation_reason": null}
```

`ok` = everything fetched and verified. `degraded` = some datasets succeeded, some failed (still
exit nonzero). `failed` = terminal error, nothing usable. `needs_human` = a form-2 run reached a
human gate and parked; `escalation_reason` then states the exact action the human must take
("click the reCAPTCHA checkbox in tab X"), because the human is the next form up. In every other
case `escalation_reason` is a sentence for the next form up, not a stack trace — say what was
observed ("zero rows for tradeKind E; raw archived at ...").

**No silent endings.** Every run — any form, any driver, scripts and models alike — terminates in
exactly one emitted envelope: stage completed (ok/degraded), terminal error (failed), or waiting
on a person (needs_human). A driver that stops, pauses, or hands work upward without emitting an
envelope is itself a defect, the same class as a fetcher that writes a short CSV without erroring.
Incomplete results are only acceptable when the envelope says they are incomplete.

Most fetch automations are "extend a CSV": each artifact carries `rows` and `newest` (the latest
period present) precisely so a cheap checker can diff two envelopes and assert "one more row,
newer period" without opening the CSV. Completion checks match on the envelope, not the data.

## Escalation contract

1. Invoke form 3. Exit 0 → done.
2. Nonzero → form 2, if the manifest has one: a cheap driver re-runs with the manifest, the
   envelope, and the raw archive in hand, trying only the known branches in the recipe.
3. Still failing → form 1: frontier model, full tools, SKILL.md open. It diagnoses, repairs the
   lower forms, refreshes snapshots if the source legitimately changed, gets the verifier green,
   and commits. Repair, then re-demote.

The std automations service (`go/std/bg_services/automations`, hosted by stdd with
`-automations-repo`) registers automations by manifest and runs form 3 when the botnet calendar
fires it: ping ticks execcal, execcal reads the calendar's active executable instances, and each
fire lands on the automations service, which answers satisfied/paced/enqueued purely from the
runs table. Every run's envelope is recorded — `needs_human` and `escalation_reason` included.
Routing envelopes up the 3→2→1 chain is still done by hand; every automation is written as if
the full chain service already exists.

## Reuse before discovery

Form-1 discovery for a NEW automation starts by surveying the registry for kin: an existing
automation on the same source or family (another FRED series, another tradedata.go.kr tradeKind,
another NBS release). If kin exists, read its SKILL.md and scripts before touching the source —
endpoint quirks, auth/session behavior, and parser shape usually transfer wholesale, and a new
FRED series may be a one-line addition to the kin's fetcher rather than a new automation at all.
Kin is not a field — find it by reading the registry and grepping automation README.mds for the
source. Prefer extending kin over creating a sibling; when a sibling is genuinely warranted,
cross-reference the two README.mds in prose so the next discovery finds the pair. Knowledge
earned once is paid for once.

## Registry

- **korea-trass** — `skills/web_control/korea_trass/` — Korean customs memory-chip exports (forms 3/2/1; per-HSK 10-day by-country capped behind paywall)
- **fred-m2** — `skills/econ_data/fred_m2/` — US M2 money supply from FRED (forms 3/1)
