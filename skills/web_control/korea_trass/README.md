---
name: korea-trass
goal: Korean customs export stats for memory HSK codes — monthly per-HS10, the same split by destination country, and the 10-day provisional category pace
forms:
  "3": python3 scripts/fetch_kcs.py all
  "2": recipes/trass_by_country.md
  "1": SKILL.md
verify: python3 scripts/tests/test_parsers.py
cadence: "KST releases: full-month provisional on the 1st ~09:00, 1~10 on the 11th, 1~20 on the 21st, monthly 확정 (incl. by-country) ~the 15th of the following month. 09:00 KST = 17:00 PT the prior day; the endpoint can trail the press release by minutes to hours, so rerun until the new period appears."
human_gates: "TRASS 잠정치조회 per-session reCAPTCHA — one human checkbox click per session unlocks the per-HSK 10-day queries in recipes/trass_by_country.md. Form 3 has no gate."
---

Form 3 covers everything reachable without a human: `scripts/fetch_kcs.py all` posts to tradedata.go.kr
(no auth, no browser, stdlib only) and maintains three CSVs in `data/` — `monthly.csv` (7 memory HS10
codes × month; `retrieveTrade.do` tradeKind `ETS_MNK_1020000A`), `monthly_by_country.csv` (code ×
destination × month; tradeKind `ETS_MNK_1020000E`), `tentative.csv` (10-day cumulative category pace,
반도체 among the columns; `retrieveTentativeValues.do`). Each run archives the raw JSON under `data/raw/`
(gitignored), appends a line to `data/fetch_log.txt`, and writes the envelope to `data/last_result.json`
as well as the last stdout line. A dataset that parses to zero rows, or whose response came back
truncated, exits 1 and leaves the existing CSV untouched.

Default window is January of the current year through the current month; `--from` / `--to` (YYYYMM),
`--hs` (6- or 10-digit; the by-country dataset also takes a comma list of 10-digit codes), and
`--countries` (comma-separated Korean names, empty = all) override it. A single dataset name in place
of `all` runs just that one.

**Not covered by any form: full 10-day per-HSK by-country tables.** They exist only on TRASS
(`bandtrass.or.kr`) behind 정회원 선택형 (₩33,000/30d), which requires Korean SMS 본인인증 and a
Korean card — no reseller, and the KCS tentative endpoint is category-level by construction. The free
stopgap is the form-2 recipe: one reCAPTCHA click per session, then targeted 국가= queries that each
return one exact row. SKILL.md holds the paywall placeholder and the unblock paths.

## Reporting

**Where the envelope lands.** `scripts/fetch_kcs.py` writes it to `data/last_result.json` (overwritten
each run, so it always describes the most recent one) and prints it as the **last stdout line**. Human
progress lines precede it and errors go to stderr, so a reader should parse the last stdout line and
nothing else. `form_used` is `3` from the script; a form-2 driver following the recipe emits its own
envelope with `form_used: 2`.

**Statuses this automation can emit.**

- `ok` (exit 0) — every requested dataset fetched, parsed to a non-empty row set, passed the
  completeness check (`count == len(items)`), and was rewritten. `artifacts` has one entry per dataset
  run: all three for `all`, one for a single-dataset invocation.
- `degraded` (exit 1) — `all` had partial success. `artifacts` lists only the datasets that succeeded;
  the ones that failed kept their previous CSV untouched, and `escalation_reason` names each with what
  was observed. Concretely, this is what a lost session cookie looks like: `monthly` and `tentative`
  are cookie-less-safe, while `monthly_by_country` (tradeKind E) answers a cookie-less POST with an
  EUC-KR block page, so it alone fails and the other two land.
- `failed` (exit 1) — nothing usable, `artifacts` empty. In practice: tradedata.go.kr unreachable, or a
  single-dataset run that failed on its own.
- `needs_human` — **`fetch_kcs.py` never emits this.** Form 3 here rides a free anonymous JSON API with
  no captcha, no login, and no human gate of any kind, so its runs can only end ok, degraded, or
  failed. Only the form-2 recipe can park on a human, and its driver emits the envelope by hand.

**No silent endings.** Every run that gets past argument parsing ends in exactly one envelope — the
script writes it on the all-datasets-failed path too, before exiting nonzero. The one exception is a
malformed command line: argparse exits 2 with usage text and no envelope, because no run started. A
server should read exit 2 with no envelope as "the invocation was wrong", not as a failed fetch.

**A `needs_human` envelope from the recipe** parks at step 6 of `recipes/trass_by_country.md` and names
the physical action, since the human is the next form up:

```json
{"automation": "korea-trass", "status": "needs_human", "form_used": 2, "artifacts": [],
 "escalation_reason": "click the 'I'm not a robot' reCAPTCHA checkbox in the open TRASS 잠정치조회 tab (recipes/trass_by_country.md step 6); the modal is already open and the token expires about 2 minutes after the click"}
```

`artifacts` is empty because nothing is written until the query returns. The gate is per session, not
per query: once a human has clicked, the driver resumes at step 7 and every further code or country
runs unattended, ending in `ok` or `failed` without parking again.

**Reading `newest`.** `monthly.csv` and `monthly_by_country.csv` carry 확정 figures, so their `newest`
lags today by six weeks or so and only moves around the 15th — unchanged values there are the normal
case, not a stall. `tentative.csv` is the fast-moving one: its `newest` is `"YYYYMM "` plus the period
(`01~10`, `01~20`, or `01~` and the month's last day) and should advance on the 1st, 11th, and 21st.

## Data spec

Common to all three artifacts: UTF-8 (no BOM), RFC4180 CSV with a header row, CRLF line endings,
minimal quoting (no field currently needs quotes). Numbers are written as plain text with no thousands
separators, no currency symbol, and no padding — the fetcher strips the source's commas and spaces and
otherwise passes the digits through untouched, so there is no rounding, unit conversion, or derived
arithmetic anywhere. No cell is ever empty: a row whose numeric field does not parse aborts the whole
dataset (exit 1) and leaves the previous file in place, so a stale file is still a complete file.
Every run rewrites its whole file from a full-window query rather than appending.

Money is **thousand USD** throughout (the source's 천 달러) — not USD. Weight is **metric tons**
(`ttwgTpcd=1000`). Getting either wrong is a 1000x error.

### data/monthly.csv

**Grain:** one row per (month, HS10 code). Key `(month, hsk)` is unique — 7 codes × each month in
range, 49 rows for Jan–Jul 2026.

| column | type | unit | meaning |
| --- | --- | --- | --- |
| `month` | `^\d{4}\.\d{2}$` e.g. `2026.07` | — | calendar month of clearance |
| `hsk` | `^\d{10}$` | — | HS10 code; one of the 7 children of HS6 854232 |
| `name` | text | — | fixed English + Korean label for that code, e.g. `DRAM (디램)` |
| `export_kusd` | integer ≥ 0 | thousand USD | export value |
| `export_tons` | decimal, 1 dp, ≥ 0 | metric tons | export weight |
| `import_kusd` | integer ≥ 0 | thousand USD | import value |
| `import_tons` | decimal, 1 dp, ≥ 0 | metric tons | import weight |
| `balance_kusd` | integer, **may be negative** | thousand USD | trade balance (source's 무역수지) |

**Sort order:** not guaranteed — source order is preserved. Observed: `month` ascending, then `hsk`
ascending within each month.

**Provenance:** 관세청 (Korea Customs Service) via its 무역통계 portal, `POST
https://tradedata.go.kr/cts/hmpg/retrieveTrade.do` with `tradeKind=ETS_MNK_1020000A` (품목별),
`priodKind=MON`, `statsBase=acptDd` (수리일 — declaration-acceptance date basis, not departure date),
`ttwgTpcd=1000`, `hsSgnGrpCol=HS10_SGN`, and by default `hsSgnWhrCol=HS6_SGN&hsSgn=854232`.
Transformations: the leading paging stub and the `총계` grand-total row are dropped; fields are renamed
(`priodTitle`→`month`, `hsSgn`→`hsk`, `expUsdAmt`→`export_kusd`, `expTtwg`→`export_tons`,
`impUsdAmt`→`import_kusd`, `impTtwg`→`import_tons`, `cmtrBlncAmt`→`balance_kusd`); `name` is supplied
by a lookup table in `fetch_kcs.py`, because the source's `korePrlstNm` comes back empty in
HS10-grouped responses. `balance_kusd` is the source's own figure, not computed here.

**Revisions:** these are 확정 (confirmed) figures, published around the 15th of the following month.
The query asks through the current month, but the source returns only months it has published — on
2026-08-31 the newest row is `2026.07`, and August simply does not exist yet rather than appearing as
a partial. Whether KCS restates already-published months is not established here; the fetcher is built
so it does not matter, since it refetches the full window and rewrites the file every run, so any
restatement propagates silently. Do not treat this file as append-only.

### data/monthly_by_country.csv

**Grain:** one row per (month, HS10 code, destination country). Key `(month, hsk, country)` is unique.
The grain is **sparse** — only combinations the source has a record for appear, so this is not a dense
7 × 76 × months grid: 1,178 rows for Jan–Jul 2026, with 41 to 61 distinct countries per month and 76
across the file.

| column | type | unit | meaning |
| --- | --- | --- | --- |
| `month` | `^\d{4}\.\d{2}$` | — | calendar month of clearance |
| `hsk` | `^\d{10}$` | — | HS10 code, as above |
| `name` | text | — | label for that code, as above |
| `country` | Korean short name, e.g. `중국`, `미국`, `대만`, `홍콩`, `베트남` | — | trade partner |
| `export_kusd` | integer ≥ 0 | thousand USD | export value to that country |
| `export_tons` | decimal, 1 dp, ≥ 0 | metric tons | export weight |
| `import_kusd` | integer ≥ 0 | thousand USD | import value from that country |
| `import_tons` | decimal, 1 dp, ≥ 0 | metric tons | import weight |
| `balance_kusd` | integer, **may be negative** | thousand USD | balance for that pair |

Zero rows are real and retained (a partner with a record but no value that month, e.g. 나미비아).

**Sort order:** not guaranteed — source order is preserved. Observed: `month` ascending, then country
in Korean collation, then `hsk` ascending.

**Reconciliation:** for a given `(month, hsk)` the country rows sum to that key's `export_kusd` in
`monthly.csv` only **within rounding** — each figure is independently rounded to thousand USD, so sums
differ by a few units (observed: at most 5 thousand USD across all 49 keys). A validator must assert
approximate agreement, never equality.

**Provenance:** same institution and endpoint as `monthly.csv`, with `tradeKind=ETS_MNK_1020000E`
(품목별 국가별) plus `selectPaging=1`, `subHsSgn=`, and `cntyNm=` (empty means all countries; the
`--countries` flag fills it with a comma-separated list of Korean names). This tradeKind rejects a
cookie-less POST with an EUC-KR block page, so the run GETs `/cts/index.do` first for a session cookie.
Transformations are those of `monthly.csv` plus: `cntyNm`→`country`, and rows with an empty `cntyNm`
are dropped as aggregates the same way the `총계` row is.

**Revisions:** 확정, identical behavior to `monthly.csv` — same publication lag, same full-rewrite
semantics, not append-only.

### data/tentative.csv

**Grain:** one row per (month, cumulative 10-day period). Key `(month, period)` is unique; a complete
month contributes 3 rows, an in-progress month 1 or 2. **Exports only** — this artifact has no import
or weight columns. Category level, not per-HSK; that split is the paywalled gap described above.

| column | type | unit | meaning |
| --- | --- | --- | --- |
| `month` | `^\d{6}$` e.g. `202608` | — | calendar month — note this is **not** the dotted `YYYY.MM` the other two files use |
| `period` | `01~10`, `01~20`, or `01~` + the month's last day (`01~28`, `01~29`, `01~30`, `01~31`) | — | days 1 through N, **cumulative from the 1st**, not a 10-day increment |
| `total` | integer ≥ 0 | thousand USD | all exports in the period |
| `semiconductors` | integer ≥ 0 | thousand USD | 반도체 |
| `steel` | integer ≥ 0 | thousand USD | 철강 |
| `passenger_cars` | integer ≥ 0 | thousand USD | 승용차 |
| `petroleum` | integer ≥ 0 | thousand USD | 석유 |
| `wireless_comm` | integer ≥ 0 | thousand USD | 무선통신 |
| `ships` | integer ≥ 0 | thousand USD | 선박 |
| `auto_parts` | integer ≥ 0 | thousand USD | 차부품 |
| `computer_peripherals` | integer ≥ 0 | thousand USD | 컴퓨터주변 |
| `precision_instr` | integer ≥ 0 | thousand USD | 정밀기기 |
| `home_appliances` | integer ≥ 0 | thousand USD | 가전 |

The ten category columns are a selected top-10 list, **not a partition of `total`** — they sum to
roughly two thirds of it, and a validator must not assert that they add up.

**Sort order:** not guaranteed — source order is preserved. Observed: `month` ascending, then `period`
by increasing end day. This ordering also makes the envelope's `newest` (`"<month> <period>"`) sort
correctly as a plain string.

**Provenance:** 관세청 10일 단위 잠정치 통계, `POST
https://tradedata.go.kr/cts/hmpg/retrieveTentativeValues.do` with `statsKind=ETS_MNK_1050000A`,
`imexTpcd=E` (exports), `priodKind=MON`, `priodDate=`. Transformations: `priodMon`→`month`,
`priodDt`→`period`, and the source's **positional** `itemUsdAmt00`..`itemUsdAmt10` columns are mapped
onto the names above by index — `00` is `total` and `01` is `semiconductors`. That positional mapping
is the fragile part of this artifact: if KCS reorders its category layout the columns will be silently
mislabelled rather than fail, so a suspicious 반도체 series is a form-1 escalation, not a parser bug.

**Revisions:** 잠정치 (provisional) throughout, and genuinely revisable — a period's value can change
after first publication, and the full-month row is later superseded by the 확정 figures that land in
`monthly.csv` (different file, different units of aggregation, so no row is overwritten across files).
Rows are not append-only: the file is rewritten from a full-window query each run.
