# korea_trass — Korean customs memory-chip export data

Goal: provisional (잠정) and monthly export stats for memory HSK codes.
Target codes: 8542321010 (DRAM), 8542323000 (MCP/HBM proxy), 8542321030 (Flash).
Fields: export value (USD) + weight, incl. 10-day provisional periods and by-country splits.

## CURRENT STATE (2026-08-31) — read this first

- `scripts/fetch_kcs.py all` (no auth, no browser) pulls three CSVs into data/: `monthly`
  (7 memory codes × month), `monthly_by_country` (code × destination country × month; 499 rows
  live-verified), `tentative` (10-day category pace incl. 반도체). 24 offline snapshot tests:
  `python3 scripts/tests/test_parsers.py` from this dir. Raw JSON archived per run; a 0-row or
  bad parse exits 1 and never overwrites a good CSV.
- Release cadence (KST): full-month provisional on the 1st ~09:00, 1~10 on the 11th, 1~20 on the
  21st; monthly 확정 (incl. by-country) ~the 15th of the following month. 09:00 KST = 5pm PT prior
  day. Endpoint refresh may trail the press release by minutes–hours; rerun until the row appears.
- Only unreachable slice: FULL 10-day per-HSK by-country tables — see the paywall placeholder
  below. Free stopgap: per-session captcha + targeted 국가= queries on TRASS.
- Human-in-the-loop capture tool: `chromeExtension_GiveItToClaude` (repo root) saves any page's
  rendered DOM + page-JS data stores (window.data_, CHART_FULL_DATA) to ~/Downloads/GiveItToClaude/
  on one click. Use when a site blocks automation: the human drives, Claude ingests files.

## Hard-won gotchas (transferable)

- TRASS reCAPTCHA is PER-SESSION: one human click then many queries. After the checkbox, the
  site's own `fn_callback` runs `goSearch()` itself — do NOT also call goSearch (double-submit
  consumes the token and re-pops the modal). Captcha tokens expire in ~2 min: never park a modal
  waiting for a human round-trip; arrange for zero automation between their click and the query.
- Free-tier truncation can be SERVER-SIDE (top-1 row + "정회원... 전체 데이터가 출력" notice) —
  check `window.data_` length vs 검색결과 N건 to tell UI truncation from a real paywall.
- jqGrid pages: rendered DOM may lag the data; read `window.data_` (page JS var) instead of the
  table. From an automation-driven tab, javascript_tool reads it directly; from a human's own tab
  you need the extension (its executeScript runs in world:MAIN to see page vars).
- Opening a URL fresh loses SPA/form state — query state is not in the URL; redo the form.
- On this site: stub window.alert/confirm/open BEFORE any risky action (alerts freeze the Chrome
  MCP extension; grid-type toggles spawn popup tabs); stubs die on page reload — reinstall.
- The MCP extension's data filter [BLOCKED: Cookie/query string data] hits raw querystrings/script
  dumps — reformat as `key => value` lines or extract identifiers only.
- Chrome auto-translate breaks Korean sites' JS forms (reads rewritten labels) — keep pages in
  Korean; drive by element ids, never visible text.

## Site map (verified 2026-08-31, freeform)

- Entry `https://www.trass.or.kr` → redirects to `https://www.bandtrass.or.kr/index.do`.
- Top-right: 로그인 (login) / 회원가입 (signup) → `https://www.bandtrass.or.kr/regist.do`.
- Signup page structure:
  - Two consent panels: 이용약관 (terms, required) + 개인정보 수집 및 이용 동의 (privacy, required)
    with checkboxes: 위의 약관에 대하여 동의합니다 / 위의 개인정보...동의합니다 / 만14세 이상입니다.
  - Member types (each with a 가입하기 button, `goNextStep('C000300X','C0004003')`):
    - 사업자 회원 — businesses; needs 사업자용 공동인증서 (corporate certificate). NOT us.
    - 비사업자 회원 — 일반인/대학생/연구기관 etc., 무역통계 조회 서비스 이용. ← the one we want.
  - Membership is free; certificates only needed for a company's own export records (자사실적증명).
- Query route once logged in (per user's research, unverified yet):
  무역통계조회 → 수출입통관통계 → 품목별 → 품목별수출입통계 / 품목의 국가별 수출입실적.
  Look for 잠정통계 (provisional) / 주별 wording vs 확정치 (finalized) for the 1–20-of-month data.
  Params: HSK code, 수출 (exports), 국가=전체, 금액=USD, 기간.

## Control notes

- Browser automation via claude-in-chrome MCP tools.
- Account creation / password entry / consent-accepting is done BY THE USER in the same tab;
  automation takes over after login.
- Site JS: clicking 가입하기 with consents unchecked likely fires a JS alert — alerts freeze the
  Chrome extension. Never click submit-like buttons unless prerequisites are known-satisfied.
- javascript_tool results that include page script source can get [BLOCKED: Cookie/query string data]
  — prefer read_page / find / screenshots for inspection on this site.

## Signup blocker (verified 2026-08-31)

비사업자 signup REQUIRES Korean SMS verification: fields hpno_1/2/3 → 인증번호 발송 →
6-digit code in `hpnoconfirm` → 인증번호 확인 sets hidden `hpnoCheck`. No i-PIN/cert alternative
on the form. User has no Korean number → TRASS account currently unobtainable.

## Fallback: 관세청 tradedata.go.kr — NO LOGIN NEEDED (verified 2026-08-31, subagent probe)

https://tradedata.go.kr → /cts/index.do (SPA, URL never changes). Anonymous queries work.

- **Monthly per-HSK-10**: menu 수출입통계 > 수출입 실적 (품목별). HS code input + [+] chip
  (resolves names: 8542321010→디램), multi-code OK, 연도별/월별 range, 수리일/출항일 basis,
  weight unit 톤/Kg. Data through 2026.07 (확정 through Jul 31). Units: 천 달러, tons.
  Columns: 기간, HS코드, 품목명, 수출 중량/금액, 수입 중량/금액, 무역수지.
- **10-day provisional (1~10/1~20/1~말일)**: menu 수출입통계 > 10일 단위 잠정치 통계.
  2026.08 available. BUT category-level only (~13 buckets: 반도체, 석유제품…), NO per-HSK.
  Amounts only, no weight. Also 국가별 toggle.
- **Direct POST API (no auth, JSON)**:
  `POST /cts/hmpg/retrieveTrade.do` body:
  `tradeKind=ETS_MNK_1020000A&priodKind=MON&priodFr=202601&priodTo=202607&statsBase=acptDd&ttwgTpcd=1000&showPagingLine=15&sortColumn=&sortOrder=&hsSgnGrpCol=HS10_SGN&hsSgnWhrCol=HS10_SGN&hsSgn=8542321010`
  → `{count, items:[{priodTitle, hsSgn, korePrlstNm, expTtwg, expUsdAmt, impTtwg, impUsdAmt, cmtrBlncAmt}]}`,
  first row = 총계. ttwgTpcd=1000 → tons. Verified from page context; bare-curl (cookie-less) untested.
  10-day provisional API (captured via fetch/XHR hook, verified cookie-less):
  `POST /cts/hmpg/retrieveTentativeValues.do` body
  `statsKind=ETS_MNK_1050000A&imexTpcd=E&priodKind=MON&priodFr=YYYYMM&priodTo=YYYYMM&priodDate=&showPagingLine=100&sortColumn=&sortOrder=`
  → rows {priodMon, priodDt '01~10'/'01~20'/'01~말일', itemUsdAmt00..10} with positional columns:
  00 전체, 01 반도체, 02 철강, 03 승용차, 04 석유, 05 무선통신, 06 선박, 07 차부품, 08 컴퓨터주변,
  09 정밀기기, 10 가전 (천$; values match the TRASS CUS00401 grid exactly).
  Note: the extension's data filter blocks raw query-string/body dumps from javascript_tool —
  reformat as `key => value` lines to read captured bodies.
- Excel download icons present while logged out (unclicked/untested).
- **Cookie-less curl WORKS** on `retrieveTrade.do` (no session needed) — plain curl POST returns the
  same JSON. The whole monthly pipeline is scriptable without a browser.
- **HS enumeration trick**: `hsSgnWhrCol=HS6_SGN&hsSgn=854232&hsSgnGrpCol=HS10_SGN` returns every
  HS10 child × month. `hsSgnWhrCol=HS8_SGN` with an 8-digit code returns 0 (HS8 filter col not
  populated) — filter at HS6 or HS10 only. `korePrlstNm` is EMPTY in grouped responses; fetch names
  via single-code queries (whr=HS10). First item of items[] is a paging stub; data rows have priodTitle.
- **Monthly HS10 × COUNTRY (free, verified 2026-08-31)**: same retrieveTrade.do with
  `tradeKind=ETS_MNK_1020000E` (품목별 국가별). Extra params: `selectPaging=1&subHsSgn=` and
  `cntyNm=` comma-separated KOREAN names (미국,중국,대만…; empty = all countries); hsSgn accepts
  a comma list of HS10 codes. REQUIRES a session cookie: GET /cts/index.do first (cookie jar),
  plus Referer header — cookie-less POST returns an EUC-KR "access blocked" page. Rows add cntyNm.
  tradeKind family (from menu JS ETS0100019Q.js): A=품목별 D=국가별 E=품목별국가별 F/G=성질별국가별
  H=대륙별 I=경제권별 J=세관별 K=거래종류별 L=항구별. ~5yr max range per query.
  Sample: 2026.07 DRAM exports → 중국 9,979,070 / 베트남 1,948,783 / 홍콩 262,606 / 대만 180,701 /
  미국 64,081 (천$). Raw snapshot: data/memory_by_country_raw.json.
- **10-day per-HSK exists NOWHERE public** (subagent sweep 2026-08-31): KCS tentative endpoint is a
  fixed top-10-category layout (반도체 = one column, statsKind A=품목 B=국가 only); 산업부 1~20
  release is MTI-coded aggregates; CEIC/Bloomberg/Refinitiv mirror aggregates only; Comtrade is HS6
  (854232 lumps DRAM+MCP+Flash). Sub-monthly per-HSK = TRASS only (free top-1-country row, or
  ₩33k/mo 정회원 선택형 for full tables). data.go.kr API exists but signup is 내국인+phone-gated.
- Memory universe under HS6 854232 (7 HS10 codes): 8542321010 디램 DRAM / 8542321020 에스램 SRAM /
  8542321030 플래시 Flash / 8542321090 기타 / 8542322000 하이브리드 / 8542323000 복합구조칩 MCP /
  8542324000 복합부품 MCOs.
- **Gotcha**: Chrome auto-translate breaks the site's search (submits hsSgn=undefined, silent 0 rows).
  Keep the page in Korean, or use the API.

Sample verified numbers (exports, thousand USD): DRAM Jul-2026 13,551,552 (155.8t);
MCP Jun 12,682,181 / Jul 10,085,845; Flash Jun 2,486,161 / Jul 1,780,647.

## TRASS logged-out map (verified 2026-08-31, subagent probe)

- 무역통계 → 수출입통계 = `/customs/total.do`, views via `viewCode`:
  - 총괄 `CUS00101`: full yearly agg table, no wall.
  - 상세조회 `CUS00301`: the query builder behind "품목별/국가별" (항목1=품목, 항목2=국가).
  - 잠정치조회 `CUS00401`: provisional 10-day cumulative (1~10 / 1~20 / 1~말일 — cumulative, not weekly).
- **The wall is reCAPTCHA, not login**: building the 품목별 query fully works logged-out, but 조회
  pops a DOM modal ("Authentication is required...") with a Google reCAPTCHA checkbox before any
  rows return. On 잠정치조회, a custom-HSK 무료조회 without the captcha token silently returns an
  EMPTY grid. Login (SSO) is separate; membership plausibly waives the captcha.
- **Free without captcha**: 총괄 tables + 잠정치조회's default grid = top-20 category provisional
  (columns incl. 반도체), 23 rows, e.g. 202601 01~20 전체 36,347,003 / 반도체 10,731,563 (천$).
- **Per-HSK provisional EXISTS** via 잠정치조회 무료조회: HSK text input `#SelectCd`, radios
  grid_type D/A/B, S0010001 E/I, S0010002 H/N, `SEARCH_YEAR`, `UNIT`, submit `goSearch()` —
  single code at a time; the Ⓟ premium modes (다중조회/일괄조회) are member-only (JS-error dead
  for non-members). Release schedule: 전월 on the 1st, 1~10일 on the 11th, 1~20일 on the 21st.
- **`#SelectCd` is NOT in the DOM on fresh load** — the D radio renders the top-20 category variant.
  Trigger: change grid_type A then back to D (`$('input[name=grid_type][value=A]').trigger('click');
  $('input[name=grid_type][value=D]').trigger('click')`); re-clicking already-checked D does nothing.
  After toggle: form = #SelectCd + 품목검색 + 국가 전체 dropdown; result cols 품목코드/품목명/국가명/
  금액(달러)/금액(원화)/중량(Kg). Gotcha: the toggle spawns a tradedata.go.kr popup tab — stub
  `window.open` first (also stub alert/confirm; page reloads kill stubs, re-install after each load).
  With HSK filled, goSearch() pops the reCAPTCHA modal (DOM modal, not JS alert) → human clicks it.
- 상세조회 landmarks: `BASE_YEAR`, `EI_DITC` (ALL/E/I), `GODS_TYPE` (H/S/N), pivot chips
  `GODS_DIV`/`NATN_DIV`/…; HSK injected via `fn_receiver({select:"1",hs_cd:"…",unit:"10"})` →
  `#FILTER1_CODE`/`#FILTER1_CODE_VALUE`/`#FILTER1_GODS_UNIT`; HS popup `/hscode/hsCode.do`;
  country multiselect `#FILTER2_SELECT_CODE` (ISO-2, 전체=all).
- Google auto-translate garbles injected labels → drive by element ids, never visible text.

## Data files (data/)

- `memory_monthly_2026.csv` — all 7 memory HS10 codes × Jan–Jul 2026, export/import value (천$) +
  weight (tons) + balance. Source: tradedata.go.kr API (raw: memory_monthly_raw.json).
- `provisional_categories_2026.csv` — TRASS 잠정치조회 free grid: 2026 exports by 10-day cumulative
  period (01~10/01~20/01~end) × top-20 categories (전체, 반도체, …), 천$. Aug = through 08-20.

## TRASS paid tiers (researched 2026-08-31)

- Free-tier per-HSK 잠정치 query returns TOP-1 COUNTRY ROW ONLY (server-side truncation; notice:
  "정회원 종합형/선택형 서비스 신청 시 전체 데이터가 출력 됩니다"). Captcha is per-session, not
  per-query — one human click then multiple queries. Country dropdown accepts a specific country,
  so targeted queries (품목 × 국가=대만 etc.) get exact rows free.
- 정회원 선택형 ₩33,000/30d VAT incl. (pick 잠정치조회 as one of 3 services) = full by-country
  tables. 종합형 ₩55,000. One-off extracts ₩205k/month-of-data (confirmed stats only).
  Institutional provisional feed (BIG FINANCE) ₩1.0–1.25M/mo.
- Purchase wall: web flow needs Korean SMS 본인인증 (free 준회원 first) + Korean-PG card
  auto-billing. US path: negotiate via statistics@ktspi.or.kr (+82-2-2140-0735, 09:00–17:30 KST)
  or a Korean proxy signup. Fee pages: support.do?command=firstGuide&viewCode=SUP00501/2/3,
  charge.do?PNAME=charge1|charge3.

## Full 10-day per-HSK by-country: SKIPPED — behind Korea-required paywall

Placeholder (2026-08-31): the complete 10-day per-HSK country tables need TRASS 정회원 선택형
(₩33k/30d), which requires Korean SMS + Korean card. No reseller exists (TRASS is the sole
distributor; terminals carry aggregates only). Unblock paths when wanted: Korean friend does
signup+payment (per-account license caveat), or email statistics@ktspi.or.kr for a direct
foreign-purchase arrangement. Until then: free targeted queries (captcha/session + 국가=specific
country) cover individual rows.

## Status / route chosen

- Per-HSK **monthly** (thru latest 확정 month): tradedata.go.kr, no account, POST API. SOLVED.
- Per-HSK **1–20 provisional**: TRASS 잠정치조회 무료조회 has it, gated by a reCAPTCHA checkbox
  per query/session. Automation cannot touch captchas → flow is: script builds the query, USER
  clicks the reCAPTCHA, script submits + extracts. One HSK per query (premium multi = members).
- TRASS membership (would waive captcha + unlock multi-code): blocked on Korean SMS verification.
