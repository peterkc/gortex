"""Tiny controller fixtures. No study workload or model download."""
import ast
from copy import deepcopy
from io import StringIO
import json
import os
import signal
import socket
import sys
import time
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch

import run as driver


class FixtureSupervisor:
    def __init__(self, root, modes, interrupt_at=None):
        self.root = root
        self.environment = {}
        self.modes = modes
        self.interrupt_at = interrupt_at
        self.calls = []

    def job_limit(self, name, now):
        return 10000

    def observe(self, groups):
        pass

    def close(self, owned):
        pass

    def start(self, args, directory, env, cwd):
        self.calls.append(directory.name)
        if len(self.calls) == self.interrupt_at:
            raise driver.ReviewRequired("fixture interruption")
        mode = self.modes[len(self.calls)-1]
        adapter = Path(args[args.index("-data")-1]).name
        job = driver.Job("screen", adapter, "tier10")
        check = {"query_id": "q", "operation": "id", "status": "passed",
                 "mismatch": False}
        report = {
            "adapter": adapter, "tier": "tier10", "run": 1,
            "manifest_sha256": driver.digest(
                self.root / "data/tier10/manifest.json"),
            "success": mode == "pass", "builds": [{"repo": "fixture"}],
            "checks": [check], "peak_rss_bytes": 1, "failures": [],
            "metrics": {key: {"count": 1} for key in (
                "build_seal", "open_first_no_cache_flush", "open_warm")},
            "delta_checks": [{**check, "query_id": key}
                             for key in driver.DELTA_IDS],
            "concurrent": {
                "commit_race_windows": {stage: 2 for stage in driver.STAGES},
                "commit_race_minimum": 2, "new_samples_per_reader": [2]*8,
                "after_publish": {"count": 16}, "checks": [check],
            },
            "crash": [{"ok": True, "killed": True, "mode": trial_mode,
                       "checkpoint": stage}
                      for trial_mode in ("checkpoint", "external-time")
                      for stage in sorted(driver.STAGES) for _ in range(2)],
        }
        code = 0 if mode == "pass" else 1
        if mode != "pass":
            report["checks"] = [{**check, "status": "failed",
                                  "mismatch": True, "detail": "named"}]
            report["failures"] = ["q: named"]
        if mode == "fatal":
            report["failures"].append("loader fatal")
        if mode == "incomplete":
            report["crash"] = []
        path = driver.result_path(self.root, job)
        path.parent.mkdir()
        driver.atomic_json(path, report)
        driver.atomic_json(directory / "time.json", {
            "exit_code": code, "elapsed_seconds": 1,
            "user_seconds": 1, "system_seconds": 0, "max_rss_kib": 1,
        })
        (directory / "process.json").write_text("{}")
        (directory / "rss.jsonl").write_text("{}\n")
        (directory / "stdout-stderr.log").write_text("fixture")
        process = SimpleNamespace(pid=100+len(self.calls),
                                  poll=lambda: code, returncode=code)
        return SimpleNamespace(process=process, started=0, peak_rss=0,
                               deadline=10000)


class DriverTests(unittest.TestCase):
    def fixture(self, root):
        (root / "jobs").mkdir()
        data = root / "data/tier10"
        data.mkdir(parents=True)
        driver.atomic_json(data / "manifest.json", {"repos": ["fixture"]})
        driver.atomic_json(data / "queries.json", [{"id": "q", "op": "id"}])
        selected = [driver.Job("screen", name, "tier10")
                    for name in ("sqlite-segment", "pebble-bleve")]
        setup = {"identity": {"driver": "fixture", "runtime": {}},
                 "artifacts": {"bin/"+j.adapter: "fixture" for j in selected},
                 "runtime_libraries": {}}
        return selected, setup, {p.name: driver.digest(p) for p in data.iterdir()}

    def execute(self, supervisor, selected, setup, receipts):
        with patch.object(driver, "process_groups", return_value={}), \
                patch.object(driver, "memory_available", return_value=10**15), \
                patch.object(driver.time, "monotonic", return_value=0), \
                patch.object(driver.time, "sleep"):
            driver.execute_stage(supervisor, selected, setup, 1, receipts)

    def test_named_failure_continues_and_completed_resume_skips(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            selected, setup, before = self.fixture(root)
            supervisor = FixtureSupervisor(root, ["named", "pass"])
            receipts = []
            self.execute(supervisor, selected, setup, receipts)
            self.assertEqual([r["state"] for r in receipts],
                             ["checks_failed", "pass"])
            self.assertEqual(len(supervisor.calls), 2)
            self.assertEqual(len(driver.eligible_timings(receipts)), 6)
            resumed = FixtureSupervisor(root, [])
            saved = []
            self.execute(resumed, selected, setup, saved)
            self.assertEqual(resumed.calls, [])
            self.assertEqual(len(saved), 2)
            self.assertEqual(before, {p.name: driver.digest(p)
                                     for p in (root/"data/tier10").iterdir()})

    def test_interruption_preserves_complete_and_refuses_partial(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            selected, setup, before = self.fixture(root)
            supervisor = FixtureSupervisor(root, ["pass"], interrupt_at=2)
            receipts = []
            with self.assertRaisesRegex(driver.ReviewRequired, "interruption"):
                self.execute(supervisor, selected, setup, receipts)
            self.assertEqual(len(receipts), 1)
            resumed = FixtureSupervisor(root, [])
            saved = []
            with self.assertRaisesRegex(driver.ReviewRequired, "incomplete attempt"):
                self.execute(resumed, selected, setup, saved)
            self.assertEqual(len(saved), 1)
            self.assertEqual(resumed.calls, [])
            self.assertEqual(before, {p.name: driver.digest(p)
                                     for p in (root/"data/tier10").iterdir()})

    def test_incomplete_and_fatal_results_stop_before_next_case(self):
        for mode in ("incomplete", "fatal"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as folder:
                root = Path(folder)
                selected, setup, before = self.fixture(root)
                supervisor = FixtureSupervisor(root, [mode, "pass"])
                with self.assertRaises(driver.ReviewRequired):
                    self.execute(supervisor, selected, setup, [])
                self.assertEqual(len(supervisor.calls), 1)
                self.assertFalse((root/"jobs"/selected[0].name/"complete.json").exists())
                self.assertEqual(before, {p.name: driver.digest(p)
                                         for p in (root/"data/tier10").iterdir()})

    def test_required_hybrid_recipe_rejections(self):
        result = {"method": "f32", "phase": "latency", "queries": 256,
                  "latency_ns": {"count": 256}, "M": 32, "ef_search": 1024}
        native = {"native_version": "2.26.5", "config": {
            "Connectivity": 32, "ExpansionAdd": 256, "Quantization": 1,
            "Dimensions": 1024}}
        driver.check_hybrid_recipe("f32", result, native)
        for key, value in (("method", "exact"), ("M", 16), ("ef_search", 512)):
            with self.subTest(key=key):
                with self.assertRaises(driver.ReviewRequired):
                    driver.check_hybrid_recipe("f32", {**result, key: value}, native)
        wrong = deepcopy(native)
        wrong["config"]["ExpansionAdd"] = 128
        with self.assertRaises(driver.ReviewRequired):
            driver.check_hybrid_recipe("f32", result, wrong)
        driver.check_hybrid_recipe("int8", {**result, "method": "int8",
            "candidate_k": 128, "required_file_components_bytes": {
                "int8_with_original_f32": 1}})
        with self.assertRaises(driver.ReviewRequired):
            driver.check_hybrid_recipe("int8", {**result, "method": "int8",
                "candidate_k": 128, "required_file_components_bytes": {
                    "original_f32": 1}})


class DeadlineContractTests(unittest.TestCase):
    def procedure(self):
        return {
            "owner": "pi-gcp", "mechanism": "GCE max-run-duration DELETE",
            "provider_ttl": True, "termination_action": "DELETE",
            "on_host_maintenance": "TERMINATE", "time_origin": "created_at",
            "no_credentials_on_vm": True, "max_run_duration_minutes": 710,
            "backstop_minute": 710,
            "live_receipt_fields": [
                "name", "zone", "project", "id", "status", "creation_time",
                "last_start_time", "termination_time", "provisioning_model",
                "instance_termination_action", "on_host_maintenance",
                "boot_disk_auto_delete", "service_accounts", "login_user",
                "uid", "gpu", "setup_confirmed", "owner", "firewall_rule",
                "max_run_duration_seconds", "boot_disk_name", "boot_disk_type",
                "boot_disk_size_gb", "source_image",
            ],
        }

    def bindings(self, root=Path("/work")):
        return {block: [{
            "id": block + "-fixture", "argv": ["/usr/bin/true"],
            "source_pin": "1" * 40, "config_pin": "2" * 64,
            "input_pin": "3" * 64,
            "receipt_path": str(root / "control" / (block + ".json")),
            "minutes": 1, "owner": "fixture-only",
        }] for block in driver.INPUT_BLOCKS}

    def predecessors(self, block):
        names = [row[0] for row in driver.BLOCKS]
        return {name: "completed" for name in names[:names.index(block)]}

    def live(self):
        allocation = {
            "name": "fixture-vm", "zone": "us-west1-b",
            "project": "peterkc-dev",
            "id": "12345", "created_at": "2026-10-26T00:00:00-07:00",
        }
        receipt = {
            **{k: allocation[k] for k in ("name", "zone", "project", "id")},
            "owner": "pi-gcp", "status": "RUNNING", "setup_confirmed": True,
            "creation_time": allocation["created_at"],
            "last_start_time": allocation["created_at"],
            "termination_time": "2026-10-26T11:50:00-07:00",
            "provisioning_model": "STANDARD",
            "instance_termination_action": "DELETE",
            "on_host_maintenance": "TERMINATE", "boot_disk_auto_delete": True,
            "service_accounts": [], "login_user": "peterkc", "uid": 1000,
            "gpu": "recorded-fixture-gpu", "max_run_duration_seconds": 42600,
            "boot_disk_name": "fixture-boot", "boot_disk_type": "pd-balanced",
            "boot_disk_size_gb": 700,
            "source_image": "common-cu129-ubuntu-2204-nvidia-580-v20261019",
            "firewall_rule": {"name": "fixture-ssh",
                              "source_range": "127.0.0.1/32"},
        }
        return allocation, receipt

    def test_complete_no_pod_preflight_has_no_effects(self):
        bindings = self.bindings()
        before = deepcopy(bindings)
        with (
            patch.object(driver.subprocess, "Popen",
                         side_effect=AssertionError),
            patch.object(driver.urllib.request, "urlopen",
                         side_effect=AssertionError),
            patch.object(Path, "mkdir", side_effect=AssertionError),
            patch.object(driver, "host_record", side_effect=AssertionError),
        ):
            result = driver.preflight(
                bindings, driver.setup_recipe(),
                self.procedure(), Path("/work"),
            )
        self.assertEqual(result["phase1"], "prepared-no-pod")
        self.assertEqual(result["backstop_minute"], 710)
        self.assertEqual(result["retrieval_deadline_minute"], 710)
        self.assertEqual(result["blocks"][0]["jobs"][0]["owner"], "pi-gcp")
        self.assertEqual(bindings, before)
        expected = [
            ("setup", 45, 0, 45), ("smoke", 25, 45, 70),
            ("calibration", 15, 70, 85), ("storage", 150, 85, 235),
            ("segment_timing", 120, 235, 355), ("catalog", 30, 355, 385),
            ("hybrid", 45, 385, 430), ("model_a4", 160, 430, 590),
            ("cpu_cost", 60, 590, 650), ("baseline_a7", 40, 650, 690),
            ("retrieval", 30, 690, 720),
        ]
        self.assertEqual([
            (b["id"], b["minutes"],
             b["latest_start_minute"], b["deadline_minute"])
            for b in result["blocks"]], expected)
        self.assertEqual(sum(b["minutes"] for b in result["blocks"]), 720)
        self.assertEqual([len(b["jobs"]) for b in result["blocks"]],
                         [1, 1, 1, 12, 12, 15, 2, 1, 1, 1, 1])
        self.assertEqual(result["blocks"][-1]["execution_scope"],
                         "external-owner")
        self.assertEqual(result["enforcement_entrypoint"],
                         "study/storage/pod/run.py:main")

    def test_actual_cli_preflight_needs_no_allocation_or_live_job(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            payloads = {
                "bindings": self.bindings(),
                "setup-recipe": driver.setup_recipe(),
                "stop-procedure": self.procedure(),
            }
            argv = ["run.py", "--preflight"]
            for option, payload in payloads.items():
                path = root / (option + ".json")
                path.write_text(json.dumps(payload))
                argv.extend(["--" + option, str(path)])
            output = StringIO()
            with (
                patch.object(sys, "argv", argv),
                patch.object(sys, "stdout", output),
                patch.object(driver.subprocess, "Popen",
                             side_effect=AssertionError),
                patch.object(driver.urllib.request, "urlopen",
                             side_effect=AssertionError),
                patch.object(driver, "host_record",
                             side_effect=AssertionError),
                patch.object(Path, "mkdir", side_effect=AssertionError),
            ):
                self.assertEqual(driver.main(), 0)
            result = json.loads(output.getvalue())
            self.assertEqual(result["phase1"], "prepared-no-pod")
            self.assertEqual(result["backstop_minute"], 710)
            self.assertEqual(result["driver_sha256"],
                             driver.digest(Path(driver.__file__)))
            self.assertEqual(sorted(p.name for p in root.iterdir()),
                             ["bindings.json", "setup-recipe.json",
                              "stop-procedure.json"])

    def test_dry_run_keeps_nonexistent_workspace_absent(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / "not-created"
            output = StringIO()
            with (
                patch.object(sys, "argv", [
                    "run.py", "--dry-run", "--root", str(root)]),
                patch.object(sys, "stdout", output),
                patch.object(driver.subprocess, "Popen",
                             side_effect=AssertionError),
                patch.object(driver.urllib.request, "urlopen",
                             side_effect=AssertionError),
                patch.object(driver, "host_record",
                             side_effect=AssertionError),
                patch.object(driver, "process_groups",
                             side_effect=AssertionError),
                patch.object(Path, "mkdir", side_effect=AssertionError),
            ):
                self.assertEqual(driver.main(), 0)
            result = json.loads(output.getvalue())
            self.assertFalse(root.exists())
            self.assertFalse(result["creation_admitted"])
            self.assertEqual(result["external_commands_executed"], 0)
            self.assertEqual(len(result["absolute_schedule"]), 11)
            self.assertEqual(result["mandatory_owner_bindings"],
                             sorted(driver.INPUT_BLOCKS))

    def test_each_missing_binding_or_field_names_the_limit(self):
        for block in sorted(driver.INPUT_BLOCKS):
            bindings = self.bindings()
            del bindings[block]
            with self.subTest(block=block), self.assertRaisesRegex(
                    driver.ReviewRequired, block):
                driver.preflight(bindings, driver.setup_recipe(),
                                 self.procedure(), Path("/work"))
            for field in ("argv", "source_pin", "config_pin", "input_pin",
                          "receipt_path", "minutes", "owner"):
                bindings = self.bindings()
                del bindings[block][0][field]
                with self.subTest(block=block, field=field), \
                        self.assertRaisesRegex(driver.ReviewRequired,
                                               block + ": missing " + field):
                    driver.preflight(bindings, driver.setup_recipe(),
                                     self.procedure(), Path("/work"))

    def test_malformed_bindings_and_impossible_schedule_refuse(self):
        mutations = [
            ("minutes", 61), ("minutes", float("nan")), ("minutes", True),
            ("argv", "not-an-argv"),
            ("argv", ["/usr/bin/apt-get", "update"]),
            ("argv", ["/usr/bin/sudo", "-n", "true"]),
            ("source_pin", "unsealed"),
            ("config_pin", "bad"), ("input_pin", "bad"),
            ("receipt_path", "/work/../outside/result.json"),
            ("id", "../../escape"), ("owner", ""),
        ]
        for key, value in mutations:
            bindings = self.bindings()
            bindings["cpu_cost"][0][key] = value
            with self.subTest(key=key, value=value), \
                    self.assertRaisesRegex(driver.ReviewRequired, "cpu_cost"):
                driver.preflight(bindings, driver.setup_recipe(),
                                 self.procedure(), Path("/work"))
        bindings = self.bindings()
        bindings["surprise"] = []
        with self.assertRaisesRegex(driver.ReviewRequired, "unlisted"):
            driver.preflight(bindings, driver.setup_recipe(),
                             self.procedure(), Path("/work"))

    def test_early_blocks_maxima_preserve_all_later_windows(self):
        now = 0
        outcomes = {}
        for block, minutes, latest, deadline in driver.BLOCKS:
            end = driver.block_limit(block, now, 0, 720 * 60, outcomes,
                                     a3_live=False)
            self.assertEqual(now, latest * 60)
            self.assertEqual(end, deadline * 60)
            self.assertEqual(end - now, minutes * 60)
            outcomes[block] = "completed"
            now = end
        self.assertEqual(now, 720 * 60)

    def test_early_start_does_not_borrow_and_clock_never_resets(self):
        outcomes = self.predecessors("storage")
        end = driver.block_limit("storage", 30 * 60, 0, 720 * 60,
                                 outcomes, a3_live=False)
        self.assertEqual(end, 180 * 60)
        supervisor = object.__new__(driver.Supervisor)
        supervisor.block_id = "storage"
        supervisor.block_deadline = end
        supervisor.job_minutes = {"first": 12.5, "second": 12.5}
        first = supervisor.job_limit("first", 30 * 60)
        second = supervisor.job_limit("second", end - 20)
        self.assertEqual(first, 42.5 * 60 - driver.CLEANUP_RESERVE_SECONDS)
        self.assertEqual(second, end - driver.CLEANUP_RESERVE_SECONDS)
        self.assertEqual(supervisor.block_deadline, end)
        with self.assertRaisesRegex(driver.DeadlineExceeded, "deadline"):
            supervisor.job_limit("second", end)

    def test_session_timing_keeps_the_completed_correctness_receipts(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            admission = driver.preflight(
                self.bindings(root), driver.setup_recipe(),
                self.procedure(), root,
            )
            supervisor = SimpleNamespace(
                root=root, allocation={}, termination={}, zero=0,
                hard_deadline=720 * 60, backstop_deadline=710 * 60,
                stop_all=unittest.mock.Mock(),
            )

            def begin(block, outcomes):
                supervisor.block_deadline = driver.block_limit(
                    block["id"], 0, 0, 720 * 60, outcomes, a3_live=False)
            supervisor.begin_block = begin
            seen = []

            def stage(owner, selected, setup, concurrency, receipts):
                seen.append(receipts)
                if selected[0].stage == "timing":
                    self.assertEqual(len(receipts), 12)
                    self.assertEqual(sum(
                        row["identity"]["job"]["stage"] == "oracle"
                        for row in receipts), 1)
                for job in selected:
                    receipts.append({
                        "identity": {"job": driver.asdict(job)},
                        "state": "pass",
                    })
            with (
                patch.object(driver.time, "monotonic", return_value=0),
                patch.object(driver.signal, "signal"),
                patch.object(driver.signal, "setitimer"),
                patch.object(driver, "prepare", return_value={}),
                patch.object(driver, "execute_stage", side_effect=stage),
                patch.object(driver, "run_catalog_hybrid"),
                patch.object(driver, "binding_block", return_value="completed"),
                patch.object(driver, "block_files", return_value={}),
            ):
                result = driver.execute_session(
                    supervisor, admission, root / "inputs", {}, 1)
            self.assertEqual(len(seen), 3)
            self.assertIs(seen[0], seen[1])
            self.assertIs(seen[1], seen[2])
            self.assertEqual(len(seen[2]), 24)
            self.assertEqual(result["acceptance"],
                             "complete-pending-independent-deletion")

    def test_guard_arms_earliest_active_job_without_resetting_block(self):
        supervisor = object.__new__(driver.Supervisor)
        supervisor.block_deadline = 1000
        supervisor.active = [SimpleNamespace(deadline=100),
                             SimpleNamespace(deadline=200)]
        with patch.object(driver.time, "monotonic", return_value=10), \
                patch.object(driver.signal, "setitimer") as timer:
            supervisor.arm_guard()
            timer.assert_called_with(signal.ITIMER_REAL, 90)
            supervisor.active.pop(0)
            supervisor.arm_guard()
            timer.assert_called_with(signal.ITIMER_REAL, 190)
            supervisor.active.clear()
            supervisor.arm_guard()
            timer.assert_called_with(signal.ITIMER_REAL, 979)
        self.assertEqual(supervisor.block_deadline, 1000)

    def test_late_launch_cpu_reserves_and_unclean_order_refuse(self):
        for block, _, latest, _ in driver.BLOCKS[1:]:
            with self.subTest(block=block), self.assertRaises(
                    driver.DeadlineExceeded):
                driver.block_limit(block, latest * 60 + 1, 0, 720 * 60,
                                   self.predecessors(block), a3_live=False)
        with self.assertRaisesRegex(driver.ReviewRequired, "predecessor"):
            driver.block_limit("storage", 0, 0, 720 * 60, {}, a3_live=False)
        with self.assertRaisesRegex(
                driver.DeadlineExceeded, "70-minute reserve"):
            driver.block_limit("cpu_cost", 650 * 60, 0, 720 * 60,
                               self.predecessors("cpu_cost"), a3_live=False)
        with self.assertRaisesRegex(driver.ReviewRequired, "A3 is still live"):
            driver.block_limit("baseline_a7", 600 * 60, 0, 720 * 60,
                               self.predecessors("baseline_a7"), a3_live=True)
        for block in ("storage", "model_a4"):
            outcomes = self.predecessors("cpu_cost")
            outcomes[block] = "partial"
            with self.subTest(block=block), self.assertRaisesRegex(
                    driver.ReviewRequired, "cpu_cost: dependency " + block):
                driver.block_limit("cpu_cost", 590 * 60, 0, 720 * 60,
                                   outcomes, a3_live=False)
        outcomes = self.predecessors("cpu_cost")
        outcomes["model_a4"] = "dependency_blocked"
        self.assertEqual(driver.block_limit(
            "cpu_cost", 590 * 60, 0, 720 * 60, outcomes, a3_live=False),
            650 * 60)

    def test_backstop_is_separate_from_session_and_bounds_retrieval(self):
        # Given GCE TTL and a shorter client backstop, use the earlier bound.
        allocation, receipt = self.live()
        wall = driver.timestamp(allocation["created_at"], "fixture")
        zero, backstop = driver.live_termination(
            allocation, receipt, wall + 1, procedure=self.procedure())
        self.assertEqual(backstop - zero, 710 * 60)
        supervisor = object.__new__(driver.Supervisor)
        supervisor.zero = 0
        supervisor.hard_deadline = 720 * 60
        supervisor.backstop_deadline = 710 * 60
        supervisor.active = []
        with patch.object(driver.time, "monotonic", return_value=0):
            supervisor.begin_block({"id": "setup", "jobs": []}, {})
        self.assertEqual(supervisor.block_deadline, 45 * 60)
        with patch.object(driver.time, "monotonic", return_value=690 * 60):
            supervisor.begin_block(
                {"id": "retrieval", "jobs": []},
                self.predecessors("retrieval"))
        self.assertEqual(supervisor.block_deadline, 710 * 60)
        shorter = {**self.procedure(), "backstop_minute": 720}
        _, earlier = driver.live_termination(
            allocation, receipt, wall + 1, procedure=shorter)
        self.assertEqual(earlier - zero, 710 * 60)
        supervisor.backstop_deadline = 689 * 60
        with patch.object(driver.time, "monotonic", return_value=650 * 60):
            supervisor.begin_block(
                {"id": "baseline_a7", "jobs": []},
                self.predecessors("baseline_a7"))
        self.assertEqual(supervisor.block_deadline, 689 * 60)

    def test_live_receipt_exact_binding_and_no_spawn_on_rejection(self):
        allocation, receipt = self.live()
        now = driver.timestamp(allocation["created_at"], "fixture") + 1
        self.assertEqual(driver.live_termination(
            allocation, receipt, now, procedure=self.procedure()),
            (now - 1, now - 1 + 710 * 60))
        for key, wrong in (("name", "another-vm"), ("zone", "another-zone"),
                           ("project", "another-project"), ("id", "999"),
                           ("setup_confirmed", False), ("owner", "other")):
            with self.subTest(key=key), self.assertRaisesRegex(
                    driver.ReviewRequired, "A9/pi-gcp deletion"):
                driver.live_termination(allocation, {**receipt, key: wrong},
                                        now, procedure=self.procedure())
        for missing in ({}, None):
            with self.assertRaisesRegex(driver.ReviewRequired, "A9/pi-gcp"):
                driver.live_termination(allocation, missing, now,
                                        procedure=self.procedure())
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            supervisor = object.__new__(driver.Supervisor)
            supervisor.allocation, supervisor.termination = allocation, {}
            supervisor.procedure = self.procedure()
            with patch.object(driver.time, "time", return_value=now), \
                    patch.object(driver.subprocess, "Popen") as spawn:
                with self.assertRaisesRegex(driver.ReviewRequired,
                                            "A9/pi-gcp"):
                    supervisor.start(["/usr/bin/true"], root, {}, root)
                spawn.assert_not_called()
                self.assertEqual(list(root.iterdir()), [])

    def package_fixture(self, root, missing=None, config_only=None):
        names = (
            "build-essential", "cmake", "git", "ca-certificates", "curl",
            "zstd", "pkg-config", "binutils", "file", "time", "util-linux",
        )
        versions = {name: "1.0-fixture" for name in names if name != missing}
        version_log = root / "versions.txt"
        state_log = root / "states.txt"
        version_log.write_text("".join(
            f"{name} {value}\n" for name, value in versions.items()))
        state_log.write_text("".join(
            f"{name} "
            f"{'config-files' if name == config_only else 'installed'}\n"
            for name in versions))
        supervisor = SimpleNamespace(run=unittest.mock.Mock(
            side_effect=[version_log, state_log]))
        return supervisor, versions, names

    def test_setup_only_queries_exact_package_versions_and_installation(self):
        with tempfile.TemporaryDirectory() as folder:
            supervisor, expected, names = self.package_fixture(Path(folder))
            self.assertEqual(driver.verify_package_versions(supervisor),
                             expected)
            calls = supervisor.run.call_args_list
            self.assertEqual(len(calls), 2)
            self.assertEqual(calls[0].args[0], [
                "dpkg-query", "-W", "-f", "${Package} ${Version}\n", *names])
            self.assertEqual(calls[1].args[0], [
                "dpkg-query", "-W", "-f",
                "${Package} ${db:Status-Status}\n", *names])
            for call in calls:
                self.assertEqual(call.kwargs, {"accepted_exit_codes": (0, 1)})
                self.assertNotIn("apt-get", call.args[0])
                self.assertNotIn("sudo", call.args[0])

    def test_setup_missing_or_config_only_package_refuses_by_name(self):
        for field in ("missing", "config_only"):
            with self.subTest(field=field), \
                    tempfile.TemporaryDirectory() as folder:
                supervisor, _, _ = self.package_fixture(
                    Path(folder), **{field: "cmake"})
                with self.assertRaisesRegex(driver.ReviewRequired, "cmake"):
                    driver.verify_package_versions(supervisor)

    def test_prepare_refuses_missing_package_before_download(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            control, inputs = root / "control", root / "inputs"
            control.mkdir()
            inputs.mkdir()
            (inputs / "evaluator").mkdir()
            (inputs / "cpu-cost").mkdir()
            (root / "lost+found").mkdir()
            archive = inputs / "tier1.tar.zst"
            archive.write_bytes(b"fixture")
            expected = {"sha256": driver.digest(archive), "bytes": 7,
                        "queries": 1}
            (inputs / "tiers-summary.json").write_text(json.dumps({
                "tier1": {"archive_sha256": expected["sha256"],
                          "archive_bytes": 7, "queries": 1},
            }))
            supervisor, _, _ = self.package_fixture(control, missing="cmake")
            supervisor.root = root
            disk = SimpleNamespace(free=10**15,
                                   _asdict=lambda: {"free": 10**15})
            with (
                patch.object(driver, "ARCHIVES", {"tier1": expected}),
                patch.object(driver, "host_record", return_value={}),
                patch.object(driver.shutil, "disk_usage", return_value=disk),
                patch.object(driver, "download") as download,
            ):
                with self.assertRaisesRegex(driver.ReviewRequired, "cmake"):
                    driver.prepare(supervisor, inputs, {},
                                   driver.setup_recipe())
                download.assert_not_called()
            self.assertEqual(supervisor.run.call_count, 2)
            self.assertFalse((root / "setup-complete.json").exists())

    def test_driver_has_no_apt_or_sudo_argv_builder(self):
        tree = ast.parse(Path(driver.__file__).read_text())
        bad = []
        for node in ast.walk(tree):
            if isinstance(node, (ast.List, ast.Tuple)):
                for element in node.elts:
                    if (isinstance(element, ast.Constant)
                            and element.value in ("apt-get", "sudo")):
                        bad.append((element.lineno, element.value))
        self.assertEqual(bad, [])
        allocation, receipt = self.live()
        wall = driver.timestamp(allocation["created_at"], "fixture") + 1
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            supervisor = object.__new__(driver.Supervisor)
            supervisor.allocation = allocation
            supervisor.termination = receipt
            supervisor.procedure = self.procedure()
            with patch.object(driver.time, "time", return_value=wall), \
                    patch.object(driver.subprocess, "Popen") as spawn:
                for executable in ("apt-get", "/usr/bin/sudo"):
                    with self.subTest(executable=executable), \
                            self.assertRaisesRegex(driver.ReviewRequired,
                                                   "pi-gcp owns SSH setup"):
                        supervisor.start([executable], root, {}, root)
                spawn.assert_not_called()
            self.assertEqual(list(root.iterdir()), [])

    def test_stop_receipt_requires_actual_pi_gcp_fields_and_state(self):
        allocation, original = self.live()
        wall = driver.timestamp(allocation["created_at"], "fixture") + 1
        for field in original:
            receipt = deepcopy(original)
            del receipt[field]
            with self.subTest(missing=field), self.assertRaises(
                    driver.ReviewRequired):
                driver.live_termination(allocation, receipt, wall,
                                        procedure=self.procedure())
        for field, value in (
                ("creation_time", "2026-10-26T00:00:01-07:00"),
                ("max_run_duration_seconds", 42599),
                ("max_run_duration_seconds", "42600"),
                ("provisioning_model", "SPOT"),
                ("instance_termination_action", "STOP"),
                ("on_host_maintenance", "MIGRATE"),
                ("boot_disk_auto_delete", False),
                ("service_accounts", {}), ("uid", True), ("uid", -1),
                ("login_user", ""), ("gpu", ""), ("firewall_rule", {}),
                ("boot_disk_name", ""), ("boot_disk_size_gb", 700.0),
                ("source_image", None),
                ("unexpected", "extra"),
                ("last_start_time", "2026-10-26T00:00:00.1-07:00")):
            with self.subTest(field=field, value=value), self.assertRaises(
                    driver.ReviewRequired):
                driver.live_termination(allocation, {**original, field: value},
                                        wall, procedure=self.procedure())

    def test_recipe_rejects_before_package_manager_or_workspace_effect(self):
        recipe = driver.setup_recipe()
        driver.validate_recipe(recipe)
        for mode in ("extra-package", "missing-package",
                     "upgrade", "path", "owner"):
            changed = deepcopy(recipe)
            if mode == "extra-package":
                changed["packages"].append("unlisted")
            elif mode == "missing-package":
                changed["packages"].pop()
            elif mode == "upgrade":
                changed["commands"][0] = "apt-get dist-upgrade"
            elif mode == "path":
                changed["allowed_system_paths"].append("/opt")
            else:
                changed["owner"] = ""
            supervisor = SimpleNamespace(run=unittest.mock.Mock())
            with (self.subTest(mode=mode),
                  self.assertRaises(driver.ReviewRequired)):
                driver.prepare(supervisor, Path("/not-created"), {}, changed)
            supervisor.run.assert_not_called()

    @unittest.skipUnless(
        sys.platform == "linux", "Linux /proc and subreaper contract")
    def test_nonreturner_whole_group_is_terminated_and_reaped(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / "setup").mkdir()
            allocation, receipt = self.live()
            now_wall = (
                driver.timestamp(allocation["created_at"], "fixture") + 1)
            listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            self.addCleanup(listener.close)
            ready = root / "ready.sock"
            listener.bind(str(ready))
            listener.listen(2)
            listener.settimeout(2)
            previous_alarm = signal.getsignal(signal.SIGALRM)
            self.addCleanup(signal.signal, signal.SIGALRM, previous_alarm)
            self.addCleanup(signal.setitimer, signal.ITIMER_REAL, 0)
            with patch.object(driver.time, "time", return_value=now_wall):
                supervisor = driver.Supervisor(
                    root, dict(os.environ), allocation=allocation,
                    termination=receipt, procedure=self.procedure(),
                )
                supervisor.block_id = "fixture-nonreturner"
                start = time.monotonic()
                supervisor.block_deadline = start + 15
                supervisor.launch_deadline = start + 2
                program = (
                    "import os,signal,socket,sys; "
                    "signal.signal(signal.SIGTERM,signal.SIG_IGN); "
                    "child=os.fork(); "
                    "s=socket.socket(socket.AF_UNIX); "
                    "s.connect(sys.argv[1]); "
                    "s.sendall(str(os.getpid()).encode()); s.close(); "
                    "signal.pause()"
                )
                try:
                    with self.assertRaisesRegex(driver.DeadlineExceeded,
                                                "absolute setup deadline"):
                        supervisor.run(
                            [sys.executable, "-c", program, str(ready)])
                    pgid = driver.read_json(
                        root / "setup/command-001/process.json")["pid"]
                finally:
                    supervisor.stop_all()
                children = []
                for _ in range(2):
                    connection, _ = listener.accept()
                    with connection:
                        connection.settimeout(2)
                        children.append(int(connection.recv(100)))
                self.assertEqual(len(set(children)), 2)
                self.assertIn(pgid, children)
                self.assertEqual(supervisor.active, [])
                self.assertEqual(supervisor.block_deadline, start + 15)
                self.assertNotIn(pgid, driver.process_groups())
                for pid in children:
                    with self.assertRaises(ChildProcessError):
                        os.waitpid(pid, os.WNOHANG)
                self.assertLess(time.monotonic(), start + 15)
                directory = root / "setup/command-001"
                self.assertTrue((directory / "process.json").exists())
                self.assertTrue((directory / "stdout-stderr.log").exists())
                failure = driver.read_json(directory / "failure.json")
                self.assertTrue(failure["whole_group_reaped"])
                self.assertEqual(failure["state"], "partial")
                self.assertLess(failure["exit_code"], 0)
                self.assertFalse((directory / "complete.json").exists())


    def test_gce_procedure_passes_preflight(self):
        """Given GCE controls, preflight keeps the exact TTL contract."""
        result = driver.preflight(self.bindings(), driver.setup_recipe(),
                                  self.procedure(), Path("/work"))
        self.assertEqual(result["termination_procedure"], self.procedure())
        self.assertEqual(result["backstop_minute"], 710)

    def test_gce_wrong_mechanism_refuses(self):
        """Given a different stop mechanism, preflight refuses it."""
        with self.assertRaises(driver.ReviewRequired):
            driver.preflight(self.bindings(), driver.setup_recipe(),
                             {**self.procedure(), "mechanism": "launchd"},
                             Path("/work"))

    def test_gce_provider_ttl_false_refuses(self):
        """Given no provider TTL, preflight refuses workload admission."""
        with self.assertRaises(driver.ReviewRequired):
            driver.preflight(self.bindings(), driver.setup_recipe(),
                             {**self.procedure(), "provider_ttl": False},
                             Path("/work"))

    def test_gce_stop_action_refuses(self):
        """Given STOP instead of DELETE, preflight refuses the procedure."""
        with self.assertRaises(driver.ReviewRequired):
            driver.preflight(self.bindings(), driver.setup_recipe(),
                             {**self.procedure(),
                              "termination_action": "STOP"},
                             Path("/work"))

    def test_gce_credentials_present_refuses(self):
        """Given credentials on the VM, preflight refuses the procedure."""
        with self.assertRaises(driver.ReviewRequired):
            driver.preflight(self.bindings(), driver.setup_recipe(),
                             {**self.procedure(),
                              "no_credentials_on_vm": False},
                             Path("/work"))

    def test_gce_missing_procedure_field_refuses(self):
        """Given any missing procedure field, preflight refuses it."""
        for field in self.procedure():
            changed = self.procedure()
            del changed[field]
            with self.subTest(field=field), self.assertRaises(
                    driver.ReviewRequired):
                driver.preflight(self.bindings(), driver.setup_recipe(),
                                 changed, Path("/work"))

    def test_gce_extra_procedure_field_refuses(self):
        """Given an unlisted procedure field, preflight refuses it."""
        with self.assertRaises(driver.ReviewRequired):
            driver.preflight(self.bindings(), driver.setup_recipe(),
                             {**self.procedure(), "extra": True},
                             Path("/work"))

    def test_gce_backstop_outside_bounds_refuses(self):
        """Given an invalid backstop, preflight preserves the time bounds."""
        for minute in (689, 721, True, float("nan"), float("inf")):
            with self.subTest(minute=minute), self.assertRaises(
                    driver.ReviewRequired):
                driver.preflight(self.bindings(), driver.setup_recipe(),
                                 {**self.procedure(),
                                  "backstop_minute": minute},
                                 Path("/work"))

    def test_gce_remaining_pinned_procedure_fields_refuse(self):
        """Given changed owner, duration or policy, preflight refuses it."""
        mutations = (("owner", "other"), ("max_run_duration_minutes", 711),
                     ("max_run_duration_minutes", 710.0),
                     ("on_host_maintenance", "MIGRATE"),
                     ("time_origin", "last_start_time"),
                     ("live_receipt_fields", []))
        for field, value in mutations:
            with self.subTest(field=field), self.assertRaises(
                    driver.ReviewRequired):
                driver.preflight(self.bindings(), driver.setup_recipe(),
                                 {**self.procedure(), field: value},
                                 Path("/work"))

    def test_gce_service_account_present_refuses(self):
        """Given a service account on the VM, the live receipt rejects."""
        allocation, receipt = self.live()
        receipt["service_accounts"] = [{"email": "fixture@example.invalid"}]
        now = driver.timestamp(allocation["created_at"], "fixture") + 1
        with self.assertRaisesRegex(driver.ReviewRequired, "service_accounts"):
            driver.live_termination(allocation, receipt, now,
                                    procedure=self.procedure())

    def test_gce_status_not_running_refuses(self):
        """Given a stopped VM, its former TTL cannot admit new work."""
        allocation, receipt = self.live()
        receipt["status"] = "TERMINATED"
        now = driver.timestamp(allocation["created_at"], "fixture") + 1
        with self.assertRaisesRegex(driver.ReviewRequired, "status"):
            driver.live_termination(allocation, receipt, now,
                                    procedure=self.procedure())

    def test_gce_old_launchd_procedure_refuses(self):
        """Given the previous launchd procedure, preflight rejects it."""
        old = {
            "owner": "pi-gcp", "mechanism": "Mac launchd one-shot",
            "terminate_argv": ["prime", "pods", "terminate", "<pod-id>",
                               "--yes", "--plain"],
            "status_argv": ["prime", "pods", "status", "<pod-id>",
                            "-o", "json"],
            "time_origin": "created_at", "existing_cli_login": True,
            "no_api_key_on_pod": True, "provider_ttl": False,
            "backstop_minute": 710, "awake": "caffeinate fixture",
            "live_receipt_fields": [
                "job_label", "plist_path", "pod_id", "created_at",
                "deadline_pdt",
                "calendar_entry", "install_time_pdt", "awake",
                "launchctl_state",
                "setup_confirmed", "owner", "existing_cli_login"],
        }
        with self.assertRaises(driver.ReviewRequired):
            driver.preflight(self.bindings(), driver.setup_recipe(), old,
                             Path("/work"))


    def test_gce_allocation_shape_and_project_refuse(self):
        """Given missing/extra identity or another project, reject it."""
        allocation, receipt = self.live()
        now = driver.timestamp(allocation["created_at"], "fixture") + 1
        for field in allocation:
            changed = {k: v for k, v in allocation.items() if k != field}
            with self.subTest(missing=field), self.assertRaises(
                    driver.ReviewRequired):
                driver.live_termination(changed, receipt, now,
                                        procedure=self.procedure())
        for changed in ({**allocation, "pod_id": "old-field"},
                        {**allocation, "project": "another-project"}):
            with self.assertRaises(driver.ReviewRequired):
                driver.live_termination(changed, receipt, now,
                                        procedure=self.procedure())

    def test_gce_wrong_boot_disk_type_refuses(self):
        """Given a different disk type, workload admission rejects it."""
        allocation, receipt = self.live()
        receipt["boot_disk_type"] = "pd-standard"
        now = driver.timestamp(allocation["created_at"], "fixture") + 1
        with self.assertRaisesRegex(driver.ReviewRequired, "boot_disk_type"):
            driver.live_termination(allocation, receipt, now,
                                    procedure=self.procedure())

    def test_gce_wrong_boot_disk_size_refuses(self):
        """Given a different disk size, workload admission rejects it."""
        allocation, receipt = self.live()
        receipt["boot_disk_size_gb"] = 699
        now = driver.timestamp(allocation["created_at"], "fixture") + 1
        with self.assertRaisesRegex(driver.ReviewRequired,
                                    "boot_disk_size_gb"):
            driver.live_termination(allocation, receipt, now,
                                    procedure=self.procedure())

    def test_gce_wrong_source_image_refuses(self):
        """Given another image family, workload admission rejects it."""
        allocation, receipt = self.live()
        receipt["source_image"] = "another-image-family"
        now = driver.timestamp(allocation["created_at"], "fixture") + 1
        with self.assertRaisesRegex(driver.ReviewRequired, "source_image"):
            driver.live_termination(allocation, receipt, now,
                                    procedure=self.procedure())

    def test_gce_termination_719_minutes_31_seconds_refuses(self):
        """Given relative TTL past 719:30, absolute admission rejects."""
        allocation, receipt = self.live()
        receipt["last_start_time"] = "2026-10-26T00:09:30-07:00"
        receipt["termination_time"] = "2026-10-26T11:59:31-07:00"
        now = driver.timestamp("2026-10-26T00:10:00-07:00", "fixture")
        with self.assertRaisesRegex(driver.ReviewRequired, "outside cap"):
            driver.live_termination(allocation, receipt, now,
                                    procedure=self.procedure())

    def test_gce_termination_709_minutes_59_seconds_refuses(self):
        """Given a stop one second before 710:00, admission rejects it."""
        allocation, receipt = self.live()
        receipt["termination_time"] = "2026-10-26T11:49:59-07:00"
        now = driver.timestamp(allocation["created_at"], "fixture") + 1
        with self.assertRaisesRegex(driver.ReviewRequired, "outside cap"):
            driver.live_termination(allocation, receipt, now,
                                    procedure=self.procedure())

    def test_gce_exact_termination_boundaries_pass(self):
        """Given 710:00 or 719:30 with valid relative TTL, admission passes."""
        allocation, original = self.live()
        zero = driver.timestamp(allocation["created_at"], "fixture")
        for start, stop in (("00:00:00", "11:50:00"),
                            ("00:09:30", "11:59:30")):
            receipt = {**original,
                       "last_start_time": f"2026-10-26T{start}-07:00",
                       "termination_time": f"2026-10-26T{stop}-07:00"}
            if stop == "11:59:30":
                receipt["source_image"] = (
                    "https://compute.googleapis.com/projects/image-project/"
                    "global/images/"
                    "common-cu129-ubuntu-2204-nvidia-580-v20261019")
            with self.subTest(stop=stop):
                self.assertEqual(driver.live_termination(
                    allocation, receipt, zero + 600,
                    procedure=self.procedure()), (zero, zero + 710 * 60))

    def test_gce_late_start_cannot_hide_inside_ttl_tolerance(self):
        """Given a late start, the 60-second TTL tolerance cannot admit it."""
        allocation, receipt = self.live()
        receipt["last_start_time"] = "2026-10-26T00:10:00-07:00"
        receipt["termination_time"] = "2026-10-26T11:59:30-07:00"
        now = driver.timestamp("2026-10-26T00:10:01-07:00", "fixture")
        with self.assertRaisesRegex(driver.ReviewRequired, "outside cap"):
            driver.live_termination(allocation, receipt, now,
                                    procedure=self.procedure())


class ReviewFixTests(unittest.TestCase):
    bindings = DeadlineContractTests.bindings
    procedure = DeadlineContractTests.procedure
    live = DeadlineContractTests.live
    def session_fixture(self, root, mode):
        (root / "aux").mkdir()
        (root / "python-runtime.json").write_text("{}")
        artifact = root / "result.txt"
        artifact.write_text("tiny")
        blocks = []
        for name in ("smoke", "calibration"):
            job = self.bindings(root)[name][0]
            job["receipt_path"] = str(root / "results" / name / "receipt.json")
            record = {k: job[k] for k in
                      ("source_pin", "config_pin", "input_pin")}
            selected = mode if name == "smoke" else "completed"
            record.update(state="partial" if selected == "partial"
                          else "completed",
                          clean_handoff=selected != "unclean",
                          exit_code=1 if selected == "partial" else 0,
                          reason_code="worker", reason="fixture partial",
                          files={"result.txt": driver.digest(artifact)})
            program = (
                "import json,pathlib,sys; p=pathlib.Path(sys.argv[1]); "
                "p.parent.mkdir(parents=True); "
                "mode=sys.argv[2]; record=json.loads(sys.argv[3]); "
                "p.write_text('{' if mode=='malformed' else "
                "json.dumps(record)) if mode!='missing' else None; "
                "sys.exit(2 if mode=='exit2' else record['exit_code'])"
            )
            job["argv"] = [sys.executable, "-c", program,
                           job["receipt_path"], selected, json.dumps(record)]
            blocks.append({"id": name, "jobs": [job]})
        owner = object.__new__(driver.Supervisor)
        owner.root, owner.environment = root, dict(os.environ)
        owner.active, owner.allocation, owner.termination = [], {}, {}
        owner.zero, owner.backstop_deadline = time.monotonic(), float("inf")
        owner.calls = []

        def begin(block, outcomes):
            owner.block_id = block["id"]
            owner.block_deadline = time.monotonic() + 30
            owner.launch_deadline = owner.block_deadline - 11
            owner.job_minutes = {j["id"]: 1 for j in block["jobs"]}

        def start(args, directory, environment, cwd):
            owner.calls.append(directory.name)
            process = driver.subprocess.Popen(
                args, env=environment, cwd=cwd, start_new_session=True)
            def stop():
                if process.poll() is None:
                    process.kill()
                process.wait(timeout=3)

            self.addCleanup(stop)
            process.wait(timeout=3)
            owned = driver.OwnedProcess(
                process, directory, time.monotonic(), StringIO(),
                deadline=owner.launch_deadline)
            owner.active.append(owned)
            return owned

        owner.begin_block, owner.start = begin, start
        return owner, {"blocks": blocks}

    @unittest.skipUnless(sys.platform == "linux", "Linux process fixture")
    def test_block_local_receipt_failures_release_the_next_block(self):
        for mode in ("missing", "exit2", "malformed", "partial"):
            with self.subTest(mode=mode), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder)
                owner, admission = self.session_fixture(root, mode)
                self.addCleanup(owner.stop_all)
                with patch.object(driver.signal, "signal"), \
                        patch.object(driver.signal, "setitimer"):
                    result = driver.execute_session(
                        owner, admission, root / "inputs", {}, 1)
                blocks = result["blocks"]
                self.assertEqual(blocks["smoke"]["state"], "partial")
                self.assertTrue(blocks["smoke"]["clean_handoff"])
                self.assertTrue(blocks["smoke"]["reason"])
                self.assertEqual(blocks["calibration"]["state"], "completed")
                self.assertEqual(owner.calls,
                                 ["smoke-fixture", "calibration-fixture"])
                self.assertEqual(owner.active, [])

    @unittest.skipUnless(sys.platform == "linux", "Linux process fixture")
    def test_unclean_receipt_and_survivor_stop_the_whole_session(self):
        for mode in ("unclean", "survivor"):
            with self.subTest(mode=mode), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder)
                owner, admission = self.session_fixture(root, mode)
                original = owner.close

                def close(owned):
                    if mode == "survivor":
                        groups = {
                            owned.process.pid: {(owned.process.pid, 1): 0}}
                        with patch.object(driver, "process_groups",
                                          return_value=groups):
                            original(owned)
                    else:
                        original(owned)

                owner.close = close
                try:
                    with patch.object(driver.signal, "signal"), \
                            patch.object(driver.signal, "setitimer"), \
                            self.assertRaises(driver.FatalSession):
                        driver.execute_session(
                            owner, admission, root / "inputs", {}, 1)
                    self.assertEqual(owner.calls, ["smoke-fixture"])
                    saved = driver.read_json(root / "session.json")
                    self.assertNotIn("calibration", saved["blocks"])
                finally:
                    owner.stop_all()

    def test_oracle_success_releases_passed_timing_after_screen_timeout(self):
        for oracle_passed in (True, False):
            with self.subTest(oracle=oracle_passed), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder)
                admission = driver.preflight(
                    self.bindings(root), driver.setup_recipe(),
                    self.procedure(), root)
                owner = SimpleNamespace(
                    root=root, allocation={}, termination={}, zero=0,
                    backstop_deadline=710 * 60, stop_all=unittest.mock.Mock())

                def begin(block, outcomes):
                    owner.block_deadline = driver.block_limit(
                        block["id"], 0, 0, 720 * 60, outcomes, a3_live=False)

                owner.begin_block = begin
                timed = []

                def stage(owner, selected, setup, concurrency, receipts):
                    if selected[0].stage == "timing":
                        timed.extend(selected)
                        return
                    if selected[0].stage == "oracle" and oracle_passed:
                        receipts.append({"identity": {"job": driver.asdict(
                            selected[0])}, "state": "pass"})
                        return
                    if oracle_passed:
                        receipts.append({"identity": {"job": driver.asdict(
                            selected[0])}, "state": "pass"})
                    raise driver.DeadlineExceeded("fixture job timeout")

                with (
                    patch.object(driver.time, "monotonic", return_value=0),
                    patch.object(driver.signal, "signal"),
                    patch.object(driver.signal, "setitimer"),
                    patch.object(driver, "prepare", return_value={}),
                    patch.object(driver, "execute_stage", side_effect=stage),
                    patch.object(driver, "run_catalog_hybrid") as dependent,
                    patch.object(driver, "binding_block",
                                 return_value="completed"),
                    patch.object(driver, "block_files", return_value={}),
                ):
                    result = driver.execute_session(
                        owner, admission, root / "inputs", {}, 1)
                self.assertEqual(result["blocks"]["storage"]["state"],
                                 "partial")
                self.assertEqual(len(timed), 6 if oracle_passed else 0)
                self.assertEqual(dependent.call_count,
                                 2 if oracle_passed else 0)
                self.assertEqual(result["blocks"]["segment_timing"]["state"],
                                 "completed" if oracle_passed
                                 else "dependency_blocked")

    def test_priority_repeat_rounds_and_remaining_window_limits(self):
        planned = driver.jobs()
        self.assertEqual([j.adapter for j in planned[:7]], [
            "oracle", "sqlite-segment", "pebble-bleve", "sqlite-vecflat",
            "sqlite-usearch", "catalog-sqlite", "catalog-refs"])
        timing = [j for j in planned if j.stage == "timing"]
        self.assertEqual([j.repeat for j in timing], [1]*4 + [2]*4 + [3]*4)
        inventory = driver.bound_jobs(Path("/work"))
        for block, minutes in (("storage", 150), ("segment_timing", 120)):
            owner = object.__new__(driver.Supervisor)
            owner.block_id, owner.block_deadline = block, minutes * 60
            owner.job_minutes = {
                j["id"]: j["minutes"] for j in inventory[block]}
            for now in (0, 60, minutes * 60 - 20):
                self.assertEqual(owner.job_limit(
                    inventory[block][0]["id"], now),
                                 minutes * 60 - driver.CLEANUP_RESERVE_SECONDS)

    def test_staging_hashes_and_missing_existing_or_symlink_rejection(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            for name in ("evaluator", "cpu-cost"):
                source = root / "inputs" / name
                source.mkdir(parents=True)
                (source / "nested").mkdir()
                (source / "nested/input.txt").write_text(name)
                destination = root / "data" / name
                hashes = driver.copy_tree(root, source, destination)
                self.assertEqual(hashes, {
                    f"data/{name}/nested/input.txt": driver.digest(
                        source / "nested/input.txt")})
                with self.assertRaises(driver.ReviewRequired):
                    driver.copy_tree(root, source, destination)
                (source / "link").symlink_to(source / "nested/input.txt")
                target = root / "data" / (name + "-refused")
                with self.assertRaises(driver.ReviewRequired):
                    driver.copy_tree(root, source, target)
                self.assertFalse(target.exists())
            with self.assertRaisesRegex(driver.ReviewRequired, "missing"):
                driver.copy_tree(root, root / "missing", root / "new")

    def test_timestamp_normalization_and_full_enforcement_invocation(self):
        base = driver.timestamp("2026-10-26T07:00:00Z", "fixture")
        for digits in ("1", "12", "123", "1234", "12345", "123456",
                       "1234567", "1234567890123456789"):
            with self.subTest(digits=digits):
                value = "2026-10-26T07:00:00." + digits
                expected = base + int(digits[:6].ljust(6, "0")) / 1e6
                self.assertAlmostEqual(
                    driver.timestamp(value + "Z", "fixture"), expected,
                    delta=1e-6)
                self.assertEqual(driver.timestamp(value + "+00:00", "fixture"),
                                 driver.timestamp(value + "Z", "fixture"))
        for value in (None, "badZ", "2026-10-26T07:00:00"):
            with self.assertRaises(driver.ReviewRequired):
                driver.timestamp(value, "fixture")
        allocation, receipt = self.live()
        allocation["created_at"] = "2026-10-26T07:00:00.123456789Z"
        receipt["creation_time"] = allocation["created_at"]
        created = driver.timestamp(allocation["created_at"], "fixture")
        with self.assertRaisesRegex(driver.ReviewRequired, "whole seconds"):
            driver.live_termination(allocation, receipt, created + 2,
                                    procedure=self.procedure())
        result = driver.preflight(self.bindings(), driver.setup_recipe(),
                                  self.procedure(), Path("/work"),
                                  concurrency=3)
        expected = ["python3", "/work/control/run.py", "--execute", "--root",
                    "/work", "--inputs", "/work/inputs", "--concurrency", "3",
                    "--bindings", "/work/control/bindings.json",
                    "--setup-recipe",
                    "/work/control/setup-recipe.json", "--stop-procedure",
                    "/work/control/stop-procedure.json", "--allocation",
                    "/work/control/allocation.json", "--termination-receipt",
                    "/work/control/termination.json"]
        self.assertEqual(result["enforcement_argv"], expected)

    def test_reap_unknown_zombie_preserves_direct_child_status(self):
        owner = object.__new__(driver.Supervisor)
        process = SimpleNamespace(pid=42, returncode=None,
                                  poll=unittest.mock.Mock())
        owner.active = [SimpleNamespace(process=process)]
        with patch.object(driver.os, "waitpid", side_effect=[
                (99, 0), (42, 7 << 8), ChildProcessError()]) as wait:
            owner.reap()
        self.assertEqual(process.returncode, 7)
        self.assertTrue(all(call.args == (-1, os.WNOHANG)
                            for call in wait.call_args_list))

    def test_partial_storage_resume_refuses_missing_receipts_cleanly(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            owner = SimpleNamespace(root=root, allocation={}, termination={},
                                    zero=0, backstop_deadline=1000)
            block = {"id": "storage", "jobs": [{"id": "missing"}]}
            admission = {"blocks": [block]}
            identity = {"configuration": admission, "setup": {},
                        "allocation": {}, "termination": {}}
            driver.atomic_json(root / "session.json", {
                "identity": identity, "blocks": {"storage": {
                    "state": "partial", "files": {}}}})
            with patch.object(driver.time, "monotonic", return_value=0), \
                    patch.object(driver.signal, "setitimer"), \
                    self.assertRaisesRegex(driver.ReviewRequired,
                                           "partial receipts; no resume"):
                driver.execute_session(
                    owner, admission, root / "inputs", {}, 1)


    def main_fixture(self, root):
        allocation, receipt = self.live()
        payloads = {"bindings": self.bindings(root),
                    "setup-recipe": driver.setup_recipe(),
                    "stop-procedure": self.procedure(),
                    "allocation": allocation, "termination-receipt": receipt}
        argv = ["run.py", "--execute"]
        for option, payload in payloads.items():
            path = root / (option + ".json")
            driver.atomic_json(path, payload)
            argv.extend(["--" + option, str(path)])
        return argv

    def test_root_user_is_recorded_not_refused(self):
        # Lead decision R2: the pod's SSH user is not under our control, and a
        # refusal would lose the whole paid session; host.json records euid.
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)

            def local_path(value):
                return root if str(value) == "/work" else Path(value)

            with (
                patch.object(sys, "argv", self.main_fixture(root)),
                patch.object(driver, "Path", side_effect=local_path),
                patch.object(driver.os, "geteuid", return_value=0),
                patch.object(driver, "live_termination", return_value=(0, 1)),
                patch.object(driver, "host_record", side_effect=(
                    driver.ReviewRequired("host fixture reached"))),
                patch.object(driver, "Supervisor"),
                patch.object(driver.signal, "signal"),
                patch.object(driver.signal, "setitimer"),
            ):
                with self.assertRaisesRegex(driver.ReviewRequired,
                                            "host fixture reached"):
                    driver.main()

    def test_uncaught_error_cannot_claim_a_clean_a9_handoff(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            argv = self.main_fixture(root)
            owner = SimpleNamespace(stop_all=unittest.mock.Mock())

            def local_path(value):
                return root if str(value) == "/work" else Path(value)

            with (
                patch.object(sys, "argv", argv),
                patch.object(driver, "Path", side_effect=local_path),
                patch.object(driver.os, "geteuid", return_value=1000),
                patch.object(driver, "host_record", return_value={
                    "kernel": "fixture", "cpuinfo": "", "affinity": []}),
                patch.object(driver, "live_termination", return_value=(0, 1)),
                patch.object(driver, "Supervisor", return_value=owner),
                patch.object(driver.signal, "signal"),
                patch.object(driver.signal, "setitimer"),
                patch.object(driver, "execute_session",
                             side_effect=ValueError("fixture uncaught")),
                self.assertRaisesRegex(ValueError, "fixture uncaught"),
            ):
                driver.main()
            result = driver.read_json(root / "A9-handoff.json")
            self.assertFalse(result["clean_handoff"])
            self.assertTrue(result["deletion_required"])
            owner.stop_all.assert_called_once()


if __name__ == "__main__":
    unittest.main()
