"""Tiny native fixtures. Never launch Gortex or load corpora."""

import copy
import importlib.util
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import time
import unittest
from types import SimpleNamespace
from unittest.mock import patch


HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("v9_runner", HERE / "run.py")
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)


def fixture():
    db = sqlite3.connect(":memory:")
    db.executescript("""
        CREATE TABLE nodes(id TEXT,view_gen INTEGER,repo_prefix TEXT);
        CREATE TABLE edges(from_id TEXT,to_id TEXT,view_gen INTEGER,
                           kind TEXT,confidence REAL,cross_repo INTEGER);
        CREATE TABLE files(id TEXT);
        CREATE TABLE file_index_failures(id TEXT);
        CREATE TABLE analysis_active_generation(
            view_gen INTEGER,slot INTEGER,generation_id INTEGER);
        CREATE TABLE analysis_generations(
            generation_id INTEGER,state INTEGER,node_count INTEGER,
            view_gen INTEGER);
        CREATE TABLE analysis_nodes(generation_id INTEGER,node_id TEXT);
        INSERT INTO nodes VALUES('a',0,'P-a'),('b',0,'P-b');
        INSERT INTO edges VALUES('a','b',0,'calls',0.85,1),
                                ('a','b',0,'imports',0,1),
                                ('a','a',0,'co_change',0,0);
        INSERT INTO files VALUES('f');
        INSERT INTO analysis_active_generation VALUES(0,1,9);
        INSERT INTO analysis_generations VALUES(9,1,2,0);
        INSERT INTO analysis_nodes VALUES(9,'a'),(9,'b');
    """)
    spec = {"repositories": [{"label": "a"}, {"label": "b"}],
            "pairs": [["a", "b"], ["b", "a"]],
            "runtime_pairs": [["a", "b"]], "history": {
                "v4": {"nodes": 2, "edges": 2},
                "v5": {"nodes": 2, "edges": 3}}}
    return db, spec


def config_fixture(root):
    root = root.resolve()
    root.mkdir(parents=True, exist_ok=True)
    control = root / "control"
    control.mkdir(exist_ok=True)
    binary, receipt, linkage = [control / name for name in (
        "gortex-1ef632d2", "receipt.json", "linkage.json")]
    binary.write_bytes(b"synthetic binary, never executed")
    runner.write(receipt, {"schema": "synthetic-d6", "receipt_text":
                          "untrusted prose; never interpreted"})
    receipt_sha, binary_sha = runner.digest(receipt), runner.digest(binary)
    runner.write(linkage, {
        "receipt_sha256": receipt_sha, "binary_sha256": binary_sha,
        "architecture": "arm64", "linked_libraries": ["/usr/lib/libSystem"]})
    manifest = control / "manifest.json"
    files = {str(p): runner.digest(p) for p in (receipt, binary, linkage)}
    runner.write(manifest, {"files": files, "jobs": {"*": list(files)}})
    runner.write(control / "identities.json", {})
    window = control / "window.json"
    runner.write(window, {
        "schema": "mac-window-go/v1", "window_id": "synthetic-window",
        "input_manifest_sha256": runner.digest(manifest),
        "start": "2020-01-01T00:00:00+00:00",
        "end": "2100-01-01T00:00:00+00:00", **{k: True for k in (
            "user_go", "migration_complete", "owner_stop_confirmed",
            "awake", "thermal_safe", "background_quiet")}})
    return {"scratch_root": str(root), "inputs": str(root / "inputs"),
            "manifest_file": str(manifest), "window_file": str(window),
            "identities_file": str(control / "identities.json"), "v9": {
                "receipt": str(receipt), "binary": str(binary),
                "linkage_file": str(linkage), "admitted_sets": "P",
                "smoke_receipt": str(root / "aux/v9-smoke/receipt.json"),
                "fit": {s: {"estimated_peak_bytes": 1} for s in "PRK"}}}


def attempt_fixture(directory, deadline, *, mode, swapouts=0,
                    gomaxprocs="10"):
    config = config_fixture(directory)
    with patch.dict(os.environ, {"GOMAXPROCS": gomaxprocs}):
        attempt = runner.Attempt(directory, deadline, mode=mode,
                                 root=directory, config=config,
                                 swapouts=swapouts)
    attempt.birth = 123
    return attempt


class NativeFixture(unittest.TestCase):
    def setUp(self):
        super().setUp()
        self.bound_tool = runner.native.external_tool
        tool = patch.object(runner.native, "external_tool",
                            side_effect=lambda name, env: name)
        tool.start()
        self.addCleanup(tool.stop)
        for target, value in (
                ("native_output", ""), ("daemon_off", None),
                ("native_resources", {"timestamp": 1000.0, "level": 30,
                    "pressure_state": 1, "swapouts": 0, "page_size": 16384,
                    "swap_usage": "total = 1G used = 100M\n",
                    "available_bytes": 64 * runner.native.GIB,
                    "load1": 1.0, "load5": 1.0, "load15": 1.0,
                    "ac_power": True}),
                ("darwin_usage", SimpleNamespace(
                    proc_start_abstime=123, resident_size=2048))):
            mocked = patch.object(runner.native, target, return_value=value)
            mocked.start()
            self.addCleanup(mocked.stop)


class EvidenceTests(unittest.TestCase):
    def test_exact_current_generation_and_hand_counted_bridges(self):
        db, spec = fixture()
        self.addCleanup(db.close)
        original = copy.deepcopy(spec)
        got = runner.measure(db, spec, "P")
        self.assertEqual((got["nodes"], got["edges"], got["files"]), (2, 3, 1))
        self.assertEqual(got["file_index_failures"], 0)
        self.assertEqual(got["co_change"], 1)
        self.assertEqual(got["owned_cross_total"], 2)
        self.assertEqual(got["cross"], [["calls", 0.85, 1, 1],
                                        ["imports", 0.0, 1, 1]])
        self.assertTrue(got["active_covers_all_current_nodes"])
        self.assertEqual(got["coverage"], [
            {"source": "a", "target": "b", "runtime": True,
             "imports": 1, "calls": 1},
            {"source": "b", "target": "a", "runtime": False,
             "imports": 0, "calls": 0}])
        self.assertEqual(got["differences"], {
            "v4": {"edges": {"historical": 2, "v9": 3}}, "v5": {}})
        self.assertEqual(spec, original)

    def test_same_count_wrong_nodes_is_not_coverage(self):
        db, spec = fixture()
        self.addCleanup(db.close)
        db.execute("UPDATE analysis_nodes SET node_id='wrong' "
                   "WHERE node_id='b'")
        self.assertFalse(runner.measure(db, spec, "P")[
            "active_covers_all_current_nodes"])

    def test_stale_history_or_different_payload_is_not_active(self):
        for sql in ("UPDATE analysis_generations SET state=2",
                    "DELETE FROM analysis_active_generation",
                    "UPDATE analysis_generations SET view_gen=1"):
            with self.subTest(sql=sql):
                db, spec = fixture()
                try:
                    db.execute(sql)
                    self.assertFalse(runner.measure(db, spec, "P")[
                        "active_covers_all_current_nodes"])
                finally:
                    db.close()

    def test_missing_member_is_not_a_whole_index_pass(self):
        db, spec = fixture()
        self.addCleanup(db.close)
        spec["repositories"].append({"label": "missing"})
        self.assertFalse(runner.measure(db, spec, "P")[
            "active_covers_all_current_nodes"])

    def test_frozen_public_manifest_has_all_23_unique_members(self):
        specs = json.loads((HERE / "inputs.json").read_text())
        self.assertEqual(set(specs), set("KPR"))
        for set_id, expected in (("K", 12), ("P", 6), ("R", 5)):
            members = specs[set_id]["repositories"]
            self.assertEqual(len(members), expected)
            self.assertEqual(len({r["label"] for r in members}), expected)
            for member in members:
                self.assertRegex(member["commit"], "^[0-9a-f]{40}$")
                self.assertTrue(member["url"].startswith(
                    "https://github.com/"))
        self.assertEqual(len(specs["P"]["runtime_pairs"]), 7)
        self.assertEqual(len(specs["R"]["runtime_pairs"]), 9)
        self.assertNotIn("v5", specs["R"]["history"])


class BoundaryTests(NativeFixture):
    def test_approval_matches_nonce_paths_pins_and_notice(self):
        request = {"nonce": "one", "phase": "restart", "build": runner.BUILD,
                   "paths": {"HOME": "/work/smoke/P/home"}}
        answer = {**request, "lead_go": True, "pi_gortex_notified": True}
        self.assertTrue(runner.go_matches(request, answer))
        for key, value in (("nonce", "old"), ("phase", "start"),
                           ("paths", {}), ("lead_go", False),
                           ("pi_gortex_notified", False)):
            self.assertFalse(runner.go_matches(
                request, {**answer, key: value}))
        self.assertEqual(request["nonce"], "one")

    def test_wrong_daemon_is_unsafe_not_semantic_rejection(self):
        with self.assertRaises(runner.Unsafe):
            runner.ready({"pid": 999}, 123, "/work/smoke/P/d.sock", [])

    def test_non_idle_or_missing_member_is_not_ready(self):
        status = {"pid": 123, "socket_path": "/work/smoke/P/d.sock",
                  "version": runner.VERSION, "ready": True,
                  "enrichment_complete": True,
                  "enrichment": {"running": False},
                  "tracked_repos": [{"prefix": "P-a"}],
                  "build_lane": {"active": False, "background_queued": 0,
                                 "interactive_queued": 0}}
        self.assertTrue(runner.ready(
            status, 123, status["socket_path"], ["P-a"]))
        self.assertFalse(runner.ready(
            status, 123, status["socket_path"], ["P-b"]))
        status["build_lane"]["active"] = True
        self.assertFalse(runner.ready(
            status, 123, status["socket_path"], ["P-a"]))
        status["build_lane"]["active"] = False
        status["tracked_repos"] = None  # a fresh daemon reports null
        self.assertTrue(runner.ready(status, 123, status["socket_path"], []))
        self.assertFalse(runner.ready(
            status, 123, status["socket_path"], ["P-a"]))

    def test_escaped_or_symlinked_paths_rejected_without_writes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "link").symlink_to(root, target_is_directory=True)
            for path in (root / ".." / "escaped", root / "link" / "new"):
                with self.assertRaises(runner.Unsafe):
                    runner.inside(path, root)
            self.assertFalse((root / "new").exists())

    def test_deadline_retains_partial_evidence_and_no_replay(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            run = root / "attempt"
            run.mkdir()
            (run / "partial.log").write_text("incomplete")
            attempt = attempt_fixture(run, time.time() - 1, mode="smoke")
            with self.assertRaises(runner.native.DeadlineExceeded):
                attempt.command([sys.executable, "-c", "raise Exception()"])
            self.assertEqual((run / "partial.log").read_text(), "incomplete")
            self.assertFalse((run / "command-1.log").exists())
            with self.assertRaises(FileExistsError):
                run.mkdir(exist_ok=False)

    def test_cleanup_reaps_owned_process_and_refuses_surviving_socket(self):
        with tempfile.TemporaryDirectory() as tmp:
            run = Path(tmp)
            attempt = attempt_fixture(run, time.time() + 30, mode="smoke")
            child = subprocess.Popen(
                [sys.executable, "-c", "import time; time.sleep(20)"],
                env=attempt.env)
            self.addCleanup(child.wait)
            attempt.daemon = child
            Path(attempt.env["GORTEX_DAEMON_SOCKET"]).touch()
            with self.assertRaises(runner.Unsafe):
                attempt.stop()
            self.assertIsNotNone(child.poll())
            self.assertTrue(Path(attempt.env["GORTEX_DAEMON_SOCKET"]).exists())

    def test_cleanup_refuses_surviving_owned_descendant(self):
        with tempfile.TemporaryDirectory() as tmp:
            attempt = attempt_fixture(Path(tmp), time.time() + 30,
                                      mode="smoke")
            child = SimpleNamespace(poll=lambda: 0,
                                    wait=unittest.mock.Mock(),
                                    terminate=unittest.mock.Mock())
            attempt.daemon = child
            with patch.object(runner.native, "native_output", return_value=(
                    "p9501\nn" + str(attempt.binary) + " (deleted)\n")):
                with self.assertRaisesRegex(runner.Unsafe, "process survived"):
                    attempt.stop()
            child.wait.assert_called_once_with(timeout=30)
            child.terminate.assert_not_called()


class PartialReceiptTests(NativeFixture):
    def finish(self, root, attempt, reason, *, state="partial"):
        receipt = root / "results/receipt.json"
        outcome = runner.failure(reason)
        code = runner.finish(root, root / "attempt", receipt,
                             {"source_pin": "fixed"}, attempt, outcome)
        self.assertEqual(code, 1)
        result = json.loads(receipt.read_text())
        self.assertEqual(result["state"], state)
        self.assertEqual(result["source_pin"], "fixed")
        self.assertEqual(result["exit_code"], 1)
        for name, sha in result["files"].items():
            self.assertEqual(runner.digest(root / name), sha)
        self.assertIn("results/outcome.json", result["files"])
        return result

    def test_deadline_after_verified_stop_has_clean_partial_receipt(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            attempt = attempt_fixture(root / "attempt", time.time() - 1,
                                      mode="smoke")
            with self.assertRaises(runner.native.DeadlineExceeded) as caught:
                attempt.observe()
            result = self.finish(root, attempt, caught.exception)
            self.assertTrue(result["clean_handoff"])
            self.assertEqual(result["reason_code"], "time")
            self.assertEqual(result["deadline_origin"], "unit")

    def test_exited_daemon_is_reaped_and_receipt_is_clean_partial(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            attempt = attempt_fixture(root / "attempt", time.time() + 30,
                                      mode="smoke")
            child = subprocess.Popen([sys.executable, "-c", "pass"],
                                     env=attempt.env)
            self.addCleanup(child.wait)
            child.wait(timeout=5)
            attempt.daemon = child
            with self.assertRaises(runner.WorkFailed) as caught:
                attempt.observe()
            result = self.finish(root, attempt, caught.exception)
            self.assertTrue(result["clean_handoff"])
            self.assertIsNone(attempt.daemon)

    def test_isolation_failure_stays_fatal_after_clean_no_process_stop(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            result = self.finish(root, None, runner.Unsafe("wrong daemon"),
                                 state="fatal")
            self.assertFalse(result["clean_handoff"])

    def test_unverified_stop_writes_fatal_receipt_even_without_daemon(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            attempt = attempt_fixture(root / "attempt", time.time() + 30,
                                      mode="smoke")
            sock = Path(attempt.env["GORTEX_DAEMON_SOCKET"])
            sock.touch()
            result = self.finish(root, attempt, OSError("local failure"),
                                 state="fatal")
            self.assertFalse(result["clean_handoff"])
            self.assertEqual(result["reason_code"], "cleanup")
            self.assertTrue(sock.exists())

    def test_fatal_cause_survives_expected_and_unexpected_cleanup(self):
        for cleanup in (runner.Rejected("slow reap"), OSError("reap bug")):
            with (self.subTest(cleanup=type(cleanup).__name__),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder)
                attempt = attempt_fixture(root / "attempt", time.time() + 30,
                                          mode="smoke")
                original = runner.failure(TypeError("original bug"))
                with patch.object(attempt, "stop", side_effect=cleanup):
                    path = root / "results/receipt.json"
                    self.assertEqual(runner.finish(
                        root, root / "attempt", path, {}, attempt,
                        original), 1)
                result = runner.native.read_json(path)
                self.assertEqual(result["state"], "fatal")
                self.assertFalse(result["clean_handoff"])
                self.assertEqual(result["initial_failure"], original)

    def test_output_collection_bug_publishes_fatal_unclean_receipt(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / "attempt").mkdir()
            path = root / "results/receipt.json"
            with patch.object(runner.os, "walk", side_effect=TypeError("bug")):
                self.assertEqual(runner.finish(
                    root, root / "attempt", path, {}, None,
                    {"state": "completed", "exit_code": 0}), 1)
            result = runner.native.read_json(path)
            self.assertEqual((result["state"], result["reason_code"]),
                             ("fatal", "output"))
            self.assertFalse(result["clean_handoff"])

    def test_writer_uses_the_native_atomic_publication_boundary(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "nested/receipt.json"
            with patch.object(runner.native, "atomic_json") as publish:
                runner.write(path, {"state": "partial"})
            self.assertTrue(path.parent.is_dir())
            publish.assert_called_once_with(path, {"state": "partial"})


class FailureRoutingTests(unittest.TestCase):
    def test_blocked_rejected_local_and_isolation_outcomes_stay_distinct(self):
        for error, state, code, clean in (
                (runner.Blocked("gate"), "dependency_blocked", 0, True),
                (runner.Rejected("identity"), "checks_failed", 1, True),
                (runner.WorkFailed("deadline"), "partial", 1, True),
                (OSError("file"), "fatal", 1, False),
                (ValueError("data"), "fatal", 1, False),
                (KeyError("field"), "fatal", 1, False),
                (sqlite3.Error("query"), "fatal", 1, False),
                (runner.native.DeadlineExceeded("time"), "partial", 1, True),
                (TypeError("bug"), "fatal", 1, False),
                (json.JSONDecodeError("bad", "{", 0), "fatal", 1, False),
                (runner.Unsafe("wrong daemon"), "fatal", 1, False)):
            with self.subTest(error=type(error).__name__):
                result = runner.failure(error)
                self.assertEqual(result["state"], state)
                self.assertEqual(result["exit_code"], code)
                self.assertEqual(result["clean_handoff"], clean)

    def test_existing_attempt_or_receipt_is_byte_identical_after_refusal(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            directory, receipt = root / "attempt", root / "receipt.json"
            directory.mkdir()
            (directory / "partial.log").write_bytes(b"original partial\x00")
            (directory / "outcome.json").write_bytes(b"original outcome")
            receipt.write_bytes(b"foreign receipt")
            before = {str(p): p.read_bytes() for p in
                      (directory / "partial.log", directory / "outcome.json",
                       receipt)}
            self.assertFalse(runner.claim_attempt(directory, receipt))
            after = {name: Path(name).read_bytes() for name in before}
            self.assertEqual(after, before)
            self.assertFalse(runner.claim_attempt(root / "fresh", receipt))
            self.assertFalse((root / "fresh").exists())
            receipt.unlink()
            self.assertFalse(runner.claim_attempt(directory, receipt))
            self.assertFalse(receipt.exists())
            self.assertEqual((directory / "partial.log").read_bytes(),
                             b"original partial\x00")

    def test_fresh_attempt_claim_does_not_write_a_receipt(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            self.assertTrue(runner.claim_attempt(root / "fresh",
                                                 root / "receipt.json"))
            self.assertEqual(list((root / "fresh").iterdir()), [])
            self.assertFalse((root / "receipt.json").exists())


class TimingCpuTests(NativeFixture):
    def test_unbound_gomax_refuses_without_a_default(self):
        with (patch.dict(os.environ, {}, clear=True),
              self.assertRaises(KeyError)):
            runner.environment(Path("/fixture/P"), mode="smoke")

    def test_native_environment_uses_bound_gomax_and_persistent_temp(self):
        """Both modes use explicit CPU and isolated temporary roots."""
        root = Path("/fixture/P")
        # Only the bound CPU limit is inherited.
        expected = {
            "HOME": "/fixture/P/home", "XDG_CONFIG_HOME": "/fixture/P/config",
            "XDG_DATA_HOME": "/fixture/P/data", "XDG_CACHE_HOME": "/fixture/P/cache",
            "XDG_STATE_HOME": "/fixture/P/state", "TMPDIR": "/fixture/P/tmp",
            "TMP": "/fixture/P/tmp", "TEMP": "/fixture/P/tmp",
            "PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "LC_ALL": "C",
            "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_TERMINAL_PROMPT": "0", "GIT_NO_LAZY_FETCH": "1",
            "GOMAXPROCS": "999",
            "GORTEX_DAEMON_SOCKET": "/fixture/P/d.sock",
            "GORTEX_DAEMON_PIDFILE": "/fixture/P/d.pid",
            "GORTEX_DAEMON_LOGFILE": "/fixture/P/daemon.log",
            "GORTEX_DAEMON_STATEFILE": "/fixture/P/d.json",
        }
        with patch.dict(os.environ, {"GOMAXPROCS": "999", "UNRELATED": "not-copied"}):
            smoke = runner.environment(root, mode="smoke")
            baseline = runner.environment(root, mode="baseline_a7")
        self.assertEqual(json.dumps(smoke).encode(), json.dumps(expected).encode())
        self.assertEqual(baseline, expected)

    def test_clients_and_daemon_use_the_mode_environment(self):
        """Given either mode, real command/start boundaries pass the same env."""
        for mode, gomax in (("baseline_a7", "4"), ("smoke", "2")):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                attempt = attempt_fixture(root / "P", time.time() + 30,
                                          mode=mode, gomaxprocs=gomax)
                attempt.observe = unittest.mock.Mock()
                daemon = SimpleNamespace(pid=4242, poll=lambda: None)
                child = SimpleNamespace(returncode=0, poll=lambda: 0,
                                        wait=unittest.mock.Mock(return_value=0))
                sock = Path(attempt.env["GORTEX_DAEMON_SOCKET"])
                status = {"pid": daemon.pid, "socket_path": str(sock),
                          "version": runner.VERSION, "ready": True,
                          "enrichment_complete": True,
                          "enrichment": {"running": False}, "tracked_repos": [],
                          "build_lane": {"active": False, "background_queued": 0,
                                         "interactive_queued": 0}}
                original_write = runner.write

                def local_write(path, value):
                    original_write(path, value)
                    if path.name.endswith(".request.json"):
                        answer = path.with_name(path.name.replace(
                            ".request.json", ".go.json"))
                        original_write(answer, {**value, "lead_go": True,
                                                "pi_gortex_notified": True})

                def spawn(argv, **kwargs):
                    self.assertEqual(kwargs["env"], attempt.env)
                    self.assertEqual(kwargs["env"]["GOMAXPROCS"], gomax)
                    if argv[1:3] == ["daemon", "start"]:
                        runner.native.daemon_off.assert_called_with(
                            {}, attempt.env)
                        sock.touch()
                        return daemon
                    kwargs["stdout"].write(json.dumps(status)
                                           if argv[1:3] == ["daemon", "status"]
                                           else "client")
                    return child

                with patch.object(runner, "write", side_effect=local_write), \
                        patch.object(runner.subprocess, "Popen", side_effect=spawn) as popen, \
                        patch.object(runner.time, "sleep", side_effect=AssertionError):
                    self.assertEqual(attempt.gx("version"), "client")
                    attempt.start("initial", {})
                self.assertEqual([call.args[0][1:3] for call in popen.call_args_list],
                                 [["version"], ["daemon", "start"], ["daemon", "status"]])

    def test_native_cpu_scope_and_gomax_metadata_are_hashed_in_receipt(self):
        """Bind native CPU scope without claiming Linux affinity."""
        for mode, gomax in (("baseline_a7", "10"), ("smoke", "10")):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                attempt = attempt_fixture(root / "attempt/P",
                                          time.time() + 30, mode=mode)
                attempt.stop = unittest.mock.Mock()
                receipt = root / "results/receipt.json"
                self.assertEqual(runner.finish(root, root / "attempt", receipt,
                    {}, attempt, {"state": "completed", "exit_code": 0}), 0)
                result = json.loads(receipt.read_text())
                metadata = root / "attempt/P/cpu.json"
                self.assertEqual(json.loads(metadata.read_text()), {
                    "cpu_scope": "M1 Max 8P+2E; no affinity claim",
                    "GOMAXPROCS": gomax})
                self.assertEqual(result["files"]["attempt/P/cpu.json"],
                                 runner.digest(metadata))


class FrozenV9Tests(NativeFixture):
    def test_command_maps_the_bound_absolute_d6_binary(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            attempt = attempt_fixture(root, time.time() + 30, mode="smoke")
            path = Path(attempt.config["manifest_file"])
            manifest = runner.native.read_json(path)
            manifest["tools"] = {}
            runner.write(path, manifest)
            binary = str(attempt.binary)
            self.assertEqual(manifest["files"][binary],
                             runner.digest(attempt.binary))
            attempt.env.update(STUDY_MANIFEST=str(path),
                               STUDY_MANIFEST_SHA=runner.digest(path))
            process = SimpleNamespace(poll=lambda: 0, returncode=0,
                                      wait=unittest.mock.Mock())
            with (patch.object(attempt, "observe"),
                  patch.object(runner.native, "external_tool",
                               wraps=self.bound_tool) as tool,
                  patch.object(runner.subprocess, "Popen",
                               return_value=process) as spawn):
                self.assertEqual(attempt.command([binary, "version"]), "")
                tool.assert_called_once_with(binary, attempt.env)
                self.assertEqual(spawn.call_args.args[0], [binary, "version"])
                manifest["files"].pop(binary)
                runner.write(path, manifest)
                attempt.env["STUDY_MANIFEST_SHA"] = runner.digest(path)
                with self.assertRaises(runner.native.ReviewRequired):
                    attempt.command([binary, "version"])
                self.assertEqual(spawn.call_count, 1)

    def pins(self, config):
        receipt = Path(config["v9"]["receipt"])
        binary = Path(config["v9"]["binary"])
        for name, value in (("D6_SHA", runner.digest(receipt)),
                            ("SHA", runner.digest(binary))):
            mocked = patch.object(runner, name, value)
            mocked.start()
            self.addCleanup(mocked.stop)

    def probe(self, argv, env):
        if argv[0] == "/usr/bin/lipo":
            return "arm64\n"
        if argv[0] == "/usr/bin/otool":
            return argv[-1] + ":\n\t/usr/lib/libSystem\n"
        if argv[1:] == ["version"]:
            return "gortex " + runner.VERSION + "\n"
        self.fail("unexpected staging probe")

    def main_fixture(self, root, config, mode, run_set):
        config_path = root / "control/native.json"
        runner.write(config_path, config)
        job = ("v9-smoke" if mode == "smoke" else
               "v9-baseline-" + config["v9"]["admitted_sets"])
        receipt = root / "aux" / job / "receipt.json"
        receipt.parent.mkdir(parents=True, exist_ok=True)
        argv = ["run.py", mode, "--root", str(root), "--native-config",
                str(config_path), "--receipt", str(receipt), "--source-pin",
                "a" * 40, "--config-pin", runner.digest(HERE / "run.py"),
                "--input-pin", runner.digest(HERE / "inputs.json")]
        with (patch.dict(os.environ, {"GOMAXPROCS": "10"}),
              patch.object(sys, "argv", argv),
              patch.object(runner.native, "native_output",
                           side_effect=self.probe),
              patch.object(runner, "run_set", side_effect=run_set),
              patch.object(runner.Attempt, "stop")):
            code = runner.main()
        return code, json.loads(receipt.read_text())

    def test_exact_d6_and_manifest_binding_precede_binary_calls(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            config = config_fixture(root)
            self.pins(config)
            original = copy.deepcopy(config)
            with patch.object(runner.native, "native_output",
                              side_effect=self.probe) as probe:
                got = runner.frozen(config, {}, "v9-smoke")
                self.assertEqual(got, {
                    "receipt_sha256": runner.D6_SHA,
                    "binary_sha256": runner.SHA,
                    "linkage_sha256": runner.digest(Path(
                        config["v9"]["linkage_file"]))})
                self.assertEqual(probe.call_count, 3)
            self.assertEqual(config, original)
            receipt = Path(config["v9"]["receipt"])
            receipt.write_text(receipt.read_text() + " ")
            with patch.object(runner.native, "native_output") as probe:
                with self.assertRaisesRegex(runner.Blocked, "staged identity"):
                    runner.frozen(config, {}, "v9-smoke")
                probe.assert_not_called()
            self.assertFalse((root / "v9").exists())

    def test_missing_unbound_or_changed_inputs_never_launch_a_daemon(self):
        for change in ("missing", "unbound", "binary", "linkage"):
            with self.subTest(change=change), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder).resolve()
                config = config_fixture(root)
                self.pins(config)
                linkage = Path(config["v9"]["linkage_file"])
                if change == "missing":
                    linkage.unlink()
                elif change == "binary":
                    Path(config["v9"]["binary"]).write_text("wrong build")
                elif change == "linkage":
                    linkage.write_text("{}")
                else:
                    manifest = Path(config["manifest_file"])
                    record = runner.native.read_json(manifest)
                    record["jobs"] = {"other-job": record["jobs"]["*"]}
                    runner.write(manifest, record)
                with (patch.object(runner.native, "native_output") as probe,
                      patch.object(runner.subprocess, "Popen") as spawn):
                    with self.assertRaises(runner.Blocked):
                        runner.frozen(config, {}, "v9-smoke")
                    probe.assert_not_called()
                    spawn.assert_not_called()

    def test_recorded_architecture_linkage_and_exact_version_must_match(self):
        for changed in ("architecture", "linked_libraries", "version",
                        "empty_version", "probe"):
            with self.subTest(changed=changed), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder).resolve()
                config = config_fixture(root)
                self.pins(config)

                def wrong_probe(argv, env):
                    if changed == "probe":
                        raise runner.native.FatalSession("probe stderr")
                    value = self.probe(argv, env)
                    if changed == "empty_version" and argv[1:] == ["version"]:
                        return ""
                    if ((changed == "architecture" and
                         argv[0].endswith("lipo"))
                            or (changed == "linked_libraries" and
                                argv[0].endswith("otool"))
                            or (changed == "version" and
                                argv[1:] == ["version"])):
                        return "wrong\n"
                    return value

                with (patch.object(runner.native, "native_output",
                                   side_effect=wrong_probe),
                      self.assertRaises(runner.Blocked)):
                    runner.frozen(config, {}, "v9-smoke")

    def test_smoke_then_fit_selected_baseline_uses_k9_not_k12(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            config = config_fixture(root)
            self.pins(config)
            seen = []
            original = (HERE / "inputs.json").read_bytes()

            def run_set(attempt, spec, set_id, identity):
                seen.append(set_id)
                if set_id == "K":
                    self.assertEqual({r["label"] for r in
                                      spec["repositories"]}, runner.K9)
                    self.assertEqual(len(spec["repositories"]), 9)
                    self.assertEqual(spec["history"], {})
                    self.assertTrue(all(set(p) <= runner.K9 for p in
                                        spec["pairs"]))

            code, smoke = self.main_fixture(root, config, "smoke", run_set)
            self.assertEqual((code, seen), (0, ["P", "R"]))
            self.assertTrue(smoke["clean_handoff"])
            self.assertEqual(smoke["completed_sets"], ["P", "R"])
            selected = {}
            for set_id in "PRK":
                config["v9"]["admitted_sets"] = set_id
                code, baseline = self.main_fixture(
                    root, config, "baseline_a7", run_set)
                self.assertEqual(code, 0)
                self.assertEqual(baseline["completed_sets"], [set_id])
                selected.update(baseline["selected"])
            self.assertEqual(seen, ["P", "R", "P", "R", "K"])
            self.assertEqual(selected, {"P": "P6", "R": "R5", "K": "K9"})
            self.assertEqual((HERE / "inputs.json").read_bytes(), original)
            path = root / "aux/v9-baseline-K/receipt.json"
            before = path.read_bytes()
            with patch.object(runner.subprocess, "Popen") as spawn:
                self.assertEqual(self.main_fixture(
                    root, config, "baseline_a7", run_set)[0], 1)
                spawn.assert_not_called()
            self.assertEqual(path.read_bytes(), before)

    def test_combined_work_deadline_keeps_its_actual_bound_origin(self):
        for origin, minutes in (("unit", 120), ("window", 20)):
            with self.subTest(origin=origin), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder).resolve()
                config = config_fixture(root)
                self.pins(config)
                now = 1800000000
                window = Path(config["window_file"])
                go = runner.native.read_json(window)
                end = now + runner.native.WINDOW_CLEANUP_SECONDS + minutes * 60
                go["end"] = time.strftime("%Y-%m-%dT%H:%M:%S+00:00",
                                           time.gmtime(end))
                runner.write(window, go)
                seen = []

                def expire(attempt, spec, set_id, identity):
                    seen.append(attempt.deadline_origin)
                    with patch.object(runner.time, "time",
                                      return_value=attempt.deadline + 1):
                        attempt.observe()

                with patch.object(runner.time, "time", return_value=now):
                    code, result = self.main_fixture(root, config, "smoke",
                                                     expire)
                self.assertEqual(code, 1)
                self.assertEqual(seen, [origin])
                self.assertEqual(result["state"], "partial")
                self.assertEqual(result["reason_code"], "time")
                self.assertEqual(result["deadline_origin"], origin)
                self.assertTrue(result["clean_handoff"])
                self.assertEqual(result["completed_sets"], [])

    def test_single_set_receipt_discloses_other_unmeasured_sets(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            config = config_fixture(root)
            self.pins(config)
            self.main_fixture(root, config, "smoke", lambda *args: None)
            del config["v9"]["fit"]["K"]
            code, result = self.main_fixture(root, config, "baseline_a7",
                                             lambda *args: None)
            self.assertEqual(code, 0)
            self.assertEqual(result["selected"], {"P": "P6"})
            self.assertEqual(result["omitted"], ["R", "K"])
            self.assertEqual(result["completed_sets"], ["P"])

    def test_bad_order_no_fit_partial_and_corrupt_smoke_remain_explicit(self):
        for variant in ("order", "partial", "smoke"):
            with self.subTest(variant=variant), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder).resolve()
                config = config_fixture(root)
                self.pins(config)
                self.main_fixture(root, config, "smoke", lambda *args: None)
                seen = []
                if variant == "order":
                    config["v9"]["admitted_sets"] = "KPR"
                elif variant == "smoke":
                    # Change a hashed output, not the wrapper receipt.
                    path = root / "aux/v9-smoke/attempt/attempt.json"
                    path.write_text("changed")

                def run_set(attempt, spec, set_id, identity):
                    seen.append(set_id)
                    if set_id == "R":
                        raise runner.WorkFailed("interrupted set")

                if variant == "partial":
                    self.main_fixture(root, config, "baseline_a7", run_set)
                    complete = root / "aux/v9-baseline-P/receipt.json"
                    prior = complete.read_bytes()
                    config["v9"]["admitted_sets"] = "R"
                code, result = self.main_fixture(
                    root, config, "baseline_a7", run_set)
                self.assertNotEqual(result["state"], "completed")
                if variant == "partial":
                    self.assertEqual((code, seen), (1, ["P", "R"]))
                    self.assertEqual(result["completed_sets"], [])
                    self.assertEqual(complete.read_bytes(), prior)
                    self.assertTrue(result["clean_handoff"])
                else:
                    self.assertEqual(seen, [])
                self.assertNotEqual(result.get("completed_sets"),
                                    ["P", "R", "K"])

    def test_daemon_rss_uses_birth_and_bytes_at_five_second_cadence(self):
        with tempfile.TemporaryDirectory() as folder:
            attempt = attempt_fixture(Path(folder), time.time() + 30,
                                      mode="smoke")
            attempt.daemon = SimpleNamespace(pid=9501, poll=lambda: None)
            start = time.time()
            with (patch.object(runner.time, "time", return_value=start),
                  patch.object(runner.native, "darwin_usage", return_value=(
                      SimpleNamespace(proc_start_abstime=123,
                                      resident_size=4096))) as usage):
                attempt.observe()
                attempt.observe()
                self.assertEqual(usage.call_count, 1)
                self.assertEqual(attempt.samples[0]["rss_bytes"], 4096)
                self.assertNotIn("rss_kib", attempt.samples[0])
            with (patch.object(runner.time, "time", return_value=start + 5),
                  patch.object(runner.native, "darwin_usage", return_value=(
                      SimpleNamespace(proc_start_abstime=999,
                                      resident_size=4096))),
                  self.assertRaisesRegex(runner.Unsafe, "birth")):
                attempt.observe()
            self.assertEqual(len(attempt.samples), 2)
            self.assertIsNone(attempt.samples[-1]["pid"])
            self.assertEqual(attempt.samples[-1]["rss_bytes"], 0)
            self.assertEqual(len(runner.native.read_json(
                attempt.directory / "rss.json")), 1)

    def test_revoked_go_stops_new_work_but_not_cleanup(self):
        with tempfile.TemporaryDirectory() as folder:
            attempt = attempt_fixture(Path(folder), time.time() + 30,
                                      mode="smoke")
            window = Path(attempt.config["window_file"])
            value = runner.native.read_json(window)
            value["user_go"] = False
            runner.write(window, value)
            with (patch.object(runner.subprocess, "Popen") as spawn,
                  self.assertRaises(runner.native.DeadlineExceeded) as caught):
                attempt.command(["never-launch"])
            spawn.assert_not_called()
            outcome = runner.failure(caught.exception)
            self.assertEqual(outcome["reason_code"], "time")
            self.assertIsNone(outcome["deadline_origin"])
            attempt.stop()
            self.assertFalse(runner.failure(runner.native.FatalSession(
                "installed daemon returned"))["clean_handoff"])

    def test_fresh_tracks_one_restart_and_at_most_one_analysis(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            directory = root / "attempt/P"
            directory.mkdir(parents=True)
            log = directory / "daemon.log"
            spec = {"repositories": [
                {"label": name, "tag": "v1", "commit": "a" * 40}
                for name in ("a", "b")]}
            calls, phases, snapshots = [], [], []

            def command(argv):
                calls.append(list(argv))
                if argv[:2] == ["git", "clone"]:
                    (Path(argv[-1]) / ".git/info").mkdir(parents=True)
                if "rev-parse" in argv:
                    return ("false" if argv[-1] == "--is-shallow-repository"
                            else "a" * 40)
                return ""

            def start(phase, identity):
                phases.append(phase)
                with log.open("a") as output:
                    output.write(json.dumps({
                        "msg": "daemon: warmup summary"}) + "\n")

            def gx(*args):
                calls.append(list(args))
                if args[:2] == ("call", "get_communities"):
                    with log.open("a") as output:
                        output.write('{"msg":"mcp: analysis pass complete"}\n')

            def snapshot(label, spec, set_id):
                snapshots.append(label)
                return {"active_covers_all_current_nodes":
                        label != "after-warmup", "file_index_failures": 0,
                        "coverage": [{"runtime": True, "imports": 1,
                                      "calls": 1}]}

            attempt = SimpleNamespace(
                directory=directory, config={"inputs": str(root / "inputs")},
                command=command, gx=gx, start=start,
                stop=unittest.mock.Mock(), snapshot=snapshot,
                wait_ready=unittest.mock.Mock(), observe=unittest.mock.Mock())
            runner.run_set(attempt, spec, "P", {})
            self.assertEqual(phases, ["start", "restart"])
            self.assertEqual([c[3] for c in calls if c[0] == "track"],
                             ["P-a", "P-b"])
            self.assertEqual(sum(c[:2] == ["call", "get_communities"]
                                 for c in calls), 1)
            self.assertEqual(snapshots, ["before-restart", "after-warmup",
                                         "after-analysis", "final"])
            self.assertEqual(attempt.stop.call_count, 2)
            self.assertFalse(any(c[0] in {"export", "edit", "update"}
                                 for c in calls))

    def test_cleanup_checks_private_binary_and_socket_not_installed_name(self):
        with tempfile.TemporaryDirectory() as folder:
            attempt = attempt_fixture(Path(folder), time.time() + 30,
                                      mode="smoke")
            installed = "/installed/gortex"
            with patch.object(runner.native, "native_output", side_effect=[
                    "n" + installed + "\n", "", ""]) as native:
                attempt.stop()
            # -w keeps lsof's unrelated mount warnings off stderr.
            self.assertTrue(all("-w" in call.args[0] for call in
                                native.call_args_list[:2]))
            with (patch.object(runner.native, "native_output", side_effect=[
                    "", "n" + attempt.env["GORTEX_DAEMON_SOCKET"] + "\n"]),
                  self.assertRaisesRegex(runner.Unsafe, "socket holder")):
                attempt.stop()
            marker = "GORTEX_DAEMON_SOCKET=" + attempt.env[
                "GORTEX_DAEMON_SOCKET"]
            with patch.object(runner.native, "native_output", side_effect=[
                    "", "", "unknown-child " + marker]) as native:
                with self.assertRaisesRegex(runner.Unsafe, "marker process"):
                    attempt.stop()
                self.assertEqual(native.call_args.args[1], {})
                self.assertEqual(native.call_args.args[0], [
                    "/bin/ps", "-Eww", "-U", str(os.getuid()),
                    "-o", "command="])
            with (patch.object(runner.native, "native_output", side_effect=(
                    runner.native.FatalSession("ownership unverified"))),
                  self.assertRaises(runner.native.FatalSession)):
                attempt.stop()

    def test_main_candidate_admission_has_no_fit_or_load_refusal(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            config = config_fixture(root)
            self.pins(config)
            del config["v9"]["fit"]
            self.main_fixture(root, config, "smoke", lambda *args: None)
            sample = {"timestamp": 1000.0, "level": 20, "pressure_state": 2,
                      "available_bytes": 4 * runner.native.GIB, "swapouts": 7,
                      "page_size": 16384, "swap_usage": "used = 100M\n",
                      "load1": 18.0, "load5": 17.0, "load15": 16.0,
                      "ac_power": True}
            seen = []

            def run_set(attempt, spec, set_id, identity):
                seen.append(set_id)
                self.assertEqual(attempt.swapouts, 7)
                attempt.observe()
                runner.write(attempt.directory / "fixture.json", {"ok": True})

            with patch.object(runner.native, "native_resources",
                              side_effect=lambda env: dict(sample)):
                code, result = self.main_fixture(
                    root, config, "baseline_a7", run_set)
            self.assertEqual((code, result["state"], seen),
                             (0, "completed", ["P"]))
            job_root = root / "aux/v9-baseline-P"
            admission = [job_root / rel for rel in result["files"]
                         if rel.endswith("P-admission.json")]
            self.assertEqual(len(admission), 1)
            self.assertEqual(runner.native.read_json(admission[0]), sample)
            recorded = [job_root / rel for rel in result["files"]
                        if rel.endswith("resources.json")]
            self.assertEqual(len(recorded), 1)
            self.assertTrue(sample.keys() <= runner.native.read_json(
                recorded[0])[0].keys())

    def test_full_samples_and_swap_baseline_survive_restart(self):
        with tempfile.TemporaryDirectory() as folder:
            attempt = attempt_fixture(Path(folder).resolve(), time.time() + 30,
                                      mode="baseline_a7", swapouts=7)
            daemon = SimpleNamespace(pid=9501, poll=lambda: None)
            attempt.daemon = daemon
            start = time.time()
            pages = (512 * 1024 * 1024) // 16384
            counts = [7, 7 + pages // 2, 7 + pages, 8 + pages]
            samples = [{"timestamp": start + i * 5, "level": 20,
                        "pressure_state": 1,
                        "available_bytes": 4 * runner.native.GIB,
                        "swapouts": count, "page_size": 16384,
                        "swap_usage": "used = 100M\n", "load1": 18.0,
                        "load5": 17.0, "load15": 16.0, "ac_power": True}
                       for i, count in enumerate(counts)]
            with patch.object(runner.native, "native_resources",
                              side_effect=samples):
                for i in range(3):
                    if i == 2:
                        attempt.daemon = None
                    with patch.object(runner.time, "time",
                                      return_value=start + i * 5):
                        attempt.observe()
                attempt.daemon = daemon
                with (patch.object(runner.time, "time",
                                   return_value=start + 15),
                      self.assertRaises(runner.native.FatalSession)):
                    attempt.observe()
            self.assertEqual(attempt.swapouts, 7)
            rows = runner.native.read_json(
                attempt.directory / "resources.json")
            self.assertEqual([r["swapouts"] for r in rows], counts)
            self.assertTrue(all(samples[0].keys() <= r.keys() for r in rows))
            self.assertEqual([r["pid"] for r in attempt.samples],
                             [9501, 9501, None, None])
            self.assertEqual(runner.failure(runner.native.BudgetExceeded(
                8 * runner.native.GIB + 1,
                8 * runner.native.GIB))["reason_code"],
                "budget-exceeded")

    def test_counter_reset_above_block_baseline_retains_unsafe_sample(self):
        with tempfile.TemporaryDirectory() as folder:
            attempt = attempt_fixture(Path(folder).resolve(), time.time() + 30,
                                      mode="smoke")
            sample = {"timestamp": 1000.0, "level": 30, "pressure_state": 1,
                      "available_bytes": 64 * runner.native.GIB,
                      "page_size": 16384, "swap_usage": "used = 100M\n",
                      "load1": 18.0, "load5": 17.0, "load15": 16.0,
                      "ac_power": True}
            start = time.time()
            with patch.object(runner.native, "native_resources", side_effect=[
                    {**sample, "swapouts": n} for n in (0, 100, 99)]):
                for i in range(2):
                    with patch.object(runner.time, "time",
                                      return_value=start + i * 5):
                        attempt.observe()
                with (patch.object(runner.time, "time",
                                   return_value=start + 10),
                      self.assertRaisesRegex(runner.native.FatalSession,
                                             "reset")):
                    attempt.observe()
            rows = runner.native.read_json(
                attempt.directory / "resources.json")
            self.assertEqual([r["swapouts"] for r in rows], [0, 100, 99])
            self.assertIsNone(rows[-1]["pid"])
            self.assertFalse(runner.failure(runner.native.FatalSession(
                "native swapout counter reset"))["clean_handoff"])


if __name__ == "__main__":
    unittest.main()
