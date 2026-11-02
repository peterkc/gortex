"""Tiny gxstudy-v2 file-contract tests. Run on CI, not on the Mac."""

import copy
import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

import fetch


class FixtureFetchTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.tier = self.root / "tier1"
        repo = self.tier / "fixture"
        repo.mkdir(parents=True)
        records = {
            "nodes.jsonl": b'{"id":"fixture::a","payload":{}}\n',
            "edges.jsonl": b'{"from":"fixture::a","to":"outside"}\n',
            "files.jsonl": b"", "symbols.jsonl": b"",
            "content.jsonl": b"", "vectors.bin": b"",
        }
        for name, content in records.items():
            (repo / name).write_bytes(content)
        self.counts = {
            "nodes": 1, "edges": 1, "files": 0, "symbols": 0,
            "content": 0, "vectors": 0, "cross_edges": 0,
            "skipped_spans": 0,
        }
        source = {"revision": "a" * 40, "source_sha256": "b" * 64}
        self.pins = {
            "generator_version": "gxstudy-v2", "seed": 20261022,
            "sources": {"fixture": source},
        }
        self.part = {
            "generator_version": "gxstudy-v2", "seed": 20261022,
            "repo": "fixture", "namespace": "fixture", "origin": "real",
            "source_revision": source["revision"],
            "source_sha256": source["source_sha256"],
            "counts": dict(self.counts),
            "sha256": {
                name: hashlib.sha256(content).hexdigest()
                for name, content in records.items()
            },
        }
        self.write(self.tier / "queries.json", [])
        self.write(self.tier / "expected.json", [])
        self.reseal()

    def write(self, path, value):
        path.write_text(json.dumps(value) + "\n", encoding="utf-8")

    def reseal(self):
        self.write(self.tier / "fixture/manifest.json", self.part)
        files = fetch.regular_files(self.tier)
        manifest = {
            "generator_version": "gxstudy-v2", "seed": 20261022,
            "repos": ["fixture"], "counts": dict(self.counts),
            "sha256": {
                name: fetch.sha256(path) for name, path in files.items()
                if name != "manifest.json"
            },
        }
        self.write(self.tier / "manifest.json", manifest)
        self.spec = {
            "repos": ["fixture"], "counts": dict(self.counts),
            "manifest_sha256": fetch.sha256(self.tier / "manifest.json"),
            "part_manifests": {
                "fixture": fetch.sha256(self.tier / "fixture/manifest.json"),
            },
            "bytes": sum(
                path.stat().st_size
                for path in fetch.regular_files(self.tier).values()
            ),
        }

    def test_valid_tier_round_trip_and_no_input_change(self):
        before = copy.deepcopy(self.pins)
        actual = fetch.verify_tier(self.tier, self.spec, self.pins)
        self.assertEqual(actual["files"], 10)
        self.assertEqual(actual["counts"]["nodes"], 1)
        self.assertEqual(actual["counts"]["edges"], 1)
        self.assertEqual(self.pins, before)
        self.assertEqual(
            actual, fetch.verify_tier(self.tier, self.spec, self.pins),
        )

    def test_dropped_edge_fails_before_use(self):
        (self.tier / "fixture/edges.jsonl").write_bytes(b"")
        with self.assertRaisesRegex(fetch.FetchError, "file hash differs"):
            fetch.verify_tier(self.tier, self.spec, self.pins)

    def test_wrong_manifest_pin_fails(self):
        spec = {**self.spec, "manifest_sha256": "0" * 64}
        with self.assertRaisesRegex(fetch.FetchError, "manifest hash"):
            fetch.verify_tier(self.tier, spec, self.pins)

    def test_extra_file_and_symlink_are_rejected(self):
        extra = self.tier / "extra"
        extra.write_text("unexpected")
        with self.assertRaisesRegex(fetch.FetchError, "inventory"):
            fetch.verify_tier(self.tier, self.spec, self.pins)
        extra.unlink()
        node = self.tier / "fixture/nodes.jsonl"
        saved = self.root / "nodes"
        node.rename(saved)
        node.symlink_to(saved)
        with self.assertRaisesRegex(fetch.FetchError, "symlink"):
            fetch.verify_tier(self.tier, self.spec, self.pins)

    def test_repository_counts_and_revision_remain_independent_checks(self):
        self.part["counts"]["edges"] = 2
        self.reseal()
        with self.assertRaisesRegex(fetch.FetchError, "count sum"):
            fetch.verify_tier(self.tier, self.spec, self.pins)
        self.part["counts"]["edges"] = 1
        self.part["source_revision"] = "c" * 40
        self.reseal()
        with self.assertRaisesRegex(fetch.FetchError, "source_revision"):
            fetch.verify_tier(self.tier, self.spec, self.pins)

    def archive(self, path, extra=()):
        with tarfile.open(path, "w") as output:
            output.add(self.tier, arcname="tier1")
            for name in extra:
                member = tarfile.TarInfo(name)
                member.size = 1
                output.addfile(member, io.BytesIO(b"x"))

    def test_safe_archive_extracts_once_and_refuses_existing_tier(self):
        archive = self.root / "good.tar"
        self.archive(archive)
        destination = self.root / "data"
        destination.mkdir()
        fetch.extract_tier(archive, destination, "tier1", self.spec)
        fetch.verify_tier(destination / "tier1", self.spec, self.pins)
        with self.assertRaisesRegex(fetch.FetchError, "existing tier"):
            fetch.extract_tier(archive, destination, "tier1", self.spec)

    def test_archive_traversal_and_duplicate_refuse_before_extraction(self):
        for index, extra in enumerate((
            "tier1/../escape", "tier1/queries.json",
            "tier1/._manifest.json", "._tier1",
        )):
            with self.subTest(extra=extra):
                archive = self.root / f"bad-{index}.tar"
                self.archive(archive, (extra,))
                destination = self.root / f"data-{index}"
                destination.mkdir()
                with self.assertRaises(fetch.FetchError):
                    fetch.extract_tier(
                        archive, destination, "tier1", self.spec,
                    )
                self.assertEqual(list(destination.iterdir()), [])
                self.assertFalse((self.root / "escape").exists())

    def test_json_arrays_and_duplicate_keys_are_rejected(self):
        path = self.root / "receipt.json"
        for text in ('[]', '{"state":"failed","state":"completed"}'):
            with self.subTest(text=text):
                path.write_text(text)
                with self.assertRaises(fetch.FetchError):
                    fetch.read_json(path)

    def test_missing_limits_are_not_defaulted(self):
        with self.assertRaises(SystemExit) as caught:
            fetch.parser().parse_args(["run", "--release-tag", "fixed-v1"])
        self.assertEqual(caught.exception.code, 2)


if __name__ == "__main__":
    unittest.main()
