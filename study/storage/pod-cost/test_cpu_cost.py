"""Tiny stdlib fixtures; never load tokenizers, models or ORT."""
import copy
import hashlib
import json
import math
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest

import cpu_cost as cost


class CostTests(unittest.TestCase):
    def test_complete_matrix(self):
        cases = cost.matrix()
        self.assertEqual(len(cases), 25)
        self.assertEqual(len([c for c in cases if c["corpus"] != "query"]), 20)
        self.assertEqual({c["threads"] for c in cases}, {1, 2, 4, 8, 16})
        self.assertEqual({(c["corpus"], c["cap"]) for c in cases},
                         {("metadata", 8192), ("body", 512), ("body", 2048),
                          ("body", 8192), ("query", 8192)})
        cases[0]["repeats"].append("local")
        self.assertEqual(cases[1]["repeats"], [])

    def test_rows_prefix_duplicate_rejection_and_no_mutation(self):
        rows = [{"id": "a", "text": "code"}, {"id": "b", "text": "query"}]
        before = copy.deepcopy(rows)
        self.assertEqual(cost.input_texts(rows, 2, "metadata"),
                         (["a", "b"], ["code", "query"]))
        self.assertEqual(cost.input_texts(rows, 2, "query")[1][0],
                         cost.PREFIX + "code")
        for bad in ([], rows[:1], [rows[0], rows[0]], [{}, rows[1]],
                    [None, rows[1]], [{"id": [], "text": "x"}, rows[1]]):
            self.assertIsInstance(cost.input_texts(bad, 2, "body"),
                                  cost.Invalid)
        self.assertEqual(rows, before)

    def test_padding_indices_and_query_batches(self):
        encoded = {"input_ids": [[101, 102], [101, 7, 102]],
                   "attention_mask": [[1, 1], [1, 1, 1]],
                   "token_type_ids": [[0, 0], [0, 0, 0]]}
        before = copy.deepcopy(encoded)
        values = cost.batches(encoded, "metadata")
        self.assertEqual(values[0]["shape"], [2, 3])
        self.assertEqual(values[0]["indices"], [0, 1])
        self.assertEqual(values[0]["inputs"]["input_ids"],
                         [101, 102, 0, 101, 7, 102])
        self.assertEqual(values[0]["inputs"]["attention_mask"],
                         [1, 1, 0, 1, 1, 1])
        self.assertEqual(encoded, before)
        single = cost.batches(encoded, "query")
        self.assertEqual([b["shape"] for b in single], [[1, 2], [1, 3]])

    def test_first_200_then_length_order_and_quadratic_limit(self):
        ids = [[101] for _ in range(200)] + [[101] * 3000, [101]]
        encoded = {name: ids for name in
                   ("input_ids", "attention_mask", "token_type_ids")}
        values = cost.batches(encoded, "metadata")
        self.assertEqual([i for b in values for i in b["indices"]],
                         [*range(200), 201, 200])
        for batch in values:
            rows, width = batch["shape"]
            self.assertTrue(rows == 1 or rows * width * width <= 4_000_000)
            self.assertLessEqual(rows, 16)
            self.assertFalse(min(batch["indices"]) < 200
                             <= max(batch["indices"]))

    def test_each_cosine_not_only_mean_and_threshold(self):
        left, right = [[1.0, 0.0]] * 200, [[1.0, 0.0]] * 200
        right[-1] = [9998, math.sqrt(10000 ** 2 - 9998 ** 2)]
        result = cost.cosine_gate(left, right)
        self.assertAlmostEqual(result["minimum"], 0.9998)
        self.assertGreater(sum(result["scores"]) / 200, 0.9999)
        self.assertFalse(result["passed"])
        boundary = cost.cosine_gate([[1, 0]], [[9999, math.sqrt(19999)]])
        self.assertTrue(boundary["passed"])
        self.assertTrue(cost.cosine_gate([[3, 4]], [[6, 8]])["passed"])
        for bad in ([], [[0, 0]], [[float("nan"), 1]], [[float("inf"), 1]],
                    [[1]], [["bad", 1]], [[1, 0], [1, 0]]):
            self.assertIsInstance(cost.cosine_gate(bad, [[1, 0]]),
                                  cost.Invalid)

    def test_symbol_cost_not_chunk_count_and_no_partial_symbol_claim(self):
        report = {"status": "completed", "seconds": 60,
                  "minutes_per_10k_symbols": 2, "vectors": [[1, 0]]}
        before = copy.deepcopy(report)
        result = cost.repeat_metrics(report, 2500, "metadata")
        self.assertEqual(result["minutes_per_10k_symbols"], 4)
        self.assertEqual(result["minutes_per_10k_text_rows"], 2)
        self.assertNotIn("vectors", result)
        self.assertNotIn("minutes_per_10k_symbols",
                         cost.repeat_metrics(report, 2500, "query"))
        self.assertNotIn("minutes_per_10k_symbols", cost.repeat_metrics(
            {**report, "status": "partial"}, 2500, "body"))
        self.assertEqual(report, before)

    def test_skip_time_admission_refusal(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            command = [sys.executable, "-c", "exit(99)"]
            with self.assertRaises(cost.Skip) as caught:
                cost.child(command, root / "none.log", 0, root, os.environ)
            self.assertEqual(caught.exception.code, "time")
            self.assertFalse((root / "none.log").exists())

    def test_skip_gate_only_complete_matching_reports(self):
        warm = {"status": "completed", "token_file_sha256": "a" * 64,
                "vectors": [[1.0, *([0.0] * 767)] for _ in range(200)]}
        reference = {"status": "completed", "token_sha256": "a" * 64,
                     "runtime": "1.31.0", "intra_op_threads": 16,
                     "inter_op_threads": 1,
                     "vectors": copy.deepcopy(warm["vectors"])}
        warm["vectors"][-1][0] = -1
        with self.assertRaises(cost.Skip) as caught:
            cost.checked_gate(warm, reference, "a" * 64)
        self.assertEqual(caught.exception.code, "gate")
        with self.assertRaises(cost.FatalWorker):
            cost.checked_gate(warm, {**reference, "vectors": []}, "a" * 64)

    def test_skip_library_missing_or_hash_mismatch(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            library = root / "library.so"
            for content in (None, b"wrong library bytes"):
                with self.subTest(content=content):
                    if content is not None:
                        library.write_bytes(content)
                    with self.assertRaises(cost.Skip) as caught:
                        cost.verify_library(library, "0" * 64, root)
                    self.assertEqual(caught.exception.code, "library")
                    if content is not None:
                        self.assertEqual(library.read_bytes(), content)

    def test_skip_check_has_complete_recorded_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            command = [sys.executable, "-c", "print('failed check'); exit(1)"]
            log = root / "check.log"
            with self.assertRaises(cost.Skip) as caught:
                cost.successor_check(command, log, time.monotonic() + 5,
                                     root, os.environ)
            self.assertEqual(caught.exception.code, "check")
            report = json.loads(log.with_suffix(".json").read_text())
            self.assertEqual(report["status"], "completed")
            self.assertEqual(report["exit_code"], 1)
            self.assertFalse(report["passed"])
            self.assertIn("failed check", log.read_text())

    def test_fatal_worker_crash_even_with_completed_report(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for kind in ("reference", "probe"):
                with self.subTest(worker=kind):
                    output = root / f"{kind}.json"
                    command = [sys.executable, "-c",
                               "import pathlib,sys; "
                               "pathlib.Path(sys.argv[1]).write_text("
                               "'{\"status\":\"completed\"}'); exit(7)",
                               str(output)]
                    with self.assertRaises(cost.FatalWorker):
                        cost.completed_worker(command, output,
                                              time.monotonic() + 5,
                                              root, os.environ)
                    data = json.loads(output.read_text())
                    self.assertEqual(data["status"], "completed")

    def test_fatal_worker_killed_or_timed_out_and_reaped(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for kind in ("reference", "probe"):
                with self.subTest(worker=kind):
                    output = root / f"{kind}.json"
                    command = [sys.executable, "-c", "import os,signal; "
                               "os.kill(os.getpid(), signal.SIGKILL)"]
                    with self.assertRaises(cost.FatalWorker):
                        cost.completed_worker(command, output,
                                              time.monotonic() + 5,
                                              root, os.environ)
            command = [sys.executable, "-c", "import os,signal; "
                       "open('pid','w').write(str(os.getpid())); "
                       "signal.pause()"]
            with self.assertRaises(cost.FatalWorker):
                cost.child(command, root / "timeout.log",
                           time.monotonic() + 1, root, os.environ)
            with self.assertRaises(ProcessLookupError):
                os.kill(int((root / "pid").read_text()), 0)

    def test_fatal_worker_reportless_success(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for kind in ("reference", "probe"):
                with self.subTest(worker=kind):
                    with self.assertRaises(cost.FatalWorker):
                        cost.completed_worker([sys.executable, "-c", "pass"],
                                              root / f"{kind}.json",
                                              time.monotonic() + 5,
                                              root, os.environ)

    def test_fatal_worker_incomplete_or_malformed_report(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for number, content in enumerate(
                    ("{}", '{"status":"partial"}', "{")):
                with self.subTest(report=content):
                    command = [sys.executable, "-c", "import pathlib,sys; "
                               "p=pathlib.Path(sys.argv[1]); "
                               "p.write_text(sys.argv[2])",
                               str(root / f"bad-{number}.json"), content]
                    with self.assertRaises(cost.FatalWorker):
                        cost.completed_worker(command,
                                              root / f"bad-{number}.json",
                                              time.monotonic() + 5,
                                              root, os.environ)

    def test_probe_flags_and_fatal_recipe_mismatch(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            tokens = root / "tokens.json"
            tokens.write_text(json.dumps([{
                "indices": [0], "shape": [1, 1], "inputs": {
                    "input_ids": [101], "attention_mask": [1],
                    "token_type_ids": [0]}}]))
            fixture = {"runtime": "1.31.0", "disable_prepacking": True,
                       "inter_op_threads": 1, "intra_op_threads": 8,
                       "token_file_sha256": hashlib.sha256(
                           tokens.read_bytes()).hexdigest(),
                       "status": "completed", "vectors": [[1.0] * 768],
                       "planned_texts": 1, "completed_texts": 1,
                       "seconds": 1, "load_seconds": 0}
            script = root / "onnxprobe"
            payload = json.dumps(json.dumps(fixture))
            script.write_text(
                "#!/usr/bin/env python3\n"
                "import json, pathlib, sys\n"
                "p=pathlib.Path(sys.argv[sys.argv.index('-output')+1])\n"
                f"r=json.loads({payload})\n"
                "p.write_text(json.dumps(r))\n"
                "p.with_suffix('.args').write_text(' '.join(sys.argv))\n")
            script.chmod(0o700)
            args = cost.argparse.Namespace(root=root, library=root / "fake.so")
            case = {"threads": 8, "corpus": "body", "cap": 512}
            output = root / "report.json"
            result = cost.probe(args, case, tokens, output, 2,
                                time.monotonic() + 5, os.environ)
            self.assertEqual(result["status"], "completed")
            invocation = output.with_suffix(".args").read_text()
            for flag in ("-no-prepack", "-threads 8", "-cap 512", "-repeat 2",
                         "-planned-repeats 3", "-model coderank", "-guard"):
                self.assertIn(flag, invocation)
            with self.assertRaises(cost.FatalWorker):
                cost.probe(args, {**case, "threads": 1}, tokens,
                           root / "wrong.json", 1,
                           time.monotonic() + 5, os.environ)

    def test_skip_identity_receipt_and_partial_replay_refusal(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = root / "config.json"
            receipt = root / "results/receipt.json"
            config.write_text("{}")
            argv = [sys.executable, cost.__file__, "--root", str(root),
                    "--config", str(config), "--receipt", str(receipt),
                    "--source-pin", "a" * 40, "--config-pin", "b" * 64,
                    "--input-pin", "c" * 64]
            result = subprocess.run(argv, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            record = json.loads(receipt.read_text())
            self.assertEqual((record["state"], record["reason_code"]),
                             ("skipped", "identity"))
            self.assertTrue(record["clean_handoff"])
            self.assertEqual(record["source_pin"], "a" * 40)
            for relative, sha in record["files"].items():
                self.assertEqual(hashlib.sha256((root / relative).read_bytes())
                                 .hexdigest(), sha)
            before = receipt.read_bytes()
            self.assertNotEqual(subprocess.run(argv, capture_output=True)
                                .returncode, 0)
            self.assertEqual(receipt.read_bytes(), before)
            receipt.unlink()
            self.assertNotEqual(subprocess.run(argv, capture_output=True)
                                .returncode, 0)
            self.assertFalse(receipt.exists())


if __name__ == "__main__":
    unittest.main()
