"""Storage workflow regression tests. Run on CI, not on the Mac."""

from pathlib import Path
import tempfile
import unittest

from workflow_helpers import (
    base_capacity_record,
    collect_linkdelta_tiers,
    require_idle_targets,
)


class TierCollectionTests(unittest.TestCase):
    def test_all_tier_results_and_fixed_order(self):
        for codes in ((0, 0), (1, 0), (0, 2), (1, 2)):
            with self.subTest(codes=codes):
                invoked = []
                recorded = []
                commands = [("tier1", ["first"]), ("tier10", ["second"])]
                before = [(tier, list(command)) for tier, command in commands]

                def run(command):
                    invoked.append(command[0])
                    return codes[len(invoked) - 1]

                def idle():
                    return None

                if any(codes):
                    with self.assertRaises(RuntimeError):
                        collect_linkdelta_tiers(commands, run, idle,
                                                recorded.append)
                else:
                    collect_linkdelta_tiers(commands, run, idle,
                                            recorded.append)
                self.assertEqual(invoked, ["first", "second"])
                self.assertEqual(recorded, [
                    {"tier": "tier1", "exit_code": codes[0]},
                    {"tier": "tier10", "exit_code": codes[1]},
                ])
                self.assertEqual(commands, before)

    def test_launch_failure_stops_without_hiding_it(self):
        recorded = []
        invoked = []

        def run(command):
            invoked.append(command[0])
            raise FileNotFoundError(2, "missing test executable")

        def idle():
            self.fail("launch failure must not reach the idle check")

        with self.assertRaises(FileNotFoundError):
            collect_linkdelta_tiers(
                [("tier1", ["first"]), ("tier10", ["second"])],
                run, idle, recorded.append)
        self.assertEqual(invoked, ["first"])
        self.assertEqual(len(recorded), 1)
        self.assertEqual(recorded[0]["launch_errno"], 2)

    def test_signal_stops_without_starting_another_tier(self):
        invoked = []
        recorded = []

        def run(command):
            invoked.append(command[0])
            return -15

        def idle():
            self.fail("signal termination must stop before continuation")

        with self.assertRaises(RuntimeError):
            collect_linkdelta_tiers(
                [("tier1", ["first"]), ("tier10", ["second"])],
                run, idle, recorded.append)
        self.assertEqual(invoked, ["first"])
        self.assertEqual(recorded, [{"tier": "tier1", "exit_code": -15}])

    def test_unsafe_cleanup_stops_another_tier(self):
        invoked = []
        recorded = []

        def run(command):
            invoked.append(command[0])
            return 1

        def idle():
            raise RuntimeError("busy test target")

        with self.assertRaises(RuntimeError):
            collect_linkdelta_tiers(
                [("tier1", ["first"]), ("tier10", ["second"])],
                run, idle, recorded.append)
        self.assertEqual(invoked, ["first"])
        self.assertEqual(recorded[0]["continuation_error"], "busy test target")

    def test_real_idle_reader_and_identity_refusal(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "driver").mkdir()
            target = root / "target"
            target.mkdir()
            events = target / "cgroup.events"
            events.write_text("populated 0\nfrozen 0\n")
            stat = root.stat()
            identity = (stat.st_dev, stat.st_ino)
            require_idle_targets(root, identity)
            with self.assertRaises(RuntimeError):
                require_idle_targets(root, (stat.st_dev, stat.st_ino + 1))
            events.write_text("populated 1\nfrozen 0\n")
            with self.assertRaises(RuntimeError):
                require_idle_targets(root, identity)
            self.assertTrue(target.is_dir())


class BaseCapacityTests(unittest.TestCase):
    def tiers(self):
        return [{"bytes": 10, "nodes": 2, "edges": 9},
                {"bytes": 20, "nodes": 8, "edges": 1},
                {"bytes": 15, "nodes": 4, "edges": 5}]

    def formula(self):
        return {"serving_sets": 2, "input_multiplier": 8, "edge_bytes": 300,
                "node_bytes": 256, "reserve_bytes": 1073741824}

    def test_independent_maxima_and_exact_boundary(self):
        # Inputs already occupy disk; do not add their bytes again.
        required = 1073751640
        result = base_capacity_record(self.tiers(), self.formula(), required)
        self.assertEqual((result["S"], result["N"], result["E"]), (20, 8, 9))
        self.assertEqual(result["required_free_bytes"], required)
        self.assertEqual(result["status"], "admitted")
        blocked = base_capacity_record(
            self.tiers(), self.formula(), required - 1)
        self.assertEqual(blocked["status"], "blocked_capacity")

    def test_invalid_capacity_metadata_refuses(self):
        for value in (-1, True):
            tiers = self.tiers()
            tiers[0]["edges"] = value
            with self.assertRaises(RuntimeError):
                base_capacity_record(tiers, self.formula(), 0)
        formula = self.formula()
        formula["serving_sets"] = 0
        with self.assertRaises(RuntimeError):
            base_capacity_record(self.tiers(), formula, 0)


if __name__ == "__main__":
    unittest.main()
