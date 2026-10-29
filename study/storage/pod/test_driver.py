"""Tiny controller fixtures. No study workload or model download."""
import ast
from copy import deepcopy
from io import StringIO
import json
import os
import signal
import sys
import time
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch

import run as driver


def fixture_process(pid, code=None):
    process = SimpleNamespace(pid=pid, returncode=code)
    process.poll = unittest.mock.Mock(side_effect=lambda: process.returncode)
    process.wait = unittest.mock.Mock(side_effect=lambda timeout=0:
                                     process.returncode)
    return process


def fixture_owner(root):
    owner = object.__new__(driver.Supervisor)
    owner.root, owner.environment = root, {}
    owner.active, owner.serial = [], 0
    owner.block_id = "fixture"
    owner.block_deadline = owner.launch_deadline = 10000
    owner.job_minutes, owner.job_bindings = {}, {}
    owner.case, owner.current_rss, owner.swapouts = {}, 0, None
    owner.native_identity = {"input_manifest_sha256": "1" * 64,
                             "daemon_off_identities_sha256": "2" * 64}
    owner.sample = {"level": 30, "available_bytes": 64 * driver.GIB,
                    "swapouts": 7, "load1": 1.0, "ac_power": True}
    return owner


class FixtureSupervisor:
    def __init__(self, root, modes, interrupt_at=None):
        self.root = root
        self.environment = {}
        self.active = []
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
        (directory / "native-time.txt").write_text(
            "real 1.00\nuser 1.00\nsys 0.00\n"
            "1024 maximum resident set size\n")
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
        supervisor.job_bindings = {job.name: {"gomaxprocs": 10}
                                   for job in selected}
        with patch.object(driver, "process_groups", return_value={}), \
                patch.object(driver, "native_resources"), \
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
        supervisor.job_bindings = {"first": {}, "second": {}}
        supervisor.active = []
        first = supervisor.job_limit("first", 30 * 60)
        self.assertEqual(first, 42.5 * 60 - driver.CLEANUP_RESERVE_SECONDS)
        for now in (end - 20, end):
            with self.assertRaisesRegex(driver.DeadlineExceeded, "too short"):
                supervisor.job_limit("second", now)
        self.assertEqual(supervisor.launch_deadline, first)
        self.assertEqual(supervisor.block_deadline, end)

    def test_session_timing_keeps_the_completed_correctness_receipts(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            admission = {"blocks": [
                {"id": block, "jobs": entries}
                for block, entries in driver.bound_jobs(root).items()
            ]}
            supervisor = SimpleNamespace(
                root=root, stop_all=unittest.mock.Mock(), native_identity={},
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


    def test_nonreturner_whole_group_is_terminated_and_reaped(self):
        # Native backend is a fixture; no Linux subreaper or real daemon.
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / "setup").mkdir()
            owner = fixture_owner(root)
            owner.block_deadline, owner.launch_deadline = 50, 3
            owner.guard = unittest.mock.Mock()
            process = fixture_process(4100)
            members = {(4100, 1000001): 1024, (4101, 1000002): 2048}
            live = {4100: dict(members)}
            clock = [0]

            def monotonic():
                clock[0] += 1
                return clock[0]

            def start(args, directory, env, cwd):
                owned = driver.OwnedProcess(
                    process, directory, 0, StringIO(), known=set(members),
                    deadline=3)
                owner.active.append(owned)
                driver.atomic_json(directory / "process.json", {"pid": 4100})
                (directory / "stdout-stderr.log").write_text("fixture")
                return owned

            def kill(pgid, sig):
                self.assertEqual(pgid, 4100)
                if sig == signal.SIGKILL:
                    live.clear()
                    process.returncode = -int(sig)

            owner.start = start
            with (patch.object(driver, "process_groups",
                               side_effect=lambda env, owned: deepcopy(live)),
                  patch.object(driver.os, "killpg",
                               side_effect=kill) as signals,
                  patch.object(driver.os, "waitpid",
                               side_effect=ChildProcessError),
                  patch.object(driver.time, "monotonic",
                               side_effect=monotonic),
                  patch.object(driver.time, "sleep"),
                  patch.object(driver.signal, "setitimer")):
                with self.assertRaises(driver.DeadlineExceeded):
                    owner.run(["/fixture/nonreturner"])
            self.assertEqual([c.args[1] for c in signals.call_args_list],
                             [signal.SIGTERM, signal.SIGKILL])
            self.assertEqual(owner.active, [])
            self.assertGreaterEqual(owner.guard.call_count, 1)
            directory = root / "setup/command-001"
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
            job.update(minutes=(driver.CLEANUP_RESERVE_SECONDS + 3) / 60,
                       kind=driver.BLOCK_KINDS[name],
                       estimated_peak_bytes=driver.GIB, gomaxprocs=10)
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
        owner = fixture_owner(root)
        owner.environment = dict(os.environ)
        owner.guard = unittest.mock.Mock()
        owner.calls = []
        groups_patch = patch.object(driver, "process_groups", return_value={})
        groups_patch.start()
        self.addCleanup(groups_patch.stop)

        def begin(block, outcomes):
            owner.block_id = block["id"]
            owner.block_deadline = time.monotonic() + 30
            owner.launch_deadline = owner.block_deadline - 11
            owner.job_minutes = {j["id"]: j["minutes"] for j in block["jobs"]}
            owner.job_bindings = {j["id"]: dict(j) for j in block["jobs"]}

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
                    root=root, stop_all=unittest.mock.Mock(),
                    native_identity={})

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
        for block, minutes in (("storage", 30), ("segment_timing", 45)):
            owner = fixture_owner(Path("/fixture"))
            owner.block_id, owner.block_deadline = block, 120 * 60
            owner.job_minutes = {
                j["id"]: j["minutes"] for j in inventory[block]}
            owner.job_bindings = {j["id"]: {} for j in inventory[block]}
            self.assertTrue(all(j["minutes"] == minutes
                                for j in inventory[block]))
            for now in (0, 60):
                self.assertEqual(owner.job_limit(
                    inventory[block][0]["id"], now),
                    now + minutes * 60 - driver.CLEANUP_RESERVE_SECONDS)
            with self.assertRaises(driver.DeadlineExceeded):
                owner.job_limit(inventory[block][0]["id"], 120 * 60 - 20)

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
            owner = fixture_owner(root)
            block = {"id": "storage", "jobs": [{"id": "missing"}]}
            admission = {"blocks": [block]}
            identity = {"configuration": admission, "setup": {},
                        "native": owner.native_identity}
            driver.atomic_json(root / "session.json", {
                "identity": identity, "blocks": {"storage": {
                    "state": "partial", "files": {}}}})
            with patch.object(driver.time, "monotonic", return_value=0), \
                    patch.object(driver.signal, "signal"), \
                    patch.object(driver.signal, "setitimer"), \
                    self.assertRaisesRegex(driver.ReviewRequired,
                                           "partial receipts; no resume"):
                driver.execute_session(
                    owner, admission, root / "inputs", {}, 1)



class NativeAdmissionTests(unittest.TestCase):
    def go(self):
        return {"schema": "mac-window-go/v1", "window_id": "fixture-window",
                "input_manifest_sha256": "1" * 64,
                "start": "1970-01-01T00:00:00+00:00",
                "end": "1970-01-01T04:00:00+00:00",
                **{key: True for key in (
                    "user_go", "migration_complete", "owner_stop_confirmed",
                    "awake", "thermal_safe", "background_quiet")}}

    def identities(self, root):
        return {"schema": "installed-daemon-identities/v2",
                "launchd": {"domain": "gui/9501", "label": "test.fixture",
                            "service_target": "gui/9501/test.fixture"},
                "plist_path": str(root / "test.fixture.plist"),
                "executable": {"path": str(root / "installed-fixture"),
                               "is_symlink": False,
                               "argv": [str(root / "installed-fixture"),
                                        "daemon", "start"]},
                "stop_marker_path": str(root / "intent-only"),
                "socket_path": str(root / "installed-fixture.sock"),
                "tcp_listeners": [{"host": "127.0.0.1", "port": 59101},
                                  {"host": "127.0.0.1", "port": 59102}],
                "notes": None}

    def test_window_current_permission_and_cleanup_reserve(self):
        go = self.go()
        before = deepcopy(go)
        self.assertEqual(driver.window_limit(go, "1" * 64, 1000), 12500)
        self.assertEqual(go, before)
        for now in (-1, 13500, 14400):
            with self.subTest(now=now), self.assertRaises(
                    driver.DeadlineExceeded):
                driver.window_limit(go, "1" * 64, now)
        for key in ("user_go", "migration_complete", "owner_stop_confirmed",
                    "awake", "thermal_safe", "background_quiet"):
            with self.subTest(key=key), self.assertRaises(driver.FatalSession):
                driver.window_limit({**go, key: False}, "1" * 64, 1000)
        for changed in ({**go, "schema": "unknown"},
                        {**go, "input_manifest_sha256": "9" * 64},
                        {**go, "window_id": False}, {"schema": go["schema"]}):
            with self.assertRaises(driver.FatalSession):
                driver.window_limit(changed, "1" * 64, 1000)

    def test_launcher_requires_exact_explicit_cpython_pin(self):
        path = Path("/fixture/pinned-python3.14")
        expected = {"path": str(path), "version": [3, 14, 7],
                    "sha256": "2" * 64}
        driver.launcher_limit(expected, path, (3, 14, 7), "2" * 64)
        for executable, version, sha in (
                (path, (3, 9, 6), "2" * 64),
                (path, (3, 12, 15), "2" * 64),
                (Path("/fixture/unknown-python"), (3, 14, 7), "2" * 64),
                (path, (3, 14, 7), "3" * 64)):
            with self.subTest(version=version), self.assertRaises(
                    driver.FatalSession):
                driver.launcher_limit(expected, executable, version, sha)

    def test_vm_stat_parser_uses_native_pages_and_swap_counter(self):
        raw = ("Mach Virtual Memory Statistics: (page size of 16384 bytes)\n"
               "Pages free: 10.\nPages inactive: 20.\n"
               "Pages speculative: 30.\nPages purgeable: 20.\nSwapouts: 7.\n")
        self.assertEqual(driver.parse_vm_stat(raw),
                         {"available_bytes": 60 * 16384, "swapouts": 7})
        for bad in (raw + "Swapouts: 7.\n", raw.replace("Swapouts: 7.\n", ""),
                    raw.replace("16384 bytes", "unknown bytes")):
            with self.assertRaises(driver.FatalSession):
                driver.parse_vm_stat(bad)

    def test_d11_boundaries_and_no_correctness_load_ceiling(self):
        peak = 3 * driver.GIB
        sample = {"level": 30, "swapouts": 7, "load1": 2.0,
                  "available_bytes": peak + driver.MEMORY_RESERVE,
                  "ac_power": True}
        driver.resource_limit(sample, peak, 7, timing=True, admission=True)
        driver.resource_limit({**sample, "load1": 10.0}, peak, 7,
                              timing=False, admission=True)
        with self.assertRaises(driver.DeadlineExceeded):
            driver.resource_limit({**sample, "load1": 2.01}, peak, 7,
                                  timing=True, admission=True)
        for key, value in (("level", 29), ("swapouts", 8),
                           ("available_bytes", sample["available_bytes"] - 1),
                           ("ac_power", False), ("load1", float("nan"))):
            with self.subTest(key=key), self.assertRaises(driver.FatalSession):
                driver.resource_limit({**sample, key: value}, peak, 7,
                                      timing=False, admission=True)
        driver.resource_limit({**sample, "level": 25}, 0, 7,
                              timing=False, admission=False)
        with self.assertRaises(driver.FatalSession):
            driver.resource_limit({**sample, "level": 24}, 0, 7,
                                  timing=False, admission=False)

    def test_all_d12_signals_aliases_and_unknown_probe_errors(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            config = self.identities(root)
            marker = Path(config["stop_marker_path"])
            marker.touch()
            binary = Path(config["executable"]["path"])
            binary.touch()
            alias = root / "alias-to-installed"
            alias.symlink_to(binary)
            state = ["off"]

            def probe(args, **kwargs):
                self.assertNotIn("shell", kwargs)
                self.assertEqual(kwargs["timeout"], 5)
                output, error, code = "", "", 0
                if args[0] == "/bin/launchctl":
                    self.assertEqual(args, ["/bin/launchctl", "print",
                                            "gui/9501/test.fixture"])
                    code = 0 if state[0] == "job" else 113
                    error = ('Could not find service "test.fixture" '
                             'in domain for user gui: 9501')
                    if state[0] == "unknown":
                        error = "permission denied"
                elif args[0] == "/bin/ps":
                    if state[0] == "argv":
                        output = " ".join(config["executable"]["argv"]) + "\n"
                elif "-d" in args:
                    if state[0] == "alias":
                        output = f"p7201\nftxt\nn{alias}\n"
                elif "-U" in args:
                    if state[0] == "socket":
                        output = "p7202\nf3\nn" + config["socket_path"] + "\n"
                elif state[0] == "tcp1" and "-iTCP@127.0.0.1:59101" in args:
                    output = "p7203\n"
                elif state[0] == "tcp2" and "-iTCP@127.0.0.1:59102" in args:
                    output = "p7204\n"
                if args[0] == "/usr/sbin/lsof" and not output:
                    code = 1
                return SimpleNamespace(returncode=code, stdout=output,
                                       stderr=error)

            with patch.object(driver.subprocess, "run", side_effect=probe):
                driver.daemon_off(config, {})
                for unsafe in ("argv", "alias", "job", "unknown", "socket",
                               "tcp1", "tcp2"):
                    state[0] = unsafe
                    with self.subTest(signal=unsafe), self.assertRaises(
                            driver.FatalSession):
                        driver.daemon_off(config, {})
                state[0] = "off"
                marker.unlink()
                with self.assertRaises(driver.FatalSession):
                    driver.daemon_off(config, {})

    def test_native_probe_partial_results_never_prove_absence(self):
        for code, output, error in ((1, "p1\n", ""), (0, "", "warning"),
                                    (2, "", "")):
            result = SimpleNamespace(returncode=code, stdout=output,
                                     stderr=error)
            with patch.object(driver.subprocess, "run", return_value=result), \
                    self.assertRaises(driver.FatalSession):
                driver.native_output(["/fixture/lsof"], {}, absent_ok=True)
        with patch.object(driver.subprocess, "run", return_value=(
                SimpleNamespace(returncode=1, stdout="", stderr=""))):
            self.assertEqual(driver.native_output(
                ["/fixture/lsof"], {}, absent_ok=True), "")

    def test_darwin_birth_rss_abi_gone_and_unknown_identity(self):
        def usage(pid, flavor, pointer):
            self.assertEqual(flavor, 0)
            if pid in (22, 23):
                driver.ctypes.set_errno(driver.errno.ESRCH if pid == 22 else
                                        driver.errno.EPERM)
                return -1
            info = driver.ctypes.cast(
                pointer, driver.ctypes.POINTER(driver.DarwinUsage)).contents
            info.proc_start_abstime, info.resident_size = 123456789, 8192
            return 0

        driver.libproc.cache_clear()
        self.addCleanup(driver.libproc.cache_clear)
        owned = [SimpleNamespace(process=fixture_process(pid))
                 for pid in (21, 23)]
        library = SimpleNamespace(proc_pid_rusage=unittest.mock.Mock(
            side_effect=usage))
        with (patch.object(driver.sys, "platform", "darwin"),
              patch.object(driver.ctypes, "CDLL",
                           return_value=library) as load_library,
              patch.object(driver, "native_output",
                           return_value="21 21\n22 21\n23 23\n24 24\n")):
            groups = driver.process_groups({}, owned)
        load_library.assert_called_once()
        self.assertEqual([call.args[0] for call in
                          library.proc_pid_rusage.call_args_list],
                         [21, 22, 23])
        self.assertNotIn(24, groups)
        self.assertEqual(driver.ctypes.sizeof(driver.DarwinUsage), 96)
        self.assertEqual(groups[21], {(21, 123456789): 8192})
        self.assertIsNone(groups[23])
        with patch.object(driver.sys, "platform", "linux"), \
                patch.object(driver, "native_output") as probe, \
                self.assertRaises(driver.FatalSession):
            driver.process_groups({}, [])
        probe.assert_not_called()

    def test_live_observations_bound_d12_and_keep_cheap_checks(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            owner = fixture_owner(root)
            owner.go_file, owner.off_file = root / "go.json", root / "off.json"
            owner.go_file.write_text(json.dumps(self.go()))
            owner.off = self.identities(root)
            owner.off_file.write_text(json.dumps(owner.off))
            owner.go_sha = driver.digest(owner.go_file)
            owner.native_identity["daemon_off_identities_sha256"] = (
                driver.digest(owner.off_file))
            owner.case = {"estimated_peak_bytes": driver.GIB}
            clock = [0]
            log = StringIO()
            self.addCleanup(log.close)
            owner.active = [driver.OwnedProcess(
                fixture_process(8600), root, 0, log, known={(8600, 10)})]
            groups = {8600: {(8600, 10): 1024}}
            with (patch.object(driver.time, "monotonic",
                               side_effect=lambda: clock[0]),
                  patch.object(driver.time, "time",
                               side_effect=lambda: 1000 + clock[0]),
                  patch.object(driver, "daemon_off") as daemon_checks,
                  patch.object(driver, "native_resources",
                               return_value=owner.sample) as resources,
                  patch.object(driver, "digest",
                               wraps=driver.digest) as hashes):
                owner.permissions()
                for step in range(1, 13):
                    clock[0] = 5 * step
                    owner.observe(groups)
                self.assertEqual(daemon_checks.call_count, 2)
                self.assertEqual(resources.call_count, 12)
                for path in (owner.go_file, owner.off_file):
                    self.assertEqual(sum(call.args[0] == path for call in
                                         hashes.call_args_list), 13)
                clock[0] = 65
                for path in (owner.go_file, owner.off_file):
                    original = path.read_text()
                    path.write_text(original + "\n")
                    with self.assertRaisesRegex(driver.FatalSession,
                                                "identities changed"):
                        owner.permissions(full=False)
                    path.write_text(original)
                self.assertEqual(daemon_checks.call_count, 2)
                owner.guard(admission=True)
                self.assertEqual(daemon_checks.call_count, 3)
                self.assertEqual(resources.call_count, 13)
            owner.active.clear()

    def test_unknown_group_does_not_block_other_proved_cleanup(self):
        with tempfile.TemporaryDirectory() as folder:
            owner = fixture_owner(Path(folder))
            unknown = fixture_process(8100, 0)
            proved = fixture_process(8200)
            owner.active = [driver.OwnedProcess(
                p, owner.root, 0, StringIO(), known={(p.pid, 10)})
                for p in (unknown, proved)]
            logs = [p.log for p in owner.active]
            live = {8100: {(8100, 99): 1}, 8200: {(8200, 10): 1},
                    8300: {(8300, 80): 1}}
            clock = [0]

            def monotonic():
                clock[0] += 1
                return clock[0]

            def kill(pgid, sig):
                self.assertEqual(pgid, 8200)
                live.pop(pgid)
                proved.returncode = -int(sig)

            with (patch.object(driver, "process_groups",
                               side_effect=lambda env, owned: deepcopy(live)),
                  patch.object(driver.os, "killpg",
                               side_effect=kill) as signals,
                  patch.object(driver.os, "waitpid",
                               side_effect=ChildProcessError),
                  patch.object(driver.time, "monotonic",
                               side_effect=monotonic),
                  patch.object(driver.time, "sleep"),
                  self.assertRaises(driver.FatalSession)):
                owner.stop_all()
            self.assertEqual([c.args[0] for c in signals.call_args_list],
                             [8200])
            self.assertEqual([p.process.pid for p in owner.active], [8100])
            self.assertIn(8300, live)
            self.assertTrue(all(log.closed for log in logs))

    def test_live_guard_cleans_after_revocation_or_resource_loss(self):
        for failure in ("revoked", "expired", "memory", "swap"):
            with self.subTest(failure=failure), \
                    tempfile.TemporaryDirectory() as folder:
                owner = fixture_owner(Path(folder))
                process = fixture_process(8400)
                owner.active = [driver.OwnedProcess(
                    process, owner.root, 0, StringIO(), known={(8400, 10)})]
                owner.case = {"estimated_peak_bytes": driver.GIB}
                owner.swapouts = 7
                owner.permissions = unittest.mock.Mock(return_value=1000)
                sample = dict(owner.sample)
                if failure == "revoked":
                    owner.permissions.side_effect = driver.FatalSession(
                        "current user go revoked")
                elif failure == "expired":
                    owner.permissions.side_effect = driver.DeadlineExceeded(
                        "current user window expired")
                elif failure == "memory":
                    sample["level"] = 24
                else:
                    sample["swapouts"] = 8
                live = {8400: {(8400, 10): 1}}
                error_type = (driver.DeadlineExceeded if failure == "expired"
                              else driver.FatalSession)

                def kill(pgid, sig):
                    self.assertEqual(pgid, 8400)
                    live.clear()
                    process.returncode = -int(sig)

                with (patch.object(driver, "native_resources",
                                   return_value=sample),
                      patch.object(
                          driver, "process_groups",
                          side_effect=lambda env, owned: deepcopy(live)),
                      patch.object(driver.os, "killpg", side_effect=kill),
                      patch.object(driver.os, "waitpid",
                                   side_effect=ChildProcessError),
                      self.assertRaises(error_type)):
                    owner.guard()
                self.assertEqual(owner.active, [])

    def test_bsd_time_units_and_malformed_receipt_refusal(self):
        raw = ("real 5.01\nuser 1.25\nsys 0.50\n"
               " 16384 maximum resident set size\n 10 page faults\n")
        result = driver.bsd_time(raw, 1)
        self.assertEqual(result["elapsed_seconds"], 5.01)
        self.assertEqual(result["user_seconds"], 1.25)
        self.assertEqual(result["system_seconds"], 0.5)
        self.assertEqual(result["max_rss_bytes"], 16384)
        self.assertEqual(result["exit_code"], 1)
        for bad in (raw + "real 9.00\n", raw.replace("sys 0.50\n", ""),
                    raw.replace("16384", "unknown")):
            with self.assertRaises(driver.FatalSession):
                driver.bsd_time(bad, 0)

    def test_high_load_cpu_block_is_partial_not_skipped(self):
        with tempfile.TemporaryDirectory() as folder:
            owner = fixture_owner(Path(folder))
            owner.start = unittest.mock.Mock()

            def begin(block, outcomes):
                driver.resource_limit({**owner.sample, "load1": 2.01}, 0, 7,
                                      timing=True, admission=True)

            owner.begin_block = begin
            with patch.object(driver.signal, "signal"), \
                    patch.object(driver.signal, "setitimer"):
                result = driver.execute_session(
                    owner, {"blocks": [{"id": "cpu_cost", "jobs": []}]},
                    owner.root / "inputs", {}, 1)
            self.assertEqual(result["blocks"]["cpu_cost"]["state"], "partial")
            owner.start.assert_not_called()

    def test_constructor_wiring_binds_config_hash_and_detects_drift(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            identities_file = root / "identities.json"
            go_file = root / "go.json"
            identities = self.identities(root)
            driver.atomic_json(identities_file, identities)
            driver.atomic_json(go_file, self.go())
            config = {
                "window_file": str(go_file),
                "identities_file": str(identities_file),
                "identities_sha256": driver.digest(identities_file),
                "input_manifest_sha256": "1" * 64,
                "d11_limits": {"admission_level": 30, "stop_level": 25,
                               "memory_reserve_bytes": driver.MEMORY_RESERVE,
                               "timing_load1_max": 2.0,
                               "swap_observation_seconds": 30},
                "launcher": {"path": str(Path(sys.executable).resolve()),
                             "version": [3, 14, 7],
                             "sha256": driver.digest(Path(sys.executable))}}
            before = deepcopy(config)
            # This fixture checks wiring, not native interpreter/platform ABI.
            with (patch.object(driver, "launcher_limit") as launcher,
                  patch.object(driver.sys, "platform", "darwin"),
                  patch.object(driver.platform, "machine",
                               return_value="arm64"),
                  patch.object(driver, "daemon_off") as off,
                  patch.object(driver.time, "time", return_value=1000),
                  patch.object(driver.signal, "signal"),
                  patch.object(driver, "native_output", return_value=(
                      f"Apple M1 Max\n{64 * driver.GIB}\n8\n2\n10\n"))):
                owner = driver.Supervisor(root, {}, config)
                self.assertEqual(owner.native_identity[
                    "daemon_off_identities_sha256"],
                    config["identities_sha256"])
                self.assertEqual(config, before)
                launcher.assert_called_once_with(
                    config["launcher"], Path(sys.executable),
                    sys.version_info[:3], driver.digest(Path(sys.executable)))
                off.assert_called_once_with(identities, {})
                identities_file.write_text(identities_file.read_text() + " ")
                with self.assertRaises(driver.FatalSession):
                    owner.permissions()
                self.assertEqual(off.call_count, 1)

    def test_start_observes_30s_and_rechecks_go_before_spawn(self):
        for revoke in (False, True):
            with self.subTest(revoke=revoke), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder)
                directory = root / "case"
                directory.mkdir()
                owner = fixture_owner(root)
                owner.go_file = root / "go.json"
                driver.atomic_json(owner.go_file, self.go())
                owner.go_sha = driver.digest(owner.go_file)
                owner.window = {"id": "fixture-window"}
                owner.case = {"gomaxprocs": 10}
                owner.block_id = "catalog"
                clock, checks, masks = [0.0], [], []
                process = fixture_process(8500)
                log = StringIO()
                original_open = Path.open

                def sleep(seconds):
                    clock[0] += seconds

                def guard(*, admission=False):
                    checks.append(admission)
                    if revoke and admission and checks.count(True) == 2:
                        changed = self.go()
                        changed["user_go"] = False
                        owner.go_file.write_text(json.dumps(changed))

                def mask(how, values):
                    masks.append(how)
                    return set()

                def birth(pid):
                    self.assertEqual(masks[-1], signal.SIG_BLOCK)
                    return SimpleNamespace(proc_start_abstime=123456789)

                def open_file(path, mode="r", *args, **kwargs):
                    if path.name == "stdout-stderr.log":
                        return log
                    return original_open(path, mode, *args, **kwargs)

                owner.guard = guard
                with (patch.object(driver.time, "monotonic",
                                   side_effect=lambda: clock[0]),
                      patch.object(driver.time, "sleep", side_effect=sleep),
                      patch.object(driver.time, "time", return_value=1000),
                      patch.object(driver.signal, "pthread_sigmask",
                                   side_effect=mask),
                      patch.object(driver.signal, "setitimer"),
                      patch.object(driver, "darwin_usage", side_effect=birth),
                      patch.object(driver, "process_groups", return_value={
                          8500: {(8500, 123456789): 8192}}),
                      patch.object(Path, "open", new=open_file),
                      patch.object(driver.subprocess, "Popen",
                                   return_value=process) as spawn):
                    if revoke:
                        with self.assertRaises(driver.FatalSession):
                            owner.start(["/fixture/private-binary"], directory,
                                        {"GOMAXPROCS": "10"}, root)
                        spawn.assert_not_called()
                        self.assertTrue(log.closed)
                    else:
                        owned = owner.start(
                            ["/fixture/private-binary"], directory,
                            {"GOMAXPROCS": "10"}, root)
                        self.assertEqual(owned.known, {(8500, 123456789)})
                        self.assertTrue(spawn.call_args.kwargs[
                            "start_new_session"])
                        receipt = driver.read_json(directory / "process.json")
                        self.assertEqual(receipt["native"][
                            "daemon_off_identities_sha256"], "2" * 64)
                        self.assertEqual(receipt["identity_kind"],
                                         "darwin-rusage-v0-start-abstime")
                        owned.log.close()
                        owner.active.clear()
                self.assertEqual(clock[0], 30)
                self.assertEqual(checks[:8], [True] + [False] * 6 + [True])
                self.assertEqual(masks[-1], signal.SIG_SETMASK)

    def test_per_case_caps_do_not_become_a_storage_group_timeout(self):
        owner = fixture_owner(Path("/fixture"))
        owner.permissions = unittest.mock.Mock(return_value=12500)
        selected = driver.jobs()[:2]
        records = [{"id": job.name, "kind": job.stage, "minutes": 999,
                    "estimated_peak_bytes": driver.GIB, "gomaxprocs": 10}
                   for job in selected]
        before = deepcopy(records)
        with patch.object(driver.time, "monotonic", return_value=0):
            owner.begin_block({"id": "storage", "jobs": records}, {})
        self.assertEqual(owner.block_deadline, 12500)
        self.assertEqual(owner.job_minutes, {job.name: 30 for job in selected})
        self.assertEqual(owner.job_limit(selected[0].name, 0), 1789)
        self.assertEqual(owner.job_limit(selected[1].name, 3600), 5389)
        self.assertEqual(records, before)
        with self.assertRaises(driver.DeadlineExceeded):
            owner.job_limit(selected[1].name, 12480)
        for key, value in (("kind", "smoke"), ("estimated_peak_bytes", True),
                           ("gomaxprocs", 0), ("minutes", float("nan"))):
            bad = [{**records[0], key: value}]
            with patch.object(driver.time, "monotonic", return_value=0), \
                    self.assertRaises(driver.FatalSession):
                owner.begin_block({"id": "storage", "jobs": bad}, {})

    def test_completed_case_rss_does_not_lower_next_admission_estimate(self):
        owner = fixture_owner(Path("/fixture"))
        owner.permissions = unittest.mock.Mock(return_value=1000)
        owner.case = {"estimated_peak_bytes": 3 * driver.GIB}
        owner.current_rss = 10 * driver.GIB  # Previous case is inactive.
        sample = {**owner.sample, "available_bytes":
                  3 * driver.GIB + driver.MEMORY_RESERVE - 1}
        with patch.object(driver, "native_resources", return_value=sample), \
                self.assertRaises(driver.FatalSession):
            owner.guard(admission=True)
        self.assertEqual(owner.active, [])


if __name__ == "__main__":
    unittest.main()
