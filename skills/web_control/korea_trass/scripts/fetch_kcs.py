#!/usr/bin/env python3
"""Pull Korean customs (tradedata.go.kr) trade stats into CSVs. Stdlib only, no auth."""

import argparse
import csv
import json
import sys
import urllib.parse
import urllib.request
from datetime import datetime
from pathlib import Path

BASE_URL = "https://tradedata.go.kr/cts/hmpg/"
USER_AGENT = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) fetch_kcs.py"
TIMEOUT_SECONDS = 60

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


def build_monthly_params(args):
    hs = str(args.hs).strip()
    if not hs.isdigit() or len(hs) not in (6, 10):
        raise ValueError(f"--hs must be a 6- or 10-digit HS code, got {args.hs!r}")
    return {
        "tradeKind": "ETS_MNK_1020000A",
        "priodKind": "MON",
        "priodFr": args.date_from,
        "priodTo": args.date_to,
        "statsBase": "acptDd",
        "ttwgTpcd": "1000",
        "showPagingLine": "500",
        "sortColumn": "",
        "sortOrder": "",
        "hsSgnGrpCol": "HS10_SGN",
        "hsSgnWhrCol": "HS6_SGN" if len(hs) == 6 else "HS10_SGN",
        "hsSgn": hs,
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


def parse_monthly(raw):
    rows = []
    for item in raw.get("items") or []:
        month = str(item.get("priodTitle") or "").strip()
        hsk = str(item.get("hsSgn") or "").strip()
        if not month or not hsk or month == GRAND_TOTAL_TITLE:
            continue
        where = f"monthly {month} {hsk}"
        rows.append(
            {
                "month": month,
                "hsk": hsk,
                "name": HS10_NAMES.get(hsk, hsk),
                "export_kusd": clean_number(item.get("expUsdAmt"), "expUsdAmt", where),
                "export_tons": clean_number(item.get("expTtwg"), "expTtwg", where),
                "import_kusd": clean_number(item.get("impUsdAmt"), "impUsdAmt", where),
                "import_tons": clean_number(item.get("impTtwg"), "impTtwg", where),
                "balance_kusd": clean_number(
                    item.get("cmtrBlncAmt"), "cmtrBlncAmt", where
                ),
            }
        )
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


DATASETS = {
    "monthly": {
        "url": BASE_URL + "retrieveTrade.do",
        "build_params": build_monthly_params,
        "parse": parse_monthly,
        "fieldnames": MONTHLY_FIELDS,
    },
    "tentative": {
        "url": BASE_URL + "retrieveTentativeValues.do",
        "build_params": build_tentative_params,
        "parse": parse_tentative,
        "fieldnames": TENTATIVE_FIELDS,
    },
}


def post(url, params):
    request = urllib.request.Request(
        url,
        data=urllib.parse.urlencode(params).encode("utf-8"),
        headers={
            "Content-Type": "application/x-www-form-urlencoded; charset=UTF-8",
            "Accept": "application/json, text/javascript, */*; q=0.01",
            "User-Agent": USER_AGENT,
            "X-Requested-With": "XMLHttpRequest",
        },
    )
    with urllib.request.urlopen(request, timeout=TIMEOUT_SECONDS) as response:
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
        rows = dataset["parse"](json.loads(body))
    except (ValueError, AttributeError) as error:
        raise FetchError(f"{name}: could not parse {raw_path}: {error}") from None
    if not rows:
        raise FetchError(f"{name}: parsed 0 rows, CSV left untouched; see {raw_path}")

    write_csv(out_dir / f"{name}.csv", dataset["fieldnames"], rows)
    append_log(
        out_dir / "fetch_log.txt",
        now.isoformat(timespec="seconds"),
        name,
        params,
        len(rows),
        raw_path,
    )
    print(f"{name}: {len(rows)} rows -> {out_dir / (name + '.csv')} (raw {raw_path})")


def yyyymm(text):
    if not (len(text) == 6 and text.isdigit() and 1 <= int(text[4:]) <= 12):
        raise argparse.ArgumentTypeError(f"expected YYYYMM, got {text!r}")
    return text


def parse_args(argv):
    today = datetime.now()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("dataset", choices=sorted(DATASETS) + ["all"])
    parser.add_argument("--hs", default="854232", help="6- or 10-digit HS code")
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
    for name in names:
        try:
            run_dataset(name, args)
        except (FetchError, ValueError) as error:
            print(f"error: {error}", file=sys.stderr)
            return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
