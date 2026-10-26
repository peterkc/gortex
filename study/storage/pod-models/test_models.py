"""Tiny NumPy-only contracts; no torch, models, GPU or corpus needed."""
import argparse
import json
import sys
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path

import numpy as np

import run_models as run


class ModelTests(unittest.TestCase):
    def test_import_does_not_load_the_model_stack(self):
        self.assertNotIn("torch", sys.modules)
        self.assertNotIn("transformers", sys.modules)

    def test_prefixes_and_documents_are_not_mutated(self):
        rows = [{"id": "../opaque", "text": "needle"}]
        expected = {
            "bge-large-en":
                "Represent this sentence for searching relevant passages: ",
            "qwen3-embedding-0-6b":
                "Instruct: Given a web search query, retrieve relevant "
                "passages that answer the query\nQuery:",
            "gte-large-en": "",
        }
        for name, prefix in expected.items():
            recipe = run.ev.MODELS[name]
            self.assertEqual(run.texts(rows, recipe.query_prefix),
                             [prefix + "needle"])
            self.assertEqual(run.texts(rows, recipe.document_prefix),
                             ["needle"])
        self.assertEqual(rows, [{"id": "../opaque", "text": "needle"}])

    def test_missing_duplicate_and_empty_rows_refuse(self):
        for rows in ([], [{"id": "a", "text": " "}],
                     [{"text": "x"}], [{"id": "a", "text": "x"}] * 2):
            with self.subTest(rows=rows), self.assertRaises(run.ev.RunError):
                run.texts(rows, "")

    def test_only_pinned_gte_pair_trusts_remote_code(self):
        for name in run.D3:
            options = run.model_options(name, run.ev.MODELS[name])
            self.assertEqual(options["trust_remote_code"],
                             name == "gte-large-en")
            if name == "gte-large-en":
                self.assertEqual(options["code_revision"],
                                 "40ced75c3017eb27626c9d4ea981bde21a2662f4")
        with self.assertRaises(run.ev.RunError):
            run.model_options("gte-large-en", replace(
                run.ev.MODELS["gte-large-en"], code_revision="0" * 40))
        with self.assertRaises(run.ev.RunError):
            run.model_options("coderank-reference",
                              run.ev.MODELS["coderank-reference"])

    def test_pooling_with_left_and_right_padding(self):
        hidden = np.array([[[1, 0], [2, 0], [9, 0]],
                           [[8, 0], [3, 0], [4, 0]]], dtype=np.float32)
        mask = np.array([[1, 1, 0], [0, 1, 1]])
        indices = run.pooling_indices(mask, "last-token")
        np.testing.assert_array_equal(indices, [1, 2])
        np.testing.assert_array_equal(hidden[np.arange(2), indices],
                                      [[2, 0], [4, 0]])
        np.testing.assert_array_equal(run.pooling_indices(mask, "cls"), [0, 0])
        np.testing.assert_array_equal(mask, [[1, 1, 0], [0, 1, 1]])
        for bad in (np.array([[0, 0]]), np.array([[1, 2]]),
                    np.array([1]), np.empty((0, 0))):
            with self.assertRaises(run.ev.RunError):
                run.pooling_indices(bad, "last-token")
        with self.assertRaises(run.ev.RunError):
            run.pooling_indices(mask, "mean")

    def test_caps_batch_budget_and_original_order(self):
        lengths = [9, 2, 4, 2]
        groups = run.batches(lengths, budget=8, max_rows=2)
        self.assertEqual(groups, [[1, 3], [2], [0]])
        values = [np.array([[20, 1], [40, 1]]), np.array([[30, 1]]),
                  np.array([[10, 1]])]
        np.testing.assert_array_equal(run.restore_order(groups, values),
                                      [[10, 1], [20, 1], [30, 1], [40, 1]])
        self.assertEqual(lengths, [9, 2, 4, 2])
        self.assertEqual(run.token_stats([2, 8, 9], 8), {
            "rows": 3, "raw_tokens": 19, "effective_tokens": 18,
            "truncated_rows": 1, "max_raw_tokens": 9, "cap": 8})
        for invalid in ([], [0], [-1]):
            with self.assertRaises(run.ev.RunError):
                run.batches(invalid)
        with self.assertRaises(run.ev.RunError):
            run.restore_order([[0, 0]], [np.ones((2, 2))])

    def test_normalization_rejects_bad_model_vectors(self):
        source = np.array([[3.0, 4.0]], dtype=np.float32)
        unit = run.ev.unit_vectors(source, 1, "batch")
        np.testing.assert_allclose(unit, [[0.6, 0.8]])
        np.testing.assert_array_equal(source, [[3, 4]])
        for bad in ([[0, 0]], [[float("nan"), 1]], [[float("inf"), 1]]):
            self.assertIsInstance(run.ev.unit_vectors(bad, 1, "batch"),
                                  run.ev.Invalid)

    def test_calibration_requires_pins_metric_hash_and_target(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            receipt = root / "complete.json"
            metric = root / "complete.metrics.json"
            config = {"calibration": {"receipt": str(receipt),
                      "source_pin": "a" * 40, "config_pin": "b" * 64,
                      "input_pin": "c" * 64}}
            self.assertIn("missing", run.calibration(config, root))
            metric.write_text(json.dumps({"observed_percent": "69.369",
                              "expected_r10_percent": "69.369",
                              "metrics": {"queries": 333}}))
            complete = {**config["calibration"], "state": "completed",
                        "clean_handoff": True, "exit_code": 0,
                        "files": {metric.name: run.ev.digest(metric)}}
            receipt.write_text(json.dumps(complete))
            self.assertEqual(run.calibration(config, root), "")
            receipt.write_text(json.dumps({
                **complete, "state": "checks_failed"}))
            self.assertIn("failed", run.calibration(config, root))
            receipt.write_text(json.dumps(complete))
            metric.write_text("{}")
            self.assertIn("hash mismatch", run.calibration(config, root))
            metric.write_text(json.dumps({"observed_percent": "69.368",
                              "expected_r10_percent": "69.369",
                              "metrics": {"queries": 333}}))
            complete["files"] = {metric.name: run.ev.digest(metric)}
            receipt.write_text(json.dumps(complete))
            self.assertIn("target", run.calibration(config, root))
            receipt.write_text("not-json")
            self.assertIn("unreadable", run.calibration(config, root))

    def test_score_schema_and_denominators(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            vector = root / "v.npy"
            np.save(vector, np.ones((3, 1024), dtype=np.float32))
            specs = {"documents": {"path": "d.jsonl", "rows": 2,
                                  "sha256": "a" * 64},
                     "queries": {"path": "q.jsonl", "rows": 1,
                                 "sha256": "b" * 64}}
            config = run.score_config("bge-large-en", "tiny", "metadata",
                                      specs, vector, 3, root)
            path = root / "score.json"
            run.write_json(path, config)
            self.assertEqual(run.ev.load_config(path), config)
            self.assertEqual(config["vectors"]["rows"], 3)
            self.assertEqual(config["tie_policy"], "corpus-order")
            before = path.read_bytes()
            with self.assertRaises(FileExistsError):
                run.write_json(path, {"overwrite": True})
            self.assertEqual(path.read_bytes(), before)

    def test_partial_and_completed_results_refuse_replay(self):
        with tempfile.TemporaryDirectory() as directory:
            for filename in ("mixed-metadata.partial.npy", "complete.json"):
                output = Path(directory) / filename.replace(".", "-")
                run.begin_output(output)
                evidence = output / filename
                evidence.write_bytes(b"original result")
                with self.assertRaisesRegex(run.ev.RunError, "preserve"):
                    run.begin_output(output)
                self.assertEqual(evidence.read_bytes(), b"original result")
                self.assertEqual(list(output.iterdir()), [evidence])

    def test_input_counts_hashes_and_labels_refuse_before_gpu(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            doc = {"id": "chunk", "parent_id": "parent", "repo": "r",
                   "text": "document"}
            query = {"id": "parent", "repo": "r", "text": "query"}
            specs = {}
            for name in ("corp_.jsonl", "docs_.jsonl", "body_.jsonl",
                         "qs_eq.jsonl", "qrows_.jsonl"):
                path = root / name
                row = query if name.startswith(("qs_", "qrows_")) else doc
                path.write_text(json.dumps(row) + "\n")
                specs[name] = {"path": name, "rows": 1,
                               "sha256": run.ev.digest(path)}
            config = {"inputs": specs}
            loaded = run.load_inputs(config, root)
            self.assertEqual(loaded["docs_.jsonl"], [doc])
            specs["docs_.jsonl"]["rows"] = 2
            with self.assertRaisesRegex(run.ev.RunError, "row-count"):
                run.load_inputs(config, root)
            specs["docs_.jsonl"]["rows"] = 1
            path = root / "qrows_.jsonl"
            path.write_text(json.dumps({**query, "id": "missing"}) + "\n")
            with self.assertRaisesRegex(run.ev.RunError, "hash mismatch"):
                run.load_inputs(config, root)
            specs["qrows_.jsonl"]["sha256"] = run.ev.digest(path)
            with self.assertRaisesRegex(run.ev.RunError, "labels"):
                run.load_inputs(config, root)

    def test_escape_and_symlink_paths_refuse(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with self.assertRaises(run.ev.RunError):
                run.ev.owned_path("../outside", root)
            (root / "link").symlink_to(root.parent, target_is_directory=True)
            with self.assertRaises(run.ev.RunError):
                run.ev.owned_path("link/outside", root)

    def test_mac_or_non_work_execution_refuses_before_import(self):
        args = argparse.Namespace(root=Path("/not-work"))
        with self.assertRaisesRegex(run.ev.RunError, "pod-only"):
            run.run(args)


if __name__ == "__main__":
    unittest.main()
