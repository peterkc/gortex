"""Given/When/Then cases for the CI-only callgraph filter."""
import hashlib
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import filter_callgraph as cg


SYMBOLS = [{"symbol": "p.Seam", "why": "fixture seam"}]


def edge(caller, callee, file="caller.go", package="p", synthetic=""):
    caller_file = "seam.go" if caller == "p.Seam" else file
    callee_file = "seam.go" if callee == "p.Seam" else "caller.go"
    return "\t".join((caller, package, "/src/" + caller_file, synthetic,
                      callee, "p", "/src/" + callee_file, "", "/src/" + file,
                      "12", "static")) + "\n"


def report(text):
    parsed = cg.parse_edges(text.splitlines(keepends=True), "/src")
    if isinstance(parsed, cg.Invalid):
        raise AssertionError(parsed.reason)
    return cg.analyze(parsed, SYMBOLS)


class FilterTests(unittest.TestCase):
    def test_direct_depth3_and_no_unrelated_callers(self):
        # Given a four-hop chain, then only the first three hops count.
        text = (edge("p.A", "p.Seam") + edge("p.B", "p.A")
                + edge("p.C", "p.B") + edge("p.D", "p.C")
                + edge("p.Other", "p.Else"))
        value = report(text)
        counts = value["symbols"]["p.Seam"]
        self.assertEqual(counts["direct"]["count"], 1)
        self.assertEqual([n["name"] for n in counts["depth3"]["nodes"]],
                         ["p.A", "p.B", "p.C"])
        self.assertEqual(value["edge_counts"]["selected"], 1)
        self.assertEqual(len(value["depth3_union"]["files"]), 1)
        self.assertEqual(value["depth3_union"]["packages"], ["p"])

    def test_cycles_duplicates_and_order_do_not_inflate(self):
        rows = [edge("p.A", "p.Seam"), edge("p.Seam", "p.A"),
                edge("p.B", "p.A"), edge("p.A", "p.Seam")]
        value = report("".join(rows))
        self.assertEqual(value, report("".join(reversed(rows))))
        self.assertEqual(value["edge_counts"],
                         {"raw": 4, "unique": 3, "duplicates": 1,
                          "selected": 1})
        self.assertEqual(value["depth3_union"]["count"], 2)

    def test_test_files_packages_and_unknown_nodes(self):
        text = (edge("p.Prod", "p.Seam")
                + edge("p.Test", "p.Seam", "caller_test.go")
                + edge("p_test.External", "p.Seam", package="p_test")
                + edge("p.test.main", "p.Seam", package="p.test")
                + edge("bridge", "p.Seam", package="", synthetic="thunk"))
        value = report(text)
        split = value["direct_union"]["split"]
        self.assertEqual({k: len(v) for k, v in split.items()},
                         {"production": 1, "test": 3, "unresolved": 1})
        self.assertEqual(value["unresolved_nodes"][0]["name"], "bridge")
        self.assertEqual(value["synthetic_nodes"][0]["synthetic"], "thunk")

    def test_zero_requires_symbol_present_as_a_caller(self):
        self.assertEqual(report(edge("p.Seam", "p.Other"))
                         ["symbols"]["p.Seam"]["direct"]["count"], 0)
        missing = report(edge("p.Other", "p.Else"))
        self.assertIsInstance(missing, cg.Invalid)
        self.assertIn("absent from edge graph (not zero)", missing.reason)
        self.assertIsInstance(report(""), cg.Invalid)

    def test_source_prefix_is_not_an_alias_for_an_external_directory(self):
        self.assertEqual(cg.filename("/src/nested/a.go", "/src"),
                         "nested/a.go")
        self.assertEqual(cg.filename("/src-other/a.go", "/src"),
                         "/src-other/a.go")

    def test_malformed_lines_are_rejected_not_ignored(self):
        for text in ("\n", "bad\tline\n", edge("p.A", "p.Seam")
                     .replace("\t12\t", "\t-1\t"),
                     edge("p.A", "p.Seam").replace("static", "unknown")):
            with self.subTest(text=text):
                self.assertIsInstance(cg.parse_edges([text], "/src"),
                                      cg.Invalid)

    def test_cli_receipt_hashes_summary_and_replay_refusal(self):
        # Given fresh files, output is bound to raw bytes and exact pins.
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            raw = root / "edges.tsv"
            raw.write_text(edge("p.A", "p.Seam"))
            manifest = root / "symbols.json"
            manifest.write_text(json.dumps({
                "schema": "seam-symbols/v1", "commit": cg.COMMIT,
                "tree": cg.TREE, "symbols": SYMBOLS,
                "non_callable_limits": [], "assertion_sites": [],
            }))
            output = root / "out"
            command = [sys.executable, "-B", cg.__file__, "--edges", str(raw),
                       "--symbols", str(manifest), "--source", "/src",
                       "--output", str(output), "--go-version", "go1.27.3",
                       "--commit", cg.COMMIT, "--tree", cg.TREE]
            completed = subprocess.run(command, capture_output=True)
            self.assertEqual(completed.returncode, 0, completed.stderr)
            value = json.loads((output / "seam-callers.json").read_text())
            receipt = json.loads((output / "receipt.json").read_text())
            self.assertEqual(receipt["state"], "completed")
            self.assertEqual(value["identity"]["algorithm"], "cha")
            self.assertEqual(value["identity"]["raw_sha256"],
                             hashlib.sha256(raw.read_bytes()).hexdigest())
            for name, expected in receipt["outputs"].items():
                self.assertEqual(hashlib.sha256(
                    (output / name).read_bytes()).hexdigest(), expected)
            self.assertIn("`p.Seam` | 1 | 1 |",
                          (output / "summary.md").read_text())
            self.assertNotEqual(subprocess.run(
                command, capture_output=True).returncode, 0)
            command[command.index("go1.27.3")] = "go1.27.2"
            command[command.index(str(output))] = str(root / "rejected")
            self.assertNotEqual(subprocess.run(
                command, capture_output=True).returncode, 0)
            self.assertFalse((root / "rejected").exists())
            command[command.index("go1.27.2")] = "go1.27.3"
            raw.write_text(edge("p.Other", "p.Else"))
            self.assertNotEqual(subprocess.run(
                command, capture_output=True).returncode, 0)
            self.assertFalse((root / "rejected").exists())


if __name__ == "__main__":
    unittest.main()
