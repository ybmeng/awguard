---
name: fred-m2
description: US M2 money supply from FRED — the no-key fredgraph.csv endpoint, the three series, the H.6 release calendar, and the gotchas that break a naive fetcher
---

# fred-m2 — US M2 money supply from FRED

Form-1 knowledge for the `fred-m2` automation. Read this when the form-3 script
(`scripts/fetch_fred.py`) fails and the offline verifier is green — that combination means FRED
changed, and the answer is somewhere below.

## Endpoint

```
https://fred.stlouisfed.org/graph/fredgraph.csv?id=<SERIES_ID>
```

Verified live 2026-08-31. Verbatim facts about it:

- **No API key, no auth, no cookie priming.** A cold `urllib.request.urlopen` with Python's
  default User-Agent returns 200. The script sends a browser-ish UA anyway because the response
  carries Akamai bot-management cookies (`_abck`, `bm_sz`), so the protection exists and could be
  tightened; do not remove the header on the theory that it is unnecessary today.
- **No redirect handling needed.** `http://` and `https://` both land on the https URL directly.
- Response headers: `content-type: application/csv`,
  `content-disposition: attachment; filename="<SERIES_ID>.csv"`.
- Body shape, LF-terminated:

  ```
  observation_date,M2SL
  1959-01-01,286.6
  ...
  2026-07-01,23218.0
  ```

- Optional `cosd=YYYY-MM-DD` (start) and `coed=YYYY-MM-DD` (end) trim the range. The script
  exposes `--start` for `cosd`; the committed snapshots were captured with it.
- An unknown series id returns **HTTP 404 with a 29 KB HTML error page**, not an empty CSV.

## Series

| key | id | meaning | cadence | units |
|---|---|---|---|---|
| `m2sl` | `M2SL` | M2, monthly average, **seasonally adjusted** — the headline M2 | monthly, from 1959-01 | $ billions |
| `wm2ns` | `WM2NS` | M2, weekly average ending **Monday**, **not** seasonally adjusted — the freshest M2 reading | weekly, from 1981-01-05 | $ billions |
| `m2real` | `M2REAL` | Real M2: M2SL deflated by CPIAUCSL | monthly, from 1959-01 | billions of chained 1982-84 dollars |

Sanity anchors as of the 2026-08-25 H.6 release: M2SL 2026-07-01 = **23218.0** ($23.2T),
WM2NS 2026-08-03 = **23207.7**, M2REAL 2026-07-01 = **6976.3**. `check_plausible()` in the script
holds a floor per series (15000 for the two nominal ones, 4000 for M2REAL). Those floors only
catch a series swapped for an unrelated one or a units change — M2 is never revised by anything
close to that much. Raise them if they ever start to look slack.

`M2REAL` comes free in the same request pattern, so the script fetches all three. It is **not**
a Board series in H.6 — the St. Louis Fed computes it — and it therefore has a second upstream
dependency on CPI. See the shutdown gap below.

## Release cadence

The upstream is the Federal Reserve **H.6 "Money Stock Measures"** release.

- **Fourth Tuesday of every month, 1:00 p.m. ET**, shifted to the next business day when that
  Tuesday is a federal holiday. Verbatim from federalreserve.gov/releases/h6/: *"These data are
  released on the fourth Tuesday of every month, generally at 1:00 p.m."*
- **H.6 has been monthly since 2021-02-23.** Before that it was weekly, Thursdays 4:30 p.m. ET;
  the last weekly release was 2021-02-11. Anything that describes H.6 as a weekly Thursday
  release is pre-2021 and wrong.
- **Weekly WM2NS does not have its own schedule.** It advances only on the monthly H.6. Each
  release extends WM2NS through the Monday-ending week containing the last day of the reference
  month, which works out to **release date minus 22 days** (checked against the Apr 28, Jul 28
  and Aug 25 2026 releases). Weekly *seasonally adjusted* M2 was discontinued outright and is
  frozen at the week ending 2021-02-01 — do not add it.
- **Lag:** monthly M2SL/M2REAL land one calendar month behind; WM2NS lands ~22 days behind.
- **FRED mirrors within about a minute.** M2SL and WM2NS both carried
  `Updated: Aug 25, 2026 12:01 PM CDT` for the 1:00 p.m. ET release. A poll at 13:05 ET is safe.
- **Next 2026 releases:** Sep 22, Oct 27, Nov 24, Dec 22 — all 1:00 p.m. ET.

Sources: [H.6 release and schedule](https://www.federalreserve.gov/releases/h6/),
[H.6 About — weekly data availability](https://www.federalreserve.gov/releases/h6/about.htm),
[H.6 feed page — the 2021 weekly-to-monthly change](https://www.federalreserve.gov/feeds/h6.html),
[FRED H.6 release calendar](https://fred.stlouisfed.org/releases/calendar?rid=21&y=2026).

## Gotchas

Each of these was hit while building, and each has a test in `scripts/tests/test_parsers.py`.

**A multi-id request across frequencies returns a ZIP, not CSV.** `?id=M2SL,M2REAL` (both
monthly) returns plain CSV with one column per series. `?id=M2SL,WM2NS` returns
`application/zip` containing `README.txt`, `monthly.csv`, and `weekly,_ending_monday.csv`. There
is no error and no warning — a naive fetcher writes a binary blob into a `.csv`. The script
therefore issues **one request per series** and rejects any body starting with `PK`. Snapshot:
`snapshots/mixed_frequency.zip`.

**A `cosd` past the end of the series is silently ignored.** `?id=M2SL&cosd=2030-01-01` returns
the *entire* series from 1959, not an empty result. So `cosd` cannot be used as a guard, and
there is no way to make fredgraph return a genuinely empty body — the zero-row path is tested
against a constructed header-only body, not a snapshot.

**The date column was renamed.** It is `observation_date` today; older captures and other FRED
export paths say `DATE`. The parser accepts either but insists the *second* column name equals
the requested series id, which is what actually catches a wrong-series response.

**Missing observations come back two ways.** FRED's documented marker is `.`, but a genuine gap
appears as an *empty* field: `2025-10-01,` in `snapshots/m2real_2024_2026.csv`. That specific
hole is real — no October 2025 CPI was published (government shutdown), so M2REAL has no October
2025 value while M2SL does. Consequence: **M2REAL is not always the same length as M2SL, and its
dates are not a gapless monthly sequence.** The parser drops both forms.

**M2REAL can fall a month behind M2SL.** CPI for month *m* lands around the 12th of *m+1*,
comfortably before the fourth Tuesday, so in practice the two advance together. A delayed CPI
print stalls M2REAL alone — which surfaces as `status: degraded` at worst, never as bad data.

**The Fed's Data Download Program is being retired**, with "Build Your Package" going away in
**November 2026** and FRED named as the replacement. This automation already reads FRED, so it is
unaffected; noted so nobody "fixes" it by pointing at DDP.

## Repair notes

- Offline tests red → the script broke. Fix the code; the snapshots are the contract.
- Offline tests green, live run red → FRED changed. The failing run's raw body is in `data/raw/`
  (error bodies included, since a non-200 is archived before it is raised). Diff it against
  `scripts/tests/snapshots/`, fix the parser, **re-capture the snapshots** with `--start` so they
  stay small, update the expected counts, and commit.
- `N observations is fewer than the M already in <file>` → the script refused to shrink a good
  CSV. That is the no-clobber guard, and it is correct to be loud. Confirm against the raw body
  that FRED really did drop history before overriding with `--allow-shrink`.
- Re-capture command for snapshots:
  ```
  B=https://fred.stlouisfed.org/graph/fredgraph.csv
  curl -sS -o scripts/tests/snapshots/m2sl_2024_2026.csv   "$B?id=M2SL&cosd=2024-01-01"
  curl -sS -o scripts/tests/snapshots/wm2ns_2026.csv       "$B?id=WM2NS&cosd=2026-01-01"
  curl -sS -o scripts/tests/snapshots/m2real_2024_2026.csv "$B?id=M2REAL&cosd=2024-01-01"
  ```
  `unknown_series_404_head.html` is the first 2048 bytes of the real `?id=NOTASERIES` body;
  `mixed_frequency.zip` is the real `?id=M2SL,WM2NS` body.

## Fallbacks, if fredgraph.csv ever goes away

Not needed as of 2026-08-31, in preference order:

1. `https://api.stlouisfed.org/fred/series/observations?series_id=<ID>&api_key=<KEY>&file_type=json`
   — the documented API, but it needs a free key, so it would introduce a secret this automation
   currently does not have. Prefer it over scraping.
2. `https://fred.stlouisfed.org/data/<SERIES>` — checked 2026-08-31 and it is **no longer a
   plain-text file**: `/data/M2SL.txt` 301s to `/data/M2SL`, which serves a 142 KB HTML page
   (`<title>Table Data - M2 …`) with all 811 observations in a table, latest 23218.0. Parseable,
   but it is HTML with no content contract. Scraping fallback only.
3. The H.6 release itself at `federalreserve.gov/releases/h6/current/` — HTML tables, monthly
   only, no weekly series. Last resort.
