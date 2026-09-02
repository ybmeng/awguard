#!/usr/bin/env python3
"""Pull Korean customs (tradedata.go.kr) trade stats into CSVs. Stdlib only, no auth."""

import argparse
import csv
import http.cookiejar
import json
import sys
import urllib.parse
import urllib.request
from datetime import datetime
from pathlib import Path

BASE_URL = "https://tradedata.go.kr/cts/hmpg/"
SESSION_URL = "https://tradedata.go.kr/cts/index.do"
USER_AGENT = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) fetch_kcs.py"
TIMEOUT_SECONDS = 60

AUTOMATION_NAME = "korea-trass"
AUTOMATION_DIR = Path(__file__).resolve().parent.parent
FORM = 3

# Grouped responses come back with korePrlstNm empty, so names live here.
HS10_NAMES = {
    "8542321010": "DRAM (디램)",
    "8542321020": "SRAM (에스램)",
    "8542321030": "Flash (플래시 메모리)",
    "8542321090": "Other memory (기타)",
    "8542322000": "Hybrid IC (하이브리드)",
    "8542323000": "MCP (복합구조칩)",
    "8542324000": "MCO (복합부품)",
}

GRAND_TOTAL_TITLE = "총계"

MONTHLY_FIELDS = [
    "month",
    "hsk",
    "name",
    "export_kusd",
    "export_tons",
    "import_kusd",
    "import_tons",
    "balance_kusd",
]

BY_COUNTRY_FIELDS = MONTHLY_FIELDS[:3] + ["country"] + MONTHLY_FIELDS[3:]

# Position in this list is the itemUsdAmtNN suffix in the response.
TENTATIVE_CATEGORIES = [
    "total",
    "semiconductors",
    "steel",
    "passenger_cars",
    "petroleum",
    "wireless_comm",
    "ships",
    "auto_parts",
    "computer_peripherals",
    "precision_instr",
    "home_appliances",
]

TENTATIVE_FIELDS = ["month", "period"] + TENTATIVE_CATEGORIES


class FetchError(Exception):
    pass


def clean_number(value, field, where):
    text = str(value if value is not None else "").replace(",", "").strip()
    try:
        float(text)
    except ValueError:
        raise ValueError(f"{where}: {field} is not numeric: {value!r}") from None
    return text


def hs_filter(hs, allow_list):
    """(hsSgn, hsSgnWhrCol). HS8 is not a populated filter column, so only 6 or 10 digits."""
    codes = [code.strip() for code in str(hs).split(",") if code.strip()]
    if not codes or not all(code.isdigit() for code in codes):
        raise ValueError(f"--hs must be digits, got {hs!r}")
    if len(codes) > 1:
        if not allow_list:
            raise ValueError(f"--hs takes one code for this dataset, got {hs!r}")
        if not all(len(code) == 10 for code in codes):
            raise ValueError(f"a comma-separated --hs must be all 10-digit, got {hs!r}")
        return ",".join(codes), "HS10_SGN"
    if len(codes[0]) == 6:
        return codes[0], "HS6_SGN"
    if len(codes[0]) == 10:
        return codes[0], "HS10_SGN"
    raise ValueError(f"--hs must be a 6- or 10-digit HS code, got {hs!r}")


def build_monthly_params(args):
    hs, where_column = hs_filter(args.hs, allow_list=False)
    return {
        "tradeKind": "ETS_MNK_1020000A",
        "priodKind": "MON",
        "priodFr": args.date_from,
        "priodTo": args.date_to,
        "statsBase": "acptDd",
        "ttwgTpcd": "1000",
        "showPagingLine": "5000",
        "sortColumn": "",
        "sortOrder": "",
        "hsSgnGrpCol": "HS10_SGN",
        "hsSgnWhrCol": where_column,
        "hsSgn": hs,
    }


def build_by_country_params(args):
    hs, where_column = hs_filter(args.hs, allow_list=True)
    return {
        "tradeKind": "ETS_MNK_1020000E",
        "priodKind": "MON",
        "priodFr": args.date_from,
        "priodTo": args.date_to,
        "statsBase": "acptDd",
        "ttwgTpcd": "1000",
        "showPagingLine": "5000",
        "selectPaging": "1",
        "sortColumn": "",
        "sortOrder": "",
        "hsSgnGrpCol": "HS10_SGN",
        "hsSgnWhrCol": where_column,
        "hsSgn": hs,
        "subHsSgn": "",
        "cntyNm": args.countries,
    }


def build_tentative_params(args):
    return {
        "statsKind": "ETS_MNK_1050000A",
        "imexTpcd": "E",
        "priodKind": "MON",
        "priodFr": args.date_from,
        "priodTo": args.date_to,
        "priodDate": "",
        "showPagingLine": "100",
        "sortColumn": "",
        "sortOrder": "",
    }


def trade_row(item):
    """A retrieveTrade.do row's shared fields, or None for a stub/total row."""
    month = str(item.get("priodTitle") or "").strip()
    hsk = str(item.get("hsSgn") or "").strip()
    if not month or not hsk or month == GRAND_TOTAL_TITLE:
        return None
    where = f"{month} {hsk}"
    return {
        "month": month,
        "hsk": hsk,
        "name": HS10_NAMES.get(hsk) or str(item.get("korePrlstNm") or "").strip() or hsk,
        "export_kusd": clean_number(item.get("expUsdAmt"), "expUsdAmt", where),
        "export_tons": clean_number(item.get("expTtwg"), "expTtwg", where),
        "import_kusd": clean_number(item.get("impUsdAmt"), "impUsdAmt", where),
        "import_tons": clean_number(item.get("impTtwg"), "impTtwg", where),
        "balance_kusd": clean_number(item.get("cmtrBlncAmt"), "cmtrBlncAmt", where),
    }


def parse_monthly(raw):
    rows = []
    for item in raw.get("items") or []:
        row = trade_row(item)
        if row is not None:
            rows.append(row)
    return rows


def parse_monthly_by_country(raw):
    rows = []
    for item in raw.get("items") or []:
        row = trade_row(item)
        if row is None:
            continue
        country = str(item.get("cntyNm") or "").strip()
        # An empty cntyNm marks an aggregate, same as the empty hsSgn on the 총계 row.
        if not country:
            continue
        row["country"] = country
        rows.append(row)
    return rows


def parse_tentative(raw):
    rows = []
    for item in raw.get("items") or []:
        month = str(item.get("priodMon") or "").strip()
        if not month:
            continue
        period = str(item.get("priodDt") or "").strip()
        row = {"month": month, "period": period}
        where = f"tentative {month} {period}"
        for index, category in enumerate(TENTATIVE_CATEGORIES):
            key = f"itemUsdAmt{index:02d}"
            row[category] = clean_number(item.get(key), key, where)
        rows.append(row)
    return rows


def newest_month(rows):
    return max(row["month"] for row in rows)


def newest_tentative_period(rows):
    """Month and 10-day period together, since one month holds three cumulative periods."""
    return max(f"{row['month']} {row['period']}" for row in rows)


DATASETS = {
    "monthly": {
        "url": BASE_URL + "retrieveTrade.do",
        "build_params": build_monthly_params,
        "parse": parse_monthly,
        "fieldnames": MONTHLY_FIELDS,
        "newest": newest_month,
    },
    "monthly_by_country": {
        "url": BASE_URL + "retrieveTrade.do",
        "build_params": build_by_country_params,
        "parse": parse_monthly_by_country,
        "fieldnames": BY_COUNTRY_FIELDS,
        "newest": newest_month,
    },
    "tentative": {
        "url": BASE_URL + "retrieveTentativeValues.do",
        "build_params": build_tentative_params,
        "parse": parse_tentative,
        "fieldnames": TENTATIVE_FIELDS,
        "newest": newest_tentative_period,
    },
}

_opener = None


def session_opener():
    """tradeKind E answers a cookie-less POST with an EUC-KR block page, so prime a session."""
    global _opener
    if _opener is None:
        opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar())
        )
        opener.addheaders = [("User-Agent", USER_AGENT)]
        opener.open(SESSION_URL, timeout=TIMEOUT_SECONDS).read()
        _opener = opener
    return _opener


def post(url, params):
    request = urllib.request.Request(
        url,
        data=urllib.parse.urlencode(params).encode("utf-8"),
        headers={
            "Content-Type": "application/x-www-form-urlencoded; charset=UTF-8",
            "Accept": "application/json, text/javascript, */*; q=0.01",
            "User-Agent": USER_AGENT,
            "X-Requested-With": "XMLHttpRequest",
            "Referer": SESSION_URL,
        },
    )
    with session_opener().open(request, timeout=TIMEOUT_SECONDS) as response:
        return response.read()


def write_csv(path, fieldnames, rows):
    with path.open("w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(handle, fieldnames=fieldnames)
        writer.writeheader()
        writer.writerows(rows)


def append_log(path, stamp, dataset, params, row_count, raw_path):
    line = "\t".join(
        [
            stamp,
            dataset,
            urllib.parse.urlencode(params),
            f"rows={row_count}",
            str(raw_path),
        ]
    )
    with path.open("a", encoding="utf-8") as handle:
        handle.write(line + "\n")


def check_not_truncated(name, raw, raw_path):
    """count is the unpaged total; showPagingLine cuts items server-side without any error."""
    try:
        count = int(raw.get("count"))
    except (TypeError, ValueError):
        return
    items = len(raw.get("items") or [])
    if count > items:
        raise FetchError(
            f"{name}: response truncated to {items} of {count} rows by showPagingLine, "
            f"CSV left untouched; see {raw_path}"
        )


def artifact_path(path):
    """Envelope paths are relative to the automation dir when --out-dir stays inside it."""
    try:
        return str(path.relative_to(AUTOMATION_DIR))
    except ValueError:
        return str(path)


def failure_sentence(name, error):
    text = str(error)
    return text if text.startswith(f"{name}:") else f"{name}: {text}"


def build_envelope(artifacts, failures):
    if failures:
        status = "degraded" if artifacts else "failed"
        escalation_reason = "; ".join(failures)
    else:
        status = "ok"
        escalation_reason = None
    return {
        "automation": AUTOMATION_NAME,
        "status": status,
        "form_used": FORM,
        "artifacts": artifacts,
        "escalation_reason": escalation_reason,
    }


def run_dataset(name, args):
    dataset = DATASETS[name]
    params = dataset["build_params"](args)
    out_dir = Path(args.out_dir)
    raw_dir = out_dir / "raw"
    raw_dir.mkdir(parents=True, exist_ok=True)

    now = datetime.now()
    body = post(dataset["url"], params)
    raw_path = raw_dir / f"{name}_{now.strftime('%Y%m%d_%H%M%S')}.json"
    raw_path.write_bytes(body)

    try:
        payload = json.loads(body)
        check_not_truncated(name, payload, raw_path)
        rows = dataset["parse"](payload)
    except (ValueError, AttributeError) as error:
        raise FetchError(f"{name}: could not parse {raw_path}: {error}") from None
    if not rows:
        raise FetchError(f"{name}: parsed 0 rows, CSV left untouched; see {raw_path}")

    csv_path = out_dir / f"{name}.csv"
    write_csv(csv_path, dataset["fieldnames"], rows)
    append_log(
        out_dir / "fetch_log.txt",
        now.isoformat(timespec="seconds"),
        name,
        params,
        len(rows),
        raw_path,
    )
    print(f"{name}: {len(rows)} rows -> {csv_path} (raw {raw_path})")
    return {
        "path": artifact_path(csv_path),
        "rows": len(rows),
        "newest": dataset["newest"](rows),
    }


def yyyymm(text):
    if not (len(text) == 6 and text.isdigit() and 1 <= int(text[4:]) <= 12):
        raise argparse.ArgumentTypeError(f"expected YYYYMM, got {text!r}")
    return text


def parse_args(argv):
    today = datetime.now()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("dataset", choices=sorted(DATASETS) + ["all"])
    parser.add_argument(
        "--hs",
        default="854232",
        help="6- or 10-digit HS code; monthly_by_country also takes a comma list of 10-digit codes",
    )
    parser.add_argument(
        "--countries",
        default="",
        help="comma-separated Korean country names for monthly_by_country; empty means all",
    )
    parser.add_argument(
        "--from",
        dest="date_from",
        type=yyyymm,
        default=f"{today.year}01",
    )
    parser.add_argument(
        "--to",
        dest="date_to",
        type=yyyymm,
        default=today.strftime("%Y%m"),
    )
    parser.add_argument(
        "--out-dir",
        default=str(Path(__file__).resolve().parent.parent / "data"),
    )
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    names = sorted(DATASETS) if args.dataset == "all" else [args.dataset]
    artifacts = []
    failures = []
    for name in names:
        # One dataset's bad --hs or a site hiccup must not block the unrelated ones.
        try:
            artifacts.append(run_dataset(name, args))
        except (FetchError, ValueError, OSError) as error:
            sentence = failure_sentence(name, error)
            print(f"error: {sentence}", file=sys.stderr)
            failures.append(sentence)

    envelope = build_envelope(artifacts, failures)
    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    (out_dir / "last_result.json").write_text(
        json.dumps(envelope, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    print(json.dumps(envelope, ensure_ascii=False))
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
