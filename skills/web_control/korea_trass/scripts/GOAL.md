# Goal

Track Korean customs export data for memory semiconductors with as little human effort as possible.

The view we want, refreshed on the customs release schedule (prior month on the 1st, day 1~10 on
the 11th, day 1~20 on the 21st):

1. Monthly per-HSK-10 series for all memory codes (HS6 854232 children, incl. DRAM 8542321010,
   MCP 8542323000, Flash 8542321030). Export and import, value (thousand USD) and weight (tons).
   Also the same series split per destination country (중국 / 베트남 / 홍콩 / 대만 / 미국 …), which
   is the 품목별 국가별 view — same endpoint, `tradeKind=ETS_MNK_1020000E`. Both are automated.
   These are 확정 (confirmed) figures and land around the 15th of the following month, so the
   newest month in the CSV lags today by more than the 1st-of-month provisional release does.
2. Current-month pace via 10-day cumulative provisional exports (01~10 / 01~20 / 01~end),
   category level (전체, 반도체, ...).
3. Per-HSK 10-day provisional (e.g. DRAM Aug 1~20). *SKIPPED for now — behind Korea-required
   paywall* (TRASS 정회원 ₩33k/mo; needs Korean SMS + Korean card; see ../SKILL.md placeholder).
   Free tier gives the top-1 country row per query via a per-session captcha.

## What is automated

`fetch_kcs.py` pulls 1 and 2 from tradedata.go.kr backend endpoints — three datasets: `monthly`,
`monthly_by_country`, `tentative`, or `all`. No login and no browser. It does keep a cookie jar:
the by-country tradeKind rejects a cookie-less POST with an EUC-KR block page, so the script GETs
`/cts/index.do` once per run to pick up a session before posting. It saves the raw JSON next to
the CSVs, so every run is auditable and testable offline.

Tests run against committed snapshots of real responses. They never touch the network, so a red
test means the parser or the script broke, not the site. If the site changes its response shape,
refresh a snapshot with a live fetch and diff.

## What needs a human

- Per-HSK 10-day provisional (item 3) lives behind a reCAPTCHA on TRASS 잠정치조회. A script can
  drive everything except the checkbox. Flow: script prepares the query in a browser tab, human
  clicks "I'm not a robot" once per session, script submits and parses. Not built yet; the manual
  recipe is in ../SKILL.md.
- Nothing else. Items 1 and 2 run unattended.
