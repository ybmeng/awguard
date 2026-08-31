#!/usr/bin/env python3
"""Pull US M2 money supply series from FRED into CSVs. Stdlib only, no API key."""

import argparse
import csv
import json
import sys
import urllib.error
import urllib.parse
import urllib.request
from datetime import date, datetime
from pathlib import Path

AUTOMATION = "fred-m2"
GRAPH_URL = "https://fred.stlouisfed.org/graph/fredgraph.csv"
USER_AGENT = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) fetch_fred.py"
TIMEOUT_SECONDS = 60

FIELDS = ["date", "value"]

# FRED renamed the first column from DATE to observation_date; both still appear in the wild.
DATE_HEADERS = {"observation_date", "date"}

MISSING_MARKER = "."

# Rough floors in the series' own units ($B, and chained 1982-84 $B for M2REAL). They only
# catch a series swapped for an unrelated one; FRED never revises M2 by anything near this.
SERIES = {
    "m2sl": {
        "series_id": "M2SL",
        "meaning": "M2, monthly average, seasonally adjusted, $ billions",
        "min_latest": 15000.0,
    },
    "wm2ns": {
        "series_id": "WM2NS",
        "meaning": "M2, weekly average ending Monday, not seasonally adjusted, $ billions",
        "min_latest": 15000.0,
    },
    "m2real": {
        "series_id": "M2REAL",
        "meaning": "Real M2 (M2SL deflated by CPIAUCSL), monthly, billions of 1982-84 dollars",
        "min_latest": 4000.0,
    },
}


class FetchError(Exception):
    pass


def series_url(series_id, start=None):
    params = {"id": series_id}
    if start:
        params["cosd"] = start
    return GRAPH_URL + "?" + urllib.parse.urlencode(params)


def fetch(url):
    """(status, body). An error status still yields its body, which is worth archiving."""
    request = urllib.request.Request(
        url,
        headers={"User-Agent": USER_AGENT, "Accept": "text/csv, application/csv, */*"},
    )
    try:
        with urllib.request.urlopen(request, timeout=TIMEOUT_SECONDS) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        with error:
            return error.code, error.read()
    except (urllib.error.URLError, OSError) as error:
        raise FetchError(f"could not reach {url}: {error}") from None


def decode_body(body):
    """A non-CSV body means the request shape was wrong, not that the data is missing."""
    if body[:2] == b"PK":
        raise FetchError(
            "response is a zip, not CSV: fredgraph zips a multi-id request whose series "
            "have different frequencies"
        )
    try:
        text = body.decode("utf-8")
    except UnicodeDecodeError:
        raise FetchError("response is not UTF-8 text") from None
    if text.lstrip()[:1] == "<":
        raise FetchError("response is HTML, not CSV (unknown series id gives a 404 page)")
    return text


def parse_series(text, series_id):
    """fredgraph.csv rows for one series. Missing observations ('.') are dropped."""
    lines = [line for line in text.replace("\r\n", "\n").split("\n") if line.strip()]
    if not lines:
        raise FetchError("empty response body")

    header = [cell.strip() for cell in lines[0].split(",")]
    if len(header) != 2:
        raise FetchError(f"expected a 2-column header, got {lines[0]!r}")
    if header[0].lower() not in DATE_HEADERS:
        raise FetchError(f"unexpected date column {header[0]!r} in header {lines[0]!r}")
    if header[1].upper() != series_id.upper():
        raise FetchError(f"header names series {header[1]!r}, expected {series_id!r}")

    rows = []
    for number, line in enumerate(lines[1:], start=2):
        cells = [cell.strip() for cell in line.split(",")]
        if len(cells) != 2:
            raise FetchError(f"line {number}: expected 2 columns, got {line!r}")
        stamp, value = cells
        try:
            date.fromisoformat(stamp)
        except ValueError:
            raise FetchError(f"line {number}: {stamp!r} is not an ISO date") from None
        if value == MISSING_MARKER or value == "":
            continue
        try:
            float(value)
        except ValueError:
            raise FetchError(f"line {number}: {value!r} is not numeric") from None
        rows.append({"date": stamp, "value": value})
    return rows


def check_plausible(name, rows):
    floor = SERIES[name]["min_latest"]
    latest = float(rows[-1]["value"])
    if latest < floor:
        raise FetchError(
            f"latest value {latest} is below the {floor} floor for {SERIES[name]['series_id']}; "
            "the response may be a different series"
        )
    stamps = [row["date"] for row in rows]
    if stamps != sorted(stamps):
        raise FetchError("observations are not in ascending date order")


def existing_row_count(path):
    if not path.exists():
        return 0
    with path.open(newline="", encoding="utf-8") as handle:
        return sum(1 for _ in csv.DictReader(handle))


def write_csv(path, rows):
    with path.open("w", newline="", encoding="utf-8") as handle:
        # csv defaults to CRLF; the data spec and the upstream body are both LF.
        writer = csv.DictWriter(handle, fieldnames=FIELDS, lineterminator="\n")
        writer.writeheader()
        writer.writerows(rows)


def append_log(path, stamp, name, url, row_count, raw_path):
    line = "\t".join([stamp, name, url, f"rows={row_count}", str(raw_path)])
    with path.open("a", encoding="utf-8") as handle:
        handle.write(line + "\n")


def run_series(name, args, out_dir):
    spec = SERIES[name]
    raw_dir = out_dir / "raw"
    raw_dir.mkdir(parents=True, exist_ok=True)

    now = datetime.now()
    url = series_url(spec["series_id"], args.start)
    status, body = fetch(url)
    raw_path = raw_dir / f"{name}_{now.strftime('%Y%m%d_%H%M%S')}.csv"
    raw_path.write_bytes(body)
    if status != 200:
        raise FetchError(f"HTTP {status} from {url}; body archived at {raw_path}")

    try:
        rows = parse_series(decode_body(body), spec["series_id"])
    except FetchError as error:
        raise FetchError(f"{error}; raw archived at {raw_path}") from None
    if not rows:
        raise FetchError(f"parsed 0 observations; raw archived at {raw_path}")
    check_plausible(name, rows)

    csv_path = out_dir / f"{name}.csv"
    previous = existing_row_count(csv_path)
    if len(rows) < previous and not args.allow_shrink:
        raise FetchError(
            f"{len(rows)} observations is fewer than the {previous} already in {csv_path.name}; "
            f"CSV left untouched, raw archived at {raw_path} (--allow-shrink to override)"
        )

    write_csv(csv_path, rows)
    append_log(
        out_dir / "fetch_log.txt",
        now.isoformat(timespec="seconds"),
        name,
        url,
        len(rows),
        raw_path,
    )
    return {"path": f"data/{csv_path.name}", "rows": len(rows), "newest": rows[-1]["date"]}


def build_envelope(artifacts, failures):
    """Only ok/degraded/failed. needs_human is a form-2 status and this automation has no gates."""
    if failures:
        status = "degraded" if artifacts else "failed"
        reason = "; ".join(f"{name}: {message}" for name, message in failures)
    else:
        status = "ok"
        reason = None
    return {
        "automation": AUTOMATION,
        "status": status,
        "form_used": 3,
        "artifacts": artifacts,
        "escalation_reason": reason,
    }


def describe(error):
    """An unexpected type is still reported as a sentence, not swallowed as a traceback."""
    if isinstance(error, FetchError):
        return str(error)
    return f"unexpected {type(error).__name__}: {error}"


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("dataset", choices=sorted(SERIES) + ["all"])
    parser.add_argument(
        "--out-dir",
        default=str(Path(__file__).resolve().parent.parent / "data"),
    )
    parser.add_argument(
        "--start",
        default=None,
        help="ISO date for cosd; a date past the last observation is ignored by FRED",
    )
    parser.add_argument(
        "--allow-shrink",
        action="store_true",
        help="write a CSV even when the fetch returned fewer rows than it already holds",
    )
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    names = sorted(SERIES) if args.dataset == "all" else [args.dataset]
    out_dir = Path(args.out_dir)

    artifacts = []
    failures = []
    # No silent endings: any exception below still leaves exactly one envelope on stdout.
    try:
        out_dir.mkdir(parents=True, exist_ok=True)
        for name in names:
            # One series failing must not stop the others; that is what degraded reports.
            try:
                artifacts.append(run_series(name, args, out_dir))
            except Exception as error:
                print(f"error: {name}: {error}", file=sys.stderr)
                failures.append((name, describe(error)))
    except Exception as error:
        print(f"error: {error}", file=sys.stderr)
        failures.append(("run", describe(error)))

    envelope = build_envelope(artifacts, failures)
    try:
        (out_dir / "last_result.json").write_text(
            json.dumps(envelope, indent=2) + "\n", encoding="utf-8"
        )
    except OSError as error:
        # The stdout envelope is the contract; the file is a convenience for the next run.
        print(f"warning: could not write last_result.json: {error}", file=sys.stderr)
    print(json.dumps(envelope))
    return 0 if envelope["status"] == "ok" else 1


if __name__ == "__main__":
    sys.exit(main())
