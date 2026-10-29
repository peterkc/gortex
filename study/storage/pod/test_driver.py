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


def fixture_manifest(root, paths=(), jobs=None, homebrew=None):
    path = root / "control" / "fixture-inputs.json"
    path.parent.mkdir(exist_ok=True)
    files = {str(p.resolve()): driver.digest(p) for p in paths}
    driver.atomic_json(path, {
        "schema": "storage-inputs/v1", "source": driver.COMMIT,
        "files": files,
        "jobs": jobs if jobs is not None else {"*": list(files)},
        "homebrew": homebrew if homebrew is not None else {}})
    return path


def fixture_resources(**changes):
    return {"timestamp": 1000.0, "level": 30, "pressure_state": 1,
            "available_bytes": 64 * driver.GIB, "swapouts": 7,
            "page_size": 16384, "swap_usage": "total = 1G used = 100M\n",
            "load1": 1.0, "load5": 1.0, "load15": 1.0,
            "ac_power": True, **changes}


def fixture_budget_stop(directory):
    peak = 8 * driver.GIB + 1
    driver.atomic_json(directory / "budget-exceeded.json", {
        "state": "partial", "observed_peak_bytes": peak,
        "budget_bytes": 8 * driver.GIB})
    (directory / "rss.jsonl").write_text(json.dumps({
        **fixture_resources(), "elapsed_seconds": 1.25,
        "group_rss_bytes": peak}) + "\n")
    return driver.BudgetExceeded(peak, 8 * driver.GIB)


def fixture_owner(root):
    owner = object.__new__(driver.Supervisor)
    owner.root, owner.environment, owner.scratch = root, {}, root
    owner.manifest_file = fixture_manifest(root)
    owner.bad_jobs, owner.window = set(), {"id": "fixture-window"}
    owner.active, owner.serial = [], 0
    owner.block_id = "fixture"
    owner.block_deadline = owner.launch_deadline = 10000
    owner.job_minutes, owner.job_bindings = {}, {}
    owner.case, owner.current_rss, owner.swapouts = {}, 0, None
    owner.native_identity = {
        "input_manifest_sha256": driver.digest(owner.manifest_file),
        "daemon_off_identities_sha256": "2" * 64}
    owner.sample = fixture_resources()
    return owner


class FixtureSupervisor:
    def __init__(self, root, modes, interrupt_at=None):
        self.root = root
        self.environment = {}
        self.active, self.bad_jobs = [], set()
        self.scratch, self.window = root, {"id": "fixture-window"}
        self.manifest_file = root / "control/fixture-inputs.json"
        if not self.manifest_file.exists():
            self.manifest_file = fixture_manifest(root)
        self.native_identity = {
            "input_manifest_sha256": driver.digest(self.manifest_file),
            "daemon_off_identities_sha256": "2" * 64}
        self.block_deadline = 10000
        self.modes = modes
        self.interrupt_at = interrupt_at
        self.calls = []

    def begin_block(self, block, outcomes):
        self.job_bindings = {job["id"]: dict(job) for job in block["jobs"]}

    def stop_all(self):
        pass

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
        (directory / "rss.jsonl").write_text(
            json.dumps(fixture_resources()) + "\n")
        (directory / "stdout-stderr.log").write_text("fixture")
        process = SimpleNamespace(pid=100+len(self.calls),
                                  poll=lambda: code, returncode=code)
        return SimpleNamespace(process=process, directory=directory,
                               started=0, peak_rss=0, deadline=10000)


class DriverTests(unittest.TestCase):
    def fixture(self, root):
        (root / "jobs").mkdir()
        data = root / "data/tier10"
        data.mkdir(parents=True)
        driver.atomic_json(data / "manifest.json", {"repos": ["fixture"]})
        driver.atomic_json(data / "queries.json", [{"id": "q", "op": "id"}])
        selected = [driver.Job("screen", name, "tier10")
                    for name in ("sqlite-segment", "pebble-bleve")]
        input_manifest = fixture_manifest(root, [data / "manifest.json",
                                                 data / "queries.json"])
        setup = {"identity": {"driver": "fixture", "runtime": {},
                              "input_manifest_sha256": driver.digest(
                                  input_manifest)},
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

    def test_interruption_reuses_complete_and_restarts_partial(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            selected, setup, before = self.fixture(root)
            supervisor = FixtureSupervisor(root, ["pass"], interrupt_at=2)
            receipts = []
            with self.assertRaisesRegex(driver.ReviewRequired, "interruption"):
                self.execute(supervisor, selected, setup, receipts)
            self.assertEqual(len(receipts), 1)
            partial = root / "jobs" / selected[1].name
            old_attempt = driver.read_json(partial / "attempt.json")
            (partial / "old-samples.json").write_text("[1,2,3]")
            resumed = FixtureSupervisor(root, ["pass"])
            resumed.window = {"id": "next-window"}
            saved = []
            self.execute(resumed, selected, setup, saved)
            self.assertEqual(resumed.calls, [selected[1].name])
            self.assertEqual(len(saved), 2)
            archive = list((root / "attempts/jobs" /
                            selected[1].name).iterdir())
            self.assertEqual(len(archive), 1)
            self.assertEqual((archive[0] / "old-samples.json").read_text(),
                             "[1,2,3]")
            self.assertEqual(driver.read_json(archive[0] / "attempt.json"),
                             old_attempt)
            self.assertFalse((partial / "old-samples.json").exists())
            self.assertNotEqual(saved[1]["attempt"]["attempt_id"],
                                old_attempt["attempt_id"])
            self.assertEqual([r["attempt"]["window_id"] for r in saved],
                             ["fixture-window", "next-window"])
            self.assertIn("native-time.txt", saved[1]["files"])
            self.assertEqual(before, {p.name: driver.digest(p)
                                     for p in (root / "data/tier10")
                                     .iterdir()})

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
            self.assertEqual(len(result["jobs"]), 25)
            self.assertEqual(sum(j["stage"] == "oracle"
                                 for j in result["jobs"]), 2)
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
        supervisor.job_bindings = {name: {"kind": "screen"}
                                   for name in ("first", "second")}
        supervisor.active = []
        supervisor.bad_jobs = set()  # Inputs verified for this deadline test.
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
                root=root, stop_all=unittest.mock.Mock(),
                native_identity={}, scratch=root,
                manifest_file=fixture_manifest(root),
            )

            def begin(block, outcomes):
                supervisor.block_deadline = 1000
            supervisor.begin_block = begin
            seen = []

            def stage(owner, selected, setup, concurrency, receipts):
                seen.append(receipts)
                if selected[0].stage == "timing":
                    self.assertEqual(len(receipts), 13)
                    self.assertEqual(sum(
                        row["identity"]["job"]["stage"] == "oracle"
                        for row in receipts), 2)
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
            self.assertEqual(len(seen[2]), 25)

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
                "p.parent.mkdir(parents=True, exist_ok=True); "
                "pathlib.Path('result.txt').write_text('tiny'); "
                "mode=sys.argv[2]; record=json.loads(sys.argv[3]); "
                "p.write_text('{' if mode=='malformed' else "
                "json.dumps(record)) if mode!='missing' else None; "
                "sys.exit(2 if mode=='exit2' else record['exit_code'])"
            )
            job["argv"] = [sys.executable, "-c", program,
                           job["receipt_path"], selected, json.dumps(record)]
            blocks.append({"id": name, "jobs": [job]})
        owner = fixture_owner(root)
        owner.environment = driver.environment(root)
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
                    root=root, stop_all=unittest.mock.Mock(), scratch=root,
                    native_identity={}, manifest_file=fixture_manifest(root))

                def begin(block, outcomes):
                    owner.block_deadline = 1000

                owner.begin_block = begin
                timed = []

                def stage(owner, selected, setup, concurrency, receipts):
                    if selected[0].stage == "timing":
                        timed.extend(selected)
                        return
                    if selected[0].stage == "oracle" and oracle_passed:
                        receipts.extend({"identity": {"job": driver.asdict(j)},
                                         "state": "pass"} for j in selected)
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
        self.assertEqual([j.adapter for j in planned[:8]], [
            "oracle", "oracle", "sqlite-segment", "pebble-bleve",
            "sqlite-vecflat", "sqlite-usearch", "catalog-sqlite",
            "catalog-refs"])
        timing = [j for j in planned if j.stage == "timing"]
        self.assertEqual([j.repeat for j in timing], [1]*4 + [2]*4 + [3]*4)
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name).resolve()
        inventory = driver.bound_jobs(root)
        for block, minutes in (("storage", 30), ("segment_timing", 45)):
            owner = fixture_owner(root)
            owner.block_id, owner.block_deadline = block, 120 * 60
            owner.job_minutes = {
                j["id"]: j["minutes"] for j in inventory[block]}
            kinds = {job.name: job.stage for job in planned}
            owner.job_bindings = {j["id"]: {
                **j, "kind": kinds[j["id"]] if block == "storage" else
                driver.BLOCK_KINDS[block], "estimated_peak_bytes": driver.GIB,
                "gomaxprocs": 10} for j in inventory[block]}
            owner.window["oracle_go"] = list(owner.job_bindings)
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

    def test_partial_storage_resume_preserves_complete_jobs(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            cases = DriverTests()
            selected, setup, before = cases.fixture(root)
            first = FixtureSupervisor(root, ["pass"], interrupt_at=2)
            completed = []
            with self.assertRaises(driver.ReviewRequired):
                cases.execute(first, selected, setup, completed)
            jobs = [{"id": job.name, "gomaxprocs": 10,
                     "receipt_path": str(root / "jobs" / job.name /
                                         "complete.json")} for job in selected]
            blocks = [{"id": "setup", "jobs": []},
                      {"id": "storage", "jobs": jobs}]
            admission = {"blocks": blocks}
            driver.atomic_json(root / "session.json", {
                "identity": {"configuration": admission,
                             "setup": setup["identity"],
                             "native": first.native_identity},
                "blocks": {"setup": {"state": "completed", "files": {}},
                           "storage": {"state": "partial", "files": {}}}})
            resumed = FixtureSupervisor(root, ["pass"])
            resumed.window = {"id": "resumed-window"}
            with (patch.object(driver, "prepare", return_value=setup),
                  patch.object(driver, "jobs", return_value=selected),
                  patch.object(driver, "process_groups", return_value={}),
                  patch.object(driver.time, "monotonic", return_value=0),
                  patch.object(driver.time, "sleep"),
                  patch.object(driver.signal, "signal"),
                  patch.object(driver.signal, "setitimer")):
                result = driver.execute_session(
                    resumed, admission, root / "inputs", setup["identity"], 1)
            self.assertEqual(result["blocks"]["storage"]["state"], "completed")
            self.assertEqual(resumed.calls, [selected[1].name])
            self.assertEqual(before, {p.name: driver.digest(p)
                                     for p in (root / "data/tier10")
                                     .iterdir()})



class NativeAdmissionTests(unittest.TestCase):
    def go(self, manifest_sha="1" * 64):
        return {"schema": "mac-window-go/v1", "window_id": "fixture-window",
                "input_manifest_sha256": manifest_sha,
                "start": "1970-01-01T00:00:00+00:00",
                "end": "1970-01-01T04:00:00+00:00",
                **{key: True for key in (
                    "user_go", "migration_complete", "owner_stop_confirmed",
                    "awake", "thermal_safe")}}

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
        self.assertEqual(driver.window_limit(
            {**go, "background_quiet": False}, "1" * 64, 1000), 12500)
        for now in (-1, 13500, 14400):
            with self.subTest(now=now), self.assertRaises(
                    driver.DeadlineExceeded):
                driver.window_limit(go, "1" * 64, now)
        for key in ("user_go", "migration_complete", "owner_stop_confirmed",
                    "awake", "thermal_safe"):
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
                         {"available_bytes": 60 * 16384, "swapouts": 7,
                          "page_size": 16384})
        for bad in (raw + "Swapouts: 7.\n", raw.replace("Swapouts: 7.\n", ""),
                    raw.replace("16384 bytes", "unknown bytes")):
            with self.assertRaises(driver.FatalSession):
                driver.parse_vm_stat(bad)

    def test_d11_boundaries_allow_load_swap_and_candidate_estimate(self):
        sample = fixture_resources(level=20, pressure_state=2, load1=18.0,
                                   load5=17.0, load15=16.0,
                                   available_bytes=4 * driver.GIB)
        for admission in (True, False):
            driver.resource_limit(sample, 64 * driver.GIB, 7,
                                  admission=admission)
        pages = (512 * 1024 * 1024) // sample["page_size"]
        driver.resource_limit({**sample, "swapouts": 7 + pages}, 0, 7,
                              admission=True)
        invalid = (("level", 19), ("level", 101), ("pressure_state", 4),
                   ("pressure_state", 0), ("pressure_state", "normal"),
                   ("swapouts", 6), ("swapouts", 8 + pages),
                   ("available_bytes", 4 * driver.GIB - 1),
                   ("ac_power", False), ("page_size", 0),
                   ("page_size", True), ("swap_usage", ""),
                   ("load5", float("nan")), ("load15", -1))
        for key, value in invalid:
            with self.subTest(key=key, value=value), self.assertRaises(
                    driver.FatalSession):
                driver.resource_limit({**sample, key: value}, 0, 7,
                                      admission=True)
        for missing in sample:
            incomplete = {k: v for k, v in sample.items() if k != missing}
            with self.subTest(missing=missing), self.assertRaises(
                    driver.FatalSession):
                driver.resource_limit(incomplete, 0, 7,
                                      admission=False)

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
            owner.go_file.write_text(json.dumps(self.go(
                owner.native_identity["input_manifest_sha256"])))
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
                    sample["level"] = 19
                else:
                    sample["swapouts"] = 8 + (
                        driver.SWAP_GROWTH_LIMIT // sample["page_size"])
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

    def test_high_load_cpu_block_is_admitted_not_skipped(self):
        with tempfile.TemporaryDirectory() as folder:
            owner = fixture_owner(Path(folder))

            def begin(block, outcomes):
                driver.resource_limit({**owner.sample, "load1": 18.0}, 0, 7,
                                      admission=True)

            owner.begin_block = begin
            with (patch.object(driver.signal, "signal"),
                  patch.object(driver.signal, "setitimer"),
                  patch.object(driver.time, "monotonic", return_value=0),
                  patch.object(driver, "binding_block",
                               return_value="completed") as work):
                result = driver.execute_session(
                    owner, {"blocks": [{"id": "cpu_cost", "jobs": []}]},
                    owner.root / "inputs", {}, 1)
            self.assertEqual(result["blocks"]["cpu_cost"]["state"],
                             "completed")
            work.assert_called_once()

    def test_constructor_wiring_binds_config_hash_and_detects_drift(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            identities_file = root / "identities.json"
            go_file = root / "go.json"
            identities = self.identities(root)
            driver.atomic_json(identities_file, identities)
            manifest_file = fixture_manifest(root)
            manifest_sha = driver.digest(manifest_file)
            go = {**self.go(manifest_sha), "oracle_go": ["oracle-fixture"]}
            driver.atomic_json(go_file, go)
            config = {
                "scratch_root": str(root),
                "manifest_file": str(manifest_file),
                "window_file": str(go_file),
                "identities_file": str(identities_file),
                "identities_sha256": driver.digest(identities_file),
                "input_manifest_sha256": manifest_sha,
                "d11_limits": {"minimum_level": 20,
                               "available_minimum_bytes": 4 * driver.GIB,
                               "candidate_budget_bytes": 8 * driver.GIB,
                               "oracle_budget_bytes": 24 * driver.GIB,
                               "swap_growth_bytes": 512 * 1024 * 1024},
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
                self.assertEqual(owner.window["oracle_go"], ["oracle-fixture"])
                owner.job_minutes = {"first": 1}
                owner.job_bindings = {"first": {"kind": "screen"}}
                owner.block_deadline = 600
                self.assertEqual(owner.bad_jobs, {"*"})
                with self.assertRaisesRegex(driver.ReviewRequired,
                                            "required input"):
                    owner.job_limit("first", 0)
                self.assertEqual(owner.launch_deadline, 0)
                owner.bad_jobs = driver.bad_jobs(manifest_file, root)
                self.assertEqual(owner.job_limit("first", 0),
                                 60 - driver.CLEANUP_RESERVE_SECONDS)
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
                env = {**driver.environment(root), "GOMAXPROCS": "10"}
                owner.go_file = root / "go.json"
                driver.atomic_json(owner.go_file, self.go(
                    owner.native_identity["input_manifest_sha256"]))
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
                        changed = self.go(
                            owner.native_identity["input_manifest_sha256"])
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
                                        env, root)
                        spawn.assert_not_called()
                        self.assertTrue(log.closed)
                    else:
                        owned = owner.start(
                            ["/fixture/private-binary"], directory,
                            env, root)
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
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        owner = fixture_owner(Path(temporary.name).resolve())
        owner.permissions = unittest.mock.Mock(return_value=12500)
        selected = driver.jobs()[:2]
        owner.window["oracle_go"] = [job.name for job in selected]
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

    def test_completed_case_rss_does_not_lower_oracle_admission_estimate(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        owner = fixture_owner(Path(temporary.name).resolve())
        owner.permissions = unittest.mock.Mock(return_value=1000)
        owner.case = {"kind": "oracle",
                      "estimated_peak_bytes": 3 * driver.GIB}
        owner.current_rss = 10 * driver.GIB  # Previous case is inactive.
        sample = {**owner.sample, "available_bytes":
                  3 * driver.GIB + driver.MEMORY_MINIMUM - 1}
        with patch.object(driver, "native_resources", return_value=sample), \
                self.assertRaises(driver.ReviewRequired):
            owner.guard(admission=True)
        self.assertEqual(owner.active, [])

    def test_native_sampler_records_pressure_loads_and_swapusage(self):
        vm = ("Mach Virtual Memory Statistics: (page size of 16384 bytes)\n"
              "Pages free: 10.\nPages inactive: 20.\n"
              "Pages speculative: 30.\nSwapouts: 7.\n")
        swap = "total = 2048.00M used = 100.25M free = 1947.75M\n"
        outputs = {("/usr/bin/vm_stat",): vm,
            ("/usr/sbin/sysctl", "-n", "kern.memorystatus_level"): "20\n",
            ("/usr/sbin/sysctl", "-n",
             "kern.memorystatus_vm_pressure_level"): "2\n",
            ("/usr/sbin/sysctl", "-n", "vm.swapusage"): swap,
            ("/usr/bin/pmset", "-g", "batt"): "Now drawing from 'AC Power'\n"}
        with (patch.object(driver, "native_output",
                           side_effect=lambda args, env: outputs[tuple(args)]),
              patch.object(driver.os, "getloadavg", return_value=(18, 17, 16)),
              patch.object(driver.time, "time", return_value=1000)):
            sample = driver.native_resources({})
            self.assertEqual(set(sample), set(fixture_resources()))
            self.assertEqual([sample[k] for k in ("load1", "load5", "load15")],
                             [18, 17, 16])
            self.assertEqual((sample["level"], sample["pressure_state"]),
                             (20, 2))
            self.assertEqual(sample["swap_usage"], swap)
            self.assertEqual((sample["timestamp"], sample["page_size"]),
                             (1000, 16384))
            outputs[("/usr/sbin/sysctl", "-n",
                     "kern.memorystatus_vm_pressure_level")] = "unknown"
            with self.assertRaises(driver.FatalSession):
                driver.native_resources({})
        with patch.object(driver, "native_output", side_effect=OSError), \
                self.assertRaises(driver.FatalSession):
            driver.native_resources({})

    def test_guard_retains_cumulative_baseline_and_the_stopping_sample(self):
        with tempfile.TemporaryDirectory() as folder:
            owner = fixture_owner(Path(folder).resolve())
            owner.permissions = unittest.mock.Mock(return_value=1000)
            owner.case = {"kind": "screen",
                          "estimated_peak_bytes": 64 * driver.GIB}
            pages = driver.SWAP_GROWTH_LIMIT // 16384
            values = [7, 7 + pages // 2, 7 + pages, 8 + pages]
            samples = [fixture_resources(swapouts=n, load1=18.0)
                       for n in values]
            with patch.object(driver, "native_resources", side_effect=samples):
                owner.guard(admission=True)
                owner.guard()
                owner.guard()
                with self.assertRaises(driver.FatalSession):
                    owner.guard()
            self.assertEqual(owner.swapouts, 7)
            rows = [json.loads(line) for line in
                    (owner.root / "resources.jsonl").read_text().splitlines()]
            self.assertEqual([r["swapouts"] for r in rows], values)
            self.assertEqual([r["swapouts_start"] for r in rows], [7] * 4)
            self.assertTrue(all(set(fixture_resources()) <= r.keys()
                                for r in rows))
            self.assertTrue(all(r["window_id"] == "fixture-window"
                                for r in rows))

    def test_guard_rejects_counter_decrease_even_above_zero_baseline(self):
        for start in (0, 7):
            with self.subTest(start=start), \
                    tempfile.TemporaryDirectory() as folder:
                owner = fixture_owner(Path(folder).resolve())
                owner.sample = fixture_resources(swapouts=start)
                owner.permissions = unittest.mock.Mock(return_value=1000)
                samples = [fixture_resources(swapouts=n)
                           for n in (start, start + 100, start + 99)]
                with patch.object(driver, "native_resources",
                                  side_effect=samples):
                    owner.guard()
                    owner.guard()
                    with self.assertRaisesRegex(driver.FatalSession, "reset"):
                        owner.guard()
                rows = [json.loads(line) for line in
                        (owner.root / "resources.jsonl").read_text()
                        .splitlines()]
                self.assertEqual(rows[-1]["swapouts"], start + 99)

    def test_owned_aggregate_candidate_and_oracle_budgets_stop_and_reap(self):
        for kind, budget in (("screen", 8 * driver.GIB),
                             ("oracle", 24 * driver.GIB)):
            with self.subTest(kind=kind), \
                    tempfile.TemporaryDirectory() as folder:
                owner = fixture_owner(Path(folder).resolve())
                owner.permissions = unittest.mock.Mock(return_value=1000)
                owner.case = {"kind": kind}
                process = fixture_process(8400)
                owned = driver.OwnedProcess(process, owner.root, 0,
                    StringIO(), known={(8400, 10)})
                owner.active = [owned]
                live = {8400: {(8400, 10): budget // 2,
                               (8401, 11): budget // 2}}

                def kill(pgid, sig):
                    self.assertEqual(pgid, 8400)
                    live.clear()
                    process.returncode = -int(sig)

                with (patch.object(driver, "native_resources",
                          side_effect=lambda env: fixture_resources()),
                      patch.object(driver, "process_groups",
                          side_effect=lambda env, active: deepcopy(live)),
                      patch.object(driver.os, "killpg",
                                   side_effect=kill) as kills,
                      patch.object(driver.os, "waitpid",
                                   side_effect=ChildProcessError)):
                    owner.observe(deepcopy(live))
                    self.assertEqual(owned.peak_rss, budget)
                    self.assertEqual(kills.call_count, 0)
                    live[8400][(8401, 11)] += 1
                    with self.assertRaises(driver.BudgetExceeded) as caught:
                        owner.observe(deepcopy(live))
                self.assertEqual(caught.exception.reason_code,
                                 "budget-exceeded")
                self.assertEqual(caught.exception.observed_peak_bytes,
                                 budget + 1)
                self.assertEqual(owner.active, [])
                process.wait.assert_called_once_with(timeout=0)
                self.assertTrue(owned.log.closed)
                self.assertEqual(owned.peak_rss, budget + 1)
                raw = driver.read_json(owner.root / "budget-exceeded.json")
                self.assertEqual(raw, {"state": "partial",
                                      "budget_bytes": budget,
                                      "observed_peak_bytes": budget + 1})
                rows = [json.loads(line) for line in
                        (owner.root / "rss.jsonl").read_text().splitlines()]
                self.assertEqual(rows[-1]["group_rss_bytes"], budget + 1)
                self.assertTrue(set(fixture_resources()) <= rows[-1].keys())
                self.assertEqual(rows[-1]["rss_scope"],
                                 "PGID RSS sum; shared pages per PID")

    def test_unhandled_budget_preserves_independent_blocks_without_retry(self):
        with tempfile.TemporaryDirectory() as folder:
            owner = fixture_owner(Path(folder).resolve())
            owner.begin_block = unittest.mock.Mock()
            config = {"blocks": [{"id": block, "jobs": []}
                                 for block in ("cpu_cost", "baseline_a7")]}
            peak = 8 * driver.GIB + 1
            with (patch.object(driver.time, "monotonic", return_value=0),
                  patch.object(driver.signal, "signal"),
                  patch.object(driver.signal, "setitimer"),
                  patch.object(driver, "binding_block", side_effect=[
                      driver.BudgetExceeded(peak, 8 * driver.GIB),
                      "completed"])
                  as work):
                result = driver.execute_session(
                    owner, config, owner.root, {}, 1)
            partial = result["blocks"]["cpu_cost"]
            self.assertEqual(partial["state"], "partial")
            self.assertEqual(partial["reason_code"], "budget-exceeded")
            self.assertEqual(partial["observed_peak_bytes"], peak)
            self.assertTrue(partial["clean_handoff"])
            self.assertEqual(result["blocks"]["baseline_a7"]["state"],
                             "completed")
            self.assertEqual(work.call_count, 2)
            with (patch.object(driver.signal, "signal"),
                  patch.object(driver.signal, "setitimer"),
                  patch.object(driver, "binding_block") as later):
                resumed = driver.execute_session(
                    owner, config, owner.root, {}, 1)
            later.assert_not_called()
            self.assertEqual(resumed, result)
            result["blocks"]["cpu_cost"]["terminal"] = "fatal"
            driver.atomic_json(owner.root / "session.json", result)
            with patch.object(driver.signal, "signal"), self.assertRaises(
                    driver.FatalSession):
                driver.execute_session(owner, config, owner.root, {}, 1)

    def test_oracle_jobs_fit_and_permission_belong_to_current_window(self):
        selected = [j for j in driver.jobs() if j.stage == "oracle"]
        self.assertEqual([j.tier for j in selected], ["tier1", "tier10"])
        self.assertEqual(len({j.name for j in selected}), 2)
        sample = fixture_resources(available_bytes=7 * driver.GIB)
        driver.resource_limit(sample, 3 * driver.GIB, 7,
                              admission=True, oracle=True)
        with self.assertRaisesRegex(driver.ReviewRequired, "oracle estimate"):
            driver.resource_limit(
                {**sample, "available_bytes": 7 * driver.GIB - 1},
                3 * driver.GIB, 7, admission=True, oracle=True)
        with tempfile.TemporaryDirectory() as folder:
            owner = fixture_owner(Path(folder).resolve())
            owner.job_minutes = {j.name: 30 for j in selected}
            owner.job_bindings = {j.name: {"kind": "oracle", "oracle_go": True}
                                  for j in selected}
            for go in (None, True, selected[0].name, [selected[1].name]):
                owner.window = {"id": "new", "oracle_go": go}
                with self.subTest(go=go), self.assertRaisesRegex(
                        driver.ReviewRequired, "current-session oracle"):
                    owner.job_limit(selected[0].name, 0)
            owner.window["oracle_go"] = [j.name for j in selected]
            for job in selected:
                owner.swapouts, owner.current_rss = 99, 8 * driver.GIB + 1
                owner.job_limit(job.name, 0)
                self.assertIsNone(owner.swapouts)
                self.assertEqual(owner.current_rss, 0)
            owner.window = {"id": "newer", "oracle_go": []}
            with self.assertRaises(driver.ReviewRequired):
                owner.job_limit(selected[0].name, 0)

    def test_verified_complete_oracle_reuses_without_new_measurement_go(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            _, setup, _ = DriverTests().fixture(root)
            job = driver.Job("oracle", "oracle", "tier10")
            setup["artifacts"]["bin/oracle"] = "fixture"
            owner = FixtureSupervisor(root, ["pass"])
            owner.window["oracle_go"] = [job.name]
            owner.job_minutes = {job.name: 30}
            owner.job_bindings = {job.name: {"kind": "oracle",
                                            "gomaxprocs": 10}}
            owner.job_limit = unittest.mock.Mock(side_effect=lambda name, now:
                driver.Supervisor.job_limit(owner, name, now))
            original = owner.start

            def start(args, directory, env, cwd):
                fake = driver.result_path(root, driver.Job(
                    "screen", "oracle", "tier10"))
                fake.parent.parent.mkdir(exist_ok=True)
                owned = original(args, directory, env, cwd)
                target = driver.result_path(root, job)
                target.parent.mkdir()
                fake.rename(target)
                return owned

            owner.start = unittest.mock.Mock(side_effect=start)
            with (patch.object(driver, "process_groups", return_value={}),
                  patch.object(driver.time, "monotonic", return_value=0),
                  patch.object(driver.time, "sleep")):
                receipts = []
                driver.execute_stage(owner, [job], setup, 1, receipts)
                self.assertEqual(owner.start.call_count, 1)
                complete = root / "jobs" / job.name / "complete.json"
                before = complete.read_bytes()
                owner.window = {"id": "new-window", "oracle_go": []}
                owner.job_limit.reset_mock()
                reused = []
                driver.execute_stage(owner, [job], setup, 1, reused)
                self.assertEqual(reused, receipts)
                self.assertEqual(complete.read_bytes(), before)
                owner.job_limit.assert_not_called()
                self.assertEqual(owner.start.call_count, 1)
                (complete.parent / "native-time.txt").write_text("changed")
                with self.assertRaisesRegex(driver.ReviewRequired,
                                            "current-session oracle"):
                    driver.execute_stage(owner, [job], setup, 1, [])
                self.assertEqual(owner.start.call_count, 1)


    def test_budgeted_sdk_job_continues_and_both_receipts_reuse(self):
        for point in ("start", "observe"):
            with self.subTest(point=point), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder).resolve()
                selected, setup, _ = DriverTests().fixture(root)
                owner = FixtureSupervisor(root, ["pass", "pass"])
                owner.stop_all = unittest.mock.Mock()
                owner.arm_guard = unittest.mock.Mock()
                original = owner.start
                armed = []

                def start(args, directory, env, cwd):
                    owned = original(args, directory, env, cwd)
                    self.assertEqual(owned.directory, directory)
                    if directory.name == selected[0].name:
                        error = fixture_budget_stop(directory)
                        if point == "start":
                            raise error
                        armed.append(error)
                    return owned

                def observe(groups):
                    if armed:
                        raise armed.pop()

                owner.start = unittest.mock.Mock(side_effect=start)
                owner.observe = observe
                config = {"blocks": [
                    {"id": "setup", "jobs": []},
                    {"id": "storage", "jobs": [
                        {"id": j.name, "kind": j.stage, "gomaxprocs": 10,
                         "minutes": 30} for j in selected]}]}
                with (patch.object(driver, "prepare", return_value=setup),
                      patch.object(driver, "jobs", return_value=selected),
                      patch.object(driver, "process_groups", return_value={}),
                      patch.object(driver.time, "monotonic", return_value=0),
                      patch.object(driver.time, "sleep"),
                      patch.object(driver.signal, "signal"),
                      patch.object(driver.signal, "setitimer")):
                    record = driver.execute_session(
                        owner, config, root, setup["identity"], 1)
                    self.assertEqual(owner.calls, [j.name for j in selected],
                                     record["blocks"]["storage"])
                    complete = [root / "jobs" / j.name / "complete.json"
                                for j in selected]
                    rows = [driver.read_json(path) for path in complete]
                    self.assertEqual([r["state"] for r in rows],
                                     ["budget_exceeded", "pass"])
                    self.assertEqual(rows[0]["observed_peak_bytes"],
                                     8 * driver.GIB + 1)
                    self.assertEqual(rows[0]["reason_code"], "budget-exceeded")
                    self.assertTrue(rows[0]["files"])
                    before = [path.read_bytes() for path in complete]
                    self.assertEqual(record["blocks"]["storage"]["state"],
                                     "completed")
                    # Verify per-job receipts on resume.
                    record["blocks"]["storage"]["state"] = "partial"
                    driver.atomic_json(root / "session.json", record)
                    owner.start.reset_mock()
                    resumed = driver.execute_session(
                        owner, config, root, setup["identity"], 1)
                    owner.start.assert_not_called()
                    self.assertEqual(resumed["blocks"]["storage"]["state"],
                                     "completed")
                    self.assertEqual([p.read_bytes() for p in complete],
                                     before)
                    raw = complete[0].parent / "rss.jsonl"
                    original_raw = raw.read_bytes()
                    raw.write_text("changed")
                    with self.assertRaises(driver.ReviewRequired):
                        driver.execute_stage(owner, selected[:1], setup, 1, [])
                    owner.start.assert_not_called()
                    raw.write_bytes(original_raw)
                    # A budget marker prevents a blind retry.
                    complete[0].unlink()
                    with self.assertRaises(OSError):
                        driver.execute_stage(owner, selected[:1], setup, 1, [])
                    owner.start.assert_not_called()

    def test_auxiliary_budget_continues_binding_jobs_and_reuses_cache(self):
        for point in ("start", "observe"):
            with self.subTest(point=point), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder).resolve()
                self.assertFalse((root / "aux").exists())
                driver.atomic_json(root / "python-runtime.json", {})
                owner = FixtureSupervisor(root, [])
                owner.stop_all = unittest.mock.Mock()
                owner.arm_guard = unittest.mock.Mock()
                armed = []
                jobs = [{"id": name, "kind": "smoke", "gomaxprocs": 10,
                         "argv": ["fixture"], "receipt_path": "unused",
                         "source_pin": "1" * 40, "config_pin": "2" * 64,
                         "input_pin": "3" * 64}
                        for name in ("first", "second")]
                owner.job_bindings = {j["id"]: j for j in jobs}

                def start(args, directory, env, cwd):
                    owner.calls.append(directory.name)
                    (directory / "stdout-stderr.log").write_text("fixture")
                    if directory.name == "first":
                        error = fixture_budget_stop(directory)
                        if point == "start":
                            raise error
                        armed.append(error)
                    else:
                        data = directory / "data.json"
                        driver.atomic_json(data, {"ok": True})
                        driver.atomic_json(directory / "receipt.json", {
                            "state": "completed", "exit_code": 0,
                            "clean_handoff": True, "source_pin": "1" * 40,
                            "config_pin": "2" * 64, "input_pin": "3" * 64,
                            "files": {"data.json": driver.digest(data)}})
                    return SimpleNamespace(process=fixture_process(
                        9000 + len(owner.calls),
                        None if directory.name == "first" else 0),
                        started=0, peak_rss=1, deadline=10000)

                def observe(groups):
                    if armed:
                        raise armed.pop()

                owner.start = unittest.mock.Mock(side_effect=start)
                owner.observe = observe
                block = {"id": "smoke", "jobs": jobs}
                with (patch.object(driver, "process_groups", return_value={}),
                      patch.object(driver.time, "monotonic", return_value=0),
                      patch.object(driver.time, "sleep")):
                    self.assertEqual(driver.binding_block(owner, block),
                                     "partial")
                    self.assertEqual(owner.calls, ["first", "second"])
                    first = root / "aux/first/complete.json"
                    receipt = driver.read_json(first)
                    self.assertEqual(receipt["state"], "budget_exceeded")
                    self.assertEqual(receipt["observed_peak_bytes"],
                                     8 * driver.GIB + 1)
                    self.assertTrue(all(name.startswith("aux/first/")
                                        for name in receipt["files"]))
                    self.assertFalse(
                        (root / "aux/first/receipt.json").exists())
                    before = first.read_bytes()
                    owner.start.reset_mock()
                    self.assertEqual(driver.binding_block(owner, block),
                                     "partial")
                    owner.start.assert_not_called()
                    self.assertEqual(first.read_bytes(), before)
                    raw = root / "aux/first/rss.jsonl"
                    raw.write_text("changed")
                    with self.assertRaises(driver.ReviewRequired):
                        driver.binding_block(owner, block)
                    owner.start.assert_not_called()

    def test_host_fatal_is_not_absorbed_by_per_job_budget_handlers(self):
        for executor in ("sdk", "auxiliary"):
            with self.subTest(executor=executor), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder).resolve()
                selected, setup, _ = DriverTests().fixture(root)
                driver.atomic_json(root / "python-runtime.json", {})
                owner = FixtureSupervisor(root, [])
                owner.start = unittest.mock.Mock(
                    side_effect=driver.FatalSession("critical host pressure"))
                owner.job_bindings = {j.name: {"gomaxprocs": 10}
                                      for j in selected}
                with (patch.object(driver, "process_groups", return_value={}),
                      patch.object(driver.time, "monotonic", return_value=0),
                      self.assertRaises(driver.FatalSession)):
                    if executor == "sdk":
                        driver.execute_stage(owner, selected, setup, 1, [])
                    else:
                        driver.auxiliary(owner, selected[0].name,
                                         ["fixture"], lambda _: [])
                owner.start.assert_called_once()
                self.assertFalse(list(root.glob("jobs/*/complete.json")))
                self.assertFalse(list(root.glob("aux/*/complete.json")))

    def test_unexpected_error_is_terminal_and_cannot_hide_later_work(self):
        for writable in (True, False):
            with self.subTest(writable=writable), \
                    tempfile.TemporaryDirectory() as folder:
                owner = fixture_owner(Path(folder).resolve())
                owner.begin_block = unittest.mock.Mock()
                owner.stop_all = unittest.mock.Mock()
                config = {"blocks": [{"id": block, "jobs": []}
                                     for block in ("cpu_cost", "baseline_a7")]}
                original = driver.atomic_json

                def save(path, value):
                    if (not writable and value.get("blocks", {}).get(
                            "cpu_cost", {}).get("terminal") == "fatal"):
                        raise OSError("synthetic receipt write failure")
                    return original(path, value)

                expected = AttributeError if writable else OSError
                with (patch.object(driver.signal, "signal"),
                      patch.object(driver.signal, "setitimer"),
                      patch.object(driver.time, "monotonic", return_value=0),
                      patch.object(driver, "atomic_json", side_effect=save),
                      patch.object(driver, "binding_block", side_effect=(
                          AttributeError("synthetic missing directory")))
                      as work,
                      self.assertRaises(expected)):
                    driver.execute_session(owner, config, owner.root, {}, 1)
                owner.stop_all.assert_called_once()
                work.assert_called_once()
                saved = driver.read_json(owner.root / "session.json")
                self.assertNotIn("baseline_a7", saved["blocks"])
                if writable:
                    failed = saved["blocks"]["cpu_cost"]
                    self.assertEqual(failed["terminal"], "fatal")
                    self.assertNotEqual(failed["state"], "partial")
                    self.assertEqual(failed["reason_code"], "AttributeError")
                    self.assertIn("missing directory", failed["reason"])
                    owner.begin_block.reset_mock()
                    with (patch.object(driver.signal, "signal"),
                          self.assertRaises(driver.FatalSession)):
                        driver.execute_session(
                            owner, config, owner.root, {}, 1)
                    owner.begin_block.assert_not_called()


class DurabilityTests(unittest.TestCase):
    def sdk_session(self, root):
        cases = DriverTests()
        selected, setup, _ = cases.fixture(root)
        owner = FixtureSupervisor(root, ["pass", "pass"])
        receipts = []
        cases.execute(owner, selected, setup, receipts)
        blocks = [{"id": "setup", "jobs": []}, {"id": "storage", "jobs": [
            {"id": job.name, "gomaxprocs": 10,
             "receipt_path": str(root / "jobs" / job.name / "complete.json")}
            for job in selected]}]
        admission = {"blocks": blocks}
        driver.atomic_json(root / "session.json", {
            "identity": {"configuration": admission,
                         "setup": setup["identity"],
                         "native": owner.native_identity},
            "blocks": {"setup": {"state": "completed", "files": {}},
                       "storage": {"state": "completed", "files":
                                   driver.block_files(root, blocks[1])}}})
        return selected, setup, admission, receipts

    def resume_sdk(self, owner, selected, setup, admission):
        with (patch.object(driver, "prepare", return_value=setup),
              patch.object(driver, "jobs", return_value=selected),
              patch.object(driver, "process_groups", return_value={}),
              patch.object(driver.time, "monotonic", return_value=0),
              patch.object(driver.time, "sleep"),
              patch.object(driver.signal, "signal"),
              patch.object(driver.signal, "setitimer")):
            return driver.execute_session(
                owner, admission, owner.root / "inputs", setup["identity"], 1)

    def test_start_and_resume_hash_every_input_and_scope_missing_files(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner = fixture_owner(root)
            common, optional = root / "source", root / "model"
            common.write_text("source")
            optional.write_text("model")
            owner.manifest_file = fixture_manifest(root, [common, optional], {
                "*": [str(common)], "cpu-fixture": [str(optional)]})
            owner.native_identity["input_manifest_sha256"] = driver.digest(
                owner.manifest_file)
            with (patch.object(driver.signal, "signal"),
                  patch.object(driver.signal, "setitimer"),
                  patch.object(driver, "digest",
                               wraps=driver.digest) as hashes):
                driver.execute_session(owner, {"blocks": []}, root, {}, 1)
                optional.write_text("changed")
                driver.execute_session(owner, {"blocks": []}, root, {}, 1)
                for path in (common, optional):
                    self.assertEqual(sum(c.args[0] == path for c in
                                         hashes.call_args_list), 2)
            self.assertEqual(owner.bad_jobs, {"cpu-fixture"})
            owner.job_minutes = {"independent": 1, "cpu-fixture": 1}
            owner.job_bindings = {name: {"gomaxprocs": 1, "kind": "coderank"}
                                  for name in owner.job_minutes}
            owner.job_limit("independent", 0)
            with self.assertRaisesRegex(driver.ReviewRequired,
                                        "required input"):
                owner.job_limit("cpu-fixture", 0)
            optional.unlink()
            self.assertEqual(driver.bad_jobs(owner.manifest_file, root),
                             {"cpu-fixture"})
            common.write_text("changed")
            with patch.object(driver.signal, "signal"), \
                    self.assertRaisesRegex(driver.ReviewRequired,
                                           "common input"):
                driver.execute_session(owner, {"blocks": []}, root, {}, 1)

    def test_manifest_schema_aliases_and_exact_managed_tool_exception(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            local = root / "input"
            local.write_text("tiny")
            path = fixture_manifest(root, [local])
            original = driver.read_json(path)
            for changed in ({**original, "source": "0" * 40},
                            {**original, "schema": "unknown"},
                            {**original, "jobs": {"x": ["unlisted"]}}):
                with self.subTest(changed=changed):
                    driver.atomic_json(path, changed)
                    with self.assertRaises(driver.ReviewRequired):
                        driver.bad_jobs(path, root)
            alias = root / "alias"
            alias.symlink_to(local)
            driver.atomic_json(path, {**original, "files": {str(alias):
                                     driver.digest(local)}})
            with self.assertRaises(driver.ReviewRequired):
                driver.bad_jobs(path, root)
            tool = Path("/opt/homebrew/Cellar/example/1.0/bin/tool")
            sha = "1" * 64
            managed = {"schema": "storage-inputs/v1", "source": driver.COMMIT,
                       "files": {str(tool): sha}, "jobs": {"*": [str(tool)]},
                       "homebrew": {str(tool): {"formula": "example",
                                    "version": "1.0", "installed_at":
                                    "2026-10-01T00:00:00+00:00"}}}
            driver.atomic_json(path, managed)
            original_file = Path.is_file
            with (patch.object(Path, "is_file", autospec=True,
                               side_effect=lambda p: p == tool or
                               original_file(p)),
                  patch.object(driver, "digest", return_value=sha)):
                self.assertEqual(driver.bad_jobs(path, root), set())
                sibling = tool.parent / "unlisted"
                driver.atomic_json(path, {**managed, "files": {
                    str(tool): sha, str(sibling): sha}})
                with self.assertRaises(driver.FatalSession):
                    driver.bad_jobs(path, root)

    def test_runtime_path_escapes_refuse_before_admission_or_spawn(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner = fixture_owner(root)
            owner.case = {"gomaxprocs": 1}
            owner.guard = unittest.mock.Mock()
            directory = root / "job"
            directory.mkdir()
            alias = root / "alias"
            alias.symlink_to(directory, target_is_directory=True)
            for value in (Path("/tmp/unsafe"), Path("/private/tmp/unsafe"),
                          root / ".." / "escape", alias):
                with self.subTest(value=value), self.assertRaises(
                        driver.FatalSession):
                    driver.persistent_path(value, root)
            for key in ("HOME", "XDG_CACHE_HOME", "TMPDIR", "TMP", "TEMP",
                        "PYTHONPYCACHEPREFIX", "PYTHONUSERBASE"):
                env = {**driver.environment(root), "GOMAXPROCS": "1",
                       key: "/tmp/unsafe"}
                with self.subTest(key=key), \
                        patch.object(driver.subprocess, "Popen") as spawn, \
                        self.assertRaises(driver.FatalSession):
                    owner.start(["fixture"], directory, env, root)
                spawn.assert_not_called()
            owner.guard.assert_not_called()

    def test_completed_block_output_drift_restarts_only_damaged_job(self):
        for damage in ("raw-time", "missing-receipt", "torn-receipt"):
            with self.subTest(damage=damage), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder).resolve()
                selected, setup, admission, old = self.sdk_session(root)
                damaged = root / "jobs" / selected[1].name
                if damage == "raw-time":
                    (damaged / "native-time.txt").write_text("changed")
                elif damage == "missing-receipt":
                    (damaged / "complete.json").unlink()
                else:
                    (damaged / "complete.json").write_text("{")
                owner = FixtureSupervisor(root, ["pass"])
                owner.window = {"id": "resumed-window"}
                result = self.resume_sdk(owner, selected, setup, admission)
                self.assertEqual(result["blocks"]["storage"]["state"],
                                 "completed")
                self.assertEqual(owner.calls, [selected[1].name])
                current = driver.read_json(damaged / "complete.json")
                self.assertNotEqual(current["attempt"]["attempt_id"],
                                    old[1]["attempt"]["attempt_id"])
                self.assertIn("native-time.txt", current["files"])
                preserved = driver.read_json(root / "jobs" / selected[0].name /
                                             "complete.json")
                self.assertEqual(preserved, old[0])
                self.assertEqual(len(list((root / "attempts/jobs" /
                                           selected[1].name).iterdir())), 1)

    def test_new_run_internal_receipts_ignore_old_output_addresses(self):
        with tempfile.TemporaryDirectory() as folder:
            scratch = Path(folder).resolve()
            first, rerun = scratch / "first", scratch / "rerun"
            first.mkdir()
            rerun.mkdir()
            _, _, old_config, _ = self.sdk_session(first)
            selected, _, _, _ = self.sdk_session(rerun)
            before = driver.digest(first / "session.json")
            files = driver.block_files(rerun, old_config["blocks"][1])
            self.assertIn("jobs/" + selected[0].name + "/complete.json", files)
            self.assertTrue(all((rerun / name).is_file() for name in files))
            self.assertEqual(driver.digest(first / "session.json"), before)

    def test_full_job_identity_change_creates_a_retained_new_attempt(self):
        for field in ("driver", "runtime", "input_manifest_sha256", "recipe"):
            with self.subTest(field=field), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder).resolve()
                cases = DriverTests()
                selected, setup, _ = cases.fixture(root)
                selected = selected[:1]
                old_owner = FixtureSupervisor(root, ["pass"])
                old = []
                cases.execute(old_owner, selected, setup, old)
                changed = deepcopy(setup)
                changed["identity"][field] = "changed"
                owner = FixtureSupervisor(root, ["pass"])
                new = []
                cases.execute(owner, selected, changed, new)
                self.assertEqual(owner.calls, [selected[0].name])
                self.assertNotEqual(new[0]["identity"], old[0]["identity"])
                self.assertNotEqual(new[0]["attempt"]["attempt_id"],
                                    old[0]["attempt"]["attempt_id"])
                archive = list((root / "attempts/jobs" /
                                selected[0].name).iterdir())
                preserved = driver.read_json(archive[0] / "complete.json")
                self.assertEqual(preserved, old[0])

    def test_binding_resume_keeps_complete_and_restarts_partial(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            cases = ReviewFixTests()
            self.addCleanup(lambda: self.assertTrue(cases.doCleanups()))
            owner, admission = cases.session_fixture(root, "partial")
            self.addCleanup(owner.stop_all)
            owner.window = {"id": "first-window"}
            with patch.object(driver.signal, "signal"), \
                    patch.object(driver.signal, "setitimer"):
                first = driver.execute_session(owner, admission,
                                               root / "inputs", {}, 1)
                second = driver.execute_session(owner, admission,
                                                root / "inputs", {}, 1)
            self.assertEqual(first["blocks"]["smoke"]["state"], "partial")
            self.assertEqual(second["blocks"]["smoke"]["state"], "partial")
            self.assertEqual(owner.calls, ["smoke-fixture",
                                          "calibration-fixture",
                                          "smoke-fixture"])
            archive = list((root / "attempts/aux/smoke-fixture").iterdir())
            self.assertEqual(len(archive), 1)
            self.assertTrue((archive[0] / "partial-complete.json").is_file())
            complete = root / "aux/calibration-fixture/complete.json"
            self.assertIn(str(complete.relative_to(root)),
                          second["blocks"]["calibration"]["files"])
            window_id = driver.read_json(complete)["attempt"]["window_id"]
            self.assertEqual(window_id, "first-window")

    def test_fatal_cache_proof_never_becomes_a_partial_retry(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            directory = root / "jobs/case"
            directory.mkdir(parents=True)
            verify = unittest.mock.Mock(
                side_effect=driver.FatalSession("unsafe proof"))
            with self.assertRaisesRegex(driver.FatalSession, "unsafe proof"):
                driver.reusable(directory, verify)
            self.assertFalse((directory / "partial.json").exists())
            self.assertFalse((root / "attempts").exists())

    def test_setup_binds_bsd_time_without_a_version_invocation(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner = fixture_owner(root)
            inputs = root / "inputs"
            inputs.mkdir()
            (inputs / "tiers-summary.json").write_text("{}")
            owner.run = unittest.mock.Mock(
                return_value=SimpleNamespace(read_text=lambda: "version"))
            original_digest = driver.digest

            def setup_digest(path):
                if path == Path("/usr/bin/time"):
                    return "3" * 64
                return original_digest(path)

            with (patch.object(driver, "ARCHIVES", {}),
                  patch.object(driver, "copy_tree", return_value={}),
                  patch.object(driver, "digest", side_effect=setup_digest),
                  patch.object(owner, "run", side_effect=[
                      SimpleNamespace(read_text=lambda: "version") for _ in
                      range(4)] + [RuntimeError("stop before source clone")])
                  as run):
                with self.assertRaisesRegex(RuntimeError, "source clone"):
                    driver.prepare(owner, inputs, {})
            versions = driver.read_json(root / "tool-versions.json")
            self.assertEqual(versions["/usr/bin/time"], {
                "path": "/usr/bin/time", "sha256": "3" * 64})
            self.assertEqual([call.args[0] for call in
                              run.call_args_list[:4]], [
                ["go", "version"], ["cmake", "--version"],
                ["c++", "--version"], ["git", "--version"]])

    def test_interrupted_setup_retention_and_fatal_stop(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner = fixture_owner(root)
            (root / "setup").mkdir()
            (root / "setup/log").write_text("interrupted")
            (root / "setup-started.json").write_text("{}")
            with self.assertRaisesRegex(FileNotFoundError, "tiers-summary"):
                driver.prepare(owner, root / "missing-inputs", {})
            archive = list((root / "attempts/setup").iterdir())
            self.assertEqual(len(archive), 1)
            self.assertEqual((archive[0] / "setup/log").read_text(),
                             "interrupted")
            self.assertFalse((root / "setup").exists())
            admission = {"blocks": [{"id": "smoke", "jobs": []}]}
            driver.atomic_json(root / "session.json", {
                "identity": {"configuration": admission, "setup": {},
                             "native": owner.native_identity},
                "blocks": {"smoke": {"state": "started",
                                     "terminal": "fatal", "files": {}}}})
            owner.begin_block = unittest.mock.Mock()
            with patch.object(driver.signal, "signal"), \
                    self.assertRaisesRegex(driver.FatalSession, "diagnosis"):
                driver.execute_session(owner, admission, root, {}, 1)
            owner.begin_block.assert_not_called()


class NativeEntryTests(unittest.TestCase):
    def inputs(self, scratch):
        control = scratch / "control"
        control.mkdir()
        inputs = scratch / "inputs"
        inputs.mkdir()
        data, launcher, staged = inputs / "data", scratch / "python", (
            scratch / "driver.py")
        data.write_text("tiny input")
        launcher.write_text("fixture launcher")
        staged.write_text("fixture driver")
        config_path = control / "config.json"
        manifest = control / "inputs.json"
        identities, go = control / "identities.json", control / "go.json"
        driver.atomic_json(identities,
                           NativeAdmissionTests().identities(scratch))
        config = {"scratch_root": str(scratch), "inputs": str(inputs),
                  "manifest_file": str(manifest), "window_file": str(go),
                  "identities_file": str(identities),
                  "identities_sha256": driver.digest(identities),
                  "d11_limits": {"minimum_level": 20,
                                 "available_minimum_bytes": 4 * driver.GIB,
                                 "candidate_budget_bytes": 8 * driver.GIB,
                                 "oracle_budget_bytes": 24 * driver.GIB,
                                 "swap_growth_bytes": 512 * 1024 * 1024},
                  "launcher": {"path": str(launcher), "version": [3, 14, 7],
                               "sha256": driver.digest(launcher)},
                  "blocks": []}
        driver.atomic_json(config_path, config)
        files = {str(p): driver.digest(p) for p in (
            data, launcher, staged, config_path, identities)}
        driver.atomic_json(manifest, {
            "schema": "storage-inputs/v1", "source": driver.COMMIT,
            "files": files, "jobs": {"*": list(files)}, "homebrew": {}})
        driver.atomic_json(go, NativeAdmissionTests().go(
            driver.digest(manifest)))
        return config_path, manifest, staged

    def test_new_run_ids_same_pins_and_finally_without_go(self):
        with tempfile.TemporaryDirectory() as folder:
            scratch = Path(folder).resolve()
            config_path, manifest, staged = self.inputs(scratch)
            baseline = driver.digest(manifest)
            owners, observed = [], []
            real_session = driver.execute_session

            def owner(root, env, config):
                record = SimpleNamespace(
                    root=root, environment=env, scratch=scratch,
                    manifest_file=manifest, window={"id": "fixture-window"},
                    go_file=Path(config["window_file"]),
                    native_identity={"input_manifest_sha256": config[
                        "input_manifest_sha256"],
                        "launcher": config["launcher"],
                        "daemon_off_identities_sha256": config[
                            "identities_sha256"],
                        "host": ["fixture", str(64 * driver.GIB),
                                 "8", "2", "10"]},
                    stop_all=unittest.mock.Mock())
                owners.append(record)
                return record

            def session(record, config, inputs, identity, concurrency):
                observed.append((config["input_manifest_sha256"],
                                 driver.COMMIT))
                real_session(record, config, inputs, identity, concurrency)
                output = record.root / "jobs" / "case" / "native-time.txt"
                if not output.exists():
                    output.parent.mkdir(parents=True)
                    output.write_text("raw native time")
                if record.root.name == "interrupted":
                    derived = record.root / "data/partial.json"
                    derived.parent.mkdir()
                    derived.write_text("partial derived artifact")
                    record.go_file.write_text("revoked")
                    raise KeyboardInterrupt()

            with (patch.object(driver, "__file__", str(staged)),
                  patch.object(driver, "Supervisor", side_effect=owner),
                  patch.object(driver, "execute_session", side_effect=session),
                  patch.object(driver.signal, "signal"),
                  patch.object(driver.signal, "setitimer"),
                  patch.object(driver.subprocess, "Popen") as spawn):
                for mode, run_id in (("--native", "original"),
                                     ("--resume", "original"),
                                     ("--native", "rerun"),
                                     ("--native", "interrupted")):
                    root = scratch / "mac-run" / run_id
                    argv = ["run.py", mode, str(config_path),
                            "--root", str(root)]
                    with patch.object(sys, "argv", argv):
                        if run_id == "interrupted":
                            with self.assertRaises(KeyboardInterrupt):
                                driver.main()
                        else:
                            self.assertEqual(driver.main(), 0)
                    checkpoint = driver.read_json(root / "checkpoint.json")
                    self.assertEqual(checkpoint["run_id"], run_id)
                    self.assertEqual(checkpoint["files"][
                        "jobs/case/native-time.txt"], driver.digest(
                            root / "jobs/case/native-time.txt"))
                    if run_id == "interrupted":
                        self.assertEqual(checkpoint["files"][
                            "data/partial.json"], driver.digest(
                                root / "data/partial.json"))
                spawn.assert_not_called()
            self.assertEqual(observed, [(baseline, driver.COMMIT)] * 4)
            self.assertEqual(driver.digest(manifest), baseline)
            for record in owners:
                record.stop_all.assert_called_once_with()
            first = scratch / "mac-run/original"
            before = driver.digest(first / "checkpoint.json")
            with (patch.object(driver, "__file__", str(staged)),
                  patch.object(driver, "Supervisor") as constructor,
                  patch.object(sys, "argv", ["run.py", "--native",
                                             str(config_path), "--root",
                                             str(first)]),
                  self.assertRaisesRegex(driver.ReviewRequired, "run-id")):
                driver.main()
            constructor.assert_not_called()
            self.assertEqual(driver.digest(first / "checkpoint.json"), before)

    def test_unlisted_or_unbound_inputs_refuse_before_creation(self):
        for damage in ("unlisted", "unbound", "parallel", "alias"):
            with self.subTest(damage=damage), \
                    tempfile.TemporaryDirectory() as folder:
                scratch = Path(folder).resolve()
                config, manifest, staged = self.inputs(scratch)
                document = driver.read_json(manifest)
                if damage == "unlisted":
                    document["files"].pop(str(scratch / "inputs/data"))
                elif damage == "unbound":
                    document["jobs"]["*"].remove(str(staged))
                elif damage == "alias":
                    target = scratch / "other-inputs"
                    target.mkdir()
                    (scratch / "inputs/alias").symlink_to(
                        target, target_is_directory=True)
                driver.atomic_json(manifest, document)
                root = scratch / "mac-run/rejected"
                argv = ["run.py", "--native", str(config), "--root", str(root)]
                if damage == "parallel":
                    argv.extend(["--concurrency", "2"])
                with (patch.object(driver, "__file__", str(staged)),
                      patch.object(driver, "Supervisor") as constructor,
                      patch.object(sys, "argv", argv),
                      self.assertRaises(driver.ReviewRequired)):
                    driver.main()
                constructor.assert_not_called()
                self.assertFalse(root.exists())


if __name__ == "__main__":
    unittest.main()
