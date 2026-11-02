"""Tiny extraction/selection/freeze fixtures; no model or inference."""
import argparse
import copy
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import numpy as np
from transformers import AutoTokenizer, BertTokenizerFast

import accuracy_bridge as bridge
import build_accuracy_corpus as corpus


class CorpusTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.tokenizer_dir = self.root / "tokenizer"
        self.tokenizer_dir.mkdir()
        vocab = self.tokenizer_dir / "vocab.txt"
        vocab.write_text("[PAD]\n[UNK]\n[CLS]\n[SEP]\n[MASK]\na\ncode\n")
        tiny = BertTokenizerFast(vocab_file=str(vocab), do_lower_case=False)
        tiny.save_pretrained(self.tokenizer_dir)
        (self.tokenizer_dir / "config.json").write_text(
            '{"model_type":"bert"}')
        self.tokenizer = AutoTokenizer.from_pretrained(
            self.tokenizer_dir, local_files_only=True, trust_remote_code=False)

    def row(self, name, tokens=20, language="go", query=None):
        return {"id": name, "parent_id": name, "repo": "repo-" + language,
                "language": language, "tokens": tokens, "query_tokens": 20,
                "text": "code " + name, "query": query or (
                    "Return the labelled fixture implementation for " + name),
                "path": name + ".go", "notice": "fixture license notice",
                "documentation": "original docs", "start": 0, "end": 10,
                "body_sha256": "a" * 64, "commit": "b" * 40}

    def test_python_unicode_nested_methods_spans_and_blanking(self):
        source = ("# license\nclass Café:\n"
                  "    def lire(self, value: str = '# ordinary'):\n"
                  "        \"\"\"Read the original documented value "
                  "without changes.\n\n"
                  "        Second paragraph.\"\"\"\n"
                  "        # erase this comment\n"
                  "        def nested():\n"
                  "            \"\"\"Erase nested documentation too.\"\"\"\n"
                  "            return 'ordinary string'\n"
                  "        return value + nested()\n"
                  "def top():\n"
                  "    \"\"\"Produce another documented implementation "
                  "for callers.\"\"\"\n"
                  "    return 1\n")
        rows = corpus.python_extract("Lib/fixture.py", source.encode())
        self.assertEqual([r["name"] for r in rows], ["Café.lire", "top"])
        method = rows[0]
        self.assertEqual((method["line"], method["end_line"]), (3, 11))
        raw = source.encode()
        span = raw[method["start"]:method["end"]]
        self.assertTrue(span.startswith(b"def lire"))
        self.assertTrue(span.endswith(b"nested()"))
        self.assertNotIn("erase this comment", method["body"])
        self.assertNotIn("Erase nested documentation", method["body"])
        self.assertNotIn("Read the original", method["body"])
        self.assertIn("'# ordinary'", method["body"])
        self.assertIn("'ordinary string'", method["body"])
        self.assertEqual(corpus.paragraph(method["documentation"]),
                         "Read the original documented value without changes.")
        self.assertEqual(method["notice"], "# license\n")
        self.assertIn("# erase this comment", method["source_comments"])
        self.assertIn("Erase nested documentation", method["source_comments"])
        self.assertEqual(method["signature"],
                         "def lire(self, value: str = '# ordinary'):")
        with self.assertRaises(SyntaxError):
            corpus.python_extract("Lib/bad.py", b"def bad(:\n")

    def test_real_go_helper_boundary_and_metadata_header(self):
        executable = Path(os.environ["GXSTUDY_CORPUS_EXTRACTOR"])
        raw = (b"package f\n"
               b"// Read preserves the complete original documented value.\n"
               b"func Read() string { /* blank */ return \"ordinary\" }\n")
        source = {"repo": "https://github.com/golang/go", "commit": "a" * 40,
                  "tree": "b" * 40, "language": "go"}
        rows = corpus.candidates({"src/f.go": raw}, source, executable)
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["query"],
                         "Read preserves the complete original "
                         "documented value.")
        self.assertTrue(rows[0]["text"].startswith(
            "function Read\nfunc Read() string\nsrc/f.go\n\nfunc Read()"))
        self.assertIn('"ordinary"', rows[0]["text"])
        self.assertNotIn("/* blank */", rows[0]["text"])
        measured = corpus.measure(rows, self.tokenizer)
        self.assertGreater(measured[0]["tokens"], len(self.tokenizer(
            rows[0]["body"], add_special_tokens=True)["input_ids"]))
        with self.assertRaisesRegex(corpus.CorpusError, "Go parse failed"):
            corpus.go_extract(executable, "src/bad.go", b"package f; func (")

    def test_exact_special_token_boundaries_and_fixed_quotas(self):
        lengths = [2048, 2049, 4096, 4097, 8192, 8193]
        rows = [{**self.row(str(n)), "text": "a " * (n - 2)} for n in lengths]
        measured = corpus.measure(rows, self.tokenizer)
        self.assertEqual([r["tokens"] for r in measured], lengths)
        self.assertEqual([corpus.bucket(r["tokens"]) for r in measured],
                         [2048, 4096, 4096, 8192, 8192, None])
        ds, _, counts = corpus.select(
            measured, ((2048, 1, 0), (4096, 2, 0), (8192, 2, 0)), ("go",))
        self.assertEqual(len(ds), 5)
        self.assertNotIn("8193", {r["id"] for r in ds})
        self.assertEqual(counts["population"]["go"], {
            "total": 6, "above_2048": 5, "above_4096": 3, "above_8192": 1})
        expected = {"go": ([450, 40, 10], [25, 20, 5]),
                    "python": ([450, 8, 0], [25, 4, 0])}
        population = []
        for language, (document_counts, _) in expected.items():
            for upper, docs in zip((2048, 4096, 8192), document_counts):
                population.extend(self.row(f"{language}-{upper}-{i}", upper,
                                           language) for i in range(docs + 2))
        documents, queries, counts = corpus.select(population)
        self.assertEqual((len(documents), len(queries)), (958, 79))
        self.assertEqual(counts["shortfalls"], [])
        for language, (document_counts, query_counts) in expected.items():
            ds = [r for r in documents if r["language"] == language]
            qs = [r for r in queries if r["language"] == language]
            self.assertEqual([sum(r["tokens"] == n for r in ds)
                              for n in (2048, 4096, 8192)], document_counts)
            self.assertEqual([sum(r["tokens"] == n for r in qs)
                              for n in (2048, 4096, 8192)], query_counts)
            self.assertEqual(len({r["id"] for r in ds}), sum(document_counts))
            self.assertTrue({r["id"] for r in qs} <= {r["id"] for r in ds})
        self.assertEqual(counts["population"]["python"]["above_4096"], 2)
        self.assertEqual(counts["availability"]["python"]["8192"]["documents"], 2)
        before = copy.deepcopy(population)
        corpus.select(population)
        self.assertEqual(population, before)

    def test_python_mid_bucket_nine_requested_with_eight_available_fails(self):
        rows = [{**self.row(f"python-mid-{i}", 3000, "python"),
                 "query_tokens": 20 if i < 4 else None} for i in range(8)]
        ds, qs, counts = corpus.select(
            rows, ((2048, 0, 0), (4096, 9, 4), (8192, 0, 0)), ("python",))
        self.assertEqual(counts["shortfalls"], ["python:4096"])
        self.assertEqual(counts["availability"]["python"]["4096"], {
            "documents": 8, "queries": 4,
            "required_documents": 9, "required_queries": 4})
        self.assertEqual((len(ds), len(qs)), (8, 4))
        output = self.root / "nine-requested"
        output.mkdir()
        corpus.write_json(output / "counts.json", counts)
        with self.assertRaisesRegex(corpus.CorpusError, "python:4096"):
            corpus.emit(output, ds, qs, counts, {"sources": []}, {})
        self.assertEqual([p.name for p in output.iterdir()], ["counts.json"])

    def test_python_non_lf_separators_and_utf8_notice_keep_physical_spans(self):
        separators = "\v\f\x85\u2028\u2029"
        prefix = "# licence Café " + separators + "\n"
        ordinary = "ordinary" + separators + "text"
        source = (prefix + "def read(value='" + ordinary + "'):\n"
                  '    "Read the original public value without changes."\n'
                  "    # erase " + separators + "\n"
                  "    return value\n")
        raw = source.encode()
        row, = corpus.python_extract("Lib/physical.py", raw)
        self.assertEqual((row["line"], row["end_line"]), (2, 5))
        self.assertEqual(row["notice"], prefix)
        self.assertEqual(row["start"], len(prefix.encode()))
        self.assertEqual(raw[row["start"]:row["end"]],
                         source[len(prefix):].rstrip("\n").encode())
        self.assertIn(ordinary, row["body"])
        self.assertIn(ordinary, row["signature"])
        self.assertNotIn("# erase", row["body"])
        self.assertNotIn("Read the original", row["body"])
        self.assertEqual(len(row["body"].encode()), row["end"] - row["start"])
        self.assertEqual(corpus.offsets(raw),
                         [0] + [i + 1 for i, byte in enumerate(raw) if byte == 10])
        self.assertEqual(corpus.python_extract("Lib/physical.py", raw[:-1]), [row])

    def test_jsonl_unicode_separators_round_trip_through_bridge_lines(self):
        self.assertEqual(corpus.canonical({"text": "Café"}),
                         '{"text":"Café"}'.encode())
        text = "Café\x85first\u2028second\u2029last"
        row = {**self.row("unicode-lines", query=text), "text": text}
        counts = {"shortfalls": []}
        corpus.write_json(self.root / "counts.json", counts)
        corpus.emit(self.root, [row], [row], counts,
                    {"sources": []}, {"go": b"complete fixture license"})
        for name in ("documents.jsonl", "queries.jsonl", "provenance.jsonl"):
            content = (self.root / name).read_text()
            self.assertEqual(len(content.splitlines()), 1)
            parsed = json.loads(content.splitlines()[0])
            if name != "provenance.jsonl":
                self.assertEqual(parsed["text"], text)
                for escaped in (r"\u0085", r"\u2028", r"\u2029"):
                    self.assertIn(escaped, content)
            self.assertIn("Café" if name != "provenance.jsonl" else "unicode-lines",
                          content)

    def test_go_directive_only_documentation_cannot_supply_query(self):
        executable = Path(os.environ["GXSTUDY_CORPUS_EXTRACTOR"])
        raw = (b"package f\n"
               b"//go:linkname OnlyDirectives local.symbol.with.long.name\n"
               b"//go:nosplit\n//export OnlyDirectives\n"
               b"//line fake.go:900\nfunc OnlyDirectives() {}\n")
        source = {"repo": "https://github.com/golang/go", "commit": "a" * 40,
                  "tree": "b" * 40, "language": "go"}
        row, = corpus.candidates({"src/directives.go": raw}, source, executable)
        self.assertEqual(row["documentation"], "")
        self.assertEqual(row["query"], "")
        _, queries, counts = corpus.select(
            corpus.measure([row], self.tokenizer), ((2048, 1, 1),), ("go",))
        self.assertEqual(queries, [])
        self.assertEqual(counts["shortfalls"], ["go:2048"])
        self.assertIn("//export OnlyDirectives", row["source_comments"])

    def test_query_character_and_prefixed_token_limits(self):
        rows = [self.row("short", query="s" * 29),
                self.row("minimum", query="m" * 30),
                self.row("maximum", query="x" * 400),
                self.row("long", query="l" * 401),
                {**self.row("tokens"), "query_tokens": 513}]
        rows[1]["query_tokens"] = 512
        _, queries, counts = corpus.select(
            rows, ((2048, 5, 2),), ("go",))
        self.assertEqual({r["id"] for r in queries}, {"minimum", "maximum"})
        self.assertEqual(counts["rejected"]["query_length"], 3)

    def test_shuffled_enumeration_freezes_identical_bytes(self):
        rows = [self.row(str(i), language=language)
                for language in ("go", "python") for i in range(4)]
        # IDs must be global, not just unique within each repository.
        rows = [{**r, "id": r["language"] + r["id"],
                 "parent_id": r["language"] + r["id"],
                 "query": r["query"] + r["language"]} for r in rows]
        snapshots = []
        for i, sequence in enumerate((rows, list(reversed(rows)))):
            out = self.root / str(i)
            out.mkdir()
            ds, qs, counts = corpus.select(sequence, ((2048, 3, 2),))
            corpus.write_json(out / "counts.json", counts)
            corpus.emit(out, ds, qs, counts, {"sources": []},
                        {"go": b"complete Go license",
                         "python": b"complete PSF license"})
            snapshots.append({p.name: p.read_bytes() for p in out.iterdir()})
        self.assertEqual(snapshots[0], snapshots[1])
        frozen = json.loads(snapshots[0]["freeze.json"])
        self.assertEqual(frozen["freeze_id"],
                         corpus.digest(snapshots[0]["freeze-canonical.json"]))
        for name, pin in frozen["files"].items():
            self.assertEqual(corpus.digest(snapshots[0][name]), pin)
        with self.assertRaises(FileExistsError):
            corpus.emit(out, ds, qs, counts, {"sources": []}, {})
        self.assertEqual(snapshots[1],
                         {p.name: p.read_bytes() for p in out.iterdir()})

    def git(self, path, *args):
        result = subprocess.run(["git", "-C", str(path), *args],
                                capture_output=True, check=True)
        return result.stdout.decode().strip()

    def test_git_revision_remove_readd_and_worktree_edits(self):
        repository = self.root / "source"
        repository.mkdir()
        self.git(repository, "init", "-q")
        self.git(repository, "config", "user.name", "Fixture")
        self.git(repository, "config", "user.email", "fixture@example.invalid")
        (repository / "Lib").mkdir()
        path = repository / "Lib/a.py"
        text = ('def read():\n'
                '    "Read a documented public value without changes."\n'
                '    return 1\n')
        path.write_text(text)
        (repository / "LICENSE").write_text("complete fixture license")
        identities, versions = [], []
        for operation in ("add", "remove", "readd"):
            if operation == "remove":
                path.unlink()
            elif operation == "readd":
                path.write_text(text)
            self.git(repository, "add", "-A")
            self.git(repository, "commit", "-qm", operation)
            source = {"repo": "public-fixture", "language": "python",
                      "commit": self.git(repository, "rev-parse", "HEAD"),
                      "tree": self.git(repository, "rev-parse", "HEAD^{tree}")}
            files, license_bytes = corpus.source_files(repository, source)
            self.assertEqual(license_bytes, b"complete fixture license")
            declarations = corpus.candidates(files, source, Path("unused"))
            versions.append(declarations)
            if declarations:
                identities.append(declarations[0]["id"])
        self.assertEqual(len(versions[1]), 0)
        self.assertNotEqual(identities[0], identities[1])
        freezes = []
        for i in (0, 2):
            output = self.root / ("revision-" + str(i))
            output.mkdir()
            counts = {"shortfalls": []}
            corpus.write_json(output / "counts.json", counts)
            freezes.append(corpus.emit(
                output, versions[i], versions[i], counts,
                {"sources": [{"commit": versions[i][0]["commit"]}]},
                {"python": b"complete fixture license"}))
        self.assertNotEqual(freezes[0], freezes[1])
        path.write_text("malformed local modification")
        frozen, _ = corpus.source_files(repository, source)
        self.assertEqual(frozen["Lib/a.py"], text.encode())
        source["tree"] = "0" * 40
        with self.assertRaisesRegex(corpus.CorpusError, "commit/tree"):
            corpus.source_files(repository, source)
        path.unlink()
        path.symlink_to("outside.py")
        self.git(repository, "add", "-A")
        self.git(repository, "commit", "-qm", "symlink")
        source["commit"] = self.git(repository, "rev-parse", "HEAD")
        source["tree"] = self.git(repository, "rev-parse", "HEAD^{tree}")
        with self.assertRaisesRegex(corpus.CorpusError, "linked/submodule"):
            corpus.source_files(repository, source)

    def test_duplicates_aliases_ownership_and_missing_coverage_reject(self):
        row = self.row("a")
        for bad in ([row, row], [row, {**self.row("b"), "parent_id": "a",
                                     "repo": "foreign"}]):
            with self.assertRaises(corpus.CorpusError):
                corpus.select(bad)
        _, _, counts = corpus.select([row, self.row("b", query=row["query"])])
        self.assertEqual(counts["rejected"]["duplicate_query_text"], 1)
        self.assertEqual(counts["availability"]["go"]["2048"]["queries"], 1)
        _, chosen, _ = corpus.select(
            [self.row("b", query=row["query"]), row], ((2048, 1, 1),), ("go",))
        self.assertEqual([r["id"] for r in chosen], ["a"])
        self.assertIn("python:4096", counts["shortfalls"])
        for name in ("../src/a.go", "./src/a.go", "/src/a.go", "src//a.go"):
            with self.assertRaises(corpus.CorpusError):
                corpus.safe_path(name)
        with self.assertRaises(corpus.CorpusError):
            corpus.unique_object([("a", 1), ("a", 2)])
        self.assertFalse(corpus.eligible_path("src/vendor/a.go", "go"))
        self.assertFalse(corpus.eligible_path("src/testdata/a.go", "go"))
        self.assertFalse(corpus.eligible_path("src/io/read_test.go", "go"))
        self.assertTrue(corpus.eligible_path("src/io/read.go", "go"))
        self.assertFalse(corpus.eligible_path("Lib/test/a.py", "python"))
        output = self.root / "shortfall"
        output.mkdir()
        corpus.write_json(output / "counts.json", counts)
        with self.assertRaisesRegex(corpus.CorpusError, "quota shortfall"):
            corpus.emit(output, [], [], counts, {"sources": []}, {})
        self.assertEqual([p.name for p in output.iterdir()], ["counts.json"])
        pins = {"data/cpu-cost/tokenizer/" + p.name: corpus.file_hash(p)
                for p in self.tokenizer_dir.iterdir()}
        files = corpus.tokenizer_files(self.tokenizer_dir, pins)
        self.assertEqual(len(files), 5)
        (self.tokenizer_dir / "config.json").unlink()
        (self.tokenizer_dir / "config.json").symlink_to(
            self.tokenizer_dir / "tokenizer_config.json")
        with self.assertRaisesRegex(corpus.CorpusError, "hash/type"):
            corpus.tokenizer_files(self.tokenizer_dir, pins)

    def test_real_input_manifest_rejects_changed_schema_pin_and_source(self):
        checked_in = Path(__file__).with_name("accuracy-sources-v1.json")
        original = json.loads(checked_in.read_text())
        path = self.root / "sources.json"
        corpus.write_json(path, original)
        self.assertEqual(corpus.input_manifest(path, corpus.file_hash(path)),
                         original)
        for kind in ("schema", "pin", "commit", "tree", "quotas", "unknown",
                     "label", "missing-label", "control-type"):
            with self.subTest(kind=kind):
                changed = copy.deepcopy(original)
                if kind == "schema":
                    changed["schema"] = "accuracy-sources/v2"
                elif kind in ("commit", "tree"):
                    changed["sources"][0][kind] = "0" * 40
                elif kind == "quotas":
                    changed["quotas"]["python"][1][1] = 9
                elif kind == "unknown":
                    changed["extra"] = True
                elif kind == "label":
                    changed["python_control"]["result_label"] = "long-body comparison"
                elif kind == "missing-label":
                    del changed["python_control"]["result_label"]
                elif kind == "control-type":
                    changed["python_control"] = None
                path.write_text(json.dumps(changed))
                pin = "0" * 64 if kind == "pin" else corpus.file_hash(path)
                with self.assertRaises(corpus.CorpusError):
                    corpus.input_manifest(path, pin)

    def test_source_object_size_boundaries_and_reader_enforcement(self):
        corpus.check_object_size(16 * 1024 * 1024, 1024 ** 3)
        for size, total in ((16 * 1024 * 1024 + 1, 1),
                            (1, 1024 ** 3 + 1)):
            with self.subTest(size=size, total=total):
                with self.assertRaisesRegex(corpus.CorpusError, "size bound"):
                    corpus.check_object_size(size, total)
        oid = "a" * 40
        source = {"commit": "b" * 40, "tree": "c" * 40, "language": "go"}
        listing = f"100644 blob {oid}\tLICENSE\0".encode()
        # A refused header needs no oversized allocation or full payload.
        payload = f"{oid} blob {16 * 1024 * 1024 + 1}\nx\n".encode()
        with mock.patch.object(corpus, "git_output", side_effect=[
                (source["commit"] + "\n").encode(),
                (source["tree"] + "\n").encode(), listing, payload]):
            with self.assertRaisesRegex(corpus.CorpusError, "size bound"):
                corpus.source_files(self.root, source)

    def test_build_forces_offline_tokenizer_and_binds_python_version(self):
        # Real tiny tokenizer/extractor; only pinned-source I/O and quota scale
        # are fixtures. This does not establish pinned-corpus feasibility.
        executable = Path(os.environ["GXSTUDY_CORPUS_EXTRACTOR"])
        tiny_quotas = {language: ((2048, 1, 1), (4096, 0, 0), (8192, 0, 0))
                       for language in ("go", "python")}
        checked_in = Path(__file__).with_name("accuracy-sources-v1.json")
        manifest = json.loads(checked_in.read_text())
        manifest["quotas"] = tiny_quotas
        path = self.root / "sources.json"
        corpus.write_json(path, manifest)
        token_pins = {p.name: corpus.file_hash(p)
                      for p in self.tokenizer_dir.iterdir()}
        inputs = {
            "go": {"src/f.go": (
                b"package f\n"
                b"// Go fixture returns the original documented public value.\n"
                b"func Read() string { return \"ordinary\" }\n")},
            "python": {"Lib/f.py": (
                b"def read():\n"
                b'    "Python fixture returns the original documented value."\n'
                b'    return "ordinary"\n')},
        }
        args = argparse.Namespace(
            manifest=path, manifest_pin=corpus.file_hash(path),
            sources=self.root / "sources", tokenizer=self.tokenizer_dir,
            go_extractor=executable, output=self.root / "built")
        with (mock.patch.object(corpus, "QUOTAS", tiny_quotas),
              mock.patch.dict(corpus.RECIPE, {"quotas": tiny_quotas}),
              mock.patch.object(corpus, "tokenizer_files", return_value=token_pins),
              mock.patch.object(corpus, "source_files", side_effect=lambda _, source:
                                (inputs[source["language"]], b"fixture license")),
              mock.patch.dict(os.environ, {"HF_HUB_OFFLINE": "0",
                                           "TRANSFORMERS_OFFLINE": "0"})):
            corpus.build(args)
            self.assertEqual(os.environ["HF_HUB_OFFLINE"], "1")
            self.assertEqual(os.environ["TRANSFORMERS_OFFLINE"], "1")
            with self.assertRaises(FileExistsError):
                corpus.build(args)
        frozen = corpus.read_json(args.output / "freeze.json")
        self.assertEqual(frozen["bindings"]["python_version"], sys.version)
        self.assertEqual(frozen["freeze_id"], corpus.file_hash(
            args.output / "freeze-canonical.json"))
        self.assertEqual(corpus.read_json(args.output / "counts.json")[
            "selected"]["documents"], 2)

    def test_real_bridge_score_ties_and_shuffled_batches(self):
        documents = [self.row(f"p{i:02}") for i in range(11)]
        queries = [documents[i] for i in (0, 9, 10)]
        counts = {"shortfalls": []}
        corpus.write_json(self.root / "counts.json", counts)
        corpus.emit(self.root, documents, queries, counts,
                    {"sources": []}, {"go": b"full license"})
        names = ["documents.jsonl", "queries.jsonl", "model", "weights",
                 "probe", "runtime"]
        for name in names[2:]:
            (self.root / name).write_text("synthetic identity")
        names += [str(p.relative_to(self.root))
                  for p in self.tokenizer_dir.iterdir()]
        manifest = {"schema": "accuracy-bridge/v1", "source_pin": "a" * 40,
                    "sample": "builder-fixture", "cap": 2048,
                    "host": "fixture",
                    "limits": {"memory_bytes": 4096, "seconds": 10,
                               "threads": 1},
                    "documents": names[0], "queries": names[1],
                    "model": "model",
                    "model_data": "weights", "probe": "probe",
                    "runtime": "runtime",
                    "tokenizer": "tokenizer", "files": {
                        n: corpus.file_hash(self.root / n) for n in names}}
        config = self.root / "manifest.json"
        corpus.write_json(config, manifest)
        prepared = self.root / "prepared"
        bridge.prepare(argparse.Namespace(
            root=self.root, manifest=config,
            manifest_pin=corpus.file_hash(config),
            output=prepared))
        record = bridge.read_json(prepared / "prepared.json")
        for role, number in (("body", 11), ("query", 3)):
            spec = record["corpora"][role]
            tokens_path = self.root / spec["path"]
            batches = bridge.read_json(tokens_path)
            batches.reverse()
            # A body batch is also permuted internally; reports are already
            # restored by Go and must not be permuted a second time.
            if role == "body":
                for batch in batches:
                    width = batch["shape"][1]
                    batch["indices"].reverse()
                    batch["inputs"] = {
                        k: [n for i in reversed(range(batch["shape"][0]))
                            for n in v[i * width:(i + 1) * width]]
                        for k, v in batch["inputs"].items()}
            tokens_path.write_text(json.dumps(batches))
            spec["sha256"] = corpus.file_hash(tokens_path)
            vectors = np.zeros((number, 768), dtype=np.float32)
            vectors[:, 0] = 1
            if role == "body":
                vectors[:, 1:12] = np.eye(11)  # Distinct vectors, exact ties.
            report = {"status": "completed", "reason": "", "corpus": role,
                      "token_cap": 2048, "token_file_sha256": spec["sha256"],
                      "batch_count": len(batches), "planned_texts": number,
                      "completed_texts": number,
                      "planned_unpadded_tokens": sum(
                          sum(b["inputs"]["attention_mask"]) for b in batches),
                      "vectors": vectors.tolist(), "model": "coderank",
                      "runtime": "1.31.0",
                      "model_path": str(self.root / "model"),
                      "intra_op_threads": 1, "inter_op_threads": 1,
                      "guard_seconds": 10, "disable_prepacking": True,
                      "mapped": False, "repeat": 1, "completed_repeats": 1,
                      "planned_repeats": 1}
            total = report["planned_unpadded_tokens"]
            report["completed_unpadded_tokens"] = total
            (prepared / (role + "-report.json")).write_text(json.dumps(report))
        (prepared / "prepared.json").write_text(json.dumps(record))
        output = self.root / "assembled"
        bridge.assemble(argparse.Namespace(
            root=self.root, prepared=prepared / "prepared.json",
            prepared_pin=corpus.file_hash(prepared / "prepared.json"),
            body_report=prepared / "body-report.json",
            query_report=prepared / "query-report.json", output=output))
        cut = bridge.read_json(output / "cut.json")
        self.assertEqual(bridge.ev.main(cut["score_command"][7:]), 0)
        score = bridge.read_json(output / "score.metrics.json")["metrics"]
        self.assertEqual(score["ranks"], [1, 10, 11])
        self.assertEqual(score["r10"], 2 / 3)
        self.assertAlmostEqual(score["mrr10"], 1.1 / 3)
        vectors = np.load(output / "vectors.npy", allow_pickle=False)
        np.testing.assert_array_equal(vectors[:, 0], np.ones(14))
        np.testing.assert_array_equal(vectors[:11, 1:12], np.eye(11))


if __name__ == "__main__":
    unittest.main()
