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


def fixture_tools(root):
    root = root.resolve()
    directory = root / "control/tools"
    directory.mkdir(parents=True, exist_ok=True)
    tools = {}
    for name in ("go cmake c++ cc make git file zstd time ps lsof sysctl "
                 "vm_stat pmset launchctl lipo otool").split():
        path = directory / name
        path.write_bytes(("fixture tool " + name).encode())
        tools[name] = str(path)
    manifest = fixture_manifest(root, [Path(p) for p in tools.values()])
    record = driver.read_json(manifest)
    record["tools"] = tools
    driver.atomic_json(manifest, record)
    env = {"PATH": "/no-path", "STUDY_MANIFEST": str(manifest),
           "STUDY_MANIFEST_SHA": driver.digest(manifest),
           "CC": tools["cc"], "CXX": tools["c++"]}
    return manifest, tools, env


def fixture_setup(root, setup=None):
    complete = root / "setup-complete.json"
    if setup is None and complete.exists():
        return driver.read_json(complete)
    setup = setup if setup is not None else {
        "identity": {"driver": "fixture", "runtime": {}},
        "artifacts": {}, "runtime_libraries": {}}
    (root / "bin").mkdir(exist_ok=True)
    for name in {*driver.ADAPTERS, "gen", "oracle"}:
        path = root / "bin" / name
        if not path.exists():
            path.write_bytes(b"fixture binary; never executed")
        setup["artifacts"]["bin/" + name] = driver.digest(path)
    archives = {}
    for tier in driver.ARCHIVES:
        path = root / (tier + "-fixture.archive")
        if not path.exists():
            path.write_bytes(b"tiny fixture archive; not study data")
        archives[tier] = {"path": str(path), "sha256": driver.digest(path),
                          "bytes": path.stat().st_size, "queries": 1}
    driver.atomic_json(root / "archives.json", archives)
    setup.update(archives=archives)
    setup["artifacts"]["archives.json"] = driver.digest(root / "archives.json")
    driver.atomic_json(complete, setup)
    return setup


def fixture_owner(root, *, setup=True):
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
    if setup:
        owner.setup = fixture_setup(root)
        owner.binding = driver.setup_binding(root, owner.setup)
    return owner


class FixtureSupervisor:
    def __init__(self, root, modes, interrupt_at=None):
        self.root = root
        self.setup = fixture_setup(root)
        self.binding = driver.setup_binding(root, self.setup)
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
        self.block_id = "unbound"  # as Supervisor.__init__ sets it
        self.modes = modes
        self.interrupt_at = interrupt_at
        self.calls = []
        self.swapouts = None

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
            raise driver.DeadlineExceeded("fixture interruption")
        mode = self.modes[len(self.calls)-1]
        adapter = Path(args[args.index("-data")-1]).name
        job = driver.Job("screen", adapter, "tier10")
        if "-timing-only" in args:
            job = next(j for j in driver.jobs() if j.name == directory.name)
        check = {"query_id": "q", "operation": "id", "status": "passed",
                 "mismatch": False}
        report = {
            "adapter": adapter, "tier": job.tier, "run": 1,
            "manifest_sha256": driver.digest(
                self.root / "data" / job.tier / "manifest.json"),
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
        if job.stage == "timing":
            report.update(timing_only=True, skipped_phases=driver.SKIPPED)
            for key in ("delta_checks", "concurrent", "crash"):
                del report[key]
            value = job.repeat * 100 * (
                job.repeat + 1 if adapter == driver.FINALISTS[1] else 1)
            for metric in report["metrics"].values():
                metric.update({key: value for key in
                               ("p50_ns", "p95_ns", "p99_ns", "mean_ns")})
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


def fixture_references(root, setup=None):
    paths = []
    for tier in driver.ARCHIVES:
        data = root / "data" / tier
        data.mkdir(parents=True, exist_ok=True)
        driver.atomic_json(data / "manifest.json", {"repos": ["fixture"]})
        driver.atomic_json(data / "queries.json", [{"id": "q", "op": "id"}])
        paths.extend((data / "manifest.json", data / "queries.json"))
    manifest = fixture_manifest(root, paths)
    if setup is None:
        setup = {"identity": {"driver": "fixture", "runtime": {}},
                 "artifacts": {"bin/oracle": "fixture"},
                 "runtime_libraries": {}}
    setup["identity"]["input_manifest_sha256"] = driver.digest(manifest)
    fixture_setup(root, setup)
    owner = FixtureSupervisor(root, ["pass", "pass"])
    receipts = []
    for tier in driver.ARCHIVES:
        job = driver.Job("oracle", "oracle", tier)
        directory = root / "jobs" / job.name
        fake = driver.result_path(
            root, driver.Job("screen", "oracle", "tier10"))
        fake.parent.parent.mkdir(parents=True, exist_ok=True)
        driver.new_attempt(owner, directory)
        owner.start(driver.command(job, root), directory, {}, root)
        report = driver.read_json(fake)
        report.update(tier=tier, manifest_sha256=driver.digest(
            root / "data" / tier / "manifest.json"))
        fake.parent.rename(directory / "output")
        old = directory / "output" / fake.name
        target = driver.result_path(root, job)
        if old != target:
            old.rename(target)
        driver.atomic_json(target, report)
        receipt = {"identity": {**driver.case_identity(job, root, setup, 10),
                               "inputs_verified": True}, "state": "pass",
                   "correctness_pass": True, "exit_code": 0,
                   "files": {str(p.relative_to(directory)): driver.digest(p)
                             for p in driver.output_files([directory])}}
        driver.atomic_json(directory / "complete.json", receipt)
        receipts.append(receipt)
    return owner, setup, receipts


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
        fixture_setup(root, setup)
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
            self.assertEqual(driver.eligible_sdk(
                supervisor, receipts, setup, {}), [])
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
            with self.assertRaisesRegex(
                    driver.DeadlineExceeded, "interruption"):
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
            with (self.subTest(mode=mode),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder)
                selected, setup, before = self.fixture(root)
                supervisor = FixtureSupervisor(root, [mode, "pass"])
                with self.assertRaises(driver.ReviewRequired):
                    self.execute(supervisor, selected, setup, [])
                self.assertEqual(len(supervisor.calls), 1)
                directory = root / "jobs" / selected[0].name
                self.assertFalse((directory / "complete.json").exists())
                marker = directory / "partial.json"
                self.assertEqual(driver.read_json(marker)["state"],
                                 "checks_failed")
                saved = {p: p.read_bytes() for p in
                         driver.output_files([directory])}
                resumed = FixtureSupervisor(root, [])
                with self.assertRaisesRegex(driver.ReviewRequired,
                                            "known negative"):
                    self.execute(resumed, selected, setup, [])
                self.assertEqual(resumed.calls, [])
                self.assertFalse((root / "attempts").exists())
                self.assertEqual({p: p.read_bytes() for p in saved}, saved)
                self.assertEqual(before, {p.name: driver.digest(p) for p in
                                         (root / "data/tier10").iterdir()})

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

    def test_worker_json_keeps_expected_output_errors_local(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "receipt.json"
            for mode in ("missing", "malformed"):
                with self.subTest(mode=mode):
                    if mode == "malformed":
                        path.write_text("{")
                    with self.assertRaises(driver.ReviewRequired) as raised:
                        driver.worker_json(path, "worker-fixture")
                    self.assertNotIsInstance(
                        raised.exception, driver.FatalSession)
                    self.assertIn("worker-fixture", str(raised.exception))
                    self.assertIn("missing or malformed",
                                  str(raised.exception))
                    expected = (FileNotFoundError if mode == "missing" else
                                json.JSONDecodeError)
                    self.assertIsInstance(raised.exception.__cause__, expected)
            path.write_text('{"state": "completed"}')
            self.assertEqual(driver.worker_json(path, "worker-fixture"),
                             {"state": "completed"})
            for kind in (AttributeError, TypeError, PermissionError, OSError):
                with self.subTest(error=kind.__name__):
                    failure = kind("synthetic unexpected read error")
                    with (patch.object(driver, "read_json",
                                       side_effect=failure),
                          self.assertRaises(kind) as raised):
                        driver.worker_json(path, "worker-fixture")
                    self.assertIs(raised.exception, failure)

    def test_timing_needs_its_exact_verified_reference(self):
        for mode in ("valid", "missing", "report-missing", "budget",
                     "manifest", "identity", "hash", "malformed", "input",
                     "gomax"):
            with self.subTest(mode=mode), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder).resolve()
                owner, setup, references = fixture_references(root)
                job = driver.Job("oracle", "oracle", "tier1")
                complete = root / "jobs" / job.name / "complete.json"
                saved = driver.read_json(complete)
                bindings = {driver.Job("oracle", "oracle", tier).name:
                            {"gomaxprocs": 10} for tier in driver.ARCHIVES}
                if mode == "missing":
                    complete.unlink()
                elif mode == "malformed":
                    path = driver.result_path(root, job)
                    path.write_text("{")
                    saved["files"][str(path.relative_to(complete.parent))] = (
                        driver.digest(path))
                    driver.atomic_json(complete, saved)
                elif mode == "report-missing":
                    driver.result_path(root, job).unlink()
                elif mode == "input":
                    owner.bad_jobs.add(job.name)
                elif mode == "gomax":
                    bindings[job.name]["gomaxprocs"] = 2
                else:
                    if mode == "budget":
                        saved["state"] = "budget_exceeded"
                    elif mode == "identity":
                        saved["identity"]["job"]["tier"] = "tier10"
                    elif mode in {"hash", "manifest"}:
                        path = driver.result_path(root, job)
                        report = driver.read_json(path)
                        report["manifest_sha256"] = "0" * 64
                        driver.atomic_json(path, report)
                        if mode == "manifest":
                            saved["files"][str(path.relative_to(
                                complete.parent))] = driver.digest(path)
                    driver.atomic_json(complete, saved)
                screens = [{"identity": {"job": driver.asdict(driver.Job(
                    "screen", name, "tier10"))}, "state": "pass"}
                    for name in driver.FINALISTS]
                before = complete.read_bytes() if complete.exists() else None
                selected = driver.eligible_sdk(
                    owner, [*references, *screens], setup, bindings)
                expected = (set(driver.ARCHIVES) if mode == "valid" else
                            {"tier10"})
                self.assertEqual({j.tier for j in selected}, expected)
                self.assertEqual(len(selected), 12 if mode == "valid" else 6)
                self.assertEqual(complete.read_bytes() if complete.exists()
                                 else None, before)
                if mode in {"valid", "missing"}:
                    owner.stop_all = unittest.mock.Mock()
                    admission = {"blocks": [
                        {"id": "setup", "jobs": []},
                        {"id": "storage", "jobs": [
                            {"id": name, **profile}
                            for name, profile in bindings.items()]},
                        {"id": "segment_timing", "jobs": []}]}
                    timed = []

                    def stage(owner, chosen, setup, concurrency, receipts):
                        if chosen[0].stage == "timing":
                            timed.extend(chosen)
                        else:
                            receipts.extend(references if chosen[0].stage == (
                                "oracle") else screens)

                    with (patch.object(driver.signal, "signal"),
                          patch.object(driver.signal, "setitimer"),
                          patch.object(driver.time, "monotonic",
                                       return_value=0),
                          patch.object(driver, "prepare", return_value=setup),
                          patch.object(driver, "execute_stage",
                                       side_effect=stage),
                          patch.object(driver, "block_files",
                                       return_value={})):
                        session = driver.execute_session(
                            owner, admission, root / "inputs", {}, 1)
                    self.assertEqual({j.tier for j in timed}, expected)
                    state = session["blocks"]["segment_timing"]["state"]
                    self.assertEqual(state, "completed" if mode == "valid" else
                                     "dependency_blocked")

    def test_reference_proof_bugs_do_not_become_missing_dependencies(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner, setup, references = fixture_references(root)
            bindings = {driver.Job("oracle", "oracle", tier).name:
                        {"gomaxprocs": 10} for tier in driver.ARCHIVES}
            job = driver.Job("oracle", "oracle", "tier1")
            artifact = root / "jobs" / job.name / "native-time.txt"
            failures = (TypeError("synthetic proof bug"),
                        PermissionError("synthetic proof bug"),
                        FileNotFoundError(2, "synthetic proof bug",
                                          str(artifact)))
            for failure in failures:
                with self.subTest(error=type(failure).__name__):
                    with (patch.object(driver, "finished_case",
                                       side_effect=failure),
                          self.assertRaises(type(failure)) as raised):
                        driver.eligible_sdk(owner, references, setup,
                                                bindings)
                    self.assertIs(raised.exception, failure)
            complete = root / "jobs" / job.name / "complete.json"
            complete.write_text("{")
            with self.assertRaises(json.JSONDecodeError):
                driver.eligible_sdk(owner, references, setup, bindings)

    def test_individual_timing_rows_never_validate_a_comparison(self):
        for mode in ("partial", "budget", "other-window", "all-individual"):
            with self.subTest(mode=mode), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder)
                rows = [{"identity": {"job": driver.asdict(driver.Job(
                    "screen", name, "tier10"))}, "state": "pass",
                    "wall_seconds": 1, "peak_rss_bytes": 1}
                    for name in driver.FINALISTS]
                for repeat in range(1, 4):
                    for name in driver.FINALISTS:
                        if mode == "partial" and repeat == 3 and name == (
                                driver.FINALISTS[1]):
                            continue
                        rows.append({"identity": {"job": driver.asdict(
                            driver.Job("timing", name, "tier10", repeat))},
                            "attempt": {"window_id": "other" if mode == (
                                "other-window") and name == driver.FINALISTS[1]
                                else "current"}, "state": "budget_exceeded"
                            if mode == "budget" and name == driver.FINALISTS[1]
                            else "pass", "wall_seconds": 1,
                            "peak_rss_bytes": 1, "metrics": {}})
                driver.summary(root, rows)
                medians = driver.read_json(root / "summary.json")[
                    "timing_medians"]
                self.assertTrue(medians)
                self.assertTrue(all(row["valid_timing_comparison"] is False
                                    for row in medians))
                self.assertTrue(all(row["wall_seconds"] is None
                                    and row["metrics"] == {}
                                    for row in medians))


class TimingGroupTests(unittest.TestCase):
    def owner(self, root, modes=None, *, window="group-window-1", stop=None):
        owner = FixtureSupervisor(root, modes if modes is not None else
                                  ["pass"] * 6, interrupt_at=stop)
        owner.window = {"id": window}
        owner.block_id, owner.block_deadline = "segment_timing", 40000
        owner.case, owner.current_rss = {}, 0
        owner.arm_guard = unittest.mock.Mock()
        owner.job_bindings = {j["id"]: {
            **j, "kind": driver.BLOCK_KINDS["segment_timing"],
            "estimated_peak_bytes": driver.GIB, "gomaxprocs": 10}
            for j in driver.bound_jobs(root)["segment_timing"]}
        owner.job_minutes = {name: 45 for name in owner.job_bindings}
        return owner

    def fixture(self, root):
        _, setup, references = fixture_references(root)
        setup["artifacts"].update({"bin/" + name: driver.digest(
            root / "bin" / name) for name in driver.FINALISTS})
        selected = [j for j in driver.jobs() if j.stage == "timing"
                    and j.tier == "tier1"]
        return self.owner(root), setup, selected, references

    def run_group(self, owner, setup, selected, *, clock=None):
        rows = []
        now = lambda: 0 if clock is None else clock[0]
        with (patch.object(driver, "process_groups", return_value={}),
              patch.object(driver.time, "sleep"),
              patch.object(driver.time, "monotonic", side_effect=now)):
            driver.execute_stage(owner, selected, setup, 1, rows)
        return rows

    def group_path(self, root):
        return root / "jobs/timing-tier1"

    def rewrite_sample(self, root, job, change):
        directory = self.group_path(root)
        case = driver.case_directory(root, job)
        row = driver.read_json(case / "complete.json")
        change(row, case)
        row["files"] = {name: driver.digest(case / name)
                        for name in row["files"]}
        driver.atomic_json(case / "complete.json", row)
        receipt = driver.read_json(directory / "complete.json")
        names = [j.name for j in driver.jobs()
                 if j.stage == "timing" and j.tier == job.tier]
        receipt["samples"][names.index(job.name)] = row
        receipt["files"] = {name: driver.digest(directory / name)
                            for name in receipt["files"]}
        driver.atomic_json(directory / "complete.json", receipt)

    def test_fresh_group_predeclares_order_and_proves_all_outputs(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner, setup, selected, references = self.fixture(root)
            screens = [{"identity": {"job": driver.asdict(driver.Job(
                "screen", name, "tier10"))}, "state": "pass"}
                for name in driver.FINALISTS]
            eligible = driver.eligible_sdk(
                owner, references + screens, setup,
                {driver.Job("oracle", "oracle", t).name: {"gomaxprocs": 10}
                 for t in driver.ARCHIVES})
            self.assertEqual(selected, [j for j in eligible
                                        if j.tier == "tier1"])
            start = owner.start
            declared = []

            def spawn(args, directory, env, cwd):
                group = self.group_path(root)
                self.assertFalse((group / "complete.json").exists())
                declaration = driver.read_json(group / "started.json")
                attempt = driver.read_json(group / "attempt.json")
                self.assertEqual(driver.read_json(directory / "attempt.json"),
                                 attempt)
                self.assertEqual(attempt["window_id"], owner.window["id"])
                declared.append(declaration)
                return start(args, directory, env, cwd)

            owner.start = spawn
            rows = self.run_group(owner, setup, selected)
            self.assertEqual(owner.calls, [j.name for j in selected])
            self.assertEqual([j.adapter for j in selected],
                             list(driver.FINALISTS) * 3)
            expected = driver.timing_identity(owner, "tier1", setup)
            self.assertEqual(declared, [expected] * 6)
            self.assertEqual(expected["pair"], list(driver.FINALISTS))
            self.assertEqual(expected["reference"], driver.FINALISTS[0])
            self.assertEqual(expected["ratio"], "B/A")
            self.assertEqual(expected["operation"], "segment-sdk")
            self.assertEqual([c["job"] for c in expected["samples"]],
                             [driver.asdict(j) for j in selected])
            proof = driver.finished_group(root, expected)
            self.assertEqual(proof["samples"], rows)
            self.assertEqual(proof["state"], "pass")
            self.assertTrue(all(row["attempt"] == proof["attempt"]
                                for row in rows))
            self.assertEqual(proof["files"], {
                name: driver.digest(self.group_path(root) / name)
                for name in proof["files"]})
            profiles = [owner.job_bindings[j.name] for j in selected]
            for job, profile in zip(selected, profiles):
                directory = driver.case_directory(root, job)
                self.assertEqual(Path(profile["receipt_path"]),
                                 directory / "complete.json")
                output = profile["argv"][profile["argv"].index("-out")+1]
                self.assertEqual(output, str(directory / "output"))
            self.assertEqual(len({p["receipt_path"] for p in profiles}), 6)
            files = driver.block_files(root, {"id": "segment_timing",
                                               "jobs": profiles})
            self.assertIn("jobs/timing-tier1/complete.json", files)
            for job in selected:
                self.assertIn(str((driver.case_directory(root, job) /
                                   "complete.json").relative_to(root)), files)
            result = driver.read_json(root / "summary.json")
            self.assertEqual(len(result["timing_medians"]), 2)
            self.assertTrue(all(m["valid_timing_comparison"]
                                for m in result["timing_medians"]))
            ratios = result["paired_ratios"]
            self.assertEqual(ratios["orientation"], "B/A")
            self.assertEqual(len(ratios["operations"]), 3)
            for ratio in ratios["operations"]:
                self.assertEqual(ratio["pairs"], [2, 3, 4])
                self.assertEqual((ratio["pair_count"], ratio["median"],
                                  ratio["min"], ratio["max"]), (3, 3, 2, 4))
            driver.summary(root, rows)
            self.assertTrue(all(not m["valid_timing_comparison"] for m in
                                driver.read_json(root / "summary.json")[
                                    "timing_medians"]))

    def test_interrupted_group_restarts_all_six_then_reuses(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner, setup, selected, _ = self.fixture(root)
            owner.interrupt_at = 2
            with self.assertRaisesRegex(driver.ReviewRequired, "interruption"):
                self.run_group(owner, setup, selected)
            directory = self.group_path(root)
            raw = driver.result_path(root, selected[0])
            original = raw.read_bytes()
            old = driver.read_json(directory / "attempt.json")
            self.assertFalse((directory / "complete.json").exists())
            resumed = self.owner(root, window="group-window-2")
            rows = self.run_group(resumed, setup, selected)
            self.assertEqual(resumed.calls, [j.name for j in selected])
            retained = list((root / "attempts/jobs/timing-tier1").iterdir())
            self.assertEqual(len(retained), 1)
            self.assertEqual((retained[0] / raw.relative_to(directory))
                             .read_bytes(), original)
            retained_attempt = driver.read_json(retained[0] / "attempt.json")
            self.assertEqual(retained_attempt, old)
            attempt = driver.read_json(directory / "attempt.json")
            self.assertNotEqual(attempt["attempt_id"], old["attempt_id"])
            self.assertEqual(attempt["window_id"], "group-window-2")
            self.assertTrue(all(row["attempt"] == attempt for row in rows))
            before = (directory / "complete.json").read_bytes()
            final = self.owner(root, [], window="group-window-3")
            cached = self.run_group(final, setup, selected)
            self.assertEqual(final.calls, [])
            self.assertEqual(cached, rows)
            after = (directory / "complete.json").read_bytes()
            self.assertEqual(after, before)
            self.assertEqual(cached[0]["attempt"]["window_id"],
                             "group-window-2")

    def test_alien_b_window_or_attempt_rejects_the_whole_group(self):
        for field in ("window_id", "attempt_id"):
            with self.subTest(field=field), tempfile.TemporaryDirectory() as f:
                root = Path(f).resolve()
                owner, setup, selected, _ = self.fixture(root)
                self.run_group(owner, setup, selected)

                def alien(row, case):
                    row["attempt"][field] = "alien"
                    driver.atomic_json(case / "attempt.json", row["attempt"])

                self.rewrite_sample(root, selected[1], alien)
                expected = driver.timing_identity(owner, "tier1", setup)
                with self.assertRaisesRegex(driver.ReviewRequired, "attempt"):
                    driver.finished_group(root, expected)
                old = (self.group_path(root) / "complete.json").read_bytes()
                resumed = self.owner(root, window="replacement-window")
                self.run_group(resumed, setup, selected)
                self.assertEqual(resumed.calls, [j.name for j in selected])
                attempts = root / "attempts/jobs/timing-tier1"
                retained = list(attempts.iterdir())
                after = (retained[0] / "complete.json").read_bytes()
                self.assertEqual(after, old)

    def test_changed_identity_or_missing_outputs_invalidates_group(self):
        for mode in ("pair", "reference", "ratio", "operation", "order",
                     "recipe", "configuration", "input", "raw",
                     "sample-receipt", "coverage"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as f:
                root = Path(f).resolve()
                owner, setup, selected, _ = self.fixture(root)
                self.run_group(owner, setup, selected)
                path = self.group_path(root) / "complete.json"
                receipt = driver.read_json(path)
                if mode == "pair":
                    receipt["identity"]["pair"].reverse()
                elif mode == "reference":
                    receipt["identity"]["reference"] = driver.FINALISTS[1]
                elif mode == "ratio":
                    receipt["identity"]["ratio"] = "A/B"
                elif mode == "operation":
                    receipt["identity"]["operation"] = "alien"
                elif mode == "order":
                    receipt["identity"]["samples"].reverse()
                elif mode == "recipe":
                    receipt["identity"]["samples"][0]["driver"] = "alien"
                elif mode == "configuration":
                    receipt["identity"]["samples"][0]["GOMAXPROCS"] = 2
                elif mode == "input":
                    receipt["identity"]["inputs"]["manifest.json"] = "0" * 64
                elif mode == "raw":
                    raw = driver.result_path(root, selected[1])
                    raw.write_text(raw.read_text() + " ")
                elif mode == "sample-receipt":
                    (driver.case_directory(root, selected[1]) /
                     "complete.json").unlink()
                else:
                    del receipt["files"][selected[1].name + "/complete.json"]
                driver.atomic_json(path, receipt)
                resumed = self.owner(root)
                self.run_group(resumed, setup, selected)
                self.assertEqual(resumed.calls, [j.name for j in selected])
                self.assertEqual(len(list((root /
                    "attempts/jobs/timing-tier1").iterdir())), 1)

    def test_torn_driver_receipt_or_unexpected_proof_bug_is_not_a_retry(self):
        for mode in ("torn", "type", "key", "permission"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as f:
                root = Path(f).resolve()
                owner, setup, selected, _ = self.fixture(root)
                self.run_group(owner, setup, selected)
                path = self.group_path(root) / "complete.json"
                failure = {"torn": json.JSONDecodeError, "type": TypeError,
                           "key": KeyError,
                           "permission": PermissionError}[mode]
                if mode == "torn":
                    path.write_text("{")
                    context = patch.object(driver, "COMMIT", driver.COMMIT)
                else:
                    context = patch.object(driver, "finished_group",
                                           side_effect=failure("proof bug"))
                before = path.read_bytes()
                resumed = self.owner(root, [])
                with context, self.assertRaises(failure):
                    self.run_group(resumed, setup, selected)
                self.assertEqual(resumed.calls, [])
                self.assertEqual(path.read_bytes(), before)

    def test_incomplete_order_and_failed_proof_never_complete(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner, setup, selected, _ = self.fixture(root)
            for samples in (selected[:-1], list(reversed(selected))):
                with self.assertRaises(driver.ReviewRequired):
                    self.run_group(owner, setup, samples)
                self.assertEqual(owner.calls, [])
            failure = driver.ReviewRequired("synthetic output proof failure")
            with (patch.object(driver, "finished_group", side_effect=failure),
                  self.assertRaises(driver.ReviewRequired)):
                self.run_group(owner, setup, selected)
            self.assertEqual(owner.calls, [j.name for j in selected])
            complete = self.group_path(root) / "complete.json"
            self.assertFalse(complete.exists())
            result = driver.read_json(root / "summary.json")
            self.assertFalse(any(m["valid_timing_comparison"]
                                 for m in result["timing_medians"]))

    def test_group_negatives_reuse_without_valid_metrics(self):
        for mode in ("budget", "checks_failed"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as f:
                root = Path(f).resolve()
                owner, setup, selected, _ = self.fixture(root)
                if mode == "checks_failed":
                    owner.modes[0] = "checks_failed"
                else:
                    start = owner.start

                    def over_budget(args, directory, env, cwd):
                        owned = start(args, directory, env, cwd)
                        if len(owner.calls) == 1:
                            sample = fixture_resources(elapsed_seconds=1)
                            (directory / "rss.jsonl").write_text(
                                json.dumps(sample) + "\n")
                            marker = directory / "budget-exceeded.json"
                            driver.atomic_json(marker, {"state": "partial"})
                            raise driver.BudgetExceeded(
                                9 * driver.GIB, driver.CANDIDATE_BUDGET)
                        return owned

                    owner.start = over_budget
                rows = self.run_group(owner, setup, selected)
                expected_state = ("budget_exceeded" if mode == "budget"
                                  else mode)
                expected = driver.timing_identity(owner, "tier1", setup)
                state = driver.finished_group(root, expected)["state"]
                self.assertEqual(state, expected_state)
                result = driver.read_json(root / "summary.json")
                self.assertFalse(any(m["valid_timing_comparison"]
                                     for m in result["timing_medians"]))
                self.assertEqual(result["paired_ratios"]["operations"], [])
                resumed = self.owner(root, [], window="later-window")
                cached = self.run_group(resumed, setup, selected)
                self.assertEqual(cached, rows)
                self.assertEqual(resumed.calls, [])

    def test_partial_known_negative_cannot_restart_a_whole_group_blindly(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner, setup, selected, _ = self.fixture(root)
            owner.modes[0], owner.interrupt_at = "checks_failed", 2
            with self.assertRaises(driver.ReviewRequired):
                self.run_group(owner, setup, selected)
            resumed = self.owner(root, [])
            with self.assertRaises(FileNotFoundError):
                self.run_group(resumed, setup, selected)
            self.assertEqual(resumed.calls, [])
            self.assertFalse((root / "attempts/jobs/timing-tier1").exists())

    def test_damaged_completed_negative_is_not_a_blind_group_retry(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner, setup, selected, _ = self.fixture(root)
            owner.modes[0] = "checks_failed"
            self.run_group(owner, setup, selected)
            path = self.group_path(root) / "complete.json"
            original = path.read_bytes()
            self.assertEqual(driver.read_json(path)["state"], "checks_failed")
            case = driver.case_directory(root, selected[0])
            (case / "complete.json").unlink()
            resumed = self.owner(root, [])
            with self.assertRaises(FileNotFoundError):
                self.run_group(resumed, setup, selected)
            self.assertEqual(resumed.calls, [])
            self.assertEqual(path.read_bytes(), original)
            self.assertFalse((root / "attempts/jobs/timing-tier1").exists())

    def clock_owner(self, owner, clock, samples):
        owner.job_limit = driver.Supervisor.job_limit.__get__(owner)
        owner.permissions = unittest.mock.Mock(return_value=40000)
        owner.stop_all = unittest.mock.Mock()
        owner.observe = lambda groups: driver.Supervisor.guard(owner)
        start = owner.start
        recorded = []

        def timed(args, directory, env, cwd):
            driver.Supervisor.guard(owner)
            recorded.append((clock[0], owner.launch_deadline, owner.swapouts))
            owned = start(args, directory, env, cwd)
            owned.deadline = owner.launch_deadline
            clock[0] += 600
            return owned

        owner.start = timed
        return recorded, patch.object(driver, "native_resources",
                                      side_effect=samples)

    def test_sample_clocks_reset_but_the_group_swap_baseline_does_not(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner, setup, selected, _ = self.fixture(root)
            clock = [0]
            sample = fixture_resources()
            pagesize = sample["page_size"]
            increments = [0, 64, 64, 128, 128, 192, 192,
                          256, 256, 384, 384, 512, 512]
            samples = [{**sample, "swapouts": 100 + n * 1024**2 // pagesize}
                       for n in increments]
            recorded, resources = self.clock_owner(owner, clock, samples)
            with resources:
                self.run_group(owner, setup, selected, clock=clock)
            self.assertEqual(len(recorded), 6)
            self.assertGreater(clock[0], 45 * 60)
            self.assertTrue(all(end == start + 45 * 60 -
                                driver.CLEANUP_RESERVE_SECONDS
                                for start, end, _ in recorded))
            baselines = [baseline for _, _, baseline in recorded]
            self.assertEqual(baselines, [100]*6)
            evidence = (root / "resources.jsonl").read_text().splitlines()
            observations = [json.loads(line) for line in evidence]
            starts = {r["swapouts_start"] for r in observations}
            self.assertEqual(starts, {100})
            self.assertEqual(observations[-1]["swapouts"] - 100,
                             512 * 1024**2 // pagesize)

    def test_cumulative_swap_and_sample_window_stop_the_unfinished_group(self):
        for mode in ("swap", "window"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as f:
                root = Path(f).resolve()
                owner, setup, selected, _ = self.fixture(root)
                clock = [0]
                sample = fixture_resources()
                pagesize = sample["page_size"]
                samples = [{**sample, "swapouts": 100 + i * 64 * 1024**2 //
                            pagesize if mode == "swap" else 100}
                           for i in range(13)]
                recorded, resources = self.clock_owner(owner, clock, samples)
                if mode == "window":
                    owner.block_deadline = 4000
                failure = driver.FatalSession if mode == "swap" else (
                    driver.DeadlineExceeded)
                with resources, self.assertRaises(failure):
                    self.run_group(owner, setup, selected, clock=clock)
                self.assertLess(len(owner.calls), 6)
                complete = self.group_path(root) / "complete.json"
                self.assertFalse(complete.exists())
                if mode == "swap":
                    self.assertTrue(owner.stop_all.called)
                    final = json.loads((root / "resources.jsonl").read_text()
                                       .splitlines()[-1])
                    self.assertEqual(final["swapouts_start"], 100)
                    self.assertGreater((final["swapouts"] - 100) * pagesize,
                                       driver.SWAP_GROWTH_LIMIT)

    def test_fatal_keeps_partial_group_evidence(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner, setup, selected, _ = self.fixture(root)
            start = owner.start

            def fatal(args, directory, env, cwd):
                if owner.calls:
                    raise driver.FatalSession("owned isolation lost")
                return start(args, directory, env, cwd)

            owner.start = fatal
            with self.assertRaises(driver.FatalSession):
                self.run_group(owner, setup, selected)
            self.assertEqual(owner.calls, [selected[0].name])
            self.assertTrue(driver.result_path(root, selected[0]).is_file())
            complete = self.group_path(root) / "complete.json"
            self.assertFalse(complete.exists())


class DeadlineContractTests(unittest.TestCase):

    def bindings(self, root=Path("/work")):
        return {block: [{
            "id": block + "-fixture", "argv": ["/usr/bin/true"],
            "source_pin": "1" * 40, "config_pin": "2" * 64,
            "input_pin": "3" * 64,
            "receipt_path": str(root / "control" / (block + ".json")),
            "minutes": 1, "owner": "fixture-only",
            "kind": driver.BLOCK_KINDS[block],
            "estimated_peak_bytes": driver.GIB, "gomaxprocs": 10,
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
                          "receipt_path", "minutes", "owner", "id", "kind",
                          "estimated_peak_bytes", "gomaxprocs"):
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
            ("kind", "oracle"), ("gomaxprocs", True),
            ("estimated_peak_bytes", -1),
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
                patch.object(driver, "eligible_sdk", side_effect=
                    lambda owner, rows, setup, bindings, stage="timing": [
                        j for j in driver.jobs() if j.stage == stage]),
                patch.object(driver, "run_catalog_hybrid",
                             return_value="completed"),
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
                    patch.object(driver, "eligible_sdk", side_effect=
                        lambda owner, rows, setup, bindings, stage="timing": [
                            j for j in driver.jobs() if j.stage == stage and
                            (stage == "screen" or
                             j.adapter == driver.FINALISTS[0])]),
                    patch.object(driver, "run_catalog_hybrid",
                                 return_value="completed") as dependent,
                    patch.object(driver, "binding_block",
                                 return_value="completed"),
                    patch.object(driver, "block_files", return_value={}),
                ):
                    result = driver.execute_session(
                        owner, admission, root / "inputs", {}, 1)
                self.assertEqual(result["blocks"]["storage"]["state"],
                                 "partial")
                self.assertEqual(len(timed), 6 if oracle_passed else 0)
                self.assertEqual(dependent.call_count, 2)
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
            _, setup, _ = fixture_references(root, setup)
            first = FixtureSupervisor(root, ["pass"], interrupt_at=2)
            completed = []
            with self.assertRaises(driver.DeadlineExceeded):
                cases.execute(first, selected, setup, completed)
            jobs = [{"id": job.name, "gomaxprocs": 10,
                     "receipt_path": str(root / "jobs" / job.name /
                                         "complete.json")} for job in selected]
            jobs.extend({"id": driver.Job("oracle", "oracle", t).name,
                         "gomaxprocs": 10} for t in driver.ARCHIVES)
            blocks = [{"id": "setup", "jobs": []},
                      {"id": "storage", "jobs": jobs}]
            admission = {"blocks": blocks}
            driver.atomic_json(root / "session.json", {
                "identity": {"configuration": admission,
                             "setup": setup["identity"],
                             "native": first.native_identity},
                "blocks": {"setup": {"state": "completed", "files": {}},
                           "storage": {"state": "partial",
                                       "reason_code": "time", "files": {}}}})
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
    def setUp(self):
        self.bound_tool = driver.external_tool
        tool = patch.object(driver, "external_tool",
                            side_effect=lambda name, env: name)
        tool.start()
        self.addCleanup(tool.stop)

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
        with self.assertRaises(driver.DeadlineExceeded):
            driver.window_limit({**go, "user_go": False}, "1" * 64, 1000)
        for key in ("migration_complete", "owner_stop_confirmed",
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
                   ("ac_power", None), ("page_size", 0),
                   ("page_size", True), ("swap_usage", ""),
                   ("load5", float("nan")), ("load15", -1))
        for key, value in invalid:
            with self.subTest(key=key, value=value), self.assertRaises(
                    driver.FatalSession):
                driver.resource_limit({**sample, key: value}, 0, 7,
                                      admission=True)
        with self.assertRaises(driver.DeadlineExceeded):
            driver.resource_limit({**sample, "ac_power": False}, 0, 7,
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

            with (patch.object(driver, "external_tool", side_effect=(
                    lambda name, env: "/bin/launchctl" if name == "launchctl"
                    else name)),
                  patch.object(driver.subprocess, "run", side_effect=probe)):
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
                    expected = (driver.DeadlineExceeded
                                if path == owner.go_file
                                else driver.FatalSession)
                    reason = ("current window go changed"
                              if path == owner.go_file
                              else "identities changed")
                    with self.assertRaisesRegex(expected, reason):
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
        for failure in ("revoked", "expired", "ac", "memory", "swap"):
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
                    owner.permissions.side_effect = driver.DeadlineExceeded(
                        "current user go revoked")
                elif failure == "expired":
                    owner.permissions.side_effect = driver.DeadlineExceeded(
                        "current user window expired")
                elif failure == "ac":
                    sample["ac_power"] = False
                elif failure == "memory":
                    sample["level"] = 19
                else:
                    sample["swapouts"] = 8 + (
                        driver.SWAP_GROWTH_LIMIT // sample["page_size"])
                live = {8400: {(8400, 10): 1}}
                error_type = (driver.DeadlineExceeded
                              if failure in {"revoked", "expired", "ac"}
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
        for revoke, bound in ((False, False), (True, False),
                              (False, True), (True, True)):
            with self.subTest(revoke=revoke, bound=bound), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder)
                directory = root / "case"
                directory.mkdir()
                owner = fixture_owner(root)
                env = {**driver.environment(root), "GOMAXPROCS": "10"}
                tools = {}
                if bound:
                    owner.manifest_file, tools, tool_env = fixture_tools(root)
                    env.update(tool_env)
                    owner.native_identity["input_manifest_sha256"] = (
                        driver.digest(owner.manifest_file))
                argv = ["go"] if bound else ["/fixture/private-binary"]
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
                with (patch.object(driver, "external_tool",
                                   wraps=self.bound_tool),
                      patch.object(driver.time, "monotonic",
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
                        with self.assertRaises(driver.DeadlineExceeded):
                            owner.start(argv, directory,
                                        env, root)
                        spawn.assert_not_called()
                        self.assertTrue(log.closed)
                    else:
                        owned = owner.start(
                            argv, directory,
                            env, root)
                        self.assertEqual(owned.known, {(8500, 123456789)})
                        self.assertEqual(spawn.call_args.args[0][0],
                            tools["go"] if bound else argv[0])
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
            setup["artifacts"]["bin/oracle"] = driver.digest(
                root / "bin/oracle")
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
                _, setup, _ = fixture_references(root, setup)
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
                config["blocks"][1]["jobs"].extend(
                    {"id": driver.Job("oracle", "oracle", tier).name,
                     "gomaxprocs": 10} for tier in driver.ARCHIVES)
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
                    # Verify per-job receipts after a time interruption.
                    record["blocks"]["storage"].update(
                        state="partial", reason_code="time")
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
        _, setup, _ = fixture_references(root, setup)
        owner = FixtureSupervisor(root, ["pass", "pass"])
        receipts = []
        cases.execute(owner, selected, setup, receipts)
        blocks = [{"id": "setup", "jobs": []}, {"id": "storage", "jobs": [
            {"id": job.name, "gomaxprocs": 10,
             "receipt_path": str(root / "jobs" / job.name / "complete.json")}
            for job in selected]}]
        blocks[1]["jobs"].extend(
            {"id": driver.Job("oracle", "oracle", tier).name,
             "gomaxprocs": 10} for tier in driver.ARCHIVES)
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
        for damage in ("raw-time", "missing-receipt"):
            with self.subTest(damage=damage), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder).resolve()
                selected, setup, admission, old = self.sdk_session(root)
                damaged = root / "jobs" / selected[1].name
                if damage == "raw-time":
                    (damaged / "native-time.txt").write_text("changed")
                else:
                    (damaged / "complete.json").unlink()
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

    # A1 review H2 changed torn receipts to unexpected failures.
    # Group coverage:
    # test_torn_driver_receipt_or_unexpected_proof_bug_is_not_a_retry.
    def test_torn_completed_driver_receipt_is_terminal_without_retry(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            selected, setup, admission, _ = self.sdk_session(root)
            path = root / "jobs" / selected[1].name / "complete.json"
            path.write_bytes(b"{")
            before = {p: p.read_bytes() for p in
                      driver.output_files([root / "jobs"])}
            owner = FixtureSupervisor(root, [])
            owner.window = {"id": "resumed-window"}
            with self.assertRaises(json.JSONDecodeError):
                self.resume_sdk(owner, selected, setup, admission)
            self.assertEqual(owner.calls, [])
            self.assertEqual(path.read_bytes(), b"{")
            after = {p: p.read_bytes() for p in
                     driver.output_files([root / "jobs"])}
            self.assertEqual(after, before)
            self.assertFalse((root / "attempts").exists())
            saved = driver.read_json(root / "session.json")
            self.assertEqual(saved["blocks"]["storage"]["terminal"], "fatal")
            self.assertEqual(saved["blocks"]["storage"]["reason_code"],
                             "JSONDecodeError")

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

    def test_binding_negative_resume_keeps_complete_and_refuses_partial(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            cases = ReviewFixTests()
            self.addCleanup(lambda: self.assertTrue(cases.doCleanups()))
            owner, admission = cases.session_fixture(root, "partial")
            self.addCleanup(owner.stop_all)
            owner.window = {"id": "first-window"}
            with (patch.object(driver.signal, "signal"),
                  patch.object(driver.signal, "setitimer")):
                first = driver.execute_session(owner, admission,
                                               root / "inputs", {}, 1)
                self.assertTrue((root /
                    "aux/smoke-fixture/partial-complete.json").is_file())
                paths = driver.output_files([root / "aux"])
                paths += [root / "session.json", root /
                          "aux/calibration-fixture/complete.json"]
                saved = {p: p.read_bytes() for p in paths}
                owner.window = {"id": "new-go-window"}
                with patch.object(owner, "begin_block",
                                  wraps=owner.begin_block) as begin:
                    second = driver.execute_session(owner, admission,
                                                    root / "inputs", {}, 1)
                begin.assert_not_called()
                self.assertEqual(second, first)
            self.assertEqual(first["blocks"]["smoke"]["state"], "partial")
            self.assertEqual(first["blocks"]["smoke"]["reason_code"], "runner")
            self.assertEqual(owner.calls, ["smoke-fixture",
                                           "calibration-fixture"])
            self.assertFalse((root / "attempts").exists())
            self.assertEqual({p: p.read_bytes() for p in saved}, saved)
            complete = root / "aux/calibration-fixture/complete.json"
            self.assertIn(str(complete.relative_to(root)),
                          first["blocks"]["calibration"]["files"])
            self.assertEqual(driver.read_json(complete)["attempt"][
                "window_id"], "first-window")

    def test_binding_time_partial_restarts_only_with_a_new_attempt(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            cases = ReviewFixTests()
            self.addCleanup(lambda: self.assertTrue(cases.doCleanups()))
            owner, admission = cases.session_fixture(root, "completed")
            self.addCleanup(owner.stop_all)
            owner.window = {"id": "first-window"}
            start = owner.start
            stopped = False

            def interrupt(args, directory, env, cwd):
                nonlocal stopped
                if directory.name == "smoke-fixture" and not stopped:
                    stopped = True
                    owner.calls.append(directory.name)
                    raise driver.DeadlineExceeded("fixture time interruption")
                return start(args, directory, env, cwd)

            owner.start = interrupt
            with (patch.object(driver.signal, "signal"),
                  patch.object(driver.signal, "setitimer")):
                first = driver.execute_session(owner, admission,
                                               root / "inputs", {}, 1)
                directory = root / "aux/smoke-fixture"
                self.assertFalse((directory / "partial.json").exists())
                saved = {str(p.relative_to(directory)): p.read_bytes()
                         for p in driver.output_files([directory])}
                owner.window = {"id": "new-go-window"}
                second = driver.execute_session(owner, admission,
                                                root / "inputs", {}, 1)
            self.assertEqual(first["blocks"]["smoke"]["reason_code"], "time")
            self.assertEqual(second["blocks"]["smoke"]["state"], "completed")
            self.assertEqual(owner.calls, ["smoke-fixture",
                                           "calibration-fixture",
                                           "smoke-fixture"])
            archives = list((root / "attempts/aux/smoke-fixture").iterdir())
            self.assertEqual(len(archives), 1)
            self.assertEqual({name: (archives[0] / name).read_bytes()
                              for name in saved}, saved)
            complete = driver.read_json(directory / "complete.json")
            self.assertEqual(complete["attempt"]["window_id"], "new-go-window")
            calibration = driver.read_json(root /
                "aux/calibration-fixture/complete.json")
            self.assertEqual(calibration["attempt"]["window_id"],
                             "first-window")

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
            owner = fixture_owner(root, setup=False)
            owner.manifest_file, tools, owner.environment = fixture_tools(root)
            inputs = root / "inputs"
            inputs.mkdir()
            archive = inputs / "tier1.tar.zst"
            archive.write_bytes(b"fixture")
            pin = {"sha256": driver.digest(archive), "bytes": 7, "queries": 1}
            driver.atomic_json(inputs / "tiers-summary.json", {"tier1": {
                "archive_sha256": pin["sha256"], "archive_bytes": 7,
                "queries": 1}})
            with (patch.object(driver, "ARCHIVES", {"tier1": pin}),
                  patch.object(driver, "copy_tree", return_value={}),
                  patch.object(owner, "run", side_effect=[
                      SimpleNamespace(read_text=lambda: "version") for _ in
                      range(4)] + [RuntimeError("stop before source clone")])
                  as run):
                with self.assertRaisesRegex(RuntimeError, "source clone"):
                    driver.prepare(owner, inputs, {})
            versions = driver.read_json(root / "tool-versions.json")
            self.assertEqual(versions["time"], {
                "path": tools["time"],
                "sha256": driver.digest(Path(tools["time"]))})
            self.assertEqual([call.args[0] for call in
                              run.call_args_list[:4]], [
                [tools["go"], "version"], [tools["cmake"], "--version"],
                [tools["c++"], "--version"], [tools["git"], "--version"]])


    def test_interrupted_setup_retention_and_fatal_stop(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner = fixture_owner(root, setup=False)
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


class FailureBoundaryTests(unittest.TestCase):
    def test_go_removal_before_spawn_is_not_a_missing_executable(self):
        for mode in ("before-hash", "after-hash", "executable"):
            with (self.subTest(mode=mode),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder).resolve()
                directory = root / "case"
                directory.mkdir()
                owner = fixture_owner(root)
                owner.go_file = root / "go.json"
                driver.atomic_json(owner.go_file, NativeAdmissionTests().go(
                    owner.native_identity["input_manifest_sha256"]))
                owner.go_sha = driver.digest(owner.go_file)
                owner.case = {"gomaxprocs": 10}
                owner.block_id, owner.swapouts = "catalog", 7
                owner.stop_all = unittest.mock.Mock()
                admissions = []
                digest, open_file = driver.digest, Path.open
                log = StringIO()
                missing = FileNotFoundError(driver.errno.ENOENT,
                                            "missing executable", "/missing")
                def guard(*, admission=False):
                    if admission:
                        admissions.append(True)
                    if mode == "before-hash" and len(admissions) == 2:
                        owner.go_file.unlink()
                def hashed(path):
                    result = digest(path)
                    if mode == "after-hash" and path == owner.go_file:
                        owner.go_file.unlink()
                    return result
                def opened(path, *args, **kwargs):
                    if path == directory / "stdout-stderr.log":
                        return log
                    return open_file(path, *args, **kwargs)
                owner.guard = guard
                expected = (FileNotFoundError if mode == "executable"
                            else driver.DeadlineExceeded)
                with (patch.object(driver, "digest", side_effect=hashed),
                      patch.object(Path, "open", new=opened),
                      patch.object(driver.time, "monotonic", return_value=0),
                      patch.object(driver.time, "time", return_value=1000),
                      patch.object(driver.signal, "pthread_sigmask",
                                   return_value=set()),
                      patch.object(driver.subprocess, "Popen",
                                   side_effect=missing) as spawn,
                      self.assertRaises(expected) as caught):
                    owner.start(["/missing"], directory,
                        {**driver.environment(root), "GOMAXPROCS": "10"}, root)
                if mode == "executable":
                    self.assertIs(caught.exception, missing)
                    spawn.assert_called_once()
                else:
                    spawn.assert_not_called()
                    self.assertIsInstance(caught.exception.__cause__,
                                          FileNotFoundError)
                self.assertTrue(log.closed)
                self.assertEqual(owner.active, [])
                owner.stop_all.assert_called_once()

    def test_go_removed_between_permission_hash_and_read_is_time(self):
        for full in (True, False):
            with (self.subTest(full=full),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder).resolve()
                owner = fixture_owner(root)
                owner.go_file = root / "go.json"
                owner.off_file = root / "off.json"
                owner.off, owner.next_d12_check = {}, 10000
                driver.atomic_json(owner.off_file, owner.off)
                owner.native_identity["daemon_off_identities_sha256"] = (
                    driver.digest(owner.off_file))
                driver.atomic_json(owner.go_file, NativeAdmissionTests().go(
                    owner.native_identity["input_manifest_sha256"]))
                owner.go_sha = driver.digest(owner.go_file)
                digest = driver.digest
                def removed_after_hash(path):
                    value = digest(path)
                    if path == owner.go_file:
                        owner.go_file.unlink()
                    return value
                with (patch.object(driver, "digest",
                                   side_effect=removed_after_hash),
                      patch.object(driver.time, "monotonic", return_value=0),
                      patch.object(driver, "daemon_off") as probe,
                      self.assertRaises(driver.DeadlineExceeded) as caught):
                    owner.permissions(full=full)
                self.assertIsInstance(caught.exception.__cause__,
                                      FileNotFoundError)
                probe.assert_not_called()
                self.assertFalse(owner.go_file.exists())

    def test_negative_markers_still_precede_a_torn_completion(self):
        for name in ("partial.json", "budget-exceeded.json"):
            with (self.subTest(marker=name),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder).resolve()
                directory = root / "jobs/case"
                directory.mkdir(parents=True)
                (directory / "complete.json").write_bytes(b"{")
                driver.atomic_json(directory / name,
                                   {"state": "checks_failed"})
                before = {p: p.read_bytes() for p in directory.iterdir()}
                verify = unittest.mock.Mock(
                    side_effect=driver.ReviewRequired("known proof failure"))
                with self.assertRaises(driver.ReviewRequired):
                    driver.reusable(directory, verify)
                after = {p: p.read_bytes() for p in directory.iterdir()}
                self.assertEqual(after, before)
                self.assertFalse((root / "attempts").exists())

    def test_unknown_cache_errors_propagate_in_both_modes_without_writes(self):
        for strict in (False, True):
            for error in (KeyError("bug"), TypeError("bug"), ValueError("bug"),
                          PermissionError("unknown I/O")):
                with (self.subTest(strict=strict, error=type(error).__name__),
                      tempfile.TemporaryDirectory() as folder):
                    root = Path(folder).resolve()
                    directory = root / "jobs/case"
                    directory.mkdir(parents=True)
                    verify = unittest.mock.Mock(side_effect=error)
                    with self.assertRaises(type(error)) as caught:
                        driver.reusable(directory, verify, strict=strict)
                    self.assertIs(caught.exception, error)
                    self.assertEqual(list(directory.iterdir()), [])
                    self.assertFalse((root / "attempts").exists())

    def test_expected_cache_misses_still_allow_fresh_work(self):
        for strict in (False, True):
            for error in (driver.ReviewRequired("proof"),
                          FileNotFoundError("missing")):
                with (self.subTest(strict=strict, error=type(error).__name__),
                      tempfile.TemporaryDirectory() as folder):
                    directory = Path(folder).resolve() / "jobs/case"
                    directory.mkdir(parents=True)
                    self.assertIsNone(driver.reusable(directory,
                        unittest.mock.Mock(side_effect=error), strict=strict))
                    self.assertEqual(driver.read_json(
                        directory / "partial.json")["state"], "partial")

    def test_collector_bugs_are_persisted_fatal_and_never_resume(self):
        for error in (TypeError("collector bug"), MemoryError("collector OOM"),
                      driver.FatalSession("collector drift")):
            with (self.subTest(error=type(error).__name__),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder).resolve()
                owner = fixture_owner(root)
                owner.begin_block = unittest.mock.Mock()
                owner.stop_all = unittest.mock.Mock()
                blocks = [{"id": "smoke", "jobs": []},
                          {"id": "calibration", "jobs": []}]
                with (patch.object(driver.signal, "signal"),
                      patch.object(driver.signal, "setitimer"),
                      patch.object(driver.time, "monotonic", return_value=0),
                      patch.object(driver, "binding_block",
                                   return_value="completed") as work,
                      patch.object(driver, "block_files", side_effect=error)):
                    with self.assertRaises(type(error)):
                        driver.execute_session(owner, {"blocks": blocks},
                                               root, {}, 1)
                    work.assert_called_once_with(owner, blocks[0])
                    saved = driver.read_json(root / "session.json")
                    self.assertEqual(saved["blocks"]["smoke"]["terminal"],
                                     "fatal")
                    self.assertNotIn("calibration", saved["blocks"])
                    owner.begin_block.reset_mock()
                    work.reset_mock()
                    with self.assertRaises(driver.FatalSession):
                        driver.execute_session(owner, {"blocks": blocks},
                                               root, {}, 1)
                    owner.begin_block.assert_not_called()
                    work.assert_not_called()

    def test_expected_collector_rejections_remain_block_local(self):
        for error, reason in ((driver.ReviewRequired("proof"), "receipt"),
                              (driver.DeadlineExceeded("clock"), "time")):
            with (self.subTest(reason=reason),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder).resolve()
                owner = fixture_owner(root)
                owner.begin_block = unittest.mock.Mock()
                owner.stop_all = unittest.mock.Mock()
                admission = {"blocks": [{"id": "smoke", "jobs": []},
                                        {"id": "calibration", "jobs": []}]}
                with (patch.object(driver.signal, "signal"),
                      patch.object(driver.signal, "setitimer"),
                      patch.object(driver.time, "monotonic", return_value=0),
                      patch.object(driver, "binding_block",
                                   return_value="completed"),
                      patch.object(driver, "block_files", side_effect=[error,
                                                                     {}])):
                    result = driver.execute_session(owner, admission,
                                                    root, {}, 1)
                first, second = (result["blocks"][name]
                                 for name in ("smoke", "calibration"))
                self.assertEqual((first["state"], first["reason_code"]),
                                 ("partial", reason))
                self.assertNotIn("terminal", first)
                self.assertEqual(second["state"], "completed")

    def test_removed_go_is_time_like_but_identity_change_is_fatal(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner = fixture_owner(root)
            owner.go_file, owner.off_file = root / "go.json", root / "off.json"
            owner.off = {}
            driver.atomic_json(owner.off_file, owner.off)
            owner.native_identity["daemon_off_identities_sha256"] = (
                driver.digest(owner.off_file))
            go = NativeAdmissionTests().go(
                owner.native_identity["input_manifest_sha256"])
            driver.atomic_json(owner.go_file, go)
            owner.go_sha = driver.digest(owner.go_file)
            with (patch.object(driver, "daemon_off"),
                  patch.object(driver.time, "time", return_value=1000)):
                owner.permissions()
                owner.go_file.unlink()
                with self.assertRaises(driver.DeadlineExceeded):
                    owner.permissions()
                driver.atomic_json(owner.go_file, {**go, "window_id": "new"})
                owner.go_sha = driver.digest(owner.go_file)
                owner.permissions()
                self.assertEqual(owner.window["id"], "new")
                owner.off_file.write_text("changed")
                with self.assertRaises(driver.FatalSession):
                    owner.permissions()

    def test_exhausted_receipt_deadline_is_time_and_can_resume(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner = fixture_owner(root)
            owner.begin_block = unittest.mock.Mock()
            owner.stop_all = unittest.mock.Mock()
            admission = {"blocks": [{"id": "smoke", "jobs": []}]}
            with (patch.object(driver.signal, "signal"),
                  patch.object(driver.signal, "setitimer"),
                  patch.object(driver.time, "monotonic",
                               side_effect=[0, 10000, 0, 0]),
                  patch.object(driver, "binding_block",
                               return_value="completed") as work,
                  patch.object(driver, "block_files", return_value={})):
                first = driver.execute_session(owner, admission, root, {}, 1)
                self.assertEqual(first["blocks"]["smoke"]["reason_code"],
                                 "time")
                owner.window = {"id": "new-go"}
                second = driver.execute_session(owner, admission, root, {}, 1)
            self.assertEqual(second["blocks"]["smoke"]["state"], "completed")
            self.assertEqual(work.call_count, 2)

    def test_later_time_stop_preserves_a_known_worker_partial(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            jobs = [{"id": name, "argv": [], "receipt_path": "unused",
                     **{key: "a" * 64 for key in
                        ("source_pin", "config_pin", "input_pin")}}
                    for name in ("known", "timed")]
            def auxiliary(owner, name, argv, validate, **options):
                directory = root / "aux" / name
                directory.mkdir(parents=True)
                data = directory / "raw.json"
                data.write_bytes(b"evidence")
                driver.atomic_json(directory / "receipt.json", {
                    **{key: "a" * 64 for key in
                       ("source_pin", "config_pin", "input_pin")},
                    "state": "partial", "exit_code": 1,
                    "reason_code": "worker" if name == "known" else "time",
                    "clean_handoff": True,
                    "files": {data.name: driver.digest(data)}})
                validate(directory)
                result = {"exit_code": 1, "state": "pass"}
                driver.atomic_json(directory / "complete.json", result)
                return result
            with patch.object(driver, "auxiliary", side_effect=auxiliary):
                self.assertEqual(driver.binding_block(
                    SimpleNamespace(root=root),
                    {"id": "smoke", "jobs": jobs}), "partial")
            for name in ("known", "timed"):
                self.assertTrue((root / "aux" / name /
                                 "partial-complete.json").is_file())

    def test_verified_worker_time_receipt_remains_resumable(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            directory = root / "aux/worker"
            directory.mkdir(parents=True)
            data = directory / "raw.json"
            data.write_bytes(b"time evidence")
            path = directory / "receipt.json"
            pins = {name: "a" * 64 for name in
                    ("source_pin", "config_pin", "input_pin")}
            driver.atomic_json(path, {**pins, "state": "partial",
                "reason_code": "time", "exit_code": 1, "clean_handoff": True,
                "files": {data.name: driver.digest(data)}})
            before = path.read_bytes()
            def auxiliary(owner, name, argv, validate, **options):
                validate(directory)
                result = {"state": "pass", "exit_code": 1}
                driver.atomic_json(directory / "complete.json", result)
                return result
            job = {"id": "worker", "argv": [], "receipt_path": str(path),
                   **pins}
            with (patch.object(driver, "auxiliary", side_effect=auxiliary),
                  self.assertRaises(driver.DeadlineExceeded)):
                driver.binding_block(SimpleNamespace(root=root),
                                     {"id": "smoke", "jobs": [job]})
            self.assertEqual(path.read_bytes(), before)
            self.assertTrue((directory / "partial-complete.json").is_file())
            self.assertFalse((directory / "complete.json").exists())

    def test_time_partial_resumes_after_owned_stop_in_a_new_window(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner = fixture_owner(root)
            owner.begin_block = unittest.mock.Mock()
            owner.stop_all = unittest.mock.Mock()
            admission = {"blocks": [{"id": "smoke", "jobs": []}]}
            with (patch.object(driver.signal, "signal"),
                  patch.object(driver.signal, "setitimer"),
                  patch.object(driver.time, "monotonic", return_value=0),
                  patch.object(driver, "block_files", return_value={}),
                  patch.object(driver, "binding_block", side_effect=[
                      driver.DeadlineExceeded("AC/go stop"), "completed"])):
                first = driver.execute_session(owner, admission, root, {}, 1)
                self.assertEqual(first["blocks"]["smoke"]["reason_code"],
                                 "time")
                self.assertNotIn("terminal", first["blocks"]["smoke"])
                owner.stop_all.assert_called_once()
                owner.window = {"id": "new-go"}
                second = driver.execute_session(owner, admission, root, {}, 1)
            self.assertEqual(second["blocks"]["smoke"]["state"], "completed")
            self.assertEqual(owner.begin_block.call_count, 2)
            self.assertEqual(owner.stop_all.call_count, 2)


class KnownOutcomeTests(unittest.TestCase):
    def test_saved_semantic_partial_allows_independent_time_resume(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner = fixture_owner(root)
            owner.stop_all = unittest.mock.Mock()
            owner.start = unittest.mock.Mock()
            blocks = [{"id": "smoke", "jobs": []},
                      {"id": "calibration", "jobs": []}]
            admission = {"blocks": blocks}
            proof = root / "known-negative.json"
            proof.write_bytes(b'{"state": "partial"}\n')
            before = proof.read_bytes()
            saved = {"state": "partial", "reason_code": "worker",
                     "reason": "known stop", "clean_handoff": True,
                     "files": {proof.name: driver.digest(proof)}}
            path = root / "session.json"
            driver.atomic_json(path, {"identity": {
                "configuration": admission, "setup": {},
                "native": owner.native_identity}, "blocks": {
                    "smoke": saved, "calibration": {
                        "state": "partial", "reason_code": "time",
                        "files": {}}}})
            begun = []

            def begin(block, outcomes):
                begun.append((block["id"], dict(outcomes)))
                owner.block_id = block["id"]

            owner.begin_block = begin
            with (patch.object(driver.signal, "signal"),
                  patch.object(driver.signal, "setitimer"),
                  patch.object(driver.time, "monotonic", return_value=0),
                  patch.object(driver, "binding_block",
                               return_value="completed") as work,
                  patch.object(driver, "block_files", return_value={})):
                result = driver.execute_session(owner, admission, root, {}, 1)
            self.assertEqual(begun, [("calibration", {"smoke": "partial"})])
            work.assert_called_once_with(owner, blocks[1])
            owner.start.assert_not_called()
            self.assertEqual(result["blocks"]["smoke"], saved)
            self.assertEqual(proof.read_bytes(), before)
            self.assertEqual(result["blocks"]["calibration"]["state"],
                             "completed")
            self.assertEqual(driver.read_json(path), result)

    def test_oracle_time_resume_releases_the_previously_blocked_segment(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner = fixture_owner(root)
            owner.stop_all = unittest.mock.Mock()
            oracles = [driver.Job("oracle", "oracle", tier)
                       for tier in driver.ARCHIVES]
            timing = [driver.Job("timing", driver.FINALISTS[0], tier, 1)
                      for tier in driver.ARCHIVES]
            screens = [driver.Job("screen", driver.FINALISTS[0], tier)
                       for tier in driver.ARCHIVES]
            admission = {"blocks": [
                {"id": "storage", "jobs": [{"id": job.name}
                                            for job in oracles + screens]},
                {"id": "segment_timing", "jobs": []}]}
            phase, stages = 1, []

            def begin(block, outcomes):
                owner.block_id = block["id"]

            def stage(supervisor, selected, setup, concurrency, rows):
                if not selected:
                    return
                kind = selected[0].stage
                stages.append((phase, kind))
                if kind == "oracle":
                    if phase == 1:
                        raise driver.DeadlineExceeded("tier1 oracle time stop")
                    rows.extend({"identity": {"job": driver.asdict(job)},
                                 "state": "pass", "correctness_pass": True}
                                for job in selected)

            owner.begin_block = begin
            with (patch.object(driver, "jobs",
                               return_value=oracles + screens + timing),
                  patch.object(driver, "execute_stage", side_effect=stage),
                  patch.object(driver, "eligible_sdk",
                               side_effect=lambda *args: screens
                               if len(args) == 5 else timing) as eligible,
                  patch.object(driver, "block_files", return_value={}),
                  patch.object(driver.time, "monotonic", return_value=0),
                  patch.object(driver.signal, "signal"),
                  patch.object(driver.signal, "setitimer")):
                owner.window = {"id": "first-window"}
                first = driver.execute_session(owner, admission, root, {}, 1)
                self.assertEqual(first["blocks"]["storage"]["state"],
                                 "partial")
                self.assertEqual(first["blocks"]["storage"]["reason_code"],
                                 "time")
                self.assertEqual(first["blocks"]["segment_timing"]["state"],
                                 "dependency_blocked")
                self.assertEqual(stages, [(1, "oracle")])
                eligible.assert_not_called()
                phase = 2
                owner.window = {"id": "new-go-window"}
                second = driver.execute_session(owner, admission, root, {}, 1)
            self.assertEqual(second["blocks"]["storage"]["state"], "completed")
            self.assertEqual(second["blocks"]["segment_timing"]["state"],
                             "completed")
            self.assertEqual(stages, [(1, "oracle"), (2, "oracle"),
                                      (2, "screen"), (2, "timing")])
            self.assertEqual(eligible.call_count, 2)
            self.assertEqual(eligible.call_args_list[0].args[-1], "screen")
            self.assertEqual(driver.read_json(root / "session.json"), second)

    def test_storage_raw_rejections_are_saved_without_a_retry(self):
        for damage in ("missing", "malformed", "manifest"):
            with (self.subTest(damage=damage),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder).resolve()
                cases = DriverTests()
                selected, setup, _ = cases.fixture(root)
                selected = selected[:1]
                owner = FixtureSupervisor(root, ["pass"])
                start = owner.start

                def damaged(args, directory, env, cwd):
                    owned = start(args, directory, env, cwd)
                    path = driver.result_path(root, selected[0])
                    if damage == "missing":
                        path.unlink()
                    elif damage == "malformed":
                        path.write_text("{")
                    else:
                        report = driver.read_json(path)
                        report["manifest_sha256"] = "0" * 64
                        driver.atomic_json(path, report)
                    return owned

                owner.start = damaged
                with self.assertRaises(driver.ReviewRequired):
                    cases.execute(owner, selected, setup, [])
                directory = root / "jobs" / selected[0].name
                marker = driver.read_json(directory / "partial.json")
                self.assertEqual(marker["state"], "checks_failed")
                expected = ("manifest digest" if damage == "manifest"
                            else "missing or malformed JSON")
                self.assertIn(expected, marker["reason"])
                saved = {p: p.read_bytes() for p in
                         driver.output_files([directory])}
                resumed = FixtureSupervisor(root, [])
                resumed.window = {"id": "new-go-window"}
                with self.assertRaisesRegex(driver.ReviewRequired,
                                            "known negative"):
                    cases.execute(resumed, selected, setup, [])
                self.assertEqual(resumed.calls, [])
                self.assertEqual({p: p.read_bytes() for p in saved}, saved)
                self.assertFalse((root / "attempts").exists())

    def test_auxiliary_launch_and_validation_negatives_never_retry(self):
        for boundary in ("launch", "validation"):
            with (self.subTest(boundary=boundary),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder).resolve()
                (root / "python-runtime.json").write_text("{}")
                owner = fixture_owner(root)
                owner.job_minutes = {"case": 1}
                owner.job_bindings = {"case": {
                    "gomaxprocs": 1, "kind": "coderank"}}
                owned = SimpleNamespace(process=fixture_process(9501, 0),
                                        started=0, peak_rss=0)
                owner.start = unittest.mock.Mock(return_value=owned)
                owner.close = unittest.mock.Mock()
                reject = driver.ReviewRequired("original recipe rejection")
                validate = unittest.mock.Mock(side_effect=reject)
                if boundary == "launch":
                    owner.start.side_effect = reject
                with patch.object(driver.time, "monotonic", return_value=0):
                    with self.assertRaises(driver.ReviewRequired) as caught:
                        driver.auxiliary(
                            owner, "case", ["never-run"], validate)
                    self.assertIs(caught.exception, reject)
                    directory = root / "aux/case"
                    self.assertEqual(driver.read_json(
                        directory / "partial.json"), {
                            "state": "checks_failed",
                            "reason": "original recipe rejection"})
                    saved = {p: p.read_bytes() for p in
                             driver.output_files([directory])}
                    with (patch.object(driver, "new_attempt") as archive,
                          self.assertRaisesRegex(driver.ReviewRequired,
                                                 "known negative")):
                        driver.auxiliary(
                            owner, "case", ["never-run"], validate)
                    archive.assert_not_called()
                owner.start.assert_called_once()
                self.assertEqual(validate.call_count,
                                 int(boundary == "validation"))
                self.assertEqual({p: p.read_bytes() for p in saved}, saved)
                self.assertFalse((directory / "complete.json").exists())
                self.assertFalse((root / "attempts").exists())

    def test_writer_preserves_first_outcome_and_excludes_interruptions(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            directory = root / "jobs/case"
            directory.mkdir(parents=True)
            path = directory / "partial.json"
            driver.failed_output(directory, driver.ReviewRequired("first"))
            saved = path.read_bytes()
            driver.failed_output(directory, driver.ReviewRequired("second"))
            self.assertEqual(path.read_bytes(), saved)
            self.assertEqual(driver.read_json(path), {
                "state": "checks_failed", "reason": "first"})
            path.unlink()
            for error in (driver.DeadlineExceeded("time"),
                          driver.BudgetExceeded(2, 1)):
                driver.failed_output(directory, error)
                self.assertFalse(path.exists())
            driver.failed_output(directory, driver.FatalSession("host stop"))
            self.assertEqual(driver.read_json(path)["state"], "fatal")
            path.unlink()
            foreign = root / "foreign"
            foreign.write_bytes(b"owned elsewhere")
            path.symlink_to(foreign)
            with self.assertRaises(driver.FatalSession):
                driver.failed_output(directory, driver.ReviewRequired("bad"))
            self.assertTrue(path.is_symlink())
            self.assertEqual(foreign.read_bytes(), b"owned elsewhere")

    def test_non_strict_reuse_preserves_saved_negative_and_time_markers(self):
        for state in ("checks_failed", "fatal", "partial"):
            with (self.subTest(state=state),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder).resolve()
                directory = root / "jobs/case"
                directory.mkdir(parents=True)
                path = directory / "partial.json"
                path.write_text(json.dumps({"state": state,
                                           "reason": "original bytes"}))
                saved = path.read_bytes()
                verify = unittest.mock.Mock(side_effect=FileNotFoundError(
                    "no completion for this attempt"))
                if state == "partial":
                    self.assertIsNone(driver.reusable(directory, verify))
                    verify.assert_called_once_with()
                else:
                    with self.assertRaisesRegex(driver.ReviewRequired,
                                                "known negative"):
                        driver.reusable(directory, verify)
                    verify.assert_not_called()
                self.assertEqual(path.read_bytes(), saved)
                self.assertFalse((root / "attempts").exists())

    def test_cache_deadline_never_demotes_a_complete_unit(self):
        for strict in (False, True):
            with (self.subTest(strict=strict),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder).resolve()
                directory = root / "jobs/case"
                directory.mkdir(parents=True)
                path = directory / "complete.json"
                driver.atomic_json(path, {"state": "pass"})
                saved = path.read_bytes()
                deadline = driver.DeadlineExceeded("expired during hashing")
                verify = unittest.mock.Mock(side_effect=deadline)
                with self.assertRaises(driver.DeadlineExceeded) as caught:
                    driver.reusable(directory, verify, strict=strict)
                self.assertIs(caught.exception, deadline)
                self.assertEqual(path.read_bytes(), saved)
                self.assertFalse((directory / "partial.json").exists())
                self.assertFalse((root / "attempts").exists())

    def test_saved_non_pass_completion_cannot_hide_a_known_negative(self):
        for state in ("checks_failed", "budget_exceeded"):
            with (self.subTest(state=state),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder).resolve()
                directory = root / "jobs/case"
                directory.mkdir(parents=True)
                path = directory / "complete.json"
                driver.atomic_json(path, {"state": state})
                saved = path.read_bytes()
                error = driver.ReviewRequired("negative proof changed")
                verify = unittest.mock.Mock(side_effect=error)
                with self.assertRaises(driver.ReviewRequired) as caught:
                    driver.reusable(directory, verify)
                self.assertIs(caught.exception, error)
                self.assertEqual(path.read_bytes(), saved)
                self.assertFalse((directory / "partial.json").exists())

    def test_block_rerun_keeps_semantic_partials_but_rechecks_gates(self):
        cases = (("partial", "worker", False),
                 ("partial", "receipt", False),
                 ("partial", "ReviewRequired", False),
                 ("partial", "runner", False),
                 ("partial", "budget-exceeded", False),
                 ("partial", None, False),
                 ("dependency_blocked", None, True),
                 ("skipped", "gate", True),
                 ("checks_failed", None, False),
                 ("started", None, True), ("partial", "time", True))
        for state, reason, resumable in cases:
            with (self.subTest(state=state, reason=reason),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder).resolve()
                owner = fixture_owner(root)
                block = {"id": "smoke", "jobs": []}
                admission = {"blocks": [block]}
                saved = {"state": state, "files": {}}
                if state == "checks_failed":
                    saved["files"] = {"missing-failed-proof": "0" * 64}
                if reason is not None:
                    saved["reason_code"] = reason
                path = root / "session.json"
                driver.atomic_json(path, {"identity": {
                    "configuration": admission, "setup": {},
                    "native": owner.native_identity},
                    "blocks": {"smoke": saved}})
                before = path.read_bytes()
                owner.begin_block = unittest.mock.Mock()
                owner.stop_all = unittest.mock.Mock()
                with (patch.object(driver.signal, "signal"),
                      patch.object(driver.signal, "setitimer"),
                      patch.object(driver.time, "monotonic", return_value=0),
                      patch.object(driver, "binding_block",
                                   return_value="completed") as work,
                      patch.object(driver, "block_files", return_value={})):
                    if resumable:
                        result = driver.execute_session(
                            owner, admission, root, {}, 1)
                        self.assertEqual(result["blocks"]["smoke"]["state"],
                                         "completed")
                        owner.begin_block.assert_called_once()
                        self.assertEqual(owner.begin_block.call_args.args[0],
                                         block)
                        work.assert_called_once_with(owner, block)
                    else:
                        if state == "checks_failed":
                            with self.assertRaisesRegex(driver.ReviewRequired,
                                                        "known .*block"):
                                driver.execute_session(
                                    owner, admission, root, {}, 1)
                        else:
                            result = driver.execute_session(
                                owner, admission, root, {}, 1)
                            self.assertEqual(result["blocks"]["smoke"], saved)
                            self.assertEqual(
                                result["blocks"]["smoke"]["state"], "partial")
                        owner.begin_block.assert_not_called()
                        work.assert_not_called()
                        self.assertEqual(path.read_bytes(), before)


class NativeEntryTests(unittest.TestCase):
    def inputs(self, scratch, run_id="original"):
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
                  "blocks": [{"id": block, "jobs": [
                      {**job, "argv": [str(launcher)]} for job in entries]}
                      for block, entries in DeadlineContractTests().bindings(
                          scratch / "mac-run" / run_id).items()]}
        driver.atomic_json(config_path, config)
        files = {str(p): driver.digest(p) for p in (
            data, launcher, staged, config_path, identities)}
        tool = scratch / "tool"
        tool.write_bytes(b"fixture tool; never executed")
        files[str(tool)] = driver.digest(tool)
        tools = dict.fromkeys(("go cmake c++ cc make git file zstd time ps "
            "lsof sysctl vm_stat pmset launchctl lipo otool").split(),
            str(tool))
        driver.atomic_json(manifest, {
            "schema": "storage-inputs/v1", "source": driver.COMMIT,
            "files": files, "jobs": {"*": list(files)}, "homebrew": {},
            "tools": tools})
        driver.atomic_json(go, NativeAdmissionTests().go(
            driver.digest(manifest)))
        return config_path, manifest, staged

    def test_new_run_ids_same_pins_and_finally_without_go(self):
        with tempfile.TemporaryDirectory() as folder:
            scratch = Path(folder).resolve()
            config_path, manifest, staged = self.inputs(scratch)
            configs = {"original": config_path}
            base = driver.read_json(config_path)
            document = driver.read_json(manifest)
            for run_id in ("rerun", "interrupted"):
                config = deepcopy(base)
                config["blocks"] = [{"id": block, "jobs": [
                    {**job, "argv": [base["launcher"]["path"]]}
                    for job in entries]} for block, entries in
                    DeadlineContractTests().bindings(
                        scratch / "mac-run" / run_id).items()]
                path = config_path.with_name(run_id + ".json")
                driver.atomic_json(path, config)
                document["files"][str(path)] = driver.digest(path)
                document["jobs"]["*"].append(str(path))
                configs[run_id] = path
            driver.atomic_json(manifest, document)
            driver.atomic_json(Path(base["window_file"]),
                NativeAdmissionTests().go(driver.digest(manifest)))
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
                real_session(record, {**config, "blocks": []}, inputs,
                             identity, concurrency)
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
                    argv = ["run.py", mode, str(configs[run_id]),
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
                config, manifest, staged = self.inputs(scratch, "rejected")
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

    def test_unbound_configured_executable_refuses_before_native_owner(self):
        with tempfile.TemporaryDirectory() as folder:
            scratch = Path(folder).resolve()
            config, manifest, staged = self.inputs(
                scratch, "unbound-executable")
            value = driver.read_json(config)
            value["blocks"][0]["jobs"][0]["argv"] = [str(scratch / "unbound")]
            driver.atomic_json(config, value)
            record = driver.read_json(manifest)
            record["files"][str(config)] = driver.digest(config)
            driver.atomic_json(manifest, record)
            root = scratch / "mac-run/unbound-executable"
            with (patch.object(driver, "__file__", str(staged)),
                  patch.object(driver.sys, "argv", ["run.py", "--native",
                      str(config), "--root", str(root)]),
                  patch.object(driver, "Supervisor") as owner,
                  self.assertRaisesRegex(driver.ReviewRequired,
                                         "unbound common")):
                driver.main()
            owner.assert_not_called()
            self.assertFalse(root.exists())

    def test_actual_native_bindings_refuse_before_any_effect(self):
        for damage in ("missing-block", "duplicate-block", "duplicate-job",
                       "missing-id", "pin", "argv", "receipt", "owner",
                       "kind", "peak", "gomax", "extra-field"):
            with self.subTest(damage=damage), \
                    tempfile.TemporaryDirectory() as folder:
                scratch = Path(folder).resolve()
                config, manifest, staged = self.inputs(scratch, "bindings")
                value = driver.read_json(config)
                job = value["blocks"][0]["jobs"][0]
                if damage == "missing-block":
                    value["blocks"].pop()
                elif damage == "duplicate-block":
                    value["blocks"].append(deepcopy(value["blocks"][0]))
                    value["blocks"][-1]["jobs"][0]["id"] = "another-job"
                elif damage == "duplicate-job":
                    value["blocks"][1]["jobs"][0]["id"] = job["id"]
                elif damage == "missing-id":
                    del job["id"]
                elif damage == "pin":
                    job["source_pin"] = "not-a-pin"
                elif damage == "argv":
                    job["argv"] = "not-an-argv"
                elif damage == "receipt":
                    job["receipt_path"] = str(scratch / "outside.json")
                elif damage == "owner":
                    job["owner"] = ""
                elif damage == "kind":
                    job["kind"] = "oracle"
                elif damage == "peak":
                    job["estimated_peak_bytes"] = -1
                elif damage == "gomax":
                    job["gomaxprocs"] = True
                else:
                    job["surprise"] = "not-bound"
                driver.atomic_json(config, value)
                document = driver.read_json(manifest)
                document["files"][str(config)] = driver.digest(config)
                driver.atomic_json(manifest, document)
                root = scratch / "mac-run/bindings"
                with (patch.object(driver, "__file__", str(staged)),
                      patch.object(driver, "environment",
                                   side_effect=AssertionError) as env,
                      patch.object(driver.subprocess, "Popen") as spawn,
                      patch.object(driver, "Supervisor") as owner,
                      patch.object(Path, "mkdir",
                                   side_effect=AssertionError) as mkdir,
                      patch.object(sys, "argv", ["run.py", "--native",
                          str(config), "--root", str(root)]),
                      self.assertRaises(driver.ReviewRequired)):
                    driver.main()
                owner.assert_not_called()
                spawn.assert_not_called()
                env.assert_not_called()
                mkdir.assert_not_called()
                self.assertFalse(root.exists())




class HybridGroupTests(unittest.TestCase):
    def owner(self, root, *, window="hybrid-window-1", stop=None, budget=None,
              swap_step=0, clock_step=100):
        owner = fixture_owner(root)
        owner.window = {"id": window}
        owner.block_id, owner.block_deadline = "hybrid", 100000
        owner.job_bindings = {job["id"]: {
            **job, "kind": "hybrid", "gomaxprocs": 10,
            "estimated_peak_bytes": driver.GIB}
            for job in driver.bound_jobs(root)["hybrid"]}
        owner.job_minutes = {name: job["minutes"]
                             for name, job in owner.job_bindings.items()}
        owner.calls, owner.clocks, owner.baselines = [], [], []
        owner.clock, owner.counter = [0.0], 10000
        owner.arm_guard = unittest.mock.Mock()
        owner.close = unittest.mock.Mock()
        owner.stop_all = unittest.mock.Mock()
        driver.atomic_json(root / "python-runtime.json", {"fixture": True})

        def start(args, directory, env, cwd):
            name = owner.case["id"]
            owner.calls.append(name)
            if len(owner.calls) == stop:
                raise driver.DeadlineExceeded("fixture interruption")
            self.assertEqual(args, owner.case["argv"])
            mode = args[3]
            if name not in {"hybrid-timing", "hybrid-latency"}:
                declaration = driver.read_json(
                    directory.parent / "started.json")
                self.assertIn(owner.case, declaration["samples"])
                self.assertEqual(declaration["reference"],
                                 None if mode == "timing" else "exact-F32")
                self.assertEqual(declaration["ratio"],
                                 None if mode == "timing" else "B/A")
                self.assertFalse((directory.parent / "complete.json").exists())
                self.assertEqual(driver.read_json(directory / "attempt.json"),
                                 driver.read_json(directory.parent /
                                                  "attempt.json"))
            selected = driver.read_json(directory / "selected-jobs.json")
            commands = driver.hybrid_commands(root, mode)
            owner.clocks.append((owner.clock[0], owner.launch_deadline))
            owner.baselines.append(owner.swapouts)
            if owner.swapouts is None:
                owner.swapouts = owner.counter
            before = fixture_resources(swapouts=owner.counter, load1=20)
            owner.counter += swap_step
            after = fixture_resources(swapouts=owner.counter, load1=22)
            driver.resource_limit(after, 0, owner.swapouts, admission=False)
            driver.atomic_json(directory / "process.json", {
                "window": owner.window, "command": args})
            (directory / "rss.jsonl").write_text(json.dumps({
                **after, "elapsed_seconds": 1.0}) + "\n")
            (directory / "stdout-stderr.log").write_text("fixture")
            if len(owner.calls) == budget:
                raise fixture_budget_stop(directory)
            for sample in selected:
                command = commands[sample]
                flags = dict(zip(command[1::2], command[2::2]))
                Path(command[0]).parent.mkdir(parents=True, exist_ok=True)
                Path(command[0]).write_bytes(b"fixture-binary")
                phase = flags["-phase"]
                kind = "timing" if phase in {"query", "latency"} else (
                    "preparation")
                if mode == "timing":
                    data = Path(flags["-data-dir"])
                    data.mkdir(parents=True, exist_ok=True)
                    (data / "fixture-index").write_bytes(b"generated")
                    if kind == "timing":
                        command += ("-output", str(
                            directory / (sample + ".result.json")))
                    result = {"queries": 256, "n": int(flags["-n"]),
                        "dimensions": int(flags["-dimensions"]),
                        "workers": int(flags.get("-workers", 1)),
                        "candidate_pool":
                        "global quantized top128, then original F32 rerank"}
                else:
                    data = Path(flags["-dataset"])
                    data.mkdir(parents=True, exist_ok=True)
                    inputs = {}
                    for file in ("document.f32", "query.f32", "records.json",
                                 "query-records.json"):
                        (data / file).write_bytes(
                            b"generated-" + file.encode())
                        inputs[file] = {"sha256": driver.digest(data / file)}
                    driver.atomic_json(data / "input-receipt.json", inputs)
                    native = data / "native-M32-f32-rep1/build.json"
                    native.parent.mkdir(exist_ok=True)
                    driver.atomic_json(native, {"native_version": "2.26.5",
                        "config": {"Connectivity": 32, "ExpansionAdd": 256,
                                   "Quantization": 1, "Dimensions": 1024}})
                    result = {"method": flags.get("-method", "exact"),
                        "phase": phase, "queries": 256, "candidate_k": 128,
                        "M": 32, "ef_search": 1024,
                        "required_file_components_bytes": {
                            "int8_with_original_f32": 1}}
                repeat = (int(sample.split("-block")[1][0])
                          if mode == "latency" and kind == "timing" else 1)
                value = 100 * repeat
                if mode == "latency" and result["method"] != "exact":
                    value *= repeat + 1
                result["latency_ns"] = {"count": 256, "p50": value,
                                         "p95": value * 2}
                receipt = {"command": command, "exit_code": 0,
                    "platform": "darwin", "architecture": "arm64",
                    "kind": kind, "result": result,
                    "configuration": {"source": "fixture-source",
                        "go_version": "fixture-go", "native_files": {},
                        "runtime_environment": {"GOMAXPROCS": "10"}},
                    "resource_before": before, "resource_after": after,
                    "valid_timing_comparison": False}
                if mode == "latency":
                    receipt["input_receipt_sha256"] = driver.digest(
                        data / "input-receipt.json")
                driver.atomic_json(directory / (sample + ".json"), receipt)
                if mode == "timing" and kind == "timing":
                    driver.atomic_json(directory / (sample + ".result.json"),
                                       result)
            started = owner.clock[0]
            owner.clock[0] += clock_step
            return SimpleNamespace(process=fixture_process(123, 0),
                directory=directory, started=started, peak_rss=1024,
                deadline=owner.launch_deadline)

        owner.start = start
        return owner

    def group(self, owner, n=10000, mode="latency", dimensions=384):
        profiles = driver.bound_jobs(owner.root)["hybrid"]
        cases = [j for j in profiles if (
            f"generated-n{n}-int8-" in j["id"] and "-w1-" in j["id"]
            if mode == "latency" else
            j["id"].startswith(f"n{n}-d{dimensions}-w"))]
        self.assertEqual(len(cases), 6 if mode == "latency" else 9)
        directory = Path(cases[0]["receipt_path"]).parents[1]
        identity = {"reference": "exact-F32" if mode == "latency" else None,
            "operation": "hybrid-" + mode,
            "input": "fixture-generated-input", "recipe": "int8/global128",
            "source": "fixture-source", "config": "fixture-config",
            "pairs": {"B": [[0, 1], [2, 3], [4, 5]]}
            if mode == "latency" else {},
            "ratio": "B/A" if mode == "latency" else None,
            "samples": [owner.job_bindings[j["id"]] for j in cases]}

        def samples(attempt, check_only=False):
            rows = []
            for job in cases:
                folder = Path(job["receipt_path"]).parent
                row = driver.auxiliary(owner, job["id"], job["argv"],
                    lambda _, j=job, d=folder: driver.hybrid_outputs(
                        owner.root, mode, j["selected_jobs"], d,
                        {"source": "fixture-source",
                         "go_version": "fixture-go", "native_files": {}}),
                    selection=job["selected_jobs"], directory=folder,
                    attempt=attempt, check_only=check_only)
                metrics = {} if row["state"] != "pass" else {
                    k: driver.read_json(folder / (job["id"] + ".json"))[
                        "result"]["latency_ns"][k] for k in ("p50", "p95")}
                rows.append({**row, "metrics": metrics})
            return rows

        return directory, identity, samples

    def run_group(self, owner, *, n=10000, mode="latency", dimensions=384,
                  before_first=0):
        directory, identity, samples = self.group(owner, n, mode, dimensions)
        def load(attempt, check_only=False):
            if not check_only:
                owner.clock[0] += before_first
            return samples(attempt, check_only=check_only)
        with patch.object(driver.time, "monotonic",
                          side_effect=lambda: owner.clock[0]):
            return driver.comparison_group(owner, directory.name,
                                           identity, load)

    def rehash(self, root, directory):
        for path in directory.glob("*/complete.json"):
            row = driver.read_json(path)
            row["files"] = {name: driver.digest(root / name)
                            for name in row["files"]}
            driver.atomic_json(path, row)
        proof = driver.read_json(directory / "complete.json")
        for index, sample in enumerate(proof["identity"]["samples"]):
            row = driver.read_json(directory / sample["id"] / "complete.json")
            metrics = proof["samples"][index]["metrics"]
            proof["samples"][index] = {**row, "metrics": metrics
                                      if row["state"] == "pass" else {}}
        proof["files"] = {name: driver.digest(root / name)
                          for name in proof["files"]}
        driver.atomic_json(directory / "complete.json", proof)

    def test_interrupt_after_a_or_b_restarts_whole_group_then_zero_spawn(self):
        for stop in (2, 3):
            with self.subTest(stop=stop), tempfile.TemporaryDirectory() as f:
                root = Path(f).resolve()
                owner = self.owner(root, stop=stop)
                with self.assertRaisesRegex(driver.DeadlineExceeded,
                                            "interruption"):
                    self.run_group(owner)
                directory, _, _ = self.group(owner)
                files = {str(p.relative_to(directory)): p.read_bytes()
                         for p in [*driver.output_files([directory]),
                                   *directory.glob("*/complete.json")]}
                self.assertNotIn("complete.json", files)
                old = driver.read_json(directory / "attempt.json")
                resumed = self.owner(root, window="hybrid-window-2")
                proof = self.run_group(resumed)
                self.assertEqual(len(resumed.calls), 6)
                self.assertEqual(["-reference" in n for n in resumed.calls],
                                 [True, False] * 3)
                archive = list((root / "attempts/aux" /
                                directory.name).iterdir())
                self.assertEqual(len(archive), 1)
                for name, raw in files.items():
                    self.assertEqual((archive[0] / name).read_bytes(), raw)
                self.assertNotEqual(old["attempt_id"],
                                    proof["attempt"]["attempt_id"])
                self.assertEqual(proof["attempt"]["window_id"],
                                 "hybrid-window-2")
                before = (directory / "complete.json").read_bytes()
                final = self.owner(root, window="hybrid-window-3")
                self.assertEqual(self.run_group(final), proof)
                self.assertEqual(final.calls, [])
                self.assertEqual((directory / "complete.json").read_bytes(),
                                 before)
                self.assertTrue(proof["valid_timing_comparison"])
                self.assertEqual(proof["paired_ratios"]["orientation"], "B/A")
                for ratio in proof["paired_ratios"]["operations"]:
                    self.assertEqual(ratio["pairs"], [2, 3, 4])
                    self.assertEqual((ratio["pair_count"], ratio["median"],
                                      ratio["min"], ratio["max"]),
                                     (3, 3, 2, 4))

    def test_wrong_identity_order_hash_coverage_and_window_reject(self):
        modes = ("reference", "recipe", "input", "source", "config", "ratio",
                 "order",
                 "hash", "coverage", "attempt_id", "window_id", "process",
                 "raw-source", "raw-config", "raw-command", "raw-recipe")
        for mode in modes:
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as f:
                root = Path(f).resolve()
                owner = self.owner(root)
                self.run_group(owner)
                directory, expected, samples = self.group(owner)
                if mode in {"reference", "recipe", "input",
                            "source", "config", "ratio"}:
                    expected[mode] = "alien"
                elif mode == "order":
                    expected["samples"] = expected["samples"][::-1]
                elif mode in {"hash", "coverage"}:
                    case = directory / expected["samples"][1]["id"]
                    if mode == "hash":
                        (case / "stdout-stderr.log").write_text("changed")
                    else:
                        receipt = driver.read_json(case / "complete.json")
                        del receipt["files"][str((case /
                            (case.name + ".json")).relative_to(root))]
                        driver.atomic_json(case / "complete.json", receipt)
                        self.rehash(root, directory)
                elif mode.startswith("raw-"):
                    case = directory / expected["samples"][1]["id"]
                    path = case / (case.name + ".json")
                    raw = driver.read_json(path)
                    if mode == "raw-source":
                        raw["configuration"]["source"] = "alien"
                    elif mode == "raw-config":
                        raw["configuration"]["native_files"] = {"alien": "0"}
                    elif mode == "raw-command":
                        raw["command"][-1] = "8"
                    else:
                        raw["result"]["candidate_k"] = 64
                    driver.atomic_json(path, raw)
                    self.rehash(root, directory)
                else:
                    case = directory / expected["samples"][1]["id"]
                    if mode == "process":
                        path = case / "process.json"
                        value = driver.read_json(path)
                        value["window"]["id"] = "alien"
                    else:
                        path = case / "attempt.json"
                        value = driver.read_json(path)
                        value[mode] = "alien"
                        receipt = driver.read_json(case / "complete.json")
                        receipt["attempt"] = value
                        driver.atomic_json(case / "complete.json", receipt)
                    driver.atomic_json(path, value)
                    self.rehash(root, directory)
                with self.assertRaises(driver.ReviewRequired):
                    driver.finished_group(root, expected,
                                          projection=(directory, samples))

    def test_budget_negative_reuses_and_corruption_never_retries(self):
        with tempfile.TemporaryDirectory() as f:
            root = Path(f).resolve()
            owner = self.owner(root, budget=2)
            proof = self.run_group(owner)
            self.assertEqual(proof["state"], "budget_exceeded")
            self.assertFalse(proof["valid_timing_comparison"])
            self.assertEqual(proof["paired_ratios"]["operations"], [])
            resumed = self.owner(root, window="hybrid-window-2")
            self.assertEqual(self.run_group(resumed), proof)
            self.assertEqual(resumed.calls, [])
            directory, _, _ = self.group(owner)
            (directory / "complete.json").unlink()
            with self.assertRaises(FileNotFoundError):
                self.run_group(resumed)
            self.assertEqual(resumed.calls, [])
            self.assertFalse((root / "attempts").exists())

    def test_known_rejection_does_not_restart_in_fresh_namespace(self):
        for mode in ("worker", "recipe", "caller"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as f:
                root = Path(f).resolve()
                owner = self.owner(root)
                start = owner.start

                def rejected(args, directory, env, cwd):
                    owned = start(args, directory, env, cwd)
                    if len(owner.calls) == 2:
                        path = directory / (directory.name + ".json")
                        raw = driver.read_json(path)
                        if mode == "worker":
                            raw["exit_code"] = 1
                            owned.process.returncode = 1
                        elif mode == "recipe":
                            raw["result"]["candidate_k"] = 64
                        else:
                            owned.process.returncode = 1
                        driver.atomic_json(path, raw)
                    return owned

                owner.start = rejected
                reason = {"worker": "wrong-platform hybrid",
                          "recipe": "original F32/top128",
                          "caller": "auxiliary command failed"}[mode]
                with self.assertRaisesRegex(driver.ReviewRequired,
                                            reason) as rejection:
                    self.run_group(owner)
                self.assertIs(type(rejection.exception), driver.ReviewRequired)
                directory, _, _ = self.group(owner)
                before = {str(p.relative_to(directory)): p.read_bytes()
                          for p in driver.output_files([directory])}
                self.assertFalse((directory / "complete.json").exists())
                rejection = directory / owner.calls[-1] / "partial.json"
                negative = driver.read_json(rejection)
                self.assertEqual(negative["state"], "checks_failed")
                self.assertRegex(negative["reason"], reason)
                resumed = self.owner(root, window="hybrid-window-2")
                with self.assertRaises((driver.ReviewRequired,
                                        FileNotFoundError)):
                    self.run_group(resumed)
                self.assertEqual(resumed.calls, [])
                self.assertFalse((root / "attempts").exists())
                for name, raw in before.items():
                    self.assertEqual((directory / name).read_bytes(), raw)

    def test_failed_group_never_emits_ratios_or_blindly_retries(self):
        with tempfile.TemporaryDirectory() as f:
            root = Path(f).resolve()
            owner = self.owner(root)
            self.run_group(owner)
            directory, expected, samples = self.group(owner)
            case = directory / expected["samples"][1]["id"]
            row = driver.read_json(case / "complete.json")
            row["state"] = "checks_failed"
            driver.atomic_json(case / "complete.json", row)
            self.rehash(root, directory)
            proof = driver.read_json(directory / "complete.json")
            proof["state"] = "checks_failed"
            driver.atomic_json(directory / "complete.json", proof)
            resumed = self.owner(root, window="hybrid-window-2")
            cached = self.run_group(resumed)
            self.assertEqual(cached["state"], "checks_failed")
            self.assertFalse(cached["valid_timing_comparison"])
            self.assertEqual(cached["paired_ratios"]["operations"], [])
            self.assertEqual(resumed.calls, [])
            del proof["files"][str((case / "complete.json").relative_to(root))]
            driver.atomic_json(directory / "complete.json", proof)
            with self.assertRaises(driver.ReviewRequired):
                self.run_group(resumed)
            self.assertEqual(resumed.calls, [])
            self.assertFalse((root / "attempts").exists())

    def test_unowned_or_aliased_output_directory_has_no_effect(self):
        with tempfile.TemporaryDirectory() as f:
            root = Path(f).resolve()
            owner = self.owner(root)
            job = next(iter(owner.job_bindings.values()))
            outside = root.parent / (root.name + "-unowned")
            self.assertFalse(outside.exists())
            with self.assertRaises(driver.FatalSession):
                driver.auxiliary(owner, job["id"], job["argv"], lambda _: [],
                                 directory=outside)
            self.assertFalse(outside.exists())
            real = root / "real"
            real.mkdir()
            alias = root / "alias"
            alias.symlink_to(real, target_is_directory=True)
            with self.assertRaises(driver.FatalSession):
                driver.auxiliary(owner, job["id"], job["argv"], lambda _: [],
                                 directory=alias / "output")
            self.assertEqual(list(real.iterdir()), [])
            self.assertEqual(owner.calls, [])

    def test_unexpected_verification_errors_propagate_without_retry(self):
        with tempfile.TemporaryDirectory() as f:
            root = Path(f).resolve()
            owner = self.owner(root)
            self.run_group(owner)
            resumed = self.owner(root, window="hybrid-window-2")
            for error in (TypeError("bug"), KeyError("bug"),
                          PermissionError("fixture"),
                          driver.FatalSession("driver JSON")):
                with self.subTest(error=type(error).__name__):
                    with patch.object(driver, "finished_group",
                                      side_effect=error):
                        with self.assertRaises(type(error)):
                            self.run_group(resumed)
                    self.assertEqual(resumed.calls, [])
            self.assertFalse((root / "attempts").exists())

    def test_one_group_clock_and_one_group_swapouts_baseline(self):
        self.assertEqual(driver.CASE_MINUTES["hybrid"], 60)
        self.assertEqual(driver.WINDOW_CLEANUP_SECONDS, 15 * 60)
        for mode, count in (("latency", 6), ("timing", 9)):
            for stop_reserve in (11, 21):
                with self.subTest(mode=mode, stop=stop_reserve), \
                        tempfile.TemporaryDirectory() as f, \
                        patch.object(driver, "CLEANUP_RESERVE_SECONDS",
                                     stop_reserve):
                    root = Path(f).resolve()
                    page_size = fixture_resources()["page_size"]
                    step = (384 * 1024**2) // (count * page_size)
                    owner = self.owner(root, swap_step=step)
                    sha = owner.native_identity["input_manifest_sha256"]
                    go = NativeAdmissionTests().go(sha)
                    go["window_id"] = owner.window["id"]
                    owner.block_deadline = driver.window_limit(go, sha, 1000)
                    self.assertEqual(owner.block_deadline,
                                     4 * 3600 - 1000 - 15 * 60)
                    proof = self.run_group(owner, mode=mode, before_first=200)
                    end = 200 + 60 * 60 - stop_reserve
                    self.assertEqual(owner.clocks, [
                        (200 + i * 100, end) for i in range(count)])
                    self.assertEqual(owner.baselines,
                                     [None] + [10000] * (count - 1))
                    self.assertEqual(owner.counter - 10000, count * step)
                    self.assertLessEqual((owner.counter - 10000) * page_size,
                                         384 * 1024**2)
                    self.assertEqual([end - start for start, end
                                      in owner.clocks], [
                        60 * 60 - stop_reserve - i * 100
                        for i in range(count)])
                    self.assertFalse(hasattr(owner, "group_deadline"))
                    owner.clock[0] = 10**9
                    owner.window = {"id": "later-window"}
                    owner.start = unittest.mock.Mock(
                        side_effect=AssertionError("cached group respawned"))
                    self.assertEqual(self.run_group(owner, mode=mode), proof)
                    owner.start.assert_not_called()
                    owner.arm_guard.assert_called_once()
                    self.assertFalse(hasattr(owner, "group_deadline"))
        with tempfile.TemporaryDirectory() as f:
            owner = self.owner(Path(f).resolve())
            owner.block_deadline = 60 * 60 - 1
            with self.assertRaises(driver.DeadlineExceeded):
                self.run_group(owner)
            self.assertEqual(owner.calls, [])
            self.assertFalse(hasattr(owner, "group_deadline"))
        with tempfile.TemporaryDirectory() as f:
            owner = self.owner(Path(f).resolve())
            sha = owner.native_identity["input_manifest_sha256"]
            go = NativeAdmissionTests().go(sha)
            go["end"] = "1970-01-01T01:15:00+00:00"
            go["window_id"] = owner.window["id"]
            owner.block_deadline = driver.window_limit(go, sha, 0)
            self.assertEqual(owner.block_deadline, 60 * 60)
            self.run_group(owner)
            self.assertEqual(owner.clocks, [
                (i * 100, 60 * 60 - driver.CLEANUP_RESERVE_SECONDS)
                for i in range(6)])
        for step in (8192, -1):
            with self.subTest(step=step), tempfile.TemporaryDirectory() as f:
                root = Path(f).resolve()
                owner = self.owner(root, swap_step=step)
                with self.assertRaises(driver.FatalSession):
                    self.run_group(owner)
                directory, _, _ = self.group(owner)
                self.assertFalse((directory / "complete.json").exists())
                self.assertEqual(len(owner.calls), 5 if step == 8192 else 1)
                self.assertFalse(hasattr(owner, "group_deadline"))

    def test_group_expiry_stops_samples_and_clears_clock(self):
        for mode in ("latency", "timing"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as f:
                root = Path(f).resolve()
                owner = self.owner(root, clock_step=1201)
                with self.assertRaises(driver.DeadlineExceeded):
                    self.run_group(owner, mode=mode)
                directory, _, _ = self.group(owner, mode=mode)
                self.assertEqual(len(owner.calls), 3)
                self.assertGreater(owner.clock[0], 60 * 60)
                self.assertFalse((directory / "complete.json").exists())
                self.assertFalse(hasattr(owner, "group_deadline"))
                self.assertEqual(len(list(directory.glob("*/complete.json"))),
                                 3)
                now = owner.clock[0]
                self.assertEqual(owner.job_limit("hybrid-latency", now),
                                 now + 120 * 60 -
                                 driver.CLEANUP_RESERVE_SECONDS)

    def test_group_clock_clears_on_unexpected_sample_error(self):
        with tempfile.TemporaryDirectory() as f:
            root = Path(f).resolve()
            owner = self.owner(root)
            with patch.object(driver, "hybrid_outputs",
                              side_effect=KeyError("fixture bug")):
                with self.assertRaisesRegex(KeyError, "fixture bug"):
                    self.run_group(owner)
            self.assertEqual(len(owner.calls), 1)
            self.assertFalse(hasattr(owner, "group_deadline"))
            self.assertEqual(owner.job_limit("hybrid-timing", owner.clock[0]),
                             owner.clock[0] + 120 * 60 -
                             driver.CLEANUP_RESERVE_SECONDS)

    def test_preparation_caps_preserve_runtime_and_other_job_limits(self):
        with tempfile.TemporaryDirectory() as f:
            root = Path(f).resolve()
            owner = self.owner(root)
            preparations = {"hybrid-timing", "hybrid-latency"}
            for name, job in owner.job_bindings.items():
                self.assertEqual(job["minutes"],
                                 120 if name in preparations else 60)
            profiles = [{**job, "minutes": 1000}
                        for job in owner.job_bindings.values()]
            owner.permissions = unittest.mock.Mock(return_value=100000)
            with patch.object(driver.time, "monotonic", return_value=0):
                owner.begin_block({"id": "hybrid", "jobs": profiles}, {})
                for name, job in owner.job_bindings.items():
                    minutes = 120 if name in preparations else 60
                    self.assertEqual(owner.job_minutes[name], minutes)
                    self.assertEqual(job["kind"], "hybrid")
                    self.assertEqual(job["gomaxprocs"], 10)
                    self.assertEqual(owner.job_limit(name, 0),
                                     minutes * 60 -
                                     driver.CLEANUP_RESERVE_SECONDS)
                job = {**profiles[0], "kind": "smoke"}
                owner.begin_block({"id": "smoke", "jobs": [job]}, {})
                self.assertEqual(owner.job_minutes[job["id"]], 60)
                job = {**profiles[0], "kind": "segment"}
                owner.begin_block({"id": "segment_timing", "jobs": [job]}, {})
                self.assertEqual(owner.job_minutes[job["id"]], 45)
                self.assertFalse(hasattr(owner, "group_deadline"))

    def test_group_timeout_records_time_partial_in_session(self):
        with tempfile.TemporaryDirectory() as f:
            root = Path(f).resolve()
            owner = self.owner(root, clock_step=1000)
            owner.permissions = unittest.mock.Mock(return_value=100000)
            profiles = list(owner.job_bindings.values())
            admission = {"blocks": [{"id": "storage", "jobs": []},
                                     {"id": "hybrid", "jobs": profiles}]}
            def oracle(owner, selected, setup, concurrency, rows):
                rows.append({"identity": {"job": {"stage": "oracle"}},
                             "state": "pass"})
            with patch.object(driver.time, "monotonic",
                              side_effect=lambda: owner.clock[0]), \
                    patch.object(driver, "execute_stage",
                                 side_effect=oracle), \
                    patch.object(driver.signal, "signal"), \
                    patch.object(driver.signal, "setitimer"):
                result = driver.execute_session(owner, admission, root, {}, 1)
            block = result["blocks"]["hybrid"]
            self.assertEqual(block["state"], "partial")
            self.assertEqual(block["reason_code"], "time")
            self.assertEqual(len(owner.calls), 5)
            self.assertFalse(hasattr(owner, "group_deadline"))
            self.assertFalse((root / "aux/n10000-d384/complete.json").exists())
            self.assertFalse((root / "hybrid-summary.json").exists())
            self.assertEqual(driver.read_json(root / "session.json"), result)

    def test_late_group_proof_records_time_partial_not_complete(self):
        with tempfile.TemporaryDirectory() as f:
            root = Path(f).resolve()
            owner = self.owner(root, clock_step=500)
            owner.permissions = unittest.mock.Mock(return_value=100000)
            directory, identity, samples = self.group(owner)
            starts, rearmed = [], []
            def late_proof(attempt, check_only=False):
                rows = samples(attempt, check_only=check_only)
                if check_only:
                    self.assertEqual(len(owner.calls), 6)
                    self.assertLess(owner.clock[0], 60 * 60)
                    driver.Supervisor.arm_guard(owner)
                    self.assertEqual(timer.call_args.args[1],
                                     60 * 60 - owner.clock[0] -
                                     driver.CLEANUP_RESERVE_SECONDS)
                    starts.append(owner.clock[0])
                    owner.clock[0] = 60 * 60 + 1
                return rows
            def run(owner, sdk, block):
                driver.comparison_group(owner, directory.name,
                                        identity, late_proof)
            def rearm():
                driver.Supervisor.arm_guard(owner)
                rearmed.append(timer.call_args.args[1])
            owner.arm_guard = rearm
            def oracle(owner, selected, setup, concurrency, rows):
                rows.append({"identity": {"job": {"stage": "oracle"}},
                             "state": "pass"})
            admission = {"blocks": [{"id": "storage", "jobs": []}, {
                "id": "hybrid", "jobs": list(owner.job_bindings.values())}]}
            with patch.object(driver.time, "monotonic",
                              side_effect=lambda: owner.clock[0]), \
                    patch.object(driver, "execute_stage",
                                 side_effect=oracle), \
                    patch.object(driver, "run_catalog_hybrid",
                                 side_effect=run), \
                    patch.object(driver.signal, "signal"), \
                    patch.object(driver.signal, "setitimer") as timer:
                result = driver.execute_session(owner, admission, root, {}, 1)
            block = result["blocks"]["hybrid"]
            self.assertEqual(starts, [3000])
            self.assertEqual(block["state"], "partial")
            self.assertEqual(block["reason_code"], "time")
            self.assertFalse((directory / "complete.json").exists())
            self.assertEqual(len(list(directory.glob("*/complete.json"))), 6)
            self.assertFalse(hasattr(owner, "group_deadline"))
            self.assertEqual(rearmed, [100000 - 3601 -
                                      driver.CLEANUP_RESERVE_SECONDS])
            self.assertEqual(driver.read_json(root / "session.json"), result)

    def test_sample_hashes_own_dataset_and_preparation_hashes_both(self):
        for n, other in ((10000, 100000), (100000, 10000)):
            with self.subTest(n=n), tempfile.TemporaryDirectory() as f:
                root = Path(f).resolve()
                owner = self.owner(root)
                proof = self.run_group(owner, n=n)
                work = (root / "home/.prime/research/gortex"
                        / "storage-architecture/tmp/vector/hybrid-followup")
                own_data, other_data = (work / f"generated-n{x}"
                                        for x in (n, other))
                self.assertTrue(own_data.is_dir())
                self.assertFalse(other_data.exists())
                profile = owner.job_bindings[owner.calls[0]]
                folder = Path(profile["receipt_path"]).parent
                paths = driver.hybrid_outputs(
                    root, "latency", profile["selected_jobs"], folder)
                self.assertEqual(paths[2:], [own_data])
                for row in proof["samples"]:
                    files = {root / p for p in row["files"]}
                    self.assertIn(own_data / "document.f32", files)
                    self.assertIn(own_data / "input-receipt.json", files)
                    self.assertFalse(any(p.is_relative_to(other_data)
                                         for p in files))
        with tempfile.TemporaryDirectory() as f:
            root = Path(f).resolve()
            owner = self.owner(root)
            profile = owner.job_bindings["hybrid-latency"]
            folder = Path(profile["receipt_path"]).parent
            with patch.object(driver.time, "monotonic", return_value=0):
                driver.auxiliary(owner, profile["id"], profile["argv"],
                    lambda _: driver.hybrid_outputs(root, "latency",
                        profile["selected_jobs"], folder),
                    selection=profile["selected_jobs"], directory=folder)
            paths = driver.hybrid_outputs(
                root, "latency", profile["selected_jobs"], folder)
            self.assertEqual({p.name for p in paths[2:]},
                             {"generated-n10000", "generated-n100000"})

    def test_full_schedule_keeps_scaling_round_and_group_only_metrics(self):
        with tempfile.TemporaryDirectory() as f:
            root = Path(f).resolve()
            owner = self.owner(root)
            with patch.object(driver.time, "monotonic",
                              side_effect=lambda: owner.clock[0]):
                driver.run_catalog_hybrid(owner, [], "hybrid")
            profiles = driver.bound_jobs(root)["hybrid"]
            self.assertEqual(owner.calls, [j["id"] for j in profiles])
            self.assertEqual(len(profiles), 86)
            self.assertEqual(sum(len(j["selected_jobs"])
                                 for j in profiles), 94)
            self.assertEqual(len({j["receipt_path"] for j in profiles}), 86)
            for mode, count in (("timing", 40), ("latency", 54)):
                self.assertEqual(len(driver.hybrid_commands(root, mode)),
                                 count)
            summary = driver.read_json(root / "hybrid-summary.json")
            groups = [g for g in summary["blocks"] if "samples" in g]
            self.assertEqual(len(groups), 12)
            for group in groups:
                self.assertTrue(group["valid_timing_comparison"])
                cases = group["identity"]["samples"]
                if group["identity"]["operation"] == "hybrid-timing":
                    self.assertEqual([int(j["id"].split("-w")[1][0])
                                      for j in cases], [1, 4, 8] * 3)
                    self.assertIsNone(group["identity"]["reference"])
                    self.assertIsNone(group["identity"]["ratio"])
                    self.assertEqual(group["paired_ratios"]["operations"], [])
                else:
                    self.assertEqual(group["identity"]["reference"],
                                     "exact-F32")
                    self.assertEqual(group["identity"]["ratio"], "B/A")
                    self.assertEqual(["-reference" in j["id"] for j in cases],
                                     [True, False] * 3)
                self.assertEqual(len({j["receipt_path"] for j in cases}),
                                 len(cases))
                for job in cases:
                    folder = Path(job["receipt_path"]).parent
                    raw = driver.read_json(folder / (job["id"] + ".json"))
                    self.assertFalse(raw["valid_timing_comparison"])
            files = driver.block_files(root, {
                "id": "hybrid", "jobs": profiles})
            for group in groups:
                path = (root / "aux" / group["attempt"]["job_id"]
                        / "complete.json")
                self.assertIn(str(path.relative_to(root)), files)


class CatalogGroupTests(unittest.TestCase):
    operations = ("publish", "publish_set", "pin_resolve",
                  "reopen_recover_pin_resolve")

    def report(self, candidate, entries, *, repeats=1, pair=0, factor=1):
        return {"success": True, "candidate": candidate, "entries": entries,
            "repeats": repeats, "seed": 20261025,
            "fixture_sha256": {f"fixture-{i}": f"{i:064x}"
                               for i in range(4 * entries)},
            "latency": {op: {"count": repeats,
                "p50_ns": 100 * (pair + 1) * (i + 1) * factor}
                for i, op in enumerate(self.operations)},
            "recoveries": [{"candidate": candidate, "pid": 123,
                "factory": "*main.catalog", "ns": 100}
                for _ in range(repeats)],
            "footprints": [{"files": 1, "bytes": 16}
                           for _ in range(repeats)]}

    def owner(self, root, *, window="catalog-window-1", stop=None,
              budget=None, change=None, clock_step=1, swap_step=256):
        owner = fixture_owner(root)
        owner.window = {"id": window}
        owner.block_id, owner.block_deadline = "catalog", 100000
        owner.job_bindings = {j["id"]: {**j, "kind": "catalog",
            "gomaxprocs": 10, "estimated_peak_bytes": driver.GIB}
            for j in driver.bound_jobs(root)["catalog"]}
        owner.job_minutes = {name: j["minutes"]
                             for name, j in owner.job_bindings.items()}
        owner.clock, owner.counter = [0.0], 10000
        owner.calls, owner.clocks, owner.baselines = [], [], []
        owner.permissions = unittest.mock.Mock(return_value=100000)
        owner.arm_guard = unittest.mock.Mock()
        owner.close = unittest.mock.Mock()
        owner.stop_all = unittest.mock.Mock()
        driver.atomic_json(root / "python-runtime.json", {"fixture": True})

        def start(args, directory, env, cwd):
            owner.calls.append((directory.parent.name, directory.name))
            if len(owner.calls) == stop:
                raise driver.DeadlineExceeded("fixture interruption")
            declared = driver.read_json(directory.parent / "started.json")
            case = next(c for c in declared["samples"]
                        if c["id"] == directory.name)
            self.assertEqual(case["binding"], owner.case["id"])
            self.assertEqual(declared["reference"], "catalog-sqlite")
            self.assertEqual(declared["ratio"], "B/A")
            self.assertEqual(args[:-1], owner.case["argv"][:-1])
            self.assertEqual(args[-1], str(directory / "output"))
            flags = dict(zip(args[2::2], args[3::2]))
            self.assertEqual(flags["-candidate"], case["candidate"])
            self.assertEqual(flags["-repeats"], "1")
            self.assertEqual(flags["-seed"], "20261025")
            self.assertEqual(env["GOMAXPROCS"], "10")
            self.assertEqual(driver.read_json(directory / "attempt.json"),
                             driver.read_json(directory.parent /
                                              "attempt.json"))
            self.assertFalse((directory.parent / "complete.json").exists())
            owner.baselines.append(owner.swapouts)
            owner.guard(admission=True)
            before = dict(owner.sample)
            owner.clocks.append((directory.parent.name, owner.clock[0],
                                 owner.launch_deadline))
            owner.counter += swap_step
            driver.atomic_json(directory / "process.json", {
                "window": dict(owner.window), "command": args,
                "resources": before})
            (directory / "rss.jsonl").write_text('{"peak": 1024}\n')
            (directory / "stdout-stderr.log").write_text("fixture")
            if len(owner.calls) == budget:
                raise fixture_budget_stop(directory)
            pair = int(directory.name[1:]) - 1
            factor = pair + 2 if directory.name.startswith("B") else 1
            raw = self.report(flags["-candidate"], int(flags["-entries"]),
                              pair=pair, factor=factor)
            if change is not None:
                change(raw, directory.name)
            out = directory / "output"
            out.mkdir()
            (out / "catalog.fixture").write_text(flags["-candidate"])
            driver.atomic_json(out / "result.json", raw)
            started = owner.clock[0]
            owner.clock[0] += clock_step
            return SimpleNamespace(process=fixture_process(123, 0),
                directory=directory, started=started, peak_rss=1024,
                deadline=owner.launch_deadline)
        owner.start = start
        return owner

    def run_catalog(self, owner, candidates=None):
        selected = driver.CATALOGS if candidates is None else candidates
        def resources(env):
            return fixture_resources(timestamp=1000 + owner.clock[0],
                swapouts=owner.counter, level=20, load1=25, load5=24,
                load15=23)
        with patch.object(driver.time, "monotonic",
                          side_effect=lambda: owner.clock[0]), \
                patch.object(driver, "native_resources",
                             side_effect=resources), \
                patch.object(driver, "CATALOGS", tuple(selected)):
            owner.outcome = driver.run_catalog_hybrid(owner, [], "catalog")
        return driver.read_json(owner.root / "catalog-summary.json")

    def test_full_catalog_matrix_counts_ratios_and_reference_extras(self):
        with tempfile.TemporaryDirectory() as f:
            root = Path(f).resolve()
            owner = self.owner(root)
            summary = self.run_catalog(owner)
            self.assertEqual(owner.outcome, "completed")
            self.assertEqual(summary["failed_catalog_checks"], {})
            self.assertEqual(summary["blocked_catalog_sizes"], [])
            groups = summary["blocks"]
            self.assertEqual(len(groups), 12)
            self.assertEqual(len(owner.calls), 720)
            self.assertEqual(len(set(owner.calls)), 720)
            self.assertEqual(summary["reference_trials"], 360)
            self.assertEqual(summary["reference_extras"], 270)
            self.assertEqual(summary["data_scope"],
                             "catalog-only sealed manifests")
            methods = {name: 0 for name in driver.CATALOGS}
            coverage = set()
            for group in groups:
                identity = group["identity"]
                cases, rows = identity["samples"], group["samples"]
                candidate = cases[1]["candidate"]
                coverage.add((candidate, identity["entries"]))
                self.assertEqual(identity["reference"], "catalog-sqlite")
                self.assertEqual(identity["ratio"], "B/A")
                self.assertEqual([c["candidate"] for c in cases],
                                 ["catalog-sqlite", candidate] * 30)
                self.assertEqual([c["id"] for c in cases], [
                    f"{role}{i:02d}" for i in range(1, 31)
                    for role in ("A", "B")])
                self.assertEqual(len(rows), 60)
                self.assertTrue(group["valid_timing_comparison"])
                ratios = group["paired_ratios"]["operations"]
                self.assertEqual({r["metric"] for r in ratios},
                                 set(self.operations))
                self.assertEqual(len(ratios), 4)
                for ratio in ratios:
                    self.assertEqual(ratio["pairs"], list(range(2, 32)))
                    self.assertEqual(ratio["pair_count"], 30)
                    self.assertEqual((ratio["median"], ratio["min"],
                                      ratio["max"]), (16.5, 2, 31))
                folder = root / "aux" / group["attempt"]["job_id"]
                clocks = [(start, end) for name, start, end in owner.clocks
                          if name == folder.name]
                self.assertEqual(len(clocks), 60)
                end = clocks[0][0] + 15 * 60 - driver.CLEANUP_RESERVE_SECONDS
                self.assertEqual([deadline for _, deadline in clocks],
                                 [end] * 60)
                baselines = []
                for case, row in zip(cases, rows):
                    methods[case["candidate"]] += 1
                    directory = folder / case["id"]
                    raw = driver.read_json(directory / "output/result.json")
                    self.assertEqual(raw["repeats"], 1)
                    self.assertEqual({v["count"] for v in
                                      raw["latency"].values()}, {1})
                    self.assertEqual(len(raw["recoveries"]), 1)
                    self.assertEqual(raw["recoveries"][0]["candidate"],
                                     case["candidate"])
                    before = driver.read_json(directory / "process.json")[
                        "resources"]
                    after_path = directory / "resources-after.json"
                    after = driver.read_json(after_path)
                    self.assertIn(str(after_path.relative_to(root)),
                                  row["files"])
                    for sample in (before, after):
                        self.assertTrue(set(fixture_resources()) <=
                                        sample.keys())
                        self.assertEqual(sample["load1"], 25)
                        self.assertIs(sample["ac_power"], True)
                        baselines.append(sample["swapouts_start"])
                    self.assertGreaterEqual(after["timestamp"],
                                            before["timestamp"])
                    self.assertEqual(after["swapouts"] - before["swapouts"],
                                     256)
                self.assertEqual(len(set(baselines)), 1)
            self.assertEqual(methods["catalog-sqlite"], 360)
            self.assertTrue(all(methods[c] == 90 for c in driver.CATALOGS[1:]))
            self.assertEqual(coverage, {(c, n) for c in driver.CATALOGS[1:]
                                       for n in (1, 10, 100)})
            files = driver.block_files(root, {"id": "catalog",
                "jobs": list(owner.job_bindings.values())})
            for group in groups:
                self.assertIn(str((root / "aux" / group["attempt"]["job_id"]
                                   / "complete.json").relative_to(root)),
                                 files)
            self.assertFalse(hasattr(owner, "group_deadline"))

    def test_catalog_factory_trial_and_input_refusals_do_not_retry(self):
        for failure in ("candidate", "missing-call", "nil-factory", "no-op",
                        "operation", "count-30", "boolean-count", "nonnumeric",
                        "footprint", "unmatched", "boolean-repeat"):
            with self.subTest(failure=failure), \
                    tempfile.TemporaryDirectory() as f:
                root = Path(f).resolve()
                def change(raw, role):
                    if failure == "candidate":
                        raw["candidate"] = "catalog-substitute"
                    elif failure == "missing-call":
                        raw["recoveries"] = []
                    elif failure == "nil-factory":
                        raw["recoveries"][0]["factory"] = "<nil>"
                    elif failure == "no-op":
                        raw["recoveries"][0]["ns"] = 0
                    elif failure == "operation":
                        raw["latency"]["publish_set"]["count"] = 0
                    elif failure == "count-30":
                        raw["repeats"] = 30
                    elif failure == "boolean-count":
                        raw["latency"]["publish"]["count"] = True
                    elif failure == "nonnumeric":
                        raw["latency"]["publish"]["p50_ns"] = "NaN"
                    elif failure == "footprint":
                        raw["footprints"][0]["bytes"] = 0
                    elif failure == "unmatched" and role.startswith("B"):
                        raw["fixture_sha256"]["fixture-0"] = "f" * 64
                    elif failure == "boolean-repeat":
                        raw["repeats"] = True
                owner = self.owner(root, change=change)
                eligible = driver.CATALOGS[:2]
                report = self.run_catalog(owner, eligible)
                self.assertEqual(owner.outcome, "checks_failed")
                self.assertEqual(report["blocked_catalog_sizes"],
                                 [] if failure == "unmatched" else
                                 [1, 10, 100])
                self.assertTrue(all(not g["valid_timing_comparison"]
                                    for g in report["blocks"]))
                self.assertTrue(all(g["samples"][-1]["metrics"] == {}
                                    for g in report["blocks"]))
                self.assertEqual(len(report["failed_catalog_checks"]), 3)
                self.assertEqual(len(owner.calls),
                                 6 if failure == "unmatched" else 3)
                folder = root / "aux/catalog-catalog-refs-n1"
                partial = list(folder.glob("*/partial.json"))
                self.assertEqual(len(partial), 1)
                self.assertEqual(driver.read_json(partial[0])["state"],
                                 "checks_failed")
                self.assertTrue((folder / "complete.json").exists())
                self.assertEqual(driver.read_json(folder / "complete.json")[
                    "state"], "checks_failed")
                saved = {str(p.relative_to(folder)): p.read_bytes()
                         for p in [*driver.output_files([folder]),
                                   folder / "complete.json",
                                   *folder.glob("*/complete.json")]}
                resumed = self.owner(root, window="catalog-window-2")
                self.assertEqual(self.run_catalog(resumed, eligible), report)
                self.assertEqual(resumed.calls, [])
                self.assertFalse((root / "attempts").exists())
                self.assertEqual(saved, {str(p.relative_to(folder)):
                    p.read_bytes() for p in [*driver.output_files([folder]),
                        folder / "complete.json",
                        *folder.glob("*/complete.json")]})

    def test_catalog_own_checks_reject_missing_metrics_or_repeats(self):
        raw = self.report("catalog-sqlite", 1, repeats=30)
        self.assertEqual(set(driver.check_catalog_report(
            raw, "catalog-sqlite", 1)), set(self.operations))
        with self.assertRaises(driver.ReviewRequired):
            driver.check_catalog_report(self.report("catalog-sqlite", 1),
                                        "catalog-sqlite", 1)
        for value in (0, float("nan"), float("inf")):
            raw = self.report("catalog-sqlite", 1)
            raw["latency"]["publish"]["p50_ns"] = value
            with self.subTest(value=value), \
                    self.assertRaises(driver.ReviewRequired):
                driver.check_catalog_report(raw, "catalog-sqlite", 1,
                                            repeats=1)

    def test_catalog_interruption_restarts_group_and_complete_reuses(self):
        with tempfile.TemporaryDirectory() as f:
            root = Path(f).resolve()
            eligible = driver.CATALOGS[:2]
            interrupted = self.owner(root, stop=2)
            with self.assertRaises(driver.DeadlineExceeded):
                self.run_catalog(interrupted, eligible)
            folder = root / "aux/catalog-catalog-refs-n1"
            old_attempt = driver.read_json(folder / "attempt.json")
            saved = {str(p.relative_to(folder)): p.read_bytes() for p in [
                *driver.output_files([folder]),
                *folder.glob("*/complete.json")]}
            restarted = self.owner(root, window="catalog-window-2")
            proof = self.run_catalog(restarted, eligible)
            self.assertEqual(len(restarted.calls), 180)
            self.assertEqual(proof["reference_trials"], 90)
            self.assertEqual(proof["reference_extras"], 0)
            archive = list((root / "attempts/aux" / folder.name).iterdir())
            self.assertEqual(len(archive), 1)
            for name, content in saved.items():
                self.assertEqual((archive[0] / name).read_bytes(), content)
            attempt = proof["blocks"][0]["attempt"]
            self.assertNotEqual(attempt["attempt_id"],
                                old_attempt["attempt_id"])
            self.assertEqual(attempt["window_id"], "catalog-window-2")
            final = self.owner(root, window="catalog-window-3")
            final.clock[0], final.block_deadline = 10**9, 10**9 + 100000
            self.assertEqual(self.run_catalog(final, eligible), proof)
            self.assertEqual(final.calls, [])
            final.permissions.assert_not_called()
            self.assertFalse(hasattr(final, "group_deadline"))

    def test_catalog_time_swap_and_counter_reset_leave_no_complete_group(self):
        for failure, step, swap, calls in (("time", 20, 0, 45),
                ("swap", 1, 1024, 33), ("counter", 1, -1, 1)):
            with self.subTest(failure=failure), \
                    tempfile.TemporaryDirectory() as f:
                root = Path(f).resolve()
                owner = self.owner(root, clock_step=step, swap_step=swap)
                expected = (driver.DeadlineExceeded if failure == "time"
                            else driver.FatalSession)
                with self.assertRaises(expected):
                    self.run_catalog(owner, driver.CATALOGS[:2])
                self.assertEqual(len(owner.calls), calls)
                self.assertFalse((root / "aux/catalog-catalog-refs-n1"
                                  / "complete.json").exists())
                self.assertFalse((root / "catalog-summary.json").exists())
                self.assertFalse(hasattr(owner, "group_deadline"))
        with tempfile.TemporaryDirectory() as f:
            owner = self.owner(Path(f).resolve())
            owner.block_deadline = 15 * 60 - 1
            with self.assertRaises(driver.DeadlineExceeded):
                self.run_catalog(owner, driver.CATALOGS[:2])
            self.assertEqual(owner.calls, [])

    def test_catalog_budget_blocks_size_without_ratios_or_retry(self):
        with tempfile.TemporaryDirectory() as f:
            root = Path(f).resolve()
            owner = self.owner(root, budget=1)
            proof = self.run_catalog(owner, driver.CATALOGS[:2])
            negative = proof["blocks"][0]
            self.assertEqual(negative["state"], "budget_exceeded")
            self.assertFalse(negative["valid_timing_comparison"])
            self.assertEqual(negative["paired_ratios"]["operations"], [])
            self.assertEqual(proof["reference_trials"], 60)
            self.assertEqual(proof["reference_extras"], 0)
            self.assertEqual(proof["blocked_catalog_sizes"], [1])
            self.assertEqual(proof["failed_catalog_checks"][
                "catalog-catalog-sqlite-n1"], "budget_exceeded")
            self.assertEqual(len(owner.calls), 121)
            self.assertEqual(owner.calls[0],
                             ("catalog-catalog-refs-n1", "A01"))
            self.assertTrue((root / "aux/catalog-catalog-refs-n1"
                             / "complete.json").exists())
            self.assertEqual(len(negative["samples"]), 1)
            resumed = self.owner(root, window="catalog-window-2")
            self.assertEqual(self.run_catalog(resumed, driver.CATALOGS[:2]),
                             proof)
            self.assertEqual(resumed.calls, [])

    def test_missing_post_sample_invalidates_whole_group_proof(self):
        with tempfile.TemporaryDirectory() as f:
            root = Path(f).resolve()
            owner = self.owner(root)
            self.run_catalog(owner, driver.CATALOGS[:2])
            folder = root / "aux/catalog-catalog-refs-n1"
            complete = folder / "complete.json"
            group = driver.read_json(complete)
            leaf_path = folder / "A01/complete.json"
            leaf = driver.read_json(leaf_path)
            missing = folder / "A01/resources-after.json"
            relative = str(missing.relative_to(root))
            missing.unlink()
            leaf["files"].pop(relative)
            leaf["file_bytes"].pop(relative)
            driver.atomic_json(leaf_path, leaf)
            group["files"].pop(relative)
            group["files"][str(leaf_path.relative_to(root))] = driver.digest(
                leaf_path)
            group["samples"][0] = {**leaf,
                                  "metrics": group["samples"][0]["metrics"]}
            driver.atomic_json(complete, group)
            resumed = self.owner(root, window="catalog-window-2")
            proof = self.run_catalog(resumed, driver.CATALOGS[:2])
            self.assertEqual(len(resumed.calls), 60)
            self.assertEqual({name for name, _ in resumed.calls},
                             {folder.name})
            self.assertTrue(proof["blocks"][0]["valid_timing_comparison"])
            self.assertNotEqual(group["attempt"]["attempt_id"],
                                proof["blocks"][0]["attempt"]["attempt_id"])
            self.assertEqual(len(list((root / "attempts/aux" /
                                      folder.name).iterdir())), 1)

    def test_catalog_observation_is_once_but_checks_precede_every_spawn(self):
        for block, baseline, duration, revoke in (
                ("catalog", None, 30, False),
                ("catalog", 10000, 0, False),
                ("catalog", 10000, 0, True),
                ("hybrid", 10000, 30, False),
                ("segment_timing", 10000, 30, False)):
            with self.subTest(block=block, baseline=baseline, revoke=revoke), \
                    tempfile.TemporaryDirectory() as f:
                root = Path(f).resolve()
                directory = root / "case"
                directory.mkdir()
                owner = fixture_owner(root)
                env = {**driver.environment(root), "GOMAXPROCS": "10"}
                owner.go_file = root / "go.json"
                go = NativeAdmissionTests().go(
                    owner.native_identity["input_manifest_sha256"])
                driver.atomic_json(owner.go_file, go)
                owner.go_sha = driver.digest(owner.go_file)
                owner.block_id, owner.swapouts = block, baseline
                owner.case = {"gomaxprocs": 10}
                owner.stop_all = unittest.mock.Mock()
                clock, checks = [0.0], []
                process = fixture_process(8500)
                log = StringIO()
                original_open = Path.open
                def sleep(seconds):
                    clock[0] += seconds
                def guard(*, admission=False):
                    checks.append(admission)
                    if revoke and admission and checks.count(True) == 2:
                        driver.atomic_json(owner.go_file,
                                           {**go, "user_go": False})
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
                                   return_value=set()),
                      patch.object(driver.signal, "setitimer"),
                      patch.object(driver, "darwin_usage",
                                   return_value=SimpleNamespace(
                                       proc_start_abstime=123)),
                      patch.object(driver, "process_groups", return_value={
                          8500: {(8500, 123): 8192}}),
                      patch.object(driver.os, "waitpid",
                                   side_effect=ChildProcessError),
                      patch.object(Path, "open", new=open_file),
                      patch.object(driver.subprocess, "Popen",
                                   return_value=process) as spawn):
                    if revoke:
                        with self.assertRaises(driver.DeadlineExceeded):
                            owner.start(["/fixture/catalog"], directory,
                                        env, root)
                        spawn.assert_not_called()
                    else:
                        owned = owner.start(["/fixture/catalog"], directory,
                                            env, root)
                        spawn.assert_called_once()
                        owned.log.close()
                        owner.active.clear()
                self.assertEqual(clock[0], duration)
                self.assertEqual(checks.count(True), 2)
                self.assertEqual(checks[:8] if duration else checks[:2],
                                 [True] + [False] * 6 + [True]
                                 if duration else [True, True])

    def test_own_factory_failure_blocks_only_the_affected_size(self):
        for method in ("catalog-sqlite", "catalog-refs"):
            with self.subTest(method=method), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder).resolve()
                def change(raw, role):
                    if raw["candidate"] == method and raw["entries"] == 10:
                        raw["recoveries"][0]["factory"] = "<nil>"
                owner = self.owner(root, change=change)
                report = self.run_catalog(owner, driver.CATALOGS[:3])
                self.assertEqual(owner.outcome, "checks_failed")
                self.assertEqual(set(report["failed_catalog_checks"]),
                                 {f"catalog-{method}-n10"})
                self.assertEqual(report["blocked_catalog_sizes"],
                                 [10] if method == "catalog-sqlite" else [])
                self.assertEqual(len(owner.calls),
                                 241 if method == "catalog-sqlite" else 302)
                self.assertEqual(report["reference_trials"],
                                 120 if method == "catalog-sqlite" else 151)
                self.assertEqual(report["reference_extras"],
                                 60 if method == "catalog-sqlite" else 61)
                negative = next(g for g in report["blocks"]
                                if g["state"] != "pass")
                self.assertFalse(negative["valid_timing_comparison"])
                self.assertEqual(negative["paired_ratios"]["operations"], [])
                self.assertEqual(len(negative["samples"]),
                                 1 if method == "catalog-sqlite" else 2)
                self.assertEqual({g["identity"]["entries"] for g in
                    report["blocks"] if g["state"] == "pass"},
                    {1, 100} if method == "catalog-sqlite" else {1, 10, 100})
                if method == "catalog-sqlite":
                    self.assertEqual([c for c in owner.calls
                                      if c[0].endswith("-n10")],
                                     [("catalog-catalog-refs-n10", "A01")])
                admission = {"blocks": [{"id": "catalog", "jobs": list(
                    owner.job_bindings.values())}]}
                files = driver.block_files(root, admission["blocks"][0])
                raw = root / "aux/catalog-catalog-refs-n10" / (
                    "A01" if method == "catalog-sqlite" else "B01") / (
                    "output/result.json")
                self.assertEqual(files[str(raw.relative_to(root))],
                                 driver.digest(raw))
                driver.atomic_json(root / "session.json", {
                    "identity": {"configuration": admission, "setup": {},
                                 "native": owner.native_identity},
                    "blocks": {"catalog": {"state": "checks_failed",
                                          "files": files}}})
                resumed = self.owner(root, window="second-window")
                resumed.begin_block = unittest.mock.Mock(
                    side_effect=AssertionError("known own-check failure"))
                with (patch.object(driver.signal, "signal"),
                      patch.object(driver.signal, "setitimer")):
                    result = driver.execute_session(resumed, admission,
                                                    root, {}, 1)
                self.assertEqual(result["blocks"]["catalog"]["state"],
                                 "checks_failed")
                self.assertEqual(resumed.calls, [])
                resumed.begin_block.assert_not_called()
                raw.write_text("changed rejected proof")
                with (patch.object(driver.signal, "signal"),
                      patch.object(driver.signal, "setitimer"),
                      self.assertRaisesRegex(driver.ReviewRequired,
                          "known failed block has unverified proof")):
                    driver.execute_session(resumed, admission, root, {}, 1)
                self.assertEqual(resumed.calls, [])
                resumed.begin_block.assert_not_called()

    def test_unknown_own_verifier_bug_stops_instead_of_skipping_a_size(self):
        with tempfile.TemporaryDirectory() as folder:
            owner = self.owner(Path(folder).resolve())
            with (patch.object(driver, "check_catalog_report",
                               side_effect=RuntimeError("verifier bug")),
                  self.assertRaisesRegex(RuntimeError, "verifier bug")):
                self.run_catalog(owner, driver.CATALOGS[:2])
            self.assertEqual(owner.calls,
                             [("catalog-catalog-refs-n1", "A01")])
            self.assertFalse((owner.root / "catalog-summary.json").exists())

    def test_incomplete_catalog_artifact_escape_refuses_before_hashing(self):
        with tempfile.TemporaryDirectory() as folder:
            base = Path(folder).resolve()
            root = base / "run"
            root.mkdir()
            outside = base / "not-owned.txt"
            outside.write_text("private synthetic fixture")
            owner = self.owner(root, stop=2)
            with self.assertRaises(driver.DeadlineExceeded):
                self.run_catalog(owner, driver.CATALOGS[:2])
            alias = root / "aux/catalog-catalog-refs-n1/unexpected-file"
            alias.symlink_to(outside)
            digest = driver.digest
            def guarded(path):
                if Path(path).resolve() == outside:
                    raise AssertionError("outside bytes must not be read")
                return digest(path)
            with (patch.object(driver, "digest", side_effect=guarded),
                  self.assertRaises(driver.ReviewRequired)):
                driver.block_files(root, {"id": "catalog", "jobs": list(
                    owner.job_bindings.values())})
            self.assertEqual(outside.read_text(), "private synthetic fixture")

    def test_failed_group_reuses_after_later_catalog_time_interruption(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            candidates = driver.CATALOGS[:3]
            def change(raw, role):
                if raw["candidate"] == "catalog-refs" and raw["entries"] == 1:
                    raw["recoveries"][0]["factory"] = "<nil>"
            first = self.owner(root, stop=3, change=change)
            admission = {"blocks": [{"id": "catalog", "jobs": list(
                first.job_bindings.values())}]}
            def session(owner):
                def resources(env):
                    return fixture_resources(timestamp=1000 + owner.clock[0],
                        swapouts=owner.counter, level=20, load1=25,
                        load5=24, load15=23)
                with (patch.object(driver, "CATALOGS", candidates),
                      patch.object(driver.time, "monotonic",
                                   side_effect=lambda: owner.clock[0]),
                      patch.object(driver, "native_resources",
                                   side_effect=resources),
                      patch.object(driver.signal, "signal"),
                      patch.object(driver.signal, "setitimer")):
                    return driver.execute_session(owner, admission,
                                                  root, {}, 1)
            stopped = session(first)
            self.assertEqual(stopped["blocks"]["catalog"]["state"], "partial")
            self.assertEqual(stopped["blocks"]["catalog"]["reason_code"],
                             "time")
            self.assertEqual(first.calls, [("catalog-catalog-refs-n1", "A01"),
                ("catalog-catalog-refs-n1", "B01"),
                ("catalog-catalog-refs-n10", "A01")])
            negative = root / "aux/catalog-catalog-refs-n1"
            proof = driver.read_json(negative / "complete.json")
            self.assertEqual(proof["state"], "checks_failed")
            self.assertEqual([r["state"] for r in proof["samples"]],
                             ["pass", "checks_failed"])
            self.assertEqual(proof["samples"][-1]["metrics"], {})
            self.assertEqual(proof["attempt"]["window_id"], "catalog-window-1")
            self.assertIn(str((negative / "complete.json").relative_to(root)),
                          stopped["blocks"]["catalog"]["files"])
            saved = {str(p.relative_to(negative)): p.read_bytes() for p in [
                *driver.output_files([negative]), negative / "complete.json",
                *negative.glob("*/complete.json")]}
            resumed = self.owner(root, window="catalog-window-2",
                                 change=change)
            result = session(resumed)
            self.assertEqual(result["blocks"]["catalog"]["state"],
                             "checks_failed")
            self.assertEqual(len(resumed.calls), 300)
            self.assertFalse(any(name == negative.name
                                 for name, _ in resumed.calls))
            self.assertEqual({name for name, _ in resumed.calls}, {
                "catalog-catalog-refs-n10", "catalog-catalog-refs-n100",
                "catalog-catalog-bbolt-n1", "catalog-catalog-bbolt-n10",
                "catalog-catalog-bbolt-n100"})
            self.assertEqual(saved, {
                str(p.relative_to(negative)): p.read_bytes()
                for p in [*driver.output_files([negative]),
                    negative / "complete.json",
                    *negative.glob("*/complete.json")]})
            self.assertFalse((root / "attempts/aux" / negative.name).exists())
            report = driver.read_json(root / "catalog-summary.json")
            self.assertEqual(len(report["blocks"]), 6)
            self.assertEqual(report["reference_trials"], 151)
            self.assertEqual(report["reference_extras"], 61)
            self.assertEqual(set(report["failed_catalog_checks"]),
                             {"catalog-catalog-refs-n1"})
            self.assertEqual(report["blocked_catalog_sizes"], [])
            self.assertTrue(all(g["valid_timing_comparison"]
                                for g in report["blocks"][1:]))

    def test_short_pass_or_empty_negative_cannot_complete_a_group(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            def change(raw, role):
                if role.startswith("B"):
                    raw["recoveries"][0]["factory"] = "<nil>"
            owner = self.owner(root, stop=3, change=change)
            with self.assertRaises(driver.DeadlineExceeded):
                self.run_catalog(owner, driver.CATALOGS[:2])
            directory = root / "aux/catalog-catalog-refs-n1"
            receipt = driver.read_json(directory / "complete.json")
            for state, rows in (("pass", receipt["samples"][:1]),
                                ("checks_failed", [])):
                with self.subTest(state=state, count=len(rows)), \
                        self.assertRaises(driver.ReviewRequired):
                    driver.finished_group(root, receipt["identity"], {
                        **receipt, "state": state, "samples": rows},
                        projection=(directory,
                            lambda attempt, check_only: rows))


class MacHybridCallerTests(unittest.TestCase):
    def load_child(self):
        import importlib.util
        path = (Path(__file__).resolve().parents[1]
                / "vector/hybrid-followup/run.py")
        spec = importlib.util.spec_from_file_location("hybrid_fixture", path)
        module = importlib.util.module_from_spec(spec)
        with patch.dict(sys.modules, {spec.name: module}):
            spec.loader.exec_module(module)
        return module

    def test_mac_commands_use_pinned_controller_and_darwin_binaries(self):
        root = Path("/fixture-root")
        jobs = driver.bound_jobs(root)["hybrid"]
        self.assertEqual(len(jobs), 86)
        for job in jobs:
            self.assertEqual(job["argv"][0],
                             str(Path(sys.executable).resolve()))
            self.assertEqual(job["argv"][1], "-B")
            directory = Path(job["receipt_path"]).parent
            output = job["argv"][job["argv"].index("--output-dir") + 1]
            self.assertEqual(output, str(directory))
            self.assertEqual(job["argv"][job["argv"].index("--jobs-file") + 1],
                             str(directory / "selected-jobs.json"))
        for mode, count in (("timing", 40), ("latency", 54)):
            commands = driver.hybrid_commands(root, mode)
            self.assertEqual(len(commands), count)
            self.assertTrue(all("/bin/darwin-arm64/" in cmd[0]
                                for cmd in commands.values()))
        self.assertEqual(driver.hybrid_names("timing"),
                         list(driver.hybrid_commands(Path("/work"), "timing")))
        child = self.load_child()
        work = (root / "home/.prime/research/gortex/storage-architecture"
                / "tmp/vector/hybrid-followup")
        with patch.object(child, "WORK", work):
            for mode in ("timing", "latency"):
                commands = driver.hybrid_commands(root, mode)
                binary = Path(next(iter(commands.values()))[0])
                jobs = (child.timing_jobs(binary) if mode == "timing" else
                        child.latency_jobs(binary, generated=True))
                actual = {j.name: j.command for j in jobs}
                self.assertEqual(commands, {name: actual[name]
                                            for name in commands})

    def test_unsupported_inputs_refuse_before_build_or_sample(self):
        child = self.load_child()
        cases = (
            ("linux", "x86_64", ["timing"], "n10000-d384-w1-rep1"),
            ("darwin", "x86_64", ["timing"], "n10000-d384-w1-rep1"),
            ("darwin", "arm64", ["timing"], "n1000000-d384-w1-rep1"),
            ("darwin", "arm64", ["timing"], "n10000-d512-w1-rep1"),
            ("darwin", "arm64", ["latency"], None),
            ("darwin", "arm64", ["latency", "--generated"], None),
            ("darwin", "arm64", [], None),
        )
        for platform_name, machine, mode, selected in cases:
            with self.subTest(platform=platform_name, machine=machine,
                              mode=mode, selected=selected), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder)
                selection = root / "selected.json"
                selection.write_text(json.dumps([selected]))
                argv = ["run.py", *mode, "--execute", "--native-dir",
                        str(root / "absent-native")]
                if selected:
                    argv += ["--jobs-file", str(selection)]
                with (patch.object(child, "WORK", root / "work"),
                      patch.object(child.sys, "platform", platform_name),
                      patch.object(child.platform, "machine",
                                   return_value=machine),
                      patch.object(child.sys, "argv", argv),
                      patch.object(child, "build") as build,
                      patch.object(child.subprocess, "run") as execute,
                      patch.object(child.importlib.util,
                                   "spec_from_file_location") as telemetry,
                      patch("sys.stderr", StringIO())):
                    with self.assertRaises(SystemExit):
                        child.main()
                    build.assert_not_called()
                    execute.assert_not_called()
                    telemetry.assert_not_called()
                    self.assertFalse((root / "work").exists())

    def test_high_load_records_complete_d11_without_comparison_claim(self):
        child = self.load_child()
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            _, tools, tool_env = fixture_tools(root)
            work = (root / "home/.prime/research/gortex/storage-architecture"
                    / "tmp/vector/hybrid-followup")
            name = "n10000-d384-w1-rep1"
            selection = root / "selected.json"
            selection.write_text(json.dumps([name]))
            before = fixture_resources(level=20, pressure_state=2,
                available_bytes=4 * driver.GIB, load1=20, load5=17, load15=16)
            after = {**before, "timestamp": 1001, "load1": 22,
                     "swapouts": before["swapouts"] + 32768}
            sample = unittest.mock.Mock(side_effect=[before, after])
            telemetry = SimpleNamespace(native_resources=sample)
            spec = SimpleNamespace(name="storage_telemetry",
                loader=SimpleNamespace(exec_module=unittest.mock.Mock()))
            expected = driver.hybrid_commands(root, "timing")[name]
            result = {"queries": 256, "n": 10000, "dimensions": 384,
                      "latency_ns": {"count": 256, "p50": 100, "p95": 200},
                      "workers": 1, "candidate_pool":
                      "global quantized top128, then original F32 rerank"}

            def execute(command, **options):
                self.assertEqual(command, expected)
                job = next(j for j in child.timing_jobs(Path(command[0]))
                           if j.name == name)
                job.artifact.parent.mkdir(parents=True)
                job.artifact.write_text(json.dumps(result))
                return SimpleNamespace(returncode=0)

            with (patch.object(child, "WORK", work),
                  patch.object(child, "SCRATCH", root / "scratch"),
                  patch.object(child.sys, "platform", "darwin"),
                  patch.object(child.platform, "machine",
                               return_value="arm64"),
                  patch.object(child.sys, "argv", ["run.py", "timing",
                      "--execute", "--run-id", "pod-A1-timing",
                      "--jobs-file", str(selection)]),
                  patch.object(child, "build") as build,
                  patch.object(child, "source_fingerprint",
                               return_value="pin"),
                  patch.object(
                      child.subprocess, "check_output",
                      return_value="go1.27.1 darwin/arm64") as version,
                  patch.object(child.subprocess, "run", side_effect=execute),
                  patch.object(child.importlib.util, "spec_from_file_location",
                               return_value=spec),
                  patch.object(child.importlib.util, "module_from_spec",
                               return_value=telemetry),
                  patch.dict(sys.modules), patch.dict(os.environ, tool_env),
                  patch("sys.stdout", StringIO())):
                child.main()
                build.assert_called_once()
                version.assert_called_once_with([tools["go"], "version"],
                                                text=True)
                self.assertEqual(sample.call_count, 2)
            path = work / "runs/timing/pod-A1-timing" / (name + ".json")
            receipt = json.loads(path.read_text())
            self.assertEqual(receipt["resource_before"], before)
            self.assertEqual(receipt["resource_after"], after)
            self.assertFalse(receipt["valid_timing_comparison"])
            self.assertNotIn("timing_clean", receipt)
            with patch.object(driver, "hybrid_commands",
                              return_value={name: expected}):
                paths = driver.hybrid_outputs(root, "timing")
                self.assertEqual(paths[1], work / "bin/darwin-arm64/timing")
                for phase in ("resource_before", "resource_after"):
                    for key in before:
                        changed = deepcopy(receipt)
                        del changed[phase][key]
                        path.write_text(json.dumps(changed))
                        with self.subTest(phase=phase, key=key):
                            with self.assertRaises(driver.ReviewRequired):
                                driver.hybrid_outputs(root, "timing")
                for field, value in (("platform", "linux"),
                                     ("architecture", "amd64")):
                    path.write_text(json.dumps({**receipt, field: value}))
                    with self.assertRaises(driver.ReviewRequired):
                        driver.hybrid_outputs(root, "timing")

    def test_unexpected_cost_worker_receipt_is_fatal(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            directory = root / "aux/cpu-cost"
            (directory / "output").mkdir(parents=True)
            job = {"id": "cpu-cost", "argv": ["fixture"],
                   "receipt_path": "unused", "source_pin": "1" * 40,
                   "config_pin": "2" * 64, "input_pin": "3" * 64}
            driver.atomic_json(directory / "output/receipt.json", {
                **{k: job[k] for k in
                   ("source_pin", "config_pin", "input_pin")},
                "clean_handoff": True, "state": "fatal", "exit_code": 1,
                "reason_code": "unexpected", "reason": "fixture bug"})

            def auxiliary(owner, name, args, validate, **options):
                return validate(directory)

            with patch.object(driver, "auxiliary", side_effect=auxiliary):
                with self.assertRaises(driver.FatalSession):
                    driver.binding_block(SimpleNamespace(root=root),
                        {"id": "cpu_cost", "jobs": [job]})
            self.assertFalse((directory / "complete.json").exists())

    def test_unexpected_cost_error_and_unclean_teardown_stay_fatal(self):
        import importlib.util
        source = Path(__file__).resolve().parents[1] / "pod-cost"
        with patch.dict(sys.modules):
            modules = []
            for name, path in (("cpu_cost", source / "cpu_cost.py"),
                               ("cost_fixture", source / "test_cpu_cost.py")):
                spec = importlib.util.spec_from_file_location(name, path)
                module = importlib.util.module_from_spec(spec)
                sys.modules[name] = module
                spec.loader.exec_module(module)
                modules.append(module)
            fixture = modules[1].CostTests()
            with tempfile.TemporaryDirectory() as folder:
                root = Path(folder)

                def bug(root, output):
                    raise TypeError("fixture unexpected before teardown")

                code, receipt, matrix = fixture.execute_fixture(
                    root, bug, clean=False, controller_output=True)
                self.assertEqual((code, receipt["state"]), (1, "fatal"))
                self.assertEqual(receipt["reason_code"], "unexpected")
                self.assertEqual(receipt["reason"],
                                 "fixture unexpected before teardown")
                self.assertEqual(receipt["teardown"], "unverified")
                self.assertFalse(receipt["clean_handoff"])
                self.assertEqual(matrix["failure"]["teardown"], "unverified")
                self.assertFalse(matrix["accepted_timings"])
                directory = root / "aux/cpu-cost"
                path = directory / "output/receipt.json"
                job = {"id": "cpu-cost", "argv": ["fixture", str(path)],
                       "receipt_path": str(path), **{k: receipt[k] for k in
                       ("source_pin", "config_pin", "input_pin")}}

                def auxiliary(owner, name, args, validate, **options):
                    return validate(directory)

                with patch.object(driver, "auxiliary", side_effect=auxiliary):
                    with self.assertRaises(driver.FatalSession):
                        driver.binding_block(SimpleNamespace(root=root),
                            {"id": "cpu_cost", "jobs": [job]})
                self.assertFalse((directory / "complete.json").exists())

    def test_cost_worker_uses_fresh_output_and_root_relative_proof(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            directory = root / "aux/cpu-cost"
            job = {"id": "cpu-cost", "argv": ["fixture", "--receipt", "old"],
                   "receipt_path": "old", "source_pin": "1" * 40,
                   "config_pin": "2" * 64, "input_pin": "3" * 64}

            def auxiliary(owner, name, args, validate, **options):
                directory.mkdir(parents=True)
                path = directory / "output/receipt.json"
                self.assertEqual(args, ["fixture", "--receipt", str(path)])
                self.assertFalse(path.parent.exists())
                path.parent.mkdir()
                data = path.parent / "matrix.json"
                driver.atomic_json(data, {"accepted_timings": False})
                driver.atomic_json(path, {
                    **{k: job[k] for k in
                       ("source_pin", "config_pin", "input_pin")},
                    "clean_handoff": True, "state": "completed",
                    "exit_code": 0, "files": {
                        str(data.relative_to(root)): driver.digest(data)}})
                self.assertEqual(validate(directory), [path, data])
                changed = driver.read_json(path)
                changed["files"] = {"../../outside": "0" * 64}
                driver.atomic_json(path, changed)
                with self.assertRaises(driver.ReviewRequired):
                    validate(directory)
                changed["files"] = {
                    str(data.relative_to(root)): driver.digest(data)}
                driver.atomic_json(path, changed)
                return {"exit_code": 0}

            with patch.object(driver, "auxiliary", side_effect=auxiliary):
                self.assertEqual(driver.binding_block(
                    SimpleNamespace(root=root),
                    {"id": "cpu_cost", "jobs": [job]}), "completed")

    def test_probe_source_uses_floor_20_and_checked_native_rss(self):
        path = Path(__file__).resolve().parents[1] / "onnx/main.go"
        source = path.read_text()
        self.assertEqual(source.count('int(C.memory_level()) < 20'), 2)
        self.assertNotIn('int(C.memory_level()) < 30', source)
        self.assertNotIn('int(C.memory_level()) < 25', source)
        self.assertIn('checkedRSS(uint64(C.current_rss()),', source)




class DatasetBindingTests(unittest.TestCase):
    """Producer stubs test bindings, not real corpus generation."""

    def setup_fixture(self, root, *, nodes=82088, tier1_nodes=81179,
                      repositories=None, bad_answers=False):
        owner = fixture_owner(root, setup=False)
        owner.manifest_file, tools, owner.environment = fixture_tools(root)
        inputs = root / "inputs"
        inputs.mkdir()
        archive = inputs / "tier1.tar.zst"
        archive.write_bytes(b"fixture archive")
        pin = {"sha256": driver.digest(archive),
               "bytes": archive.stat().st_size, "queries": 1}
        driver.atomic_json(inputs / "tiers-summary.json", {"tier1": {
            "archive_sha256": pin["sha256"],
            "archive_bytes": pin["bytes"], "queries": 1}})
        manifest = driver.read_json(owner.manifest_file)
        for name, count in driver.TIER10.items():
            source = inputs / "exports" / (name[0] + "-v4") / name
            source.mkdir(parents=True)
            checksums = {}
            for filename in ("nodes.jsonl", "edges.jsonl", "files.jsonl",
                             "symbols.jsonl", "content.jsonl", "vectors.bin"):
                path = source / filename
                path.write_bytes(b"tiny sealed producer fixture")
                checksums[filename] = driver.digest(path)
                manifest["files"][str(path)] = driver.digest(path)
            path = source / "manifest.json"
            driver.atomic_json(path, {"repo": name, "namespace": name,
                "origin": "real", "generator_version": "gxstudy-v2",
                "seed": 20261022, "counts": {"nodes": count},
                "sha256": checksums})
            manifest["files"][str(path)] = driver.digest(path)
        manifest["jobs"]["*"] = list(manifest["files"])
        driver.atomic_json(owner.manifest_file, manifest)
        owner.environment["STUDY_MANIFEST_SHA"] = driver.digest(
            owner.manifest_file)
        owner.calls = []

        def dataset(tier, count, repos):
            directory = root / "data" / tier
            directory.mkdir(parents=True)
            driver.atomic_json(directory / "queries.json", [{"id": "q"}])
            driver.atomic_json(directory / "expected.json", [{
                "query_id": "bad" if bad_answers and tier == "tier10"
                else "q"}])
            driver.atomic_json(directory / "manifest.json", {
                "repos": repos, "counts": {"nodes": count},
                "sha256": {name: driver.digest(directory / name) for name in
                            ("queries.json", "expected.json")}})

        def extract(archive, destination, prefix):
            if prefix == "tier1":
                dataset("tier1", tier1_nodes, ["K-client-go"])
            else:
                path = destination / prefix / "c/usearch.h"
                path.parent.mkdir(parents=True)
                path.write_bytes(b"header")

        def run(args, **options):
            owner.calls.append(list(args))
            if args[:2] == ["git", "clone"]:
                path = Path(args[-1]) / ".git/HEAD"
                path.parent.mkdir(parents=True)
                path.write_text(driver.COMMIT)
            if args[:2] == ["cmake", "--build"]:
                path = root / "native/build/libusearch_static_c.a"
                path.parent.mkdir(parents=True)
                path.write_bytes(b"static library")
            if args[:2] == ["go", "build"]:
                Path(args[args.index("-o") + 1]).write_bytes(b"binary")
            if args[0] == str(root / "bin/gen"):
                self.assertEqual(args[args.index("-repos") + 1], "10")
                self.assertIn("-synthetic-cross-repo=false", args)
                self.assertEqual(args[args.index("-seed") + 1], "20261022")
                self.assertEqual(len(args[args.index("-sources") + 1]
                                     .split(",")), 10)
                self.assertEqual(driver.read_json(root / "generation.json")
                                 ["route"], "local-mac-setup")
                dataset("tier10", nodes, list(driver.TIER10) if
                        repositories is None else repositories)
            if args[0] == "zstd":
                Path(args[args.index("--output") + 1]).write_bytes(b"archive")
            log = root / "setup" / ("log-" + str(len(owner.calls)))
            log.write_text(driver.COMMIT if args[:2] == ["git", "rev-parse"]
                           else "fixture version")
            return log

        owner.run = run
        owner.window = {"id": "fixture"}
        patches = (patch.object(driver, "ARCHIVES", {
            "tier1": pin, "tier10": None}),
            patch.object(driver, "download"),
            patch.object(driver, "extract", side_effect=extract),
            patch.object(driver, "copy_tree", return_value={}))
        return owner, inputs, patches

    def prepare_fixture(self, root, **options):
        from contextlib import ExitStack
        owner, inputs, patches = self.setup_fixture(root, **options)
        with ExitStack() as context:
            for item in patches:
                context.enter_context(item)
            setup = driver.prepare(owner, inputs, {"driver": "fixture",
                                                     "runtime": {}})
        return owner, inputs, setup

    def test_generated_binding_is_carried_and_resume_hashes_outputs(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            owner, inputs, setup = self.prepare_fixture(root)
            record = setup["archives"]["tier10"]
            self.assertEqual(record["queries"], 1)
            self.assertEqual(record["sha256"], driver.digest(
                Path(record["path"])))
            self.assertNotEqual(record["sha256"],
                "5f2158772b3ebba975935d38cf06c52aab"
                "6a9dacc382bb3c31e08a090bd61c2c"
            )
            job = driver.Job("oracle", "oracle", "tier10")
            identity = driver.case_identity(job, root, setup, 10)
            self.assertEqual(identity["archives"], setup["archives"])
            self.assertEqual(identity["binaries"]["bin/gen"],
                             setup["artifacts"]["bin/gen"])
            with patch.object(owner, "run") as spawn:
                self.assertEqual(driver.prepare(owner, inputs,
                    setup["identity"]), setup)
                spawn.assert_not_called()
            Path(record["path"]).write_bytes(b"drift")
            with self.assertRaises(driver.ReviewRequired):
                driver.prepare(owner, inputs, setup["identity"])

    def test_superseded_and_mismatched_counts_never_bind(self):
        for options in ({"nodes": 423567}, {"nodes": 82087},
                        {"tier1_nodes": 81178}, {"bad_answers": True},
                        {"repositories": ["K-client-go"]}):
            with (self.subTest(options=options),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder).resolve()
                with self.assertRaisesRegex(driver.ReviewRequired,
                                            "repositories/count/query"):
                    self.prepare_fixture(root, **options)
                self.assertFalse((root / "setup-complete.json").exists())
                self.assertFalse((root / "archives.json").exists())

    def test_setup_drift_refuses_at_next_block_before_spawn(self):
        for damage in ("missing", "metadata", "archive", "binary"):
            with (self.subTest(damage=damage),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder)
                selected, setup, _ = DriverTests().fixture(root)
                owner = fixture_owner(root)
                owner.setup = setup
                with patch.object(owner, "permissions", return_value=10000):
                    owner.begin_block({"id": "smoke", "jobs": []}, {})
                if damage == "missing":
                    setup.pop("archives")
                elif damage == "metadata":
                    setup["archives"]["tier10"]["queries"] += 1
                elif damage == "archive":
                    Path(setup["archives"]["tier10"]["path"]).write_bytes(b"x")
                else:
                    (root / "bin" / selected[0].adapter).write_bytes(b"x")
                with (patch.object(owner, "permissions", return_value=10000),
                      patch.object(driver.subprocess, "Popen") as spawn,
                      self.assertRaises(driver.ReviewRequired)):
                    owner.begin_block({"id": "calibration", "jobs": []}, {})
                spawn.assert_not_called()

    def test_case_and_auxiliary_carry_values_without_payload_hashing(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            selected, setup, _ = DriverTests().fixture(root)
            owner = fixture_owner(root)
            owner.environment = driver.environment(root)
            owner.job_bindings = {"fixture": {"gomaxprocs": 10}}
            (root / "python-runtime.json").write_text("{}")
            with patch.object(driver, "digest") as hashes:
                identity = driver.case_identity(selected[0], root, setup, 10)
                self.assertEqual(identity["archives"], setup["archives"])
                self.assertEqual(identity["binary"], setup["artifacts"][
                    "bin/" + selected[0].adapter])
                hashes.assert_not_called()
            owner.job_limit = unittest.mock.Mock()
            with (patch.object(driver, "setup_binding",
                               side_effect=AssertionError("case rehash")),
                  patch.object(driver, "digest",
                               wraps=driver.digest) as hashes,
                  patch.object(owner, "start", side_effect=(
                      driver.DeadlineExceeded("fixture; no study spawn"))),
                  self.assertRaises(driver.DeadlineExceeded)):
                driver.auxiliary(owner, "fixture", ["/fixture/program"],
                                 lambda _: ())
            payloads = {Path(r["path"]) for r in setup["archives"].values()}
            payloads.update(root / p for p in setup["artifacts"]
                            if p.startswith("bin/"))
            self.assertFalse(any(call.args[0] in payloads for call in
                                 hashes.call_args_list))
            started = driver.read_json(root / "aux/fixture/started.json")
            self.assertEqual(started["archives"], owner.binding["archives"])



class ExternalToolBindingTests(unittest.TestCase):
    def test_absolute_hash_bound_tools_ignore_path_and_reject_drift(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            manifest, tools, env = fixture_tools(root)
            with patch.dict(os.environ, {"PATH": "/missing"}):
                self.assertEqual(driver.bad_jobs(manifest, root), set())
                for name, path in tools.items():
                    self.assertEqual(driver.external_tool(name, env), path)
                output = SimpleNamespace(returncode=0, stdout="ok", stderr="")
                with patch.object(driver.subprocess, "run",
                                  return_value=output) as spawn:
                    self.assertEqual(driver.native_output(
                        ["/usr/sbin/sysctl", "-n", "fixture"], env), "ok")
                    self.assertEqual(spawn.call_args.args[0][0],
                                     tools["sysctl"])
                Path(tools["go"]).write_bytes(b"changed")
                with self.assertRaises(driver.ReviewRequired):
                    driver.external_tool("go", env)
            alias = root / "go-alias"
            alias.symlink_to(tools["go"])
            record = driver.read_json(manifest)
            record["tools"]["go"] = str(alias)
            driver.atomic_json(manifest, record)
            env["STUDY_MANIFEST_SHA"] = driver.digest(manifest)
            with self.assertRaises(driver.ReviewRequired):
                driver.external_tool("go", env)


class ScreenReferenceTests(unittest.TestCase):
    def oracle_bindings(self):
        return {driver.Job("oracle", "oracle", t).name: {"gomaxprocs": 10}
                for t in driver.ARCHIVES}

    def test_each_screen_requires_its_verified_same_tier_proof(self):
        for damage in (None, "budget", "correctness", "manifest", "missing"):
            with self.subTest(damage=damage), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder).resolve()
                owner, setup, rows = fixture_references(root)
                oracle = driver.Job("oracle", "oracle", "tier10")
                path = driver.case_directory(root, oracle) / "complete.json"
                receipt = driver.read_json(path)
                if damage == "budget":
                    receipt["state"] = "budget_exceeded"
                    receipt["correctness_pass"] = False
                elif damage == "correctness":
                    receipt["correctness_pass"] = False
                elif damage == "manifest":
                    output = driver.result_path(root, oracle)
                    report = driver.read_json(output)
                    report["manifest_sha256"] = "0" * 64
                    driver.atomic_json(output, report)
                    receipt["files"][str(output.relative_to(path.parent))] = (
                        driver.digest(output))
                if damage == "missing":
                    path.unlink()
                else:
                    driver.atomic_json(path, receipt)
                screens = [driver.Job("screen", driver.FINALISTS[0], t)
                           for t in driver.ARCHIVES]
                before = path.read_bytes() if path.exists() else None
                with (patch.object(driver, "jobs", return_value=screens),
                      patch.object(owner, "start",
                                   side_effect=AssertionError) as spawn):
                    selected = driver.eligible_sdk(
                        owner, rows, setup, self.oracle_bindings(), "screen")
                self.assertEqual(selected, screens if damage is None else
                                 [screens[0]])
                spawn.assert_not_called()
                self.assertEqual(path.read_bytes() if path.exists() else None,
                                 before)

    def test_blocked_screen_keeps_independent_tier_catalog_and_hybrid(self):
        for accepted in ({"tier1"}, set()):
            with self.subTest(accepted=accepted), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder).resolve()
                owner, setup, rows = fixture_references(root)
                oracles = [driver.Job("oracle", "oracle", t)
                           for t in driver.ARCHIVES]
                screens = [driver.Job("screen", driver.FINALISTS[0], t)
                           for t in driver.ARCHIVES]
                for oracle in oracles:
                    if oracle.tier not in accepted:
                        path = driver.case_directory(root, oracle) / (
                            "complete.json")
                        receipt = driver.read_json(path)
                        receipt.update(state="budget_exceeded",
                                       correctness_pass=False)
                        driver.atomic_json(path, receipt)
                admission = {"blocks": [{"id": "setup", "jobs": []}, {
                    "id": "storage", "jobs": [{"id": j.name,
                    "gomaxprocs": 10} for j in oracles + screens]},
                    {"id": "catalog", "jobs": []},
                    {"id": "hybrid", "jobs": []}]}
                launched = []
                def begin(block, outcomes):
                    owner.block_id = block["id"]
                    owner.block_deadline = 10000
                def stage(supervisor, selected, prepared, concurrency, sdk):
                    for job in selected:
                        if job.stage == "oracle":
                            sdk.append(driver.read_json(
                                driver.case_directory(root, job) /
                                "complete.json"))
                        else:
                            launched.append(job)
                            sdk.append({"identity": {
                                "job": driver.asdict(job)}, "state": "pass"})
                owner.begin_block = begin
                owner.stop_all = unittest.mock.Mock()
                with (patch.object(driver, "jobs", return_value=tuple(
                          oracles + screens)),
                      patch.object(driver, "prepare", return_value=setup),
                      patch.object(driver, "execute_stage", side_effect=stage),
                      patch.object(driver, "run_catalog_hybrid",
                                   return_value="completed") as independent,
                      patch.object(driver, "block_files", return_value={}),
                      patch.object(driver.time, "monotonic", return_value=0),
                      patch.object(driver.signal, "signal"),
                      patch.object(driver.signal, "setitimer"),
                      patch.object(driver.subprocess, "Popen") as spawn):
                    result = driver.execute_session(owner, admission,
                                                    root, {}, 1)
                self.assertEqual(launched, [j for j in screens
                                           if j.tier in accepted])
                self.assertEqual(result["blocks"]["storage"]["state"],
                                 "dependency_blocked")
                self.assertIn("tier10", result["blocks"]["storage"]["reason"])
                self.assertEqual([c.args[-1]
                                  for c in independent.call_args_list],
                                 ["catalog", "hybrid"])
                for block in ("catalog", "hybrid"):
                    self.assertEqual(result["blocks"][block]["state"],
                                     "completed")
                spawn.assert_not_called()


if __name__ == "__main__":
    unittest.main()
