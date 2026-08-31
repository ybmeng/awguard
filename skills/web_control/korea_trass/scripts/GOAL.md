# Goal

Track Korean customs export data for memory semiconductors with as little human effort as possible.

The view we want, refreshed on the customs release schedule (prior month on the 1st, day 1~10 on
the 11th, day 1~20 on the 21st):

1. Monthly per-HSK-10 series for all memory codes (HS6 854232 children, incl. DRAM 8542321010,
   MCP 8542323000, Flash 8542321030). Export and import, value (thousand USD) and weight (tons).
2. Current-month pace via 10-day cumulative provisional exports (01~10 / 01~20 / 01~end),
   category level (전체, 반도체, ...).
3. Per-HSK 10-day provisional (e.g. DRAM Aug 1~20). This one is captcha-gated on TRASS.

## What is automated

`fetch_kcs.py` pulls 1 and 2 from tradedata.go.kr backend endpoints. No login, no cookies, no
browser. It saves the raw JSON next to the CSVs, so every run is auditable and testable offline.

Tests run against committed snapshots of real responses. They never touch the network, so a red
test means the parser or the script broke, not the site. If the site changes its response shape,
refresh a snapshot with a live fetch and diff.

## What needs a human

- Per-HSK 10-day provisional (item 3) lives behind a reCAPTCHA on TRASS 잠정치조회. A script can
  drive everything except the checkbox. Flow: script prepares the query in a browser tab, human
  clicks "I'm not a robot" once per session, script submits and parses. Not built yet; the manual
  recipe is in ../SKILL.md.
- Nothing else. Items 1 and 2 run unattended.
