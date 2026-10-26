"""Hand-computed examples only; no model or real corpus is loaded."""
import copy
import json
import math
import subprocess
import sys
import tempfile
import unittest
from dataclasses import asdict
from pathlib import Path

import numpy as np

import evaluate as ev


def doc(name, parent=None, repo="a"):
    return {"id": name, "parent_id": parent or name, "repo": repo}


def query(name, repo="a"):
    return {"id": name, "repo": repo}


class RetrievalTests(unittest.TestCase):
    def score(self, docs, queries, dv, qv, tie="corpus-order"):
        dataset = ev.parse_dataset(docs, queries, dv, qv)
        self.assertIsInstance(dataset, ev.Dataset)
        return ev.retrieval(dataset, tie)

    def test_hand_computed_ranks_and_no_mutation(self):
        docs = [doc("a1", "a"), doc("a2", "a"), doc("b"), doc("c")]
        queries = [query("a"), query("b")]
        dv = np.array([[1, 0], [.8, .6], [0, 2], [-1, 0]])
        qv = np.array([[2, 0], [.6, .8]])
        before = (copy.deepcopy(docs), dv.copy(), qv.copy())
        result = self.score(docs, queries, dv, qv)
        self.assertEqual(result["ranks"], [1, 2])
        self.assertEqual(result["r1"], .5)
        self.assertEqual(result["r10"], 1)
        self.assertEqual(result["mrr10"], .75)
        self.assertAlmostEqual(result["ndcg10"], (1 + 1 / math.log2(3)) / 2)
        self.assertEqual(docs, before[0])
        np.testing.assert_array_equal(dv, before[1])
        np.testing.assert_array_equal(qv, before[2])

    def test_rank_ten_counts_and_rank_eleven_is_zero(self):
        docs = [doc(f"p{i:02}") for i in range(11)]
        result = self.score(docs, [query("p00"), query("p09"), query("p10")],
                            [[1, 0]] * 11, [[1, 0]] * 3)
        self.assertEqual(result["ranks"], [1, 10, 11])
        self.assertEqual(result["queries"], 3)
        self.assertEqual(result["r1"], 1 / 3)
        self.assertEqual(result["r10"], 2 / 3)
        self.assertAlmostEqual(result["mrr10"], 1.1 / 3)
        self.assertAlmostEqual(result["ndcg10"], (1 + 1 / math.log2(11)) / 3)

    def test_chunks_take_maximum_and_count_unique_parents(self):
        docs = [doc(f"a{i}", "a") for i in range(12)] + [doc("b")]
        result = self.score(docs, [query("b")],
                            [[1, 0]] * 12 + [[.8, .6]], [[1, 0]])
        self.assertEqual(result["ranks"], [2])
        self.assertEqual(result["unique_parents"], 2)
        interleaved = [doc("a0", "a"), doc("b"), doc("a1", "a")]
        result = self.score(interleaved, [query("a")],
                            [[-1, 0], [1, 0], [1, 0]], [[1, 0]])
        self.assertEqual(result["ranks"], [1])

    def test_both_ties_and_reordering(self):
        for docs, corpus_rank in [([doc("a"), doc("z")], 1),
                                  ([doc("z"), doc("a")], 2)]:
            with self.subTest(order=docs):
                for tie, rank in [("corpus-order", corpus_rank),
                                  ("descending-parent-ID", 2)]:
                    result = self.score(docs, [query("a")],
                                        [[1, 0], [1, 0]], [[1, 0]], tie)
                    self.assertEqual(result["ranks"], [rank])
                    self.assertEqual(result["tie_policy"], tie)

    def test_same_repository_and_strict_not_epsilon_ties(self):
        result = self.score([doc("a"), doc("z"), doc("foreign", repo="b")],
                            [query("a")], [[.80001, .6], [.8, .6], [1, 0]],
                            [[1, 0]], "descending-parent-ID")
        self.assertEqual(result["ranks"], [1])

    def test_scaled_finite_vectors_still_have_exact_cosine(self):
        result = self.score([doc("a"), doc("b")], [query("a")],
                            [[1e308, 0], [0, 1e-308]], [[1e-308, 0]])
        self.assertEqual(result["ranks"], [1])

    def test_reject_vectors_without_partial_metrics_or_mutation(self):
        cases = [([[0, 0]], "zero"), ([[math.nan, 1]], "nonfinite"),
                 ([[math.inf, 1]], "nonfinite"), ([["x", "y"]], "nonnumeric"),
                 ([[1, 0], [0, 1]], "row-count"), ([], "shape"),
                 ([[1, 0], [1]], "ragged"), ([[True, False]], "nonnumeric")]
        for values, reason in cases:
            with self.subTest(reason=reason, values=values):
                result = ev.parse_dataset([doc("a")], [query("a")],
                                          values, [[1, 0]])
                self.assertIsInstance(result, ev.Invalid)
                self.assertIn(reason, result.reason)
                self.assertFalse(hasattr(result, "metrics"))
        original = np.array([[0., 0.]])
        before = original.copy()
        ev.parse_dataset([doc("a")], [query("a")], original, [[1, 0]])
        np.testing.assert_array_equal(original, before)
        result = ev.parse_dataset([doc("a")], [query("a")], [[1, 0]], [[1]])
        self.assertIsInstance(result, ev.Invalid)
        self.assertIn("dimension", result.reason)
        for qv in ([[0, 0]], [[math.inf, 0]], [[1, 0], [1, 0]]):
            self.assertIsInstance(ev.parse_dataset(
                [doc("a")], [query("a")], [[1, 0]], qv), ev.Invalid)

    def test_reject_missing_labels_duplicates_empty_and_wrong_repo(self):
        cases = [([], [query("a")]), ([doc("a")], []),
                 ([doc("a"), doc("a")], [query("a")]),
                 ([doc("a")], [query("a"), query("a")]),
                 ([doc("a")], [query("missing")]),
                 ([doc("a")], [query("a", "b")]),
                 ([doc("a"), doc("z", "a", "b")], [query("a")]),
                 ([{"id": "a", "repo": "a"}], [query("a")]),
                 ([doc("a")], [{"repo": "a"}]), ([None], [query("a")])]
        for docs, queries in cases:
            with self.subTest(docs=docs, queries=queries):
                result = ev.parse_dataset(docs, queries, [[1, 0]], [[1, 0]])
                self.assertIsInstance(result, ev.Invalid)
                self.assertFalse(hasattr(result, "r10"))

    def test_model_configs_pin_card_settings_without_loading_models(self):
        for name in ("qwen3-embedding-0-6b", "bge-large-en", "gte-large-en"):
            config = asdict(ev.MODELS[name])
            self.assertTrue(ev.is_pin(config["revision"], (40,)))
            self.assertEqual(config["dimensions"], 1024)
            self.assertEqual(config["document_prefix"], "")
        qwen = ev.MODELS["qwen3-embedding-0-6b"]
        self.assertEqual(qwen.pooling, "last-token")
        self.assertEqual(qwen.query_prefix,
                         "Instruct: Given a web search query, "
                         "retrieve relevant passages that answer the query"
                         "\nQuery:")
        self.assertTrue(ev.MODELS["bge-large-en"].query_prefix.endswith(": "))
        self.assertEqual(ev.MODELS["bge-large-en"].max_tokens, 512)
        gte = ev.MODELS["gte-large-en"]
        self.assertTrue(ev.is_pin(gte.code_revision, (40,)))
        result = subprocess.run(
            [sys.executable, ev.__file__, "model-config", "gte-large-en"],
            capture_output=True, text=True, timeout=20, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout), asdict(gte))
        self.assertEqual(ev.MODELS["bge-large-en"].query_prefix,
                         "Represent this sentence for searching "
                         "relevant passages: ")


class FileBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.receipt = self.root / "complete.json"
        rows = {"documents": [doc("a"), doc("b")], "queries": [query("a")]}
        self.config = {"schema": "retrieval-evaluator/v1",
                       "model": "coderank-reference", "sample": "synthetic",
                       "text_policy": "metadata",
                       "tie_policy": "corpus-order",
                       "expected_r10_percent": "100.000",
                       "evaluator_sha256": ev.digest(Path(ev.__file__))}
        for name, values in rows.items():
            path = self.root / (name + ".jsonl")
            content = "\n".join(json.dumps(row) for row in values) + "\n"
            path.write_text(content)
            self.config[name] = {"path": str(path), "sha256": ev.digest(path),
                                 "rows": len(values)}
        vectors = np.zeros((3, 768), dtype=np.float32)
        vectors[0, 0] = vectors[2, 0] = 1
        vectors[1, 1] = 1
        path = self.root / "vectors.npy"
        np.save(path, vectors)
        self.config["vectors"] = {
            "path": str(path), "sha256": ev.digest(path), "rows": 3}

    def invoke(self, *, source="1" * 40, config_pin=None, input_pin=None):
        path = self.root / "config.json"
        path.write_text(json.dumps(self.config, indent=2) + "\n")
        inputs = {name: self.config[name]["sha256"]
                  for name in ("documents", "queries", "vectors")}
        return subprocess.run(
            [sys.executable, str(Path(ev.__file__).resolve()), "score",
             "--config", str(path), "--receipt", str(self.receipt),
             "--root", str(self.root), "--source-pin", source,
             "--config-pin", config_pin or ev.digest(path),
             "--input-pin", input_pin or ev.json_pin(inputs)],
            capture_output=True, text=True, timeout=20, check=False)

    def test_cli_success_and_driver_receipt(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads(self.receipt.read_text())
        self.assertEqual(receipt["state"], "completed")
        self.assertEqual(receipt["exit_code"], 0)
        self.assertTrue(receipt["clean_handoff"])
        self.assertEqual(receipt["source_pin"], "1" * 40)
        config_path = self.root / "config.json"
        self.assertEqual(receipt["config_pin"], ev.digest(config_path))
        inputs = {name: self.config[name]["sha256"]
                  for name in ("documents", "queries", "vectors")}
        self.assertEqual(receipt["input_pin"], ev.json_pin(inputs))
        self.assertEqual(len(receipt["files"]), 1)
        for name, sha in receipt["files"].items():
            self.assertEqual(ev.digest(self.root / name), sha)
            self.assertEqual(json.loads((self.root / name).read_text())[
                "metrics"]["ranks"], [1])
        before = self.receipt.read_bytes()
        self.assertEqual(self.invoke().returncode, 2)
        self.assertEqual(self.receipt.read_bytes(), before)

    def test_calibration_mismatch_preserves_metrics_and_blocks(self):
        self.config["expected_r10_percent"] = "69.369"
        result = self.invoke()
        self.assertEqual(result.returncode, 1, result.stderr)
        receipt = json.loads(self.receipt.read_text())
        self.assertEqual((receipt["state"], receipt["exit_code"]),
                         ("checks_failed", 1))
        metrics = json.loads(
            self.receipt.with_suffix(".metrics.json").read_text())
        self.assertEqual(metrics["observed_percent"], "100.000")
        self.assertEqual(metrics["expected_r10_percent"], "69.369")

    def test_bad_pins_and_missing_label_publish_nothing(self):
        cases = [{"source": "LEAD"}, {"config_pin": "0" * 64},
                 {"input_pin": "0" * 64}]
        for kwargs in cases:
            with self.subTest(kwargs=kwargs):
                result = self.invoke(**kwargs)
                self.assertEqual(result.returncode, 2)
                self.assertIn("rejected", result.stderr)
                self.assertFalse(self.receipt.exists())
                self.assertFalse(
                    self.receipt.with_suffix(".metrics.json").exists())
        path = Path(self.config["queries"]["path"])
        path.write_text(json.dumps(query("missing")) + "\n")
        self.config["queries"]["sha256"] = ev.digest(path)
        result = self.invoke()
        self.assertEqual(result.returncode, 2)
        self.assertIn("missing same-repository label", result.stderr)
        self.assertFalse(self.receipt.exists())
        path.write_text(json.dumps(query("a")) + "\n")
        self.config["queries"]["sha256"] = ev.digest(path)
        path = Path(self.config["vectors"]["path"])
        np.save(path, np.zeros((3, 768), dtype=np.float32))
        self.config["vectors"]["sha256"] = ev.digest(path)
        self.assertEqual(self.invoke().returncode, 2)
        self.assertFalse(self.receipt.with_suffix(".metrics.json").exists())

    def test_input_hash_source_hash_path_escape_and_partial_refuse(self):
        for key in ("documents", "queries", "vectors"):
            before = self.config[key]["sha256"]
            self.config[key]["sha256"] = "0" * 64
            self.assertEqual(self.invoke().returncode, 2)
            self.config[key]["sha256"] = before
        self.config["evaluator_sha256"] = "0" * 64
        self.assertEqual(self.invoke().returncode, 2)
        self.config["evaluator_sha256"] = ev.digest(Path(ev.__file__))
        self.config["documents"]["path"] = str(self.root.parent / "outside")
        self.assertEqual(self.invoke().returncode, 2)
        self.assertFalse(self.receipt.exists())
        with tempfile.TemporaryDirectory() as outside:
            external = Path(outside) / "external.jsonl"
            external.write_bytes((self.root / "documents.jsonl").read_bytes())
            link = self.root / "linked.jsonl"
            link.symlink_to(external)
            self.config["documents"]["path"] = str(link)
            result = self.invoke()
            self.assertEqual(result.returncode, 2)
            self.assertIn("path escapes", result.stderr)
        partial = self.receipt.with_suffix(".metrics.json")
        partial.write_text("partial")
        self.assertEqual(self.invoke().returncode, 2)
        self.assertEqual(partial.read_text(), "partial")


if __name__ == "__main__":
    unittest.main()
