"""Synthetic bridge fixtures; no model, network or inference is used."""
import argparse
import copy
import io
import json
import math
import tempfile
import unittest
from contextlib import redirect_stderr
from pathlib import Path
from unittest.mock import patch

import numpy as np

import accuracy_bridge as bridge


class BridgeTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        documents = [
            {"id": "a0", "parent_id": "a", "repo": "r", "text": "aa",
             "language": "go"},
            {"id": "b0", "parent_id": "b", "repo": "r", "text": "bbb",
             "language": "python"},
            {"id": "a1", "parent_id": "a", "repo": "r", "text": "c",
             "language": "go"}]
        queries = [{"id": "a", "repo": "r", "text": "find a",
                    "language": "go"},
                   {"id": "b", "repo": "r", "text": "find b",
                    "language": "python"}]
        for name, rows in (("documents", documents), ("queries", queries)):
            (self.root / (name + ".jsonl")).write_text(
                "".join(json.dumps(row) + "\n" for row in rows))
        (self.root / "tokenizer").mkdir()
        names = ["documents.jsonl", "queries.jsonl", "model.onnx",
                 "model.onnx.data", "probe", "runtime",
                 "tokenizer/tokenizer.json"]
        for name in names[2:]:
            (self.root / name).write_text("synthetic")
        self.config = {
            "schema": "accuracy-bridge/v1", "source_pin": "1" * 40,
            "sample": "synthetic", "cap": 2048, "host": "fixture",
            "limits": {"memory_bytes": 1024, "seconds": 30, "threads": 1},
            "documents": names[0], "queries": names[1],
            "tokenizer": "tokenizer", "model": names[2],
            "model_data": names[3], "probe": names[4], "runtime": names[5],
            "files": {name: bridge.ev.digest(self.root / name)
                      for name in names}}
        self.manifest = self.root / "manifest.json"
        self.save_manifest()
        self.prepared = self.root / "prepared"
        self.output = self.root / "assembled"
        self.calls = []

    def save_manifest(self):
        self.manifest.write_text(json.dumps(self.config))

    def tokenize(self, texts, **kwargs):
        self.calls.append((list(texts), kwargs))
        ids = [[10 + i] * (i + 1) for i in range(len(texts))]
        return {"input_ids": ids,
                "attention_mask": [[1] * len(row) for row in ids],
                "token_type_ids": [[0] * len(row) for row in ids]}

    def prepare(self):
        args = argparse.Namespace(
            root=self.root, manifest=self.manifest,
            manifest_pin=bridge.ev.digest(self.manifest),
            output=self.prepared)
        with patch("transformers.AutoTokenizer.from_pretrained") as loader:
            loader.return_value = self.tokenize
            bridge.prepare(args)
            loader.assert_called_once_with(
                self.root / "tokenizer", local_files_only=True,
                trust_remote_code=False)

    def fixture_reports(self):
        self.prepare()
        prepared_path = self.prepared / "prepared.json"
        prepared = json.loads(prepared_path.read_text())
        # Token positions, not batch traversal, define the final vector rows.
        body_path = self.prepared / "body-tokens.json"
        batches = json.loads(body_path.read_text())
        original = batches[0]
        width = original["shape"][1]
        original["indices"] = [2, 0, 1]
        original["inputs"] = {
            name: [n for i in (2, 0, 1)
                   for n in values[i * width:(i + 1) * width]]
            for name, values in original["inputs"].items()}
        body_path.write_text(json.dumps(batches))
        prepared["corpora"]["body"]["sha256"] = bridge.ev.digest(body_path)
        prepared_path.write_text(json.dumps(prepared))
        # These are Go's already index-restored outputs, not traversal order.
        body_vectors = np.zeros((3, 768), dtype=np.float32)
        body_vectors[0, 0] = body_vectors[1, 1] = body_vectors[2, 2] = 1
        query_vectors = np.zeros((2, 768), dtype=np.float32)
        query_vectors[0, 2] = query_vectors[1, 1] = 1
        for role, vectors in (("body", body_vectors),
                              ("query", query_vectors)):
            spec = prepared["corpora"][role]
            tokens = json.loads((self.root / spec["path"]).read_text())
            count = len(vectors)
            total = sum(sum(b["inputs"]["attention_mask"]) for b in tokens)
            report = {
                "status": "completed", "reason": "", "corpus": role,
                "token_cap": 2048, "token_file_sha256": spec["sha256"],
                "batch_count": len(tokens), "planned_texts": count,
                "completed_texts": count, "planned_unpadded_tokens": total,
                "completed_unpadded_tokens": total, "model": "coderank",
                "runtime": "1.31.0", "vectors": vectors.tolist(),
                "model_path": str(self.root / "model.onnx"),
                "intra_op_threads": 1, "inter_op_threads": 1,
                "guard_seconds": 30, "disable_prepacking": True,
                "mapped": False, "repeat": 1, "completed_repeats": 1,
                "planned_repeats": 1}
            (self.prepared / (role + "-report.json")).write_text(
                json.dumps(report))
        return body_vectors, query_vectors

    def assemble(self):
        return bridge.main([
            "assemble", "--root", str(self.root), "--prepared",
            str(self.prepared / "prepared.json"), "--prepared-pin",
            bridge.ev.digest(self.prepared / "prepared.json"),
            "--body-report", str(self.prepared / "body-report.json"),
            "--query-report", str(self.prepared / "query-report.json"),
            "--output", str(self.output)])

    def test_local_export_prefix_cap_and_input_preservation(self):
        before = (self.root / "documents.jsonl").read_bytes()
        self.prepare()
        self.assertEqual(self.calls[0][0], ["aa", "bbb", "c"])
        self.assertEqual(self.calls[1][0], [
            bridge.cpu.PREFIX + "find a", bridge.cpu.PREFIX + "find b"])
        for _, options in self.calls:
            self.assertEqual(options, {"padding": False, "truncation": True,
                                       "max_length": 2048})
        self.assertEqual((self.root / "documents.jsonl").read_bytes(), before)
        receipt = json.loads((self.prepared / "prepared.json").read_text())
        self.assertEqual(receipt["corpora"]["body"]["ids"],
                         ["a0", "b0", "a1"])
        self.assertEqual(receipt["corpora"]["body"]["languages"],
                         {"go": 2, "python": 1})
        self.assertEqual(receipt["corpora"]["query"]["languages"],
                         {"go": 1, "python": 1})

    def test_shuffled_batches_to_exact_rows_and_real_score_command(self):
        body, queries = self.fixture_reports()
        self.assertEqual(self.assemble(), 0)
        vectors = np.load(self.output / "vectors.npy", allow_pickle=False)
        np.testing.assert_array_equal(vectors, np.concatenate((body, queries)))
        config = bridge.ev.load_config(self.output / "config.json")
        self.assertIsInstance(config, dict)
        self.assertEqual(config["text_policy"], "body")
        self.assertEqual(config["tie_policy"], "corpus-order")
        self.assertIsNone(config["expected_r10_percent"])
        receipt = json.loads((self.output / "cut.json").read_text())
        command = receipt["score_command"]
        self.assertEqual(bridge.ev.main(command[7:]), 0)
        score = json.loads((self.output / "score.metrics.json").read_text())
        self.assertEqual(score["metrics"]["ranks"], [1, 1])
        self.assertEqual(score["metrics"]["mrr10"], 1)
        for name, pin in receipt["files"].items():
            self.assertEqual(bridge.ev.digest(self.root / name), pin)
        self.assertEqual(receipt["state"], "ready_to_score")
        self.assertNotIn("comparison", receipt)
        before = (self.output / "cut.json").read_bytes()
        with redirect_stderr(io.StringIO()):
            self.assertEqual(self.assemble(), 2)
        self.assertEqual((self.output / "cut.json").read_bytes(), before)

    def test_partial_wrong_cut_missing_dimensions_and_nonfinite_refuse(self):
        self.fixture_reports()
        path = self.prepared / "body-report.json"
        original = json.loads(path.read_text())
        cases = [("status", "partial"), ("token_cap", 8192),
                 ("completed_texts", 2), ("token_file_sha256", "0" * 64),
                 ("vectors", original["vectors"][:2]),
                 ("vectors", [[1, 0]] * 3), ("vectors", [None] * 3),
                 ("vectors", [[math.nan] * 768] * 3),
                 ("vectors", [[math.inf] * 768] * 3),
                 ("vectors", [[0] * 768] * 3),
                 ("model", "nomic"), ("intra_op_threads", 2)]
        for key, value in cases:
            with self.subTest(field=key, value=str(value)[:50]):
                report = {**original, key: value}
                path.write_text(json.dumps(report))
                with redirect_stderr(io.StringIO()):
                    self.assertEqual(self.assemble(), 2)
                self.assertFalse(self.output.exists())
        path.unlink()
        with redirect_stderr(io.StringIO()):
            self.assertEqual(self.assemble(), 2)
        self.assertFalse(self.output.exists())

    def test_duplicate_missing_indices_and_wrong_cap_rejected(self):
        self.fixture_reports()
        path = self.prepared / "body-tokens.json"
        batches = json.loads(path.read_text())
        before = copy.deepcopy(batches)
        for indices in ([2, 0, 0], [2, 0], [2, 0, 3], [2, 0, True]):
            candidate = copy.deepcopy(batches)
            candidate[0]["indices"] = indices
            self.assertIsInstance(bridge.token_count(
                candidate, 3, 2048, "body"), bridge.Invalid)
        self.assertIsInstance(bridge.token_count(
            batches, 3, 2, "body"), bridge.Invalid)
        self.assertEqual(batches, before)
        with path.open("a") as stream:
            stream.write(" ")
        with redirect_stderr(io.StringIO()):
            self.assertEqual(self.assemble(), 2)
        self.assertFalse(self.output.exists())

    def test_bad_ids_source_and_binding_refuse_before_publication(self):
        path = self.root / "documents.jsonl"
        original = path.read_text()
        first = json.loads(original.splitlines()[0])
        for changed in (first | {"id": ""}, first | {"id": "b0"},
                        first | {"text": None}, first | {"language": ""}):
            with self.subTest(changed=changed):
                path.write_text(json.dumps(changed) + "\n" +
                                "\n".join(original.splitlines()[1:]) + "\n")
                with self.assertRaises(bridge.BridgeError):
                    bridge.corpus_rows(path, "body")
        path.write_text(original)
        (self.root / "model.onnx.data").write_text("changed")
        with self.assertRaisesRegex(bridge.BridgeError, "hash mismatch"):
            self.prepare()
        self.assertFalse(self.prepared.exists())

    def test_changed_corpus_and_partial_restart_preserve_evidence(self):
        self.fixture_reports()
        path = self.root / "documents.jsonl"
        original = path.read_text()
        path.write_text("\n".join(reversed(original.splitlines())) + "\n")
        with redirect_stderr(io.StringIO()):
            self.assertEqual(self.assemble(), 2)
        self.assertFalse(self.output.exists())
        path.write_text(original)
        self.output.mkdir()
        marker = self.output / "partial"
        marker.write_text("interrupted attempt")
        with redirect_stderr(io.StringIO()):
            self.assertEqual(self.assemble(), 2)
        self.assertEqual(marker.read_text(), "interrupted attempt")
        self.assertFalse((self.output / "cut.json").exists())

    def test_missing_bindings_bad_pins_and_source_drift_refuse(self):
        for field in ("files", "host", "limits", "source_pin"):
            with self.subTest(field=field):
                config = self.config.copy()
                config.pop(field)
                self.manifest.write_text(json.dumps(config))
                with self.assertRaises(bridge.BridgeError):
                    self.prepare()
                self.assertFalse(self.prepared.exists())
        self.save_manifest()
        with self.assertRaisesRegex(bridge.BridgeError, "manifest hash"):
            bridge.manifest(self.manifest, "0" * 64, self.root)
        self.fixture_reports()
        path = self.prepared / "prepared.json"
        prepared = json.loads(path.read_text())
        prepared["sources"] = {}
        path.write_text(json.dumps(prepared))
        with redirect_stderr(io.StringIO()):
            self.assertEqual(self.assemble(), 2)
        self.assertFalse(self.output.exists())

    def test_duplicate_report_keys_refuse_without_output(self):
        self.fixture_reports()
        path = self.prepared / "body-report.json"
        text = path.read_text()
        path.write_text(text[:-1] + ', "status": "completed"}')
        with redirect_stderr(io.StringIO()):
            self.assertEqual(self.assemble(), 2)
        self.assertFalse(self.output.exists())

    def test_duplicate_json_alias_and_symlink_escape_refuse(self):
        with self.assertRaisesRegex(bridge.BridgeError, "duplicate JSON"):
            bridge.unique_object([("id", "a"), ("id", "b")])
        files = {"model.onnx": self.config["files"]["model.onnx"],
                 "./model.onnx": self.config["files"]["model.onnx"]}
        with self.assertRaisesRegex(bridge.BridgeError, "aliased"):
            bridge.verify_files(files, self.root)
        with tempfile.TemporaryDirectory() as external:
            (self.root / "escape").symlink_to(Path(external),
                                             target_is_directory=True)
            with self.assertRaisesRegex(bridge.BridgeError, "escapes"):
                bridge.owned("escape/model", self.root)


if __name__ == "__main__":
    unittest.main()
