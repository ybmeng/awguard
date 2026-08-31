# Recipe: TRASS per-HSK 10-day provisional, by country

Form 2. Frozen steps — a driver may observe, retry, and pick among the known branches at the bottom,
but must not invent new ones. Anything not covered here is a form-1 escalation (SKILL.md).

Gets: one HS10 code's 10-day cumulative provisional exports (01~10 / 01~20 / 01~말일) split by
destination country. This is the slice form 3 cannot reach — the KCS tentative endpoint is
category-level only. Free tier returns the top country row per query, so specific countries are
fetched one targeted query at a time (step 9).

Needs: a browser (claude-in-chrome), and a human on call for step 6 exactly once per session.

Drive everything by element id. Google auto-translate rewrites the labels and breaks the form's JS
(it submits `hsSgn=undefined` and silently returns zero rows) — keep the page in Korean and decline
any translate prompt.

---

1. **Open the 잠정치조회 view.**
   `https://www.bandtrass.or.kr/customs/total.do?command=CUS001View&viewCode=CUS00401`
   *Expect:* the page renders in Korean with the default free grid — top-20 categories × 10-day
   period, ~23 rows, columns including 전체 and 반도체. No login prompt: the wall on this site is the
   captcha, not membership.

2. **Stub the dialogs and the popup opener, before touching any control.**
   `window.alert = window.confirm = function () { return true }; window.open = function () { return null }`
   *Expect:* no error. A native alert freezes the Chrome MCP extension, and step 3 tries to spawn a
   tradedata.go.kr popup tab. These stubs die on every page load — reinstall them after any reload.

3. **Reveal the per-HSK form by toggling the grid type A → D.**
   `$('input[name=grid_type][value=A]').trigger('click'); $('input[name=grid_type][value=D]').trigger('click')`
   *Expect:* `#SelectCd` now exists in the DOM (it is absent on fresh load — D is already checked and
   renders the category variant, so re-clicking D alone does nothing; the hop through A is what
   re-renders). Alongside it: a 품목검색 button and a 국가 dropdown defaulting to 전체.

4. **Fill the query.** `#SelectCd` = the HS10 code (e.g. `8542321010`); `SEARCH_YEAR` = year;
   `S0010001` = `E` (수출); `S0010002` = `H`; `UNIT` as wanted; the 국가 dropdown = 전체 for now.
   *Expect:* the fields hold what you set and nothing navigates. One code per query — the Ⓟ
   다중조회 / 일괄조회 modes are member-only and dead with a JS error for non-members.

5. **Submit: call `goSearch()` once.**
   *Expect:* a DOM modal (not a JS alert) carrying an authentication notice and a Google reCAPTCHA
   "I'm not a robot" checkbox.

6. **HUMAN GATE — the human clicks the reCAPTCHA checkbox.** Hand off with the modal already open and
   run zero automation between their click and the result: tokens expire in about two minutes, so a
   modal parked waiting on a round-trip goes stale.
   *Expect:* the modal closes by itself and rows render.
   **Do not call `goSearch()` again after the click.** The site's own `fn_callback` runs it — a second
   call double-submits, burns the token, and re-pops the modal.
   The token is **per session, not per query**: once this step has succeeded, repeat steps 4–5 for
   further codes and countries and skip step 6 for as long as the session lives.

7. **Read the results from the page JS var, not the table:** `window.data_`.
   *Expect:* an array of row objects; the rendered jqGrid lags the data. Columns are
   품목코드 / 품목명 / 국가명 / 금액(달러) / 금액(원화) / 중량(Kg). From an automation-driven tab
   `javascript_tool` reads the var directly; from a tab the human drives, capture it with the
   GiveItToClaude extension instead (its executeScript runs in world MAIN, so it sees page vars).

8. **Check for the server-side paywall cut:** compare `window.data_.length` against the 검색결과 N건
   count printed on the page.
   *Expect on the free tier:* N건 reports many rows while `window.data_` holds exactly one — the top
   country — next to the notice "정회원 종합형/선택형 서비스 신청 시 전체 데이터가 출력 됩니다".
   That is the paywall, not a page-size setting. Do not retry it and do not try to page past it.

9. **For each country you actually want, repeat steps 4–5 with the 국가 dropdown set to that country**
   (대만, 중국, 베트남, 홍콩, 미국 …), reading each result per step 7.
   *Expect:* one exact row per query, free. This is the whole free path to per-HSK 10-day by-country
   numbers; a complete table needs 정회원 선택형 (see the paywall placeholder in SKILL.md).

---

## Known branches

- **Rows appear with no modal at all** — a session token is already live from an earlier step 6.
  Continue at step 7.
- **Empty grid, no modal, no error** — the query went out without a captcha token; a custom-HSK
  무료조회 returns an empty grid silently. Reload from step 1 (reinstall the stubs) and redo the form.
- **Modal re-pops right after the human clicked** — `goSearch()` was called a second time (step 6).
  Redo from step 4; the human must click again.
- **`#SelectCd` missing** — step 3's A → D toggle did not take effect. Redo step 3.
- **Zero rows, or a submitted `hsSgn=undefined`** — the page got auto-translated. Reload in Korean.
- **A popup tab opened, or the extension hangs** — the step 2 stubs were lost to a page reload.
  Reinstall them and redo from step 3.
- **Opening a URL to "restore" a previous query** — query state is not in the URL; the form must be
  rebuilt from step 3 every time.

## Escalate to form 1 (SKILL.md) when

The element ids are gone (`#SelectCd`, `grid_type`, `goSearch`, `window.data_`), the gate is no longer
a reCAPTCHA, or the free tier stops returning even the top-1 row.

## Reporting

No silent endings: end every run of this recipe by emitting exactly one envelope with `form_used: 2`,
even when you stop early. Three ways a run can end here.

Parked at step 6 waiting on the person — name the physical action, because the human is the next form
up, and leave the modal open so their click lands on a live token:

```json
{"automation": "korea-trass", "status": "needs_human", "form_used": 2, "artifacts": [],
 "escalation_reason": "click the 'I'm not a robot' reCAPTCHA checkbox in the open TRASS 잠정치조회 tab (step 6); the modal is already open and the token expires about 2 minutes after the click"}
```

Rows read successfully — `status: "ok"`, `escalation_reason: null`, with an artifact entry for whatever
you wrote (`rows` = data rows, `newest` = the latest period in it). A result the free tier truncated to
the top-1 row (step 8) is still `ok` with a null reason: that cap is the documented paywall and the
expected outcome here, not an anomaly to escalate. Keep `escalation_reason` non-null only when
something needs the next form up — otherwise a server watching for it cannot tell a normal run from
one that needs attention.

A known branch exhausted without rows — `status: "failed"`, `artifacts: []`, and `escalation_reason`
stating what was observed and which branch you tried, so form 1 starts from evidence.
