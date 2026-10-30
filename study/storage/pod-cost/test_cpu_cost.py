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
from types import SimpleNamespace
from unittest.mock import patch

import cpu_cost as cost


MAC_WHEEL = ("aaab9b3af536b06ca27ab5e35e3d429c9"
             "7457ce76cf298af103f687e8b9975c0")


class CostTests(unittest.TestCase):
    def test_host_stop_and_expected_gate_receipts_remain_distinct(self):
        for error, state, reason, clean in (
                (cost.HostSafetyStop("memory"), "fatal", "host-stop", False),
                (cost.GateFailure("recipe"), "partial", "gate", True)):
            with (self.subTest(error=type(error).__name__),
                  tempfile.TemporaryDirectory() as folder):
                def prepare(root, output):
                    raise error
                code, record, matrix = self.execute_fixture(
                    Path(folder), prepare)
                self.assertEqual((code, record["exit_code"]), (1, 1))
                self.assertEqual((record["state"], record["reason_code"],
                                  record["clean_handoff"]),
                                 (state, reason, clean))
                self.assertFalse(matrix["accepted_timings"])

    def test_exit_78_is_host_stop_even_without_a_worker_report(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            with (patch.object(cost, "child", return_value=78),
                  self.assertRaises(cost.HostSafetyStop) as caught):
                cost.completed_worker(["probe"], root / "missing.json", 10,
                                      root, {})
            self.assertEqual(caught.exception.reason_code, "host-stop")

    def test_atomic_receipt_syncs_and_preserves_existing_data(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "receipt.json"
            trace = []
            sync, replace = cost.os.fsync, cost.os.replace
            def synced(fd):
                trace.append("fsync")
                sync(fd)
            def published(source, target):
                self.assertEqual(json.loads(source.read_text()), {"a": 1})
                trace.append("replace")
                replace(source, target)
            with (patch.object(cost.os, "fsync", side_effect=synced),
                  patch.object(cost.os, "replace", side_effect=published)):
                cost.save(path, {"a": 1})
            self.assertEqual(trace, ["fsync", "replace", "fsync"])
            before = path.read_bytes()
            with self.assertRaises(FileExistsError):
                cost.save(path, {"a": 2})
            self.assertEqual(path.read_bytes(), before)
            alias = path.with_name("alias.json")
            alias.symlink_to(path)
            with self.assertRaises(FileExistsError):
                cost.save(alias, {"a": 3})
            self.assertEqual(path.read_bytes(), before)

    def test_failed_receipt_sync_keeps_only_the_claimed_temp(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "receipt.json"
            with (patch.object(cost.os, "fsync", side_effect=OSError("sync")),
                  self.assertRaisesRegex(OSError, "sync")):
                cost.save(path, {"a": 1})
            self.assertFalse(path.exists())
            temporary = path.with_name(path.name + ".tmp")
            before = temporary.read_bytes()
            with self.assertRaises(FileExistsError):
                cost.save(path, {"a": 2})
            self.assertEqual(temporary.read_bytes(), before)

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

    def test_named_gate_rejects_nonmatching_reports_and_cosines(self):
        warm = {"status": "completed", "token_file_sha256": "a" * 64,
                "vectors": [[1.0, *([0.0] * 767)] for _ in range(200)]}
        reference = {"status": "completed", "token_sha256": "a" * 64,
                     "runtime": "1.31.0", "intra_op_threads": 16,
                     "inter_op_threads": 1,
                     "vectors": copy.deepcopy(warm["vectors"])}
        warm["vectors"][-1][0] = -1
        with self.assertRaises(cost.GateFailure) as caught:
            cost.checked_gate(warm, reference, "a" * 64)
        self.assertEqual(caught.exception.reason_code, "gate")
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
            with self.assertRaises(cost.Skip) as caught:
                cost.child(command, root / "timeout.log",
                           time.monotonic() + 1, root, os.environ)
            self.assertEqual(caught.exception.code, "time")
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
                    "--input-pin", "c" * 64, "--case", "0"]
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


    def test_frozen_controller_and_source_bindings_refuse_before_work(self):
        for damage in ("controller", "source"):
            with (self.subTest(damage=damage),
                  tempfile.TemporaryDirectory() as folder):
                root = Path(folder).resolve()
                controller = root / "control/pod-cost/cpu_cost.py"
                frozen = root / "checkout/study/storage/onnx/main.go"
                frozen.parent.mkdir(parents=True)
                frozen.write_bytes(b"frozen source")
                old_controller = root / "checkout/study/storage/pod-cost"
                old_controller.mkdir()
                old_controller = old_controller / "cpu_cost.py"
                old_controller.write_bytes(b"frozen controller")
                sources = ({"checkout/study/storage/onnx/main.go":
                            cost.digest(frozen)} if damage == "source" else {})
                config = root / "config.json"
                config.write_text(json.dumps({"inputs": {},
                    "source_files": sources, "ort": {}, "recipe": {}}))
                args = cost.argparse.Namespace(
                    root=root, config=config, case=0,
                    receipt=root / "results/receipt.json",
                    source_pin="a" * 40, config_pin=cost.digest(config),
                    input_pin=hashlib.sha256(b"{}").hexdigest())
                location = (old_controller if damage == "controller"
                            else controller)
                with (patch.object(cost, "__file__", str(location)),
                      patch.object(cost, "successor_check") as build,
                      patch.object(cost, "precompute") as tokens,
                      patch.object(cost, "probe") as probe):
                    self.assertEqual(cost.execute(args), 0)
                record = json.loads(args.receipt.read_text())
                self.assertEqual((record["state"], record["reason_code"]),
                                 ("skipped", "identity"))
                self.assertTrue(record["clean_handoff"])
                build.assert_not_called()
                tokens.assert_not_called()
                probe.assert_not_called()

    def execute_fixture(self, root, prepare, probe=None, cases=None, *,
                        clean=True, wheel_pin=MAC_WHEEL, case=0,
                        tamper_config=False,
                        member_names=("libonnxruntime.1.31.0.dylib",),
                        version="1.31.0", platform_name="darwin",
                        machine="arm64", controller_output=False):
        config = root / "config.json"
        config.write_text(json.dumps({"inputs": {}, "source_files": {},
                                      "ort": {"sha256": wheel_pin},
                                      "recipe": {}}))
        receipt = (root / "aux/cpu-cost/output/receipt.json"
                   if controller_output else root / "results/receipt.json")
        process = receipt.parent.parent / "process.json"
        process.parent.mkdir(parents=True, exist_ok=True)
        process.write_text(json.dumps({
            "absolute_deadline": time.monotonic() + 600}))
        library = root / "library.dylib"
        library.write_bytes(b"fixture library bytes; never loaded")
        members = [SimpleNamespace(
            name=name, hash=SimpleNamespace(
                mode="sha256", value=cost.base64.urlsafe_b64encode(
                    hashlib.sha256(library.read_bytes()).digest())
                .decode().rstrip("="))) for name in member_names]
        distribution = SimpleNamespace(
            version=version, files=members,
            locate_file=unittest.mock.Mock(return_value=library))
        args = cost.argparse.Namespace(
            root=root, config=config, receipt=receipt, case=case,
            source_pin="a"*40, config_pin=cost.digest(config),
            input_pin=hashlib.sha256(b"{}").hexdigest())
        if tamper_config:
            value = json.loads(config.read_text())
            value["ort"]["sha256"] = "0" * 64
            config.write_text(json.dumps(value))
        with (
            patch.dict(os.environ, dict(os.environ)),
            patch.object(cost, "__file__", str(
                root / "control/pod-cost/cpu_cost.py")),
            patch.object(cost, "sys", SimpleNamespace(
                platform=platform_name, executable=sys.executable)),
            patch.object(cost.platform, "machine", return_value=machine),
            patch.object(cost.importlib.metadata, "distribution",
                         return_value=distribution),
            patch.object(cost, "successor_check"),
            patch.object(cost, "external_tool",
                         return_value=str(root / "fixture-go")),
            patch.object(cost, "precompute", side_effect=prepare),
            patch.object(cost, "probe", side_effect=probe),
            patch.object(cost, "matrix", return_value=(
                cases if cases is not None else cost.matrix())),
            patch.object(cost, "reap_children", side_effect=(
                cost.reap_children if clean else lambda: False)),
        ):
            code = cost.execute(args)
        return code, json.loads(args.receipt.read_text()), json.loads(
            (args.receipt.parent / "matrix.json").read_text())

    def test_config_pin_carries_wheel_metadata_protection(self):
        with tempfile.TemporaryDirectory() as folder:
            prepare = unittest.mock.Mock()
            code, receipt, summary = self.execute_fixture(
                Path(folder), prepare, tamper_config=True)
            self.assertEqual(code, 0)
            self.assertEqual(receipt["state"], "skipped")
            self.assertEqual(receipt["reason_code"], "identity")
            self.assertIn("config pin mismatch", receipt["reason"])
            self.assertFalse(summary["accepted_timings"])
            prepare.assert_not_called()

    def test_cli_requires_one_valid_case_before_execute(self):
        base = ["cpu_cost.py", "--root", "/fixture/run", "--config",
                "/fixture/config", "--receipt", "/fixture/receipt"]
        for key in ("source-pin", "config-pin", "input-pin"):
            base += ["--" + key, "a" * 64]
        for selector in ([], ["--case", "-1"], ["--case", "25"],
                         ["--case", "wrong"]):
            with (self.subTest(selector=selector),
                  patch.object(cost.sys, "argv", base + selector),
                  patch.object(cost, "execute") as run,
                  self.assertRaises(SystemExit)):
                cost.main()
            run.assert_not_called()
        for case in range(25):
            with (self.subTest(case=case),
                  patch.object(cost.sys, "argv", base + ["--case", str(case)]),
                  patch.object(cost, "execute", return_value=0) as run):
                self.assertEqual(cost.main(), 0)
                self.assertEqual(run.call_args.args[0].case, case)

    def test_mac_wheel_and_unique_record_route_before_native_work(self):
        cases = (
            ({"member_names": ()}, "shared library unavailable"),
            ({"member_names": ("libonnxruntime.so.1.31.0",)},
             "shared library unavailable"),
            ({"member_names": ("libonnxruntime.1.31.0.dylib",
                               "libonnxruntime.dylib")},
             "shared library unavailable"),
            ({"version": "1.30.0"}, "shared library unavailable"),
        )
        for options, reason in cases:
            with self.subTest(options=options), \
                    tempfile.TemporaryDirectory() as folder:
                prepare = unittest.mock.Mock()
                code, receipt, matrix = self.execute_fixture(
                    Path(folder), prepare, **options)
                prepare.assert_not_called()
                self.assertEqual(code, 0)
                self.assertEqual(receipt["state"], "skipped")
                self.assertEqual(receipt["reason_code"], "library")
                self.assertIn(reason, receipt["reason"])
                self.assertFalse(matrix["accepted_timings"])
                library = Path(folder) / "results/library.json"
                self.assertFalse(library.exists())
        with tempfile.TemporaryDirectory() as folder:
            def stop_after_library(root, output):
                raise cost.Skip("check", "fixture: library bound, no model")

            code, receipt, matrix = self.execute_fixture(
                Path(folder), stop_after_library)
            self.assertEqual((code, receipt["state"], receipt["reason_code"]),
                             (0, "skipped", "check"))
            library = json.loads((Path(folder) / "results/library.json")
                                 .read_text())
            self.assertEqual(library["sha256"],
                             cost.digest(Path(folder) / "library.dylib"))
            self.assertEqual(library["wheel"]["sha256"], MAC_WHEEL)
            self.assertFalse(matrix["accepted_timings"])

    def test_controller_metadata_directory_keeps_fresh_worker_output(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)

            def stop(root, output):
                self.assertEqual(output, root / "aux/cpu-cost/output")
                self.assertTrue((output.parent / "process.json").is_file())
                raise cost.Skip("check", "fixture no native work")

            code, receipt, matrix = self.execute_fixture(
                root, stop, controller_output=True)
            self.assertEqual((code, receipt["state"], receipt["reason_code"]),
                             (0, "skipped", "check"))
            self.assertFalse(matrix["accepted_timings"])
            for relative, sha in receipt["files"].items():
                self.assertTrue(relative.startswith("aux/cpu-cost/output/"))
                self.assertEqual(cost.digest(root / relative), sha)

    def test_mac_only_admission_and_unexpected_error_is_fatal(self):
        for options in ({"platform_name": "linux", "machine": "x86_64"},
                        {"machine": "x86_64"}):
            with self.subTest(options=options), \
                    tempfile.TemporaryDirectory() as folder:
                prepare = unittest.mock.Mock()
                code, receipt, matrix = self.execute_fixture(
                    Path(folder), prepare, **options)
                prepare.assert_not_called()
                self.assertEqual((code, receipt["state"]), (1, "partial"))
                self.assertIn("only Darwin arm64", receipt["reason"])
                self.assertFalse(matrix["accepted_timings"])
        with tempfile.TemporaryDirectory() as folder:
            def bug(root, output):
                raise TypeError("fixture unexpected error")

            code, receipt, matrix = self.execute_fixture(Path(folder), bug)
            self.assertEqual((code, receipt["state"], receipt["reason_code"]),
                             (1, "fatal", "unexpected"))
            self.assertEqual(receipt["reason"], "fixture unexpected error")
            self.assertFalse(receipt["clean_handoff"])
            self.assertFalse(matrix["accepted_timings"])

    @unittest.skipUnless(sys.platform == "linux", "Linux runner contract")
    def test_worker_failures_and_deadlines_have_distinct_receipts(self):
        programs = {
            "crash": "raise SystemExit(7)",
            "killed": "import os,signal; os.kill(os.getpid(),signal.SIGKILL)",
            "reportless": "pass",
            "malformed": "import pathlib,sys; "
                         "pathlib.Path(sys.argv[1]).write_text('{')",
            "timeout": "import signal; signal.pause()",
        }
        for mode, program in programs.items():
            with self.subTest(mode=mode), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder)

                def prepare(root, output):
                    command = [sys.executable, "-c", program,
                               str(output / "worker.json")]
                    try:
                        return cost.completed_worker(
                            command, output / "worker.json",
                            time.monotonic() + (1 if mode == "timeout" else 5),
                            root, os.environ)
                    finally:
                        self.assertTrue(cost.reap_children())

                code, record, _ = self.execute_fixture(root, prepare)
                expected = ((0, "skipped", "time", True) if mode == "timeout"
                            else (1, "fatal", "unexpected", False))
                self.assertEqual((code, record["state"], record["reason_code"],
                                  record["clean_handoff"]), expected)
                self.assertEqual(record["exit_code"], code)
                self.assertTrue(cost.reap_children())
                for relative, expected in record["files"].items():
                    self.assertEqual(cost.digest(root / relative), expected)

    @unittest.skipUnless(sys.platform == "linux", "Linux runner contract")
    def test_unverified_reaping_preserves_expected_and_fatal_causes(self):
        for error, state, reason in (
                (cost.GateFailure("gate"), "partial", "process"),
                (cost.FatalWorker("crash"), "fatal", "unexpected")):
            with (self.subTest(error=type(error).__name__),
                  tempfile.TemporaryDirectory() as folder):
                def reject(root, output):
                    raise error
                code, record, _ = self.execute_fixture(Path(folder), reject,
                                                       clean=False)
                self.assertEqual(code, 1)
                self.assertEqual(record["state"], state)
                self.assertEqual(record["reason_code"], reason)
                self.assertFalse(record["clean_handoff"])
                if state == "fatal":
                    self.assertEqual(record["reason"], "crash")
                    self.assertEqual(record["teardown"], "unverified")

    def test_repeat_estimate_scales_gate_rows_without_scaling_load(self):
        warm = {"load_seconds": 2, "seconds": 3, "completed_texts": 200}
        measured = {"load_seconds": 2, "seconds": 75,
                    "completed_texts": 5000}
        self.assertEqual(cost.repeat_seconds(warm, 5000), 79)
        self.assertEqual(cost.repeat_seconds(measured, 5000), 79)
        self.assertEqual(warm["seconds"], 3)

    @unittest.skipUnless(sys.platform == "linux", "Linux runner contract")
    def test_native_guard_partial_report_is_not_a_worker_crash(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            tokens = root / "tokens.json"
            tokens.write_text(json.dumps([{
                "indices": [0, 1], "shape": [2, 1], "inputs": {
                    "input_ids": [101, 101], "attention_mask": [1, 1],
                    "token_type_ids": [0, 0]}}]))
            report = {"runtime": "1.31.0", "disable_prepacking": True,
                      "inter_op_threads": 1, "intra_op_threads": 8,
                      "token_file_sha256": cost.digest(tokens),
                      "status": "partial", "reason": "runner guard exceeded",
                      "vectors": [[1.0]*768, None], "planned_texts": 2,
                      "completed_texts": 1, "seconds": 1, "load_seconds": 0}
            script = root / "onnxprobe"
            script.write_text(
                "#!/usr/bin/env python3\nimport json,pathlib,sys\n"
                f"report=json.loads({json.dumps(json.dumps(report))})\n"
                "p=pathlib.Path(sys.argv[sys.argv.index('-output')+1])\n"
                "p.write_text(json.dumps(report))\nraise SystemExit(1)\n")
            script.chmod(0o700)
            args = cost.argparse.Namespace(root=root, library=root / "fake.so")
            case = {"threads": 8, "corpus": "body", "cap": 512}
            result = cost.probe(args, case, tokens, root / "partial.json",
                                1, time.monotonic() + 5, os.environ)
            self.assertEqual(result["status"], "partial")
            self.assertEqual(result["completed_texts"], 1)
            self.assertTrue(cost.reap_children())
            script.write_text(script.read_text().replace(
                "runner guard exceeded", "unknown stop"))
            with self.assertRaises(cost.FatalWorker):
                cost.probe(args, case, tokens, root / "bad.json",
                           1, time.monotonic() + 5, os.environ)

    @unittest.skipUnless(sys.platform == "linux", "Linux runner contract")
    def test_independent_case_jobs_keep_partial_and_completed_coverage(self):
        for outcome in ("completed", "partial", "admission-refused"):
            all_partial = outcome == "partial"
            with self.subTest(next_case=outcome), \
                    tempfile.TemporaryDirectory() as folder:
                root = Path(folder)
                cases = cost.matrix()[:2]
                vector = [1.0, *([0.0]*767)]
                calls = []

                def prepare(root, output):
                    for suffix in (".json", "-gate.json"):
                        (output / ("metadata-8192" + suffix)).write_text("[]")
                    (output / "metadata-8192-reference.json").write_text(
                        json.dumps({"status": "completed", "runtime": "1.31.0",
                                    "intra_op_threads": 16,
                                    "inter_op_threads": 1,
                                    "token_sha256": cost.digest(
                                        output / "metadata-8192-gate.json"),
                                    "vectors": [vector]*200}))
                    return {"metadata-8192": {"ids": list(range(400)),
                                              "symbols": 400}}

                def probe(args, case, tokens, output, repeat, end, env):
                    calls.append((case["threads"], repeat))
                    deadline = json.loads((args.receipt.parent.parent /
                        "process.json").read_text())["absolute_deadline"]
                    self.assertEqual(end, deadline - 30)
                    if outcome == "admission-refused" and case["threads"] == 2:
                        raise cost.Skip("time", "absolute deadline; no spawn")
                    partial = all_partial or (case["threads"] == 1 and repeat)
                    planned = 200 if repeat == 0 else 400
                    completed = 96 if partial else planned
                    return {"status": "partial" if partial else "completed",
                            "reason": ("runner guard exceeded"
                                       if partial else ""),
                            "runtime": "1.31.0",
                            "intra_op_threads": case["threads"],
                            "inter_op_threads": 1,
                            "token_file_sha256": cost.digest(tokens),
                            "planned_texts": planned,
                            "completed_texts": completed,
                            "vectors": [vector]*completed
                            + [None]*(planned-completed),
                            "seconds": 0.01, "load_seconds": 0.01,
                            "minutes_per_10k_symbols": 1}

                results = []
                for selected in (0, 1):
                    job_root = root / f"case-{selected}"
                    job_root.mkdir()
                    result = self.execute_fixture(
                        job_root, prepare, probe, cases, case=selected)
                    self.assertEqual(len(result[2]["cases"]), 1)
                    self.assertEqual(result[2]["cases"][0]["threads"],
                                     cases[selected]["threads"])
                    results.append(result)
                code, record, first = results[0]
                summary = {"cases": [r[2]["cases"][0] for r in results],
                           "accepted_timings": any(
                               r[2]["accepted_timings"] for r in results)}
                self.assertTrue(all(r[1]["clean_handoff"] for r in results))
                self.assertEqual(code, 1)
                self.assertEqual((record["state"], record["reason_code"]),
                                 ("partial", "time"))
                self.assertTrue(record["clean_handoff"])
                self.assertIn((2, 0), calls)
                self.assertEqual(summary["cases"][0]["status"], "partial")
                self.assertEqual(summary["cases"][1]["status"],
                                 "completed" if outcome == "completed"
                                 else "partial")
                self.assertEqual(summary["cases"][0]["actual_repeats"], 0)
                self.assertEqual(summary["accepted_timings"],
                                 outcome == "completed")




class BoundGoTests(unittest.TestCase):
    def test_common_manifest_go_is_absolute_and_hash_bound_without_path(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            go = root / "go"
            go.write_bytes(b"fixture Go; not executed")
            manifest = root / "inputs.json"
            manifest.write_text(json.dumps({"tools": {"go": str(go)},
                "files": {str(go): cost.digest(go)}}))
            with patch.dict(os.environ, {"PATH": "/unusable",
                    "STUDY_MANIFEST": str(manifest),
                    "STUDY_MANIFEST_SHA": cost.digest(manifest)}):
                self.assertEqual(cost.external_tool("go"), str(go))
                go.write_bytes(b"changed")
                with self.assertRaisesRegex(RuntimeError, "unbound"):
                    cost.external_tool("go")
                manifest.write_text("{}")
                with self.assertRaisesRegex(RuntimeError, "manifest"):
                    cost.external_tool("go")


if __name__ == "__main__":
    unittest.main()
