"""Storage workflow regression tests. Run on CI, not on the Mac."""

from dataclasses import replace
from pathlib import Path
import tempfile
import unittest

from workflow_helpers import (
    MeasurementHost,
    admit_dispatch,
    admit_measurement,
    base_capacity_record,
    collect_linkdelta_tiers,
    require_idle_targets,
    unique_object,
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


class AdmissionTests(unittest.TestCase):
    def test_exact_and_push(self):
        admit_dispatch(self.request(), source="a" * 40, actual="a" * 40,
                       policy_pin="b" * 64, attempt="1")
        admit_dispatch({"event": "push"}, source="a" * 40,
                       actual="a" * 40, policy_pin="b" * 64, attempt="2")

    def request(self):
        return {"event": "workflow_dispatch", "approval_id": "batch-record",
                "fork_commit": "a" * 40, "policy_sha256": "b" * 64,
                "release_tag": "storage-fixtures-v1"}

    def test_refusal_before_execution(self):
        cases = (("approval_id", ""), ("approval_id", "  "),
                 ("fork_commit", "a" * 39),
                 ("fork_commit", "c" * 40),
                 ("policy_sha256", "b" * 63),
                 ("policy_sha256", "c" * 64),
                 ("release_tag", "latest"), ("release_tag", "../other"))
        for field, value in cases:
            with self.subTest(field=field, value=value):
                request = {**self.request(), field: value}
                with self.assertRaises(RuntimeError):
                    admit_dispatch(request, source="a" * 40,
                                   actual="a" * 40, policy_pin="b" * 64,
                                   attempt="1")
        for actual, attempt in (("c" * 40, "1"), ("a" * 40, "2")):
            with self.assertRaises(RuntimeError):
                admit_dispatch(self.request(), source="a" * 40,
                               actual=actual, policy_pin="b" * 64,
                               attempt=attempt)

    def test_duplicate_policy_key(self):
        with self.assertRaises(RuntimeError):
            unique_object([("wall_seconds", 1), ("wall_seconds", 2)])

    def test_request_mapping_is_not_changed(self):
        for changes in ({}, {"approval_id": ""}):
            request = {**self.request(), **changes}
            before = dict(request)
            if changes:
                with self.assertRaises(RuntimeError):
                    admit_dispatch(request, source="a" * 40, actual="a" * 40,
                                   policy_pin="b" * 64, attempt="1")
            else:
                admit_dispatch(request, source="a" * 40, actual="a" * 40,
                               policy_pin="b" * 64, attempt="1")
            self.assertEqual(request, before)


class MeasurementAdmissionTests(unittest.TestCase):
    def setUp(self):
        self.host = MeasurementHost(
            system="Linux", python_version=(3, 12), cpu_count=4,
            physical_bytes=16 * 1024 ** 3, private_repository=False,
            zstd_available=True)
        self.arguments = {
            "event": "workflow_dispatch", "role": "base",
            "checkout_status": b"", "host": self.host, "cpus": 4,
            "job_memory_bytes": 12 * 1024 ** 3,
            "host_reserve_bytes": 2 * 1024 ** 3, "price_cap_usd": 0,
        }

    def test_exact_public_host_and_memory_boundary(self):
        for role in ("base", "linkdelta", "vector"):
            with self.subTest(role=role):
                self.assertIsNone(admit_measurement(
                    **{**self.arguments, "role": role}))
        boundary = replace(self.host, physical_bytes=14 * 1024 ** 3)
        self.assertIsNone(admit_measurement(
            **{**self.arguments, "host": boundary}))

    def test_manual_only_clean_checkout_and_known_group(self):
        cases = (("event", "push"), ("event", "workflow_run"),
                 ("checkout_status", b" M source.py\n"),
                 ("role", "unknown"), ("role", ""))
        for field, value in cases:
            with self.subTest(field=field, value=value):
                with self.assertRaises(RuntimeError):
                    admit_measurement(**{**self.arguments, field: value})

    def test_platform_cpu_and_memory_refusals(self):
        cases = (
            ("system", replace(self.host, system="Darwin")),
            ("python", replace(self.host, python_version=(3, 11))),
            ("cpu_low", replace(self.host, cpu_count=3)),
            ("cpu_high", replace(self.host, cpu_count=5)),
            ("cpu_unknown", replace(self.host, cpu_count=None)),
            ("memory", replace(
                self.host, physical_bytes=14 * 1024 ** 3 - 1)),
        )
        for reason, host in cases:
            with self.subTest(reason=reason):
                with self.assertRaises(RuntimeError):
                    admit_measurement(**{**self.arguments, "host": host})

    def test_private_or_paid_venue_refusals(self):
        private = replace(self.host, private_repository=True)
        for changes in ({"host": private}, {"price_cap_usd": 1}):
            with self.subTest(changes=changes):
                with self.assertRaises(RuntimeError):
                    admit_measurement(**{**self.arguments, **changes})

    def test_zstd_requirement_tracks_fixture_consumers(self):
        host = replace(self.host, zstd_available=False)
        for role in ("base", "linkdelta"):
            with self.subTest(role=role):
                with self.assertRaises(RuntimeError):
                    admit_measurement(
                        **{**self.arguments, "host": host, "role": role})
        self.assertIsNone(admit_measurement(
            **{**self.arguments, "host": host, "role": "vector"}))


if __name__ == "__main__":
    unittest.main()
