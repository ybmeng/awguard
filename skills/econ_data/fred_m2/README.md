---
name: fred-m2
goal: Keep US M2 money supply CSVs (monthly SA, weekly NSA, real) current from FRED's no-key CSV endpoint
forms:
  "3": python3 scripts/fetch_fred.py all
  "1": SKILL.md
verify: python3 scripts/tests/test_parsers.py
cadence: Monthly. Fed H.6 "Money Stock Measures" posts the fourth Tuesday at 1:00 p.m. ET (next business day if that Tuesday is a federal holiday); FRED mirrors within ~1 minute, so poll 13:05 ET and retry through the next business day. Next 2026 dates - Sep 22, Oct 27, Nov 24, Dec 22.
schedule:
  rrule: "FREQ=MONTHLY;BYDAY=4TU"
  at: "13:05"
  tz: "America/New_York"
  retry_every: 2h
  retry_for: 30h
human_gates: none
---

Pure API automation — no browser, no auth, no form 2. Every series is one GET against
`https://fred.stlouisfed.org/graph/fredgraph.csv?id=<SERIES_ID>`, no key required.

## Current state

Last verified live 2026-08-31, `status: ok`. Contents of each artifact are specified under
**Data spec** below; this is where they stood at that run.

| CSV | FRED series | rows | newest observation |
|---|---|---|---|
| `data/m2sl.csv` | `M2SL` | 811 | 2026-07-01 = 23218.0 |
| `data/wm2ns.csv` | `WM2NS` | 2379 | 2026-08-03 = 23207.7 |
| `data/m2real.csv` | `M2REAL` | 810 | 2026-07-01 = 6976.3 |

Each run rewrites the full series, so `rows` grows by one per release rather than the file being
appended to; a completion check is still "one more row, newer `newest`" on the envelope.

`python3 scripts/fetch_fred.py <m2sl|wm2ns|m2real>` runs a single series. `--out-dir` redirects
output (used by the tests and by any dry run). `--start YYYY-MM-DD` sets `cosd`.

## Before invoking

- Exit 0 only when `status` is `ok`. `degraded` (some series fetched, some failed) and `failed`
  both exit 1, and `degraded` still leaves the successful CSVs updated on disk.
- The envelope is the last stdout line and is also written to `data/last_result.json`.
- **The script never overwrites a good CSV.** A parse failure, a zero-row body, an implausible
  latest value, or a fetch that returns fewer rows than the CSV already holds all abort that
  series with the file untouched. `--allow-shrink` overrides the last of those and is a form-1
  decision, not something a driver should reach for.
- Every response is archived to `data/raw/<series>_<timestamp>.csv` **before** it is validated,
  non-200 error bodies included, so an escalation always has evidence. `data/raw/` and
  `data/fetch_log.txt` are gitignored; the CSVs and `last_result.json` are committed.
- M2REAL is not from H.6 — the St. Louis Fed computes it from M2SL and CPI, so it can lag M2SL by
  a month if a CPI print is delayed. That shows up as `degraded`, never as bad data, and it is
  also why `m2real.csv` is one row shorter than `m2sl.csv`: October 2025 has no CPI (shutdown)
  and so no real M2.

## Reporting

**Form 3 is the only form.** FRED serves every series this automation needs over a free, keyless
GET, so per the spec there are no recipes, no browser paths, and no form-2 machinery to maintain.
`recipes/` does not exist and `forms."2"` is absent from the frontmatter. SKILL.md (form 1) is a
repair manual, not an invocation path — a driver never "falls back to form 1" to get today's
data, it escalates to form 1 to fix form 3.

**Where the envelope lands.** Every run writes exactly one envelope to two places: the **last
line of stdout** (single-line JSON, and it is the only thing this script prints to stdout) and
**`data/last_result.json`** (pretty-printed, overwritten every run including failed ones). Human
progress and error text go to stderr, never stdout. If the `last_result.json` write itself fails
the stdout envelope is still emitted and a warning goes to stderr — stdout is the contract.

**No silent endings.** A run cannot end without an envelope. Any unexpected exception, including
one from a bug in this script rather than from FRED, is caught, described in
`escalation_reason` as `unexpected <ExceptionType>: <message>`, and reported as `failed` with
exit 1. Three tests in the offline suite pin this down, including one that makes the output
directory genuinely unwritable.

**Statuses this automation can emit** — three of the four:

| status | exit | what it concretely means here |
|---|---|---|
| `ok` | 0 | All three series fetched, parsed, sanity-checked, and written. `escalation_reason` is null. |
| `degraded` | 1 | At least one series succeeded and at least one failed. The successful CSVs **are** updated on disk and are safe to consume; only the named series is stale. In practice this is `m2real` alone failing, since it depends on CPI as well as H.6. |
| `failed` | 1 | No series produced a usable CSV. Every CSV on disk is untouched and therefore still the last known-good vintage. Typically FRED being unreachable or returning non-CSV to every request. |

**`needs_human` can never occur.** It is defined as a form-2 run parking at a human gate; this
automation has no form 2 and `human_gates: none`. Nothing in the code can construct that status,
and a test asserts `build_envelope` only ever returns `ok`, `degraded`, or `failed`. A server
routing envelopes from `fred-m2` never needs a human-notification path.

**Reading `newest` — the WM2NS caveat, which will otherwise look like a failure.** All three
artifacts advance on the *same* monthly beat. `wm2ns` is the freshest *reading* (its newest
observation is ~22 days old rather than ~1 month) but it is **not** refreshed more often: weekly
M2 has had no publication schedule of its own since H.6 went monthly in February 2021, and it
moves only when the monthly H.6 lands.

So a completion check of the form "one more row, newer period" holds **only across a release
boundary**. Between releases, every run is expected to return a byte-identical envelope, and
**`wm2ns` showing no new rows for up to about four weeks is normal, not a failure.** Do not treat
a flat `newest` as staleness, do not retry against it, and do not escalate on it. The only
staleness worth alerting on is `newest` failing to advance *after* a scheduled release date has
passed — see `cadence` in the frontmatter for those dates.

Because the source restates history (see Data spec), `rows` staying the same does not mean the
file is unchanged: a re-fetch can rewrite past values with the same row count.

## Data spec

All three artifacts share one shape, so the format, grain, and columns are stated once and the
per-artifact subsections give only what differs.

**Format.** CSV, UTF-8, LF line endings, one header row `date,value`, no quoting (no field
contains a comma), trailing newline. No index column, no footer, no blank lines.

**Grain.** One row is one observation of one series at one period. `date` is the key and is
unique within a file. There is exactly one file per series, so the series id is not a column.

**Columns.**

| column | type | meaning |
|---|---|---|
| `date` | ISO 8601 calendar date, `YYYY-MM-DD` | The period the observation covers, labelled by its **first** day for monthly series and by the **Monday that ends** the week for `wm2ns`. Not the publication date. |
| `value` | decimal number, written as-is from the source (one decimal place in practice; parse as float, do not assume the precision) | The series level for that period. Units differ per artifact — see below. |

**Sort order.** Guaranteed ascending by `date`; the fetcher rejects a response that is not, so a
consumer may rely on the last row being the newest observation.

**Provenance.** Source institution: the **Board of Governors of the Federal Reserve System**,
statistical release H.6 "Money Stock Measures" — except `m2real`, which the **Federal Reserve
Bank of St. Louis** computes from Board M2 and BLS CPI. All three are read **via FRED**, one GET
per series, no API key:

```
https://fred.stlouisfed.org/graph/fredgraph.csv?id=M2SL
https://fred.stlouisfed.org/graph/fredgraph.csv?id=WM2NS
https://fred.stlouisfed.org/graph/fredgraph.csv?id=M2REAL
```

**Transformations applied by the fetcher.** Exactly three, all structural:

1. Rename column `observation_date` (or legacy `DATE`) → `date`.
2. Rename column `<SERIES_ID>` → `value`.
3. Drop rows whose value is missing — FRED writes those as `.` or as an empty field.

Nothing else. Values are copied verbatim as text: no rounding, rescaling, deflating,
interpolating, filling, resampling, or derived columns. No rows are filtered on date.

**Revision behavior — the source restates history.** FRED is **not append-only**. The Board
revises earlier months and re-estimates seasonal factors (an annual seasonal refactoring moves a
long run of past M2SL values at once), so **a re-fetch can change the `value` on rows that
already existed, not just add a new one**. A consumer must treat each CSV as a full snapshot
taken at the fetch time in `data/fetch_log.txt`, never as an append-only log, and must not cache
old rows on the assumption they are final. Diffing two vintages is the only way to see a
revision; the previous vintages are in `data/raw/`. What does *not* happen: dates are never
removed and the series never shortens, and the fetcher enforces that by refusing to write a file
with fewer rows than it already has.

### `data/m2sl.csv`

M2 money stock, monthly average of daily figures, **seasonally adjusted**. Units: **billions of
current US dollars**. One row per calendar month, no gaps, from `1959-01-01`. FRED series `M2SL`.

### `data/wm2ns.csv`

M2 money stock, weekly average of daily figures, **not seasonally adjusted**. Units: **billions
of current US dollars**. One row per week, dates exactly 7 days apart with no gaps, from
`1981-01-05`. FRED series `WM2NS`. The seasonally adjusted weekly counterpart was discontinued in
2021 and must not be substituted.

### `data/m2real.csv`

Real M2: `m2sl` deflated by CPIAUCSL. Units: **billions of chained 1982-84 US dollars** — a
different scale from the other two (roughly 6,900 where nominal M2 is roughly 23,200), so never
compare or concatenate the levels across artifacts. FRED series `M2REAL`. Monthly from
`1959-01-01`, but **gaps are possible and one exists**: `2025-10-01` is absent because no October
2025 CPI was published. A consumer must not assume a gapless monthly sequence here, nor that this
file has the same row count as `m2sl.csv`.

## Escalation

Offline verifier red means the code broke. Verifier green plus a red live run means FRED changed
— go to `SKILL.md`, which carries the endpoint spec, the release calendar, the gotchas
(mixed-frequency requests returning a ZIP, `cosd` past the series end being ignored, the
`observation_date`/`DATE` header rename, gaps arriving as an empty field rather than `.`), and
the snapshot re-capture commands.

## Adding another FRED series

This is the repo's FRED automation — treat it as kin for **any** future FRED series, not just M2
ones. Adding a series here is a one-entry addition to the `SERIES` dict in
`scripts/fetch_fred.py` (id, meaning, plausibility floor) plus a snapshot and its tests; the
fetch, parse, archive, no-clobber, and envelope machinery is series-agnostic and already handles
monthly and weekly cadences. Prefer extending this fetcher over standing up a sibling automation
that would rediscover the same endpoint quirks. A sibling is only warranted for a genuinely
different FRED access path — the keyed `api.stlouisfed.org` route, say — and if one is ever
created, cross-reference it here and in its own README so the next discovery finds the pair.
