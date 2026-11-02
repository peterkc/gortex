"""CI-only fixture tests; no measurements or local Go commands."""

import copy
import subprocess
from pathlib import Path
import unittest

from measure import RunError, choose_base, latency_groups, percentile


class SelectionTests(unittest.TestCase):
    def candidate(self, passes=True, cpu=10, total=20, read=5):
        return {"passes": passes, "construction_cpu_ns": cpu,
                "total_bytes": total, "read_p95_ns": {"one": read}}

    def test_all_five_rows(self):
        cases = [
            (self.candidate(False), self.candidate(),
             "only_c_passes", "recommend_c"),
            (self.candidate(), self.candidate(False),
             "only_b_passes", "recommend_b"),
            (self.candidate(cpu=20), self.candidate(),
             "both_pass", "recommend_c"),
            (self.candidate(False), self.candidate(False),
             "neither_passes", "stop_and_amend"),
            (None, self.candidate(),
             "comparison_unavailable", "stop_no_recommendation"),
        ]
        for b, c, row, branch in cases:
            with self.subTest(row=row):
                self.assertEqual(
                    choose_base(b, c, cpu_selection_approved=True),
                    (row, branch))

    def test_cpu_then_bytes_and_no_unauthorized_fallback(self):
        b = self.candidate(cpu=1, total=100)
        c = self.candidate(cpu=2, total=1)
        self.assertEqual(choose_base(b, c, cpu_selection_approved=True)[1],
                         "recommend_b")
        b = self.candidate(total=30)
        c = self.candidate(total=20)
        self.assertEqual(choose_base(b, c, cpu_selection_approved=True)[1],
                         "recommend_c")
        self.assertEqual(choose_base(b, c, cpu_selection_approved=False)[0],
                         "comparison_unavailable")
        self.assertNotEqual(
            choose_base(self.candidate(False), self.candidate(False),
                        cpu_selection_approved=True)[1], "recommend_b")

    def test_read_conflict_and_unresolved_tie_return_to_user(self):
        b = self.candidate()
        c = self.candidate()
        self.assertEqual(choose_base(b, c, cpu_selection_approved=True)[1],
                         "ask_user_unresolved_tie")
        c = {**c, "read_p95_ns": {"one": 4, "two": 7}}
        b = {**b, "read_p95_ns": {"one": 6, "two": 5}}
        original = copy.deepcopy((b, c))
        self.assertEqual(choose_base(b, c, cpu_selection_approved=True)[1],
                         "ask_user_read_tradeoff")
        self.assertEqual((b, c), original)


class TimingEvidenceTests(unittest.TestCase):
    def test_lookup_strata_are_separate_and_warmup_is_not_measured(self):
        rows = [
            {"status": "passed", "elapsed_ns": 10, "warmup": False,
             "operation": "id", "missing": False},
            {"status": "passed", "elapsed_ns": 99, "warmup": False,
             "operation": "id", "missing": True},
            {"status": "passed", "elapsed_ns": 500, "warmup": True,
             "operation": "id", "missing": True},
        ]
        groups = latency_groups(rows)
        self.assertEqual(groups["id_hit"], [10])
        self.assertEqual(groups["id_miss"], [99])
        self.assertEqual(groups["id"], [10, 99])
        self.assertEqual(percentile([1, 2, 3, 4]), 4)

    def test_missing_zero_or_failed_evidence_is_rejected(self):
        for fields in ({}, {"elapsed_ns": 0}, {"elapsed_ns": -1},
                       {"elapsed_ns": 4, "status": "unsupported"}):
            row = {"status": "passed", "warmup": False,
                   "operation": "name", "missing": False, **fields}
            with self.subTest(fields=fields), self.assertRaises(RunError):
                latency_groups([row])

    def test_entrypoint_has_no_path_or_limit_defaults(self):
        script = Path(__file__).with_name("run.sh")
        result = subprocess.run([str(script)], capture_output=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn(b"--config", result.stderr)


if __name__ == "__main__":
    unittest.main()
