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

Reading the envelope: `monthly.csv` and `monthly_by_country.csv` carry 확정 figures, so their `newest`
lags today by six weeks or so and only moves around the 15th. `tentative.csv` is the fast-moving one —
its `newest` is `"YYYYMM 01~10|01~20|01~말일"` and should advance on the 1st, 11th, and 21st.
