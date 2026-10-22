import copy
import json
import pathlib
import tempfile
import unittest

import snapshot


def fixture():
    return {"schema_version": 1, "product_code": "rkrs_color_leads", "account_id": 42,
            "source": {"platform": "ssd", "revision": "fixture-v1", "boundary": "fixture-B",
                       "exported_at": "2026-10-22T00:00:00Z"},
            "settings": {"main_settings": [{"id": 1, "name": "Первое"}, {"id": 2}],
                         "unknown_preserved": {"value": None}},
            "colors": [{"id": 100, "amo_account_id": 42, "lead_id": 1, "stage_id": 2,
                        "pipeline_id": 3, "backgroundColor": "#fff", "borderColor": None,
                        "timestamp": "2026-10-20 12:00:00", "created_at": "source metadata"}]}


class SnapshotTests(unittest.TestCase):
    def test_target_limit_and_format_block_without_truncating_archive(self):
        source = fixture()
        result = snapshot.plan(source)
        self.assertFalse(result["target_compatible"])  # legacy shorthand #fff
        self.assertEqual(result["source_rows"], source["colors"])
        source["colors"][0]["backgroundColor"] = "#ffffff"
        self.assertTrue(snapshot.plan(source)["target_compatible"])
        source["colors"] = [dict(source["colors"][0], lead_id=i+1) for i in range(1001)]
        result = snapshot.plan(source)
        self.assertFalse(result["target_compatible"])
        self.assertEqual(len(result["payload"]["colors"]), 1001)

    def test_preserves_rules_unknown_settings_and_source_metadata(self):
        source = fixture()
        out = snapshot.plan(source)
        self.assertEqual(out["payload"]["settings"], source["settings"])
        self.assertEqual(out["source_rows"], source["colors"])
        self.assertFalse(out["activation_allowed"])

    def test_reordering_rows_is_equivalent_but_rule_order_is_not(self):
        a = fixture()
        a["colors"].append(dict(a["colors"][0], lead_id=2))
        b = copy.deepcopy(a)
        b["colors"].reverse()
        b["source"]["platform"] = "core"
        self.assertTrue(snapshot.reconcile(a, b)["matches"])
        b["settings"]["main_settings"].reverse()
        self.assertFalse(snapshot.reconcile(a, b)["settings_match"])

    def test_equal_counts_do_not_hide_missing_or_changed_rows(self):
        a = fixture()
        b = copy.deepcopy(a)
        b["colors"][0]["backgroundColor"] = None
        self.assertEqual(snapshot.reconcile(a, b)["changed_keys"], [(1, 2, 3)])
        b["colors"][0]["lead_id"] = 9
        out = snapshot.reconcile(a, b)
        self.assertEqual(out["missing_keys"], [(1, 2, 3)])
        self.assertEqual(out["extra_keys"], [(9, 2, 3)])

    def test_rejects_duplicates_instead_of_arbitrary_last_write(self):
        a = fixture()
        a["colors"].append(dict(a["colors"][0], backgroundColor="#000"))
        with self.assertRaisesRegex(ValueError, "duplicate composite"):
            snapshot.plan(a)

    def test_rejects_cross_tenant_and_invalid_ids(self):
        for value in (True, 0, "1", -1):
            a = fixture()
            a["colors"][0]["lead_id"] = value
            with self.assertRaises(ValueError):
                snapshot.plan(a)
        a = fixture()
        a["colors"][0]["amo_account_id"] = 43
        with self.assertRaisesRegex(ValueError, "cross-account"):
            snapshot.plan(a)
        b = fixture()
        b["account_id"] = 43
        b["colors"][0]["amo_account_id"] = 43
        with self.assertRaises(ValueError):
            snapshot.reconcile(fixture(), b)

    def test_rejects_unresolved_defaults_and_missing_boundary(self):
        for settings in (None, [], False, "{}"):
            a = fixture()
            a["settings"] = settings
            with self.assertRaises(ValueError):
                snapshot.plan(a)
        a = fixture()
        del a["source"]["boundary"]
        with self.assertRaises(ValueError):
            snapshot.plan(a)

    def test_rollback_reconciles_current_data_not_original_snapshot(self):
        old = fixture()
        current = copy.deepcopy(old)
        current["source"]["platform"] = "core"
        current["settings"]["new_setting"] = True
        current["colors"][0]["backgroundColor"] = "#000"
        self.assertFalse(snapshot.reconcile(current, old)["matches"])
        returned = copy.deepcopy(current)
        returned["source"]["platform"] = "ssd"
        self.assertTrue(snapshot.reconcile(current, returned)["matches"])

    def test_rejects_ambiguous_json(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "input.json"
            for text in ('{"a":1,"a":2}', '{"a":NaN}'):
                path.write_text(text)
                with self.assertRaises(ValueError):
                    snapshot.read(path)


if __name__ == "__main__":
    unittest.main()
