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

    def bindings(self, root=Path("/work")):
        return {block: [{
            "id": block + "-fixture", "argv": ["/usr/bin/true"],
            "source_pin": "1" * 40, "config_pin": "2" * 64,
            "input_pin": "3" * 64,
            "receipt_path": str(root / "control" / (block + ".json")),
            "minutes": 1, "owner": "fixture-only",
        }] for block in driver.INPUT_BLOCKS}


    def test_complete_no_pod_preflight_has_no_effects(self):
        bindings = self.bindings()
        before = deepcopy(bindings)
        with (
            patch.object(driver.subprocess, "Popen",
                         side_effect=AssertionError),
            patch.object(driver.urllib.request, "urlopen",
                         side_effect=AssertionError),
            patch.object(Path, "mkdir", side_effect=AssertionError),
        ):
            result = driver.preflight(
                bindings, Path("/work"),
            )
        self.assertEqual(result["phase1"], "prepared-no-pod")
        self.assertEqual(bindings, before)
        self.assertEqual(result["enforcement_entrypoint"],
                         "study/storage/pod/run.py:main")


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
                patch.object(driver, "process_groups",
                             side_effect=AssertionError),
                patch.object(Path, "mkdir", side_effect=AssertionError),
            ):
                self.assertEqual(driver.main(), 0)
            result = json.loads(output.getvalue())
            self.assertFalse(root.exists())
            self.assertFalse(result["creation_admitted"])
            self.assertEqual(result["external_commands_executed"], 0)
            self.assertEqual(result["mandatory_owner_bindings"],
                             sorted(driver.INPUT_BLOCKS))

    def test_removed_cloud_cli_refuses_before_effects(self):
        cases = [
            ["--execute"], ["--preflight"],
            ["--dry-run", "--allocation", "missing.json"],
            ["--dry-run", "--termination-receipt", "missing.json"],
            ["--dry-run", "--setup-recipe", "missing.json"],
            ["--dry-run", "--stop-procedure", "missing.json"],
        ]
        for options in cases:
            with (
                self.subTest(options=options),
                patch.object(sys, "argv", ["run.py", *options]),
                patch.object(sys, "stderr", StringIO()),
                patch.object(driver.subprocess, "Popen",
                             side_effect=AssertionError),
                patch.object(driver.urllib.request, "urlopen",
                             side_effect=AssertionError),
                patch.object(driver, "process_groups",
                             side_effect=AssertionError),
                patch.object(Path, "mkdir", side_effect=AssertionError),
                self.assertRaises(SystemExit) as rejected,
            ):
                driver.main()
            self.assertEqual(rejected.exception.code, 2)

    def test_each_missing_binding_or_field_names_the_limit(self):
        for block in sorted(driver.INPUT_BLOCKS):
            bindings = self.bindings()
            del bindings[block]
            with self.subTest(block=block), self.assertRaisesRegex(
                    driver.ReviewRequired, block):
                driver.preflight(bindings, Path("/work"))
            for field in ("argv", "source_pin", "config_pin", "input_pin",
                          "receipt_path", "minutes", "owner"):
                bindings = self.bindings()
                del bindings[block][0][field]
                with self.subTest(block=block, field=field), \
                        self.assertRaisesRegex(driver.ReviewRequired,
                                               block + ": missing " + field):
                    driver.preflight(bindings, Path("/work"))

    def test_malformed_bindings_and_impossible_schedule_refuse(self):
        mutations = [
            ("minutes", float("nan")), ("minutes", True),
            ("argv", "not-an-argv"),
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
                driver.preflight(bindings, Path("/work"))
        bindings = self.bindings()
        bindings["surprise"] = []
        with self.assertRaisesRegex(driver.ReviewRequired, "unlisted"):
            driver.preflight(bindings, Path("/work"))


    def test_early_start_does_not_borrow_and_clock_never_resets(self):
        end = 180 * 60
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
            admission = {"blocks": [
                {"id": block, "jobs": entries}
                for block, entries in driver.bound_jobs(root).items()
            ]}
            supervisor = SimpleNamespace(
                root=root, stop_all=unittest.mock.Mock(),
            )

            def begin(block, outcomes):
                supervisor.block_deadline = 1000
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


    @unittest.skipUnless(
        sys.platform == "linux", "Linux /proc and subreaper contract")
    def test_nonreturner_whole_group_is_terminated_and_reaped(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / "setup").mkdir()
            now_wall = 0
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
                    root, dict(os.environ),
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


class ReviewFixTests(unittest.TestCase):
    bindings = DeadlineContractTests.bindings
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
        owner.active = []
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
                admission = {"blocks": [
                    {"id": block, "jobs": entries}
                    for block, entries in driver.bound_jobs(root).items()
                ]}
                owner = SimpleNamespace(
                    root=root, stop_all=unittest.mock.Mock())

                def begin(block, outcomes):
                    owner.block_deadline = 1000

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
            owner = SimpleNamespace(root=root)
            block = {"id": "storage", "jobs": [{"id": "missing"}]}
            admission = {"blocks": [block]}
            identity = {"configuration": admission, "setup": {}}
            driver.atomic_json(root / "session.json", {
                "identity": identity, "blocks": {"storage": {
                    "state": "partial", "files": {}}}})
            with patch.object(driver.time, "monotonic", return_value=0), \
                    patch.object(driver.signal, "setitimer"), \
                    self.assertRaisesRegex(driver.ReviewRequired,
                                           "partial receipts; no resume"):
                driver.execute_session(
                    owner, admission, root / "inputs", {}, 1)


if __name__ == "__main__":
    unittest.main()
