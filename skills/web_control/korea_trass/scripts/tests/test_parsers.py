#!/usr/bin/env python3
"""Offline parser tests against committed snapshots of real responses. No network."""

import argparse
import json
import sys
import unittest
from pathlib import Path

SCRIPTS_DIR = Path(__file__).resolve().parents[1]
SNAPSHOTS_DIR = Path(__file__).resolve().parent / "snapshots"
sys.path.insert(0, str(SCRIPTS_DIR))

from fetch_kcs import (  # noqa: E402
    BY_COUNTRY_FIELDS,
    HS10_NAMES,
    MONTHLY_FIELDS,
    TENTATIVE_FIELDS,
    build_by_country_params,
    build_monthly_params,
    parse_monthly,
    parse_monthly_by_country,
    parse_tentative,
)

EMPTY_RESPONSE = {"count": 0, "items": []}


def load_snapshot(name):
    return json.loads((SNAPSHOTS_DIR / name).read_text(encoding="utf-8"))


def find(rows, **match):
    hits = [row for row in rows if all(row[k] == v for k, v in match.items())]
    if len(hits) != 1:
        raise AssertionError(f"expected exactly 1 row for {match}, got {len(hits)}")
    return hits[0]


class MonthlyParserTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.rows = parse_monthly(load_snapshot("monthly_202601_202607.json"))

    def test_row_count(self):
        self.assertEqual(len(self.rows), 49)

    def test_known_values(self):
        january_dram = find(self.rows, month="2026.01", hsk="8542321010")
        self.assertEqual(january_dram["export_kusd"], "5059379")

        july_dram = find(self.rows, month="2026.07", hsk="8542321010")
        self.assertEqual(july_dram["export_kusd"], "13551552")
        self.assertEqual(float(july_dram["export_tons"]), 155.8)

        june_mcp = find(self.rows, month="2026.06", hsk="8542323000")
        self.assertEqual(june_mcp["export_kusd"], "12682181")

    def test_every_field_present_and_numeric(self):
        for row in self.rows:
            self.assertEqual(sorted(row), sorted(MONTHLY_FIELDS))
            for field in MONTHLY_FIELDS:
                self.assertTrue(row[field], f"{row['month']} {row['hsk']}: {field}")
            for field in MONTHLY_FIELDS[3:]:
                float(row[field])

    def test_hsk_universe(self):
        self.assertEqual(set(row["hsk"] for row in self.rows), set(HS10_NAMES))

    def test_names_resolved(self):
        self.assertEqual(
            find(self.rows, month="2026.01", hsk="8542321010")["name"],
            HS10_NAMES["8542321010"],
        )

    def test_totals_and_stub_skipped(self):
        self.assertNotIn("총계", [row["month"] for row in self.rows])

    def test_empty_response(self):
        self.assertEqual(parse_monthly(EMPTY_RESPONSE), [])


class TentativeParserTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.rows = parse_tentative(load_snapshot("tentative_202606_202608.json"))

    def test_row_shape(self):
        self.assertEqual(len(self.rows), 8)
        self.assertEqual(
            [(row["month"], row["period"]) for row in self.rows],
            [
                ("202606", "01~10"),
                ("202606", "01~20"),
                ("202606", "01~30"),
                ("202607", "01~10"),
                ("202607", "01~20"),
                ("202607", "01~31"),
                ("202608", "01~10"),
                ("202608", "01~20"),
            ],
        )

    def test_known_values(self):
        june = find(self.rows, month="202606", period="01~20")
        self.assertEqual(june["total"], "61749369")
        self.assertEqual(june["semiconductors"], "25503660")

        august = find(self.rows, month="202608", period="01~20")
        self.assertEqual(august["semiconductors"], "26031978")

    def test_every_field_present_and_numeric(self):
        for row in self.rows:
            self.assertEqual(sorted(row), sorted(TENTATIVE_FIELDS))
            self.assertEqual(len(row), 13)
            for field in TENTATIVE_FIELDS[2:]:
                float(row[field])

    def test_empty_response(self):
        self.assertEqual(parse_tentative(EMPTY_RESPONSE), [])


class ByCountryParserTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.rows = parse_monthly_by_country(
            load_snapshot("by_country_202601_202607.json")
        )

    def test_row_count(self):
        self.assertEqual(len(self.rows), 105)

    def test_known_values(self):
        china = find(self.rows, month="2026.07", hsk="8542321010", country="중국")
        self.assertEqual(china["export_kusd"], "9979070")

        taiwan_july = find(self.rows, month="2026.07", hsk="8542321010", country="대만")
        self.assertEqual(taiwan_july["export_kusd"], "180701")
        float(taiwan_july["export_tons"])

        taiwan_january = find(
            self.rows, month="2026.01", hsk="8542321010", country="대만"
        )
        self.assertEqual(taiwan_january["export_kusd"], "89633")

    def test_every_field_present_and_numeric(self):
        for row in self.rows:
            self.assertEqual(sorted(row), sorted(BY_COUNTRY_FIELDS))
            for field in BY_COUNTRY_FIELDS:
                self.assertTrue(row[field], f"{row['month']} {row['hsk']}: {field}")
            for field in ("export_kusd", "export_tons", "import_kusd", "import_tons"):
                float(row[field])

    def test_country_stripped(self):
        for row in self.rows:
            self.assertEqual(row["country"], row["country"].strip())
        self.assertEqual(
            set(row["country"] for row in self.rows),
            {"미국", "중국", "대만", "홍콩", "베트남"},
        )

    def test_totals_skipped(self):
        self.assertNotIn("총계", [row["month"] for row in self.rows])

    def test_empty_response(self):
        self.assertEqual(parse_monthly_by_country(EMPTY_RESPONSE), [])


class BuildParamsTest(unittest.TestCase):
    def args(self, hs, countries=""):
        return argparse.Namespace(
            hs=hs, countries=countries, date_from="202601", date_to="202607"
        )

    def params(self, hs):
        return build_monthly_params(self.args(hs))

    def test_hs6_filters_at_hs6(self):
        self.assertEqual(self.params("854232")["hsSgnWhrCol"], "HS6_SGN")

    def test_hs10_filters_at_hs10(self):
        params = self.params("8542321010")
        self.assertEqual(params["hsSgnWhrCol"], "HS10_SGN")
        self.assertEqual(params["hsSgnGrpCol"], "HS10_SGN")
        self.assertEqual(params["hsSgn"], "8542321010")

    def test_bad_length_rejected(self):
        with self.assertRaises(ValueError):
            self.params("85423210")

    def test_monthly_rejects_code_list(self):
        with self.assertRaises(ValueError):
            self.params("8542321010,8542323000")

    def test_by_country_accepts_code_list(self):
        params = build_by_country_params(
            self.args("8542321010,8542323000,8542321030", "미국,중국,대만")
        )
        self.assertEqual(params["tradeKind"], "ETS_MNK_1020000E")
        self.assertEqual(params["hsSgn"], "8542321010,8542323000,8542321030")
        self.assertEqual(params["hsSgnWhrCol"], "HS10_SGN")
        self.assertEqual(params["cntyNm"], "미국,중국,대만")
        self.assertEqual(params["selectPaging"], "1")
        self.assertEqual(params["subHsSgn"], "")

    def test_by_country_defaults_to_all_countries(self):
        self.assertEqual(build_by_country_params(self.args("854232"))["cntyNm"], "")

    def test_by_country_rejects_mixed_length_list(self):
        with self.assertRaises(ValueError):
            build_by_country_params(self.args("854232,8542321010"))


if __name__ == "__main__":
    unittest.main(verbosity=2)
