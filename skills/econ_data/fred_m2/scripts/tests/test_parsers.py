#!/usr/bin/env python3
"""Offline tests against committed snapshots of real fredgraph.csv responses. No network."""

import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

SCRIPTS_DIR = Path(__file__).resolve().parents[1]
SNAPSHOTS_DIR = Path(__file__).resolve().parent / "snapshots"
sys.path.insert(0, str(SCRIPTS_DIR))

from fetch_fred import (  # noqa: E402
    FIELDS,
    SERIES,
    FetchError,
    build_envelope,
    check_plausible,
    decode_body,
    describe,
    existing_row_count,
    parse_series,
    series_url,
    write_csv,
)


def snapshot_bytes(name):
    return (SNAPSHOTS_DIR / name).read_bytes()


def snapshot_text(name):
    return decode_body(snapshot_bytes(name))


class M2SLSnapshotTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.rows = parse_series(snapshot_text("m2sl_2024_2026.csv"), "M2SL")

    def test_row_count(self):
        self.assertEqual(len(self.rows), 31)

    def test_known_values(self):
        self.assertEqual(self.rows[0], {"date": "2024-01-01", "value": "20835.5"})
        self.assertEqual(self.rows[-1], {"date": "2026-07-01", "value": "23218.0"})

    def test_monthly_observations_are_first_of_month(self):
        for row in self.rows:
            self.assertTrue(row["date"].endswith("-01"), row["date"])

    def test_fields_and_order(self):
        for row in self.rows:
            self.assertEqual(sorted(row), sorted(FIELDS))
            float(row["value"])
        self.assertEqual(
            [row["date"] for row in self.rows],
            sorted(row["date"] for row in self.rows),
        )

    def test_plausible(self):
        check_plausible("m2sl", self.rows)


class WM2NSSnapshotTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.rows = parse_series(snapshot_text("wm2ns_2026.csv"), "WM2NS")

    def test_row_count(self):
        self.assertEqual(len(self.rows), 31)

    def test_known_values(self):
        self.assertEqual(self.rows[0], {"date": "2026-01-05", "value": "22605.9"})
        self.assertEqual(self.rows[-1], {"date": "2026-08-03", "value": "23207.7"})

    def test_observations_are_seven_days_apart(self):
        from datetime import date

        stamps = [date.fromisoformat(row["date"]) for row in self.rows]
        gaps = {(later - earlier).days for earlier, later in zip(stamps, stamps[1:])}
        self.assertEqual(gaps, {7})

    def test_plausible(self):
        check_plausible("wm2ns", self.rows)


class M2RealSnapshotTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.rows = parse_series(snapshot_text("m2real_2024_2026.csv"), "M2REAL")

    def test_row_count_is_m2sl_minus_the_gap(self):
        self.assertEqual(len(self.rows), 30)

    def test_known_values(self):
        self.assertEqual(self.rows[0], {"date": "2024-01-01", "value": "6727.7"})
        self.assertEqual(self.rows[-1], {"date": "2026-07-01", "value": "6976.3"})

    def test_shutdown_gap_dropped(self):
        # 2025-10-01 comes back as "2025-10-01," — no October 2025 CPI, so no real M2.
        self.assertIn("2025-10-01,\n", snapshot_text("m2real_2024_2026.csv"))
        self.assertNotIn("2025-10-01", [row["date"] for row in self.rows])
        self.assertEqual(
            [row["date"] for row in self.rows[20:23]],
            ["2025-09-01", "2025-11-01", "2025-12-01"],
        )

    def test_plausible(self):
        check_plausible("m2real", self.rows)

    def test_m2sl_floor_would_reject_m2real(self):
        with self.assertRaises(FetchError):
            check_plausible("m2sl", self.rows)


class NonCsvBodyTest(unittest.TestCase):
    def test_unknown_series_html_rejected(self):
        # Genuine 404 page for id=NOTASERIES, truncated to its first 2048 bytes.
        with self.assertRaises(FetchError) as caught:
            decode_body(snapshot_bytes("unknown_series_404_head.html"))
        self.assertIn("HTML", str(caught.exception))

    def test_mixed_frequency_zip_rejected(self):
        # Genuine body of id=M2SL,WM2NS: fredgraph zips series of differing frequency.
        with self.assertRaises(FetchError) as caught:
            decode_body(snapshot_bytes("mixed_frequency.zip"))
        self.assertIn("zip", str(caught.exception))


class MalformedCsvTest(unittest.TestCase):
    def test_empty_body(self):
        with self.assertRaises(FetchError):
            parse_series("", "M2SL")

    def test_header_only_yields_no_rows(self):
        self.assertEqual(parse_series("observation_date,M2SL\n", "M2SL"), [])

    def test_legacy_date_header_accepted(self):
        rows = parse_series("DATE,M2SL\n2026-07-01,23218.0\n", "M2SL")
        self.assertEqual(rows, [{"date": "2026-07-01", "value": "23218.0"}])

    def test_wrong_series_in_header(self):
        with self.assertRaises(FetchError) as caught:
            parse_series("observation_date,M1SL\n2026-07-01,18000.0\n", "M2SL")
        self.assertIn("expected 'M2SL'", str(caught.exception))

    def test_extra_column_rejected(self):
        with self.assertRaises(FetchError):
            parse_series("observation_date,M2SL,M2REAL\n2026-07-01,23218.0,6976.3\n", "M2SL")

    def test_non_numeric_value_rejected(self):
        with self.assertRaises(FetchError):
            parse_series("observation_date,M2SL\n2026-07-01,n/a\n", "M2SL")

    def test_bad_date_rejected(self):
        with self.assertRaises(FetchError):
            parse_series("observation_date,M2SL\n2026-13-01,23218.0\n", "M2SL")

    def test_missing_marker_dropped(self):
        rows = parse_series(
            "observation_date,M2SL\n2026-06-01,.\n2026-07-01,23218.0\n", "M2SL"
        )
        self.assertEqual(rows, [{"date": "2026-07-01", "value": "23218.0"}])

    def test_crlf_tolerated(self):
        rows = parse_series("observation_date,M2SL\r\n2026-07-01,23218.0\r\n", "M2SL")
        self.assertEqual(len(rows), 1)

    def test_descending_order_rejected(self):
        rows = parse_series(
            "observation_date,M2SL\n2026-07-01,23218.0\n2026-06-01,23115.2\n", "M2SL"
        )
        with self.assertRaises(FetchError):
            check_plausible("m2sl", rows)

    def test_wrong_scale_rejected(self):
        # M2 quoted in trillions rather than billions would slip through a bare float().
        rows = parse_series("observation_date,M2SL\n2026-07-01,23.218\n", "M2SL")
        with self.assertRaises(FetchError):
            check_plausible("m2sl", rows)


class NoClobberTest(unittest.TestCase):
    """run_series refuses to shrink a CSV; this covers the count it decides on."""

    def test_counts_rows_not_lines(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "m2sl.csv"
            rows = parse_series(snapshot_text("m2sl_2024_2026.csv"), "M2SL")
            write_csv(path, rows)
            self.assertEqual(existing_row_count(path), 31)

    def test_missing_file_counts_zero(self):
        with tempfile.TemporaryDirectory() as directory:
            self.assertEqual(existing_row_count(Path(directory) / "absent.csv"), 0)

    def test_round_trip_preserves_values(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "wm2ns.csv"
            rows = parse_series(snapshot_text("wm2ns_2026.csv"), "WM2NS")
            write_csv(path, rows)
            self.assertEqual(
                path.read_text(encoding="utf-8").splitlines()[:2],
                ["date,value", "2026-01-05,22605.9"],
            )

    def test_written_file_matches_the_data_spec_format(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "m2sl.csv"
            write_csv(path, parse_series(snapshot_text("m2sl_2024_2026.csv"), "M2SL"))
            raw = path.read_bytes()
            self.assertNotIn(b"\r", raw)
            self.assertNotIn(b'"', raw)
            self.assertTrue(raw.endswith(b"\n"))
            lines = raw.decode("utf-8").split("\n")[:-1]
            self.assertEqual(lines[0], "date,value")
            self.assertTrue(all(line.count(",") == 1 for line in lines))


class EnvelopeTest(unittest.TestCase):
    def artifact(self, name, rows, newest):
        return {"path": f"data/{name}.csv", "rows": rows, "newest": newest}

    def test_ok(self):
        envelope = build_envelope([self.artifact("m2sl", 811, "2026-07-01")], [])
        self.assertEqual(
            envelope,
            {
                "automation": "fred-m2",
                "status": "ok",
                "form_used": 3,
                "artifacts": [
                    {"path": "data/m2sl.csv", "rows": 811, "newest": "2026-07-01"}
                ],
                "escalation_reason": None,
            },
        )

    def test_degraded_keeps_the_successes(self):
        envelope = build_envelope(
            [self.artifact("m2sl", 811, "2026-07-01")],
            [("wm2ns", "parsed 0 observations; raw archived at data/raw/x.csv")],
        )
        self.assertEqual(envelope["status"], "degraded")
        self.assertEqual(len(envelope["artifacts"]), 1)
        self.assertIn("wm2ns", envelope["escalation_reason"])
        self.assertIn("raw archived", envelope["escalation_reason"])

    def test_failed_when_nothing_succeeded(self):
        envelope = build_envelope([], [("m2sl", "HTTP 503"), ("wm2ns", "HTTP 503")])
        self.assertEqual(envelope["status"], "failed")
        self.assertEqual(envelope["artifacts"], [])

    def test_serializes_to_one_line(self):
        line = json.dumps(build_envelope([self.artifact("m2sl", 1, "2026-07-01")], []))
        self.assertNotIn("\n", line)
        self.assertEqual(json.loads(line)["form_used"], 3)

    def test_never_emits_needs_human(self):
        # needs_human is a form-2 status; this automation has no human gates.
        cases = [
            ([], []),
            ([self.artifact("m2sl", 1, "2026-07-01")], []),
            ([self.artifact("m2sl", 1, "2026-07-01")], [("wm2ns", "boom")]),
            ([], [("m2sl", "boom")]),
        ]
        for artifacts, failures in cases:
            status = build_envelope(artifacts, failures)["status"]
            self.assertIn(status, {"ok", "degraded", "failed"})

    def test_unexpected_error_type_is_described_not_swallowed(self):
        reason = build_envelope([], [("m2sl", describe(TypeError("bad glue")))])[
            "escalation_reason"
        ]
        self.assertIn("unexpected TypeError", reason)
        self.assertIn("bad glue", reason)

    def test_fetch_error_is_described_verbatim(self):
        self.assertEqual(describe(FetchError("parsed 0 observations")), "parsed 0 observations")


class NoSilentEndingsTest(unittest.TestCase):
    """Every run ends in exactly one envelope on stdout, whatever goes wrong."""

    def run_main(self, argv, patch=None):
        import contextlib
        import io

        import fetch_fred

        saved = dict(fetch_fred.SERIES)
        stdout, stderr = io.StringIO(), io.StringIO()
        try:
            if patch:
                patch(fetch_fred)
            with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                code = fetch_fred.main(argv)
        finally:
            fetch_fred.SERIES.clear()
            fetch_fred.SERIES.update(saved)
        return code, stdout.getvalue(), stderr.getvalue()

    def last_envelope(self, stdout):
        lines = [line for line in stdout.split("\n") if line.strip()]
        self.assertTrue(lines, "no stdout at all — that is a silent ending")
        return json.loads(lines[-1])

    def test_unexpected_exception_still_emits_failed(self):
        def explode(module):
            def boom(*_args, **_kwargs):
                raise TypeError("bad glue")

            module.fetch = boom

        with tempfile.TemporaryDirectory() as directory:
            code, stdout, stderr = self.run_main(["m2sl", "--out-dir", directory], explode)
            envelope = self.last_envelope(stdout)
            self.assertEqual(envelope["status"], "failed")
            self.assertEqual(envelope["artifacts"], [])
            self.assertIn("unexpected TypeError", envelope["escalation_reason"])
            self.assertEqual(code, 1)
            self.assertIn("bad glue", stderr)
            self.assertEqual(
                json.loads((Path(directory) / "last_result.json").read_text()), envelope
            )

    def test_unwritable_out_dir_still_emits_failed(self):
        """The out-dir mkdir itself failing must not skip the envelope."""
        with tempfile.TemporaryDirectory() as directory:
            locked = Path(directory) / "locked"
            locked.mkdir(mode=0o500)
            try:
                if os.access(locked, os.W_OK):
                    self.skipTest("cannot make a directory unwritable as this user")
                code, stdout, _ = self.run_main(["all", "--out-dir", str(locked / "out")])
                envelope = self.last_envelope(stdout)
                self.assertEqual(envelope["status"], "failed")
                self.assertEqual(envelope["artifacts"], [])
                self.assertIn("run:", envelope["escalation_reason"])
                self.assertEqual(code, 1)
            finally:
                locked.chmod(0o700)

    def test_envelope_is_the_only_stdout_line(self):
        def explode(module):
            def boom(*_args, **_kwargs):
                raise FetchError("no network in tests")

            module.fetch = boom

        with tempfile.TemporaryDirectory() as directory:
            _, stdout, _ = self.run_main(["all", "--out-dir", directory], explode)
            self.assertEqual(len([l for l in stdout.split("\n") if l.strip()]), 1)


class UrlTest(unittest.TestCase):
    def test_no_key_and_id_only(self):
        self.assertEqual(
            series_url("M2SL"),
            "https://fred.stlouisfed.org/graph/fredgraph.csv?id=M2SL",
        )

    def test_start_becomes_cosd(self):
        self.assertIn("cosd=2024-01-01", series_url("WM2NS", "2024-01-01"))

    def test_every_series_has_a_distinct_id(self):
        ids = [spec["series_id"] for spec in SERIES.values()]
        self.assertEqual(len(ids), len(set(ids)))
        for name, spec in SERIES.items():
            self.assertEqual(spec["series_id"], name.upper())


if __name__ == "__main__":
    unittest.main(verbosity=2)
