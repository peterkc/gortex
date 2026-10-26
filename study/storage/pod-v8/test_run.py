"""Tiny Linux fixtures only. Never launch Gortex or load real corpora."""

import copy
import importlib.util
import json
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import time
import unittest


HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("v8_runner", HERE / "run.py")
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
            "v4": {"edges": {"historical": 2, "v8": 3}}, "v5": {}})
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


class BoundaryTests(unittest.TestCase):
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
            attempt = runner.Attempt(run, time.time() - 1)
            with self.assertRaises(runner.Unsafe):
                attempt.command([sys.executable, "-c", "raise Exception()"])
            self.assertEqual((run / "partial.log").read_text(), "incomplete")
            self.assertFalse((run / "command-1.log").exists())
            with self.assertRaises(FileExistsError):
                run.mkdir(exist_ok=False)

    @unittest.skipUnless(sys.platform == "linux", "Linux lifecycle fixture")
    def test_cleanup_reaps_owned_process_and_refuses_surviving_socket(self):
        with tempfile.TemporaryDirectory() as tmp:
            run = Path(tmp)
            attempt = runner.Attempt(run, time.time() + 30)
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


if __name__ == "__main__":
    unittest.main()
