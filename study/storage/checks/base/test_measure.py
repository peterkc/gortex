"""CI-only fixture tests; no measurements or local Go commands."""

import copy
import subprocess
import tempfile
import json
import sys
import os
import time
from unittest.mock import patch
from pathlib import Path
import unittest

from measure import (
    RunError, SemanticMismatch, aggregate, artifact_inventory, base_verdict,
    canonical_output, checked_file, choose_base, collect_run, digest,
    latency_groups, lookup_verdict, paired_ratios, percentile, run,
    protected_snapshot, ratio_status, remove_serving, validate_config,
    validate_fixture_pins, phase_deadlines, finalization_reserve,
)


class SelectionTests(unittest.TestCase):
    def candidate(self, passes=True, cpu=10, total=20):
        return {"passes": passes, "status": "pass" if passes else "miss",
                "construction_cpu_ns": cpu, "total_bytes": total,
                "read_directions": {"one": 0}}

    def test_all_five_rows(self):
        cases = [
            (self.candidate(False), self.candidate(),
             "only_c_passes", "recommend_c"),
            (self.candidate(), self.candidate(False),
             "only_b_passes", "recommend_b"),
            (self.candidate(cpu=20), self.candidate(),
             "both_pass", "recommend_c"),
            (self.candidate(False), self.candidate(False),
             "neither_passes", "stop_and_amend"),
            (None, self.candidate(),
             "comparison_unavailable", "stop_no_recommendation"),
        ]
        for b, c, row, branch in cases:
            with self.subTest(row=row):
                self.assertEqual(
                    choose_base(b, c, cpu_selection_approved=True),
                    (row, branch))

    def test_cpu_then_bytes_and_no_unauthorized_fallback(self):
        b = self.candidate(cpu=1, total=100)
        c = self.candidate(cpu=2, total=1)
        self.assertEqual(choose_base(b, c, cpu_selection_approved=True)[1],
                         "recommend_b")
        b = self.candidate(total=30)
        c = self.candidate(total=20)
        self.assertEqual(choose_base(b, c, cpu_selection_approved=True)[1],
                         "recommend_c")
        self.assertEqual(choose_base(b, c, cpu_selection_approved=False)[0],
                         "comparison_unavailable")
        self.assertNotEqual(
            choose_base(self.candidate(False), self.candidate(False),
                        cpu_selection_approved=True)[1], "recommend_b")

    def test_read_conflict_and_unresolved_tie_return_to_user(self):
        b = self.candidate()
        c = self.candidate()
        self.assertEqual(choose_base(b, c, cpu_selection_approved=True)[1],
                         "ask_user_unresolved_tie")
        c = {**c, "read_directions": {"one": -1, "two": 1}}
        original = copy.deepcopy((b, c))
        self.assertEqual(choose_base(b, c, cpu_selection_approved=True)[1],
                         "ask_user_read_tradeoff")
        self.assertEqual((b, c), original)


class TimingEvidenceTests(unittest.TestCase):
    def test_lookup_strata_are_separate_and_warmup_is_not_measured(self):
        rows = [
            {"status": "passed", "elapsed_ns": 10, "warmup": False,
             "operation": "id", "missing": False},
            {"status": "passed", "elapsed_ns": 99, "warmup": False,
             "operation": "id", "missing": True},
            {"status": "passed", "elapsed_ns": 500, "warmup": True,
             "operation": "id", "missing": True},
        ]
        groups = latency_groups(rows)
        self.assertEqual(groups["id_hit"], [10])
        self.assertEqual(groups["id_miss"], [99])
        self.assertEqual(groups["id"], [10, 99])
        self.assertEqual(percentile([1, 2, 3, 4]), 4)

    def test_missing_zero_or_failed_evidence_is_rejected(self):
        for fields in ({}, {"elapsed_ns": 0}, {"elapsed_ns": -1},
                       {"elapsed_ns": 4, "status": "unsupported"}):
            row = {"status": "passed", "warmup": False,
                   "operation": "name", "missing": False, **fields}
            with self.subTest(fields=fields), self.assertRaises(RunError):
                latency_groups([row])

    def test_entrypoint_has_no_path_or_limit_defaults(self):
        script = Path(__file__).with_name("run.sh")
        result = subprocess.run([str(script)], capture_output=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn(b"--config", result.stderr)


def dump_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value), encoding="utf-8")


def observations(values=(100, 100, 100), *, graph=300, cpu=10):
    operations = ["id", "name", "file", "symbols", "in_calls", "traverse2",
                  "traverse3", "multi_name", "multi_symbols", "multi_in_calls",
                  "multi_traverse2", "multi_traverse3"]
    operations += [op + "_" + kind for op in ("id", "name", "file")
                   for kind in ("hit", "miss")]
    result = {}
    for tier in ("tier1", "tier10", "tier100"):
        result[tier] = {
            "sample_counts": {key: 60 for key in operations},
            "runs": [{"pair_index": index,
                      "p95_ns": {key: value for key in operations},
                      "sample_counts": {key: 20 for key in operations}}
                     for index, value in enumerate(values, 1)],
            "bytes_per_edge": graph, "construction_cpu_ns": cpu,
            "total_bytes": 1000,
        }
    return result


def physical_fixture(root, adapter="pebble-bleve", *, vector_bytes=50000):
    path = root / "run-01/segments/repo"
    payloads = {"graph/000001.sst": ("graph", b"g" * 300),
                "text-records/000001.sst": ("text", b"t" * 400),
                "symbols.bleve/index": ("text", b"s" * 20),
                "vectors/000001.sst": ("vectors", b"v" * vector_bytes)}
    if adapter.startswith("parquet-"):
        payloads = {"nodes.parquet": ("graph", b"g" * 300),
                    "symbols.parquet": ("text", b"t" * 400),
                    "vectors.parquet": ("vectors", b"v" * vector_bytes)}
    if adapter == "pebble-bleve":
        payloads.update({"graph/000002.log": ("graph", b"g" * 64),
                         "text-records/000002.log": ("text", b"t" * 80),
                         "vectors/000002.log": ("vectors", b"v" * 96)})
    entries = []
    for name, (category, payload) in payloads.items():
        target = path / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(payload)
        entries.append({"name": name, "size": len(payload),
                        "sha256": digest(target), "category": category})
    seal = {"files": entries}
    if adapter.startswith("parquet-"):
        counts = {name: 1 for name in (
            "nodes", "edges", "files", "symbols", "content", "vectors")}
        families = {"nodes": "nodes", "names": "nodes", "paths": "nodes",
                    "out": "edges", "in": "edges", "files": "files",
                    "symbols": "symbols", "content": "content",
                    "vectors": "vectors"}
        seal["meta"] = {"counts": counts}
        seal["verification"] = {
            name: {"rows": counts[family], "input_sha256": "a" * 64,
                   "output_sha256": "a" * 64, "cpu_ns": 1}
            for name, family in families.items()}
    dump_json(path / "seal.json", seal)
    build = {"repo": "repo", "cpu_ns": 100,
             "ref": {"path": str(path), "digest": digest(path / "seal.json")},
             "artifact_bytes": sum(item.stat().st_size
                                   for item in path.glob("**/*")
                                   if item.is_file())}
    return build, seal


def refresh_seal(build, seal):
    path = Path(build["ref"]["path"]) / "seal.json"
    old_size = path.stat().st_size
    dump_json(path, seal)
    return {**build, "ref": {**build["ref"], "digest": digest(path)},
            "artifact_bytes": build["artifact_bytes"]
                              + path.stat().st_size - old_size}


class PairedProtocolTests(unittest.TestCase):
    def test_exact_lookup_boundary_and_crossing_are_not_rounded(self):
        control = observations()
        for values, status in [((150, 150, 150), "pass"),
                               ((151, 151, 151), "miss"),
                               ((149, 151, 150), "partial")]:
            with self.subTest(values=values):
                result = lookup_verdict(observations(values), control)
                self.assertEqual(result["status"], status)
                self.assertEqual(len(result["measured_value"]), 12)

    def test_insufficient_or_missing_stratum_is_partial(self):
        for count in (0, 1, 19):
            candidate = observations()
            candidate["tier1"]["runs"][0]["sample_counts"]["file_miss"] = count
            result = lookup_verdict(candidate, observations())
            self.assertEqual(result["status"], "partial")
            self.assertEqual(result["insufficient_strata"],
                             ["tier1/file_miss"])
        candidate["tier1"]["runs"][0]["sample_counts"].pop("file_miss")
        self.assertEqual(lookup_verdict(candidate, observations())["status"],
                         "partial")

    def test_duplicate_missing_and_wrong_pairs_are_rejected(self):
        for indices in ((1, 2), (1, 2, 2), (4, 5, 6), (1, 2, 3, 3)):
            candidate = observations()["tier1"]
            row = candidate["runs"][0]
            candidate["runs"] = [{**row, "pair_index": index}
                                 for index in indices]
            with self.subTest(indices=indices), self.assertRaises(RunError):
                paired_ratios(candidate, observations()["tier1"], "name")

    def test_serving_and_read_ceilings_remain_300_and_one(self):
        control = observations()
        for byte_count, status in ((300, "pass"), (301, "miss")):
            result = base_verdict(observations(graph=byte_count),
                                  control, control)
            self.assertEqual(result["status"], status)
        candidate = observations()
        for row in candidate["tier10"]["runs"]:
            row["p95_ns"]["multi_name"] = 101
        self.assertEqual(base_verdict(candidate, control, control)["status"],
                         "miss")
        candidate["tier10"]["runs"][0]["p95_ns"]["multi_name"] = 99
        self.assertEqual(base_verdict(candidate, control, control)["status"],
                         "partial")

    def test_read_conflict_requires_consistent_opposite_directions(self):
        control = observations()
        candidate = observations()
        for row, value in zip(candidate["tier1"]["runs"], (90, 110, 90)):
            row["p95_ns"]["name_hit"] = value
            row["p95_ns"]["in_calls"] = 80
        result = base_verdict(candidate, control, control)
        self.assertEqual(result["read_directions"]["tier1/name_hit"], 0)
        self.assertEqual(result["read_directions"]["tier1/in_calls"], -1)
        self.assertEqual(choose_base(result, result,
                                    cpu_selection_approved=True)[1],
                         "ask_user_unresolved_tie")
        for row in candidate["tier1"]["runs"]:
            row["p95_ns"]["name_hit"] = 110
        result = base_verdict(candidate, control, control)
        self.assertEqual(choose_base(result, result,
                                    cpu_selection_approved=True)[1],
                         "ask_user_read_tradeoff")

    def test_partial_candidate_never_becomes_b_only_pass(self):
        b = SelectionTests().candidate()
        c = {**SelectionTests().candidate(False), "status": "partial"}
        self.assertEqual(choose_base(b, c, cpu_selection_approved=True),
                         ("comparison_unavailable", "stop_no_recommendation"))
        self.assertEqual(ratio_status([1, 1, 1], 1), "pass")
        self.assertEqual(ratio_status([1, 1.01, 0.99], 1), "partial")


class PhysicalAccountingTests(unittest.TestCase):
    def test_exact_scope_sums_and_vector_growth(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            first, _ = physical_fixture(root / "one")
            second, _ = physical_fixture(root / "two", vector_bytes=90000)
            one = artifact_inventory(first, "pebble-bleve")
            two = artifact_inventory(second, "pebble-bleve")
            self.assertEqual(sum(one["bytes"].values()), one["total"])
            self.assertEqual(one["bytes"]["graph"], two["bytes"]["graph"])
            self.assertEqual(two["bytes"]["vectors"]
                             - one["bytes"]["vectors"], 40000)
            self.assertEqual(one["bytes"]["text"], 500)

    def test_misclassified_or_unbound_bytes_are_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            build, seal = physical_fixture(Path(tmp))
            seal["files"][-1]["category"] = "graph"
            build = refresh_seal(build, seal)
            with self.assertRaises(RunError):
                artifact_inventory(build, "pebble-bleve")
            seal["files"][-1]["category"] = "vectors"
            build = refresh_seal(build, seal)
            (Path(build["ref"]["path"]) / "unbound").write_bytes(b"x")
            with self.assertRaises(RunError):
                artifact_inventory(build, "pebble-bleve")

    def test_readback_cpu_is_separate_and_bad_witnesses_are_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            build, seal = physical_fixture(Path(tmp), "parquet-base")
            inventory = artifact_inventory(build, "parquet-base")
            self.assertEqual(inventory["raw_build_cpu_ns"], 100)
            self.assertEqual(inventory["verification_cpu_ns"], 9)
            self.assertEqual(inventory["construction_cpu_ns"], 91)
            for field, value in (("rows", 2), ("output_sha256", "b" * 64),
                                 ("cpu_ns", -1), ("cpu_ns", 100)):
                changed = copy.deepcopy(seal)
                changed["verification"]["nodes"][field] = value
                changed_build = refresh_seal(build, changed)
                with self.subTest(field=field), self.assertRaises(RunError):
                    artifact_inventory(changed_build, "parquet-base")
                build = refresh_seal(changed_build, seal)

    def test_cpu_uses_median_and_graph_uses_edge_divisor(self):
        reports = []
        for index, cpu in enumerate((10, 100, 20), 1):
            inventory = {"construction_cpu_ns": cpu, "raw_build_cpu_ns": cpu,
                         "verification_cpu_ns": 0, "bytes": {"graph": 300},
                         "total": 1000}
            report = {"pair_index": index, "_inventory": [inventory]}
            reports.append((report, {"id_hit": [100] * 20}))
        result = aggregate(reports, {"counts": {"edges": 2}, "repos": ["r"]})
        self.assertEqual(result["construction_cpu_ns"], 20)
        self.assertEqual(result["bytes_per_edge"], 150)
        self.assertEqual(result["sample_counts"]["id_hit"], 60)
        with self.assertRaises(RunError):
            aggregate(reports, {"counts": {"edges": 0}, "repos": ["r"]})
        with self.assertRaises(RunError):
            aggregate([], {"counts": {"edges": 2}, "repos": ["r"]})

    def test_successful_removal_keeps_inventory_and_receipts(self):
        with tempfile.TemporaryDirectory() as tmp:
            run = Path(tmp) / "timing"
            build, _ = physical_fixture(run)
            inventory = artifact_inventory(build, "pebble-bleve")
            proof = Path(tmp) / "inventory.json"
            dump_json(proof, inventory)
            log = Path(tmp) / "engine.log"
            log.write_text("retained", encoding="utf-8")
            receipt = Path(tmp) / "removal.jsonl"
            removed = remove_serving([inventory], run, receipt)
            self.assertFalse(Path(inventory["path"]).exists())
            self.assertEqual(len(removed), 1)
            self.assertTrue(proof.exists())
            self.assertEqual(log.read_text(encoding="utf-8"), "retained")
            records = [json.loads(line) for line in
                       receipt.read_text(encoding="utf-8").splitlines()]
            self.assertEqual([row["state"] for row in records],
                             ["remove_intent", "removed"])
            self.assertEqual(records[0]["files"], inventory["files"])

    def test_replaced_or_foreign_serving_directory_is_never_removed(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            build, _ = physical_fixture(root / "timing")
            inventory = artifact_inventory(build, "pebble-bleve")
            for change in ({"identity": [-1, -1]}, {"repo": "other"}):
                with self.subTest(change=change), self.assertRaises(RunError):
                    remove_serving([{**inventory, **change}], root / "timing",
                                   root / ("receipt-" + next(iter(change))))
                self.assertTrue(Path(inventory["path"]).exists())


def terminal_fixture(root, *, failure_kind=None):
    root.mkdir()
    run = root / "run-01"
    run.mkdir()
    latency = run / "queries.jsonl"
    row = {"status": "passed", "elapsed_ns": 10, "warmup": False,
           "operation": "id", "missing": False}
    latency.write_text(json.dumps(row) + "\n", encoding="utf-8")
    failed = failure_kind is not None
    report = {"adapter": "parquet-base", "run": 1,
              "manifest_sha256": "a" * 64, "timing_only": True,
              "success": not failed, "builds": [{"cpu_ns": 100}],
              "checks": [{"query_id": "wrong-edge", "detail": "edge differs",
                          "status": "failed", "mismatch": True,
                          "failure_kind": failure_kind}] if failed else [],
              "failures": ["wrong-edge: edge differs"] if failed else [],
              "delta_checks": [], "crash": [],
              "query_latency_sha256": {"queries.jsonl": digest(latency)}}
    components = {name: {"oom_kills": 0, "exit_code": 0,
                         "status": "completed"}
                  for name in ("runner", "checker", "engine")}
    if failed:
        components["runner"].update(exit_code=1, status="failed")
    receipt = {"success": not failed, "components": components,
               "output_sha256": {}}
    save_terminal(root, report, receipt)
    return report, receipt


def save_terminal(root, report, receipt):
    path = root / "parquet-base-tier-01.json"
    dump_json(path, report)
    receipt = {**receipt, "output_sha256": {path.name: digest(path)}}
    dump_json(root / "components-01.json", receipt)


class TerminalEvidenceTests(unittest.TestCase):
    def collect(self, root, *, full=False):
        return collect_run(root, "parquet-base", {"sha256": "a" * 64},
                           full_protocol=full)

    def assert_blocked(self, root):
        with self.assertRaises(RunError) as caught:
            self.collect(root)
        self.assertIs(type(caught.exception), RunError)

    def test_complete_success_and_semantic_failure_are_distinct(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            terminal_fixture(root / "success")
            collected = self.collect(root / "success")
            self.assertEqual(collected[0][1]["id_hit"], [10])
            terminal_fixture(root / "semantic", failure_kind="semantic")
            with self.assertRaises(SemanticMismatch) as caught:
                self.collect(root / "semantic")
            self.assertEqual(len(caught.exception.reports), 1)
            self.assertTrue((root / "semantic/run-01/queries.jsonl").exists())

    def test_operation_unknown_and_mixed_failures_never_fall_back(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for kind in ("operation_error", "", "unknown"):
                directory = root / ("kind-" + kind)
                terminal_fixture(directory, failure_kind=kind)
                self.assert_blocked(directory)
            directory = root / "mixed"
            report, receipt = terminal_fixture(directory,
                                                failure_kind="semantic")
            report["checks"].append({"query_id": "io", "detail": "I/O error",
                                     "status": "failed", "mismatch": True,
                                     "failure_kind": "operation_error"})
            report["failures"].append("io: I/O error")
            save_terminal(directory, report, receipt)
            self.assert_blocked(directory)

    def test_oom_deadline_hash_zero_cpu_and_protocol_are_not_misses(self):
        changes = ("oom", "deadline", "output-hash", "latency-hash",
                   "zero-cpu", "missing-cpu", "no-builds", "wrong-protocol",
                   "inconsistent-success")
        with tempfile.TemporaryDirectory() as tmp:
            for change in changes:
                directory = Path(tmp) / change
                report, receipt = terminal_fixture(directory,
                                                    failure_kind="semantic")
                if change == "oom":
                    receipt["components"]["engine"]["oom_kills"] = 1
                elif change == "deadline":
                    receipt["components"]["runner"].update(
                        status="deadline", exit_code=1)
                elif change == "latency-hash":
                    report["query_latency_sha256"]["queries.jsonl"] = "0" * 64
                elif change == "zero-cpu":
                    report["builds"][0]["cpu_ns"] = 0
                elif change == "missing-cpu":
                    report["builds"][0].pop("cpu_ns")
                elif change == "no-builds":
                    report["builds"] = []
                elif change == "inconsistent-success":
                    receipt["success"] = True
                elif change == "wrong-protocol":
                    report["timing_only"] = False
                save_terminal(directory, report, receipt)
                if change == "output-hash":
                    path = directory / "parquet-base-tier-01.json"
                    path.write_text("{}", encoding="utf-8")
                with self.subTest(change=change):
                    self.assert_blocked(directory)
                    self.assertTrue((directory / "run-01").exists())

    def test_full_protocol_and_missing_terminal_evidence_are_required(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            report, receipt = terminal_fixture(root / "timing")
            report["timing_only"] = False
            save_terminal(root / "timing", report, receipt)
            with self.assertRaises(RunError):
                self.collect(root / "timing", full=True)
            (root / "empty").mkdir()
            self.assert_blocked(root / "empty")


def pinned_inputs(root):
    source = root / "source"
    config_dir = root / "policy"
    config_dir.mkdir()
    config_path = config_dir / "run.json"
    approval_path = config_dir / "approval.json"
    dump_json(config_path, {})
    dump_json(approval_path, {})
    for name in ("harness", "adapters/sqlite-segment",
                 "adapters/pebble-bleve", "checks/base"):
        (source / "study/storage" / name).mkdir(parents=True)
    data = {}
    pins = {"schema": "design-c-fixtures/v1", "tiers": {}}
    for label in ("tier1", "tier10"):
        directory = root / label
        part = directory / "repo/manifest.json"
        dump_json(part, {"repo": "repo"})
        manifest = {"repos": ["repo"], "counts": {"nodes": 1, "edges": 2},
                    "sha256": {"repo/manifest.json": digest(part)}}
        dump_json(directory / "manifest.json", manifest)
        data[label] = {"path": str(directory),
                       "sha256": digest(directory / "manifest.json")}
        pins["tiers"][label] = {
            "repos": manifest["repos"], "counts": manifest["counts"],
            "manifest_sha256": data[label]["sha256"],
            "part_manifests": {"repo": digest(part)}}
    pin_path = source / "study/storage/checks/fixtures/fixture-pins.json"
    dump_json(pin_path, pins)
    config = {"source_root": str(source), "data": data,
              "approval_path": str(approval_path),
              "output_dir": str(root / "output")}
    return config, config_path, pin_path


class InputBoundaryTests(unittest.TestCase):
    def test_exact_pins_not_self_consistent_alternate_data(self):
        with tempfile.TemporaryDirectory() as tmp:
            config, _, pin_path = pinned_inputs(Path(tmp))
            self.assertEqual(validate_fixture_pins(config), digest(pin_path))
            path = Path(config["data"]["tier1"]["path"]) / "manifest.json"
            changed = json.loads(path.read_text(encoding="utf-8"))
            changed["counts"]["edges"] = 3
            dump_json(path, changed)
            config["data"]["tier1"]["sha256"] = digest(path)
            with self.assertRaises(RunError):
                validate_fixture_pins(config)

    def test_part_manifest_pin_is_independent_of_top_level_pin(self):
        with tempfile.TemporaryDirectory() as tmp:
            config, _, _ = pinned_inputs(Path(tmp))
            part = Path(config["data"]["tier10"]["path"])
            dump_json(part / "repo/manifest.json", {"repo": "changed"})
            with self.assertRaises(RunError):
                validate_fixture_pins(config)

    def test_added_source_fixture_and_policy_files_are_detected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            config, config_path, _ = pinned_inputs(root)
            before = protected_snapshot(config, config_path)
            targets = [Path(config["source_root"]) / "study/storage/harness/x",
                       Path(config["data"]["tier1"]["path"]) / "unexpected",
                       config_path.parent / "unexpected"]
            for target in targets:
                target.write_bytes(b"new")
                self.assertNotEqual(protected_snapshot(config, config_path),
                                    before)
                target.unlink()
                self.assertEqual(protected_snapshot(config, config_path),
                                 before)

    def test_canonical_fresh_output_and_alias_rejections(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            config, config_path, _ = pinned_inputs(root)
            self.assertEqual(canonical_output(config, config_path),
                             root / "output")
            alias = root / "alias"
            alias.symlink_to(Path(config["source_root"]),
                             target_is_directory=True)
            candidates = [alias / "new", root / "source/../output",
                          Path(config["source_root"]) / "output",
                          Path(config["data"]["tier1"]["path"]) / "output",
                          config_path.parent / "output", root / "source"]
            for path in candidates:
                with self.subTest(path=path), self.assertRaises(RunError):
                    canonical_output({**config, "output_dir": str(path)},
                                     config_path)

    def test_checked_file_rejects_parent_absolute_and_symlink_alias(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "file").write_bytes(b"x")
            (root / "alias").symlink_to(root / "file")
            self.assertEqual(checked_file(root, "file"), root / "file")
            for name in (str(root / "file"), "../file", "alias"):
                with self.subTest(name=name), self.assertRaises(RunError):
                    checked_file(root, name)


class ConfigRefusalTests(unittest.TestCase):
    def config(self):
        root = Path(__file__).resolve().parents[4]
        return {
            "schema": "design-c-base-run/v1", "source_root": str(root),
            "source_revision": "0" * 40, "recipe_sha256": "a" * 64,
            "approval_path": "/not-read-before-source-refusal",
            "approval_sha256": "a" * 64,
            "base": {"main_wall_seconds": 9000, "side_wall_seconds": 2700},
            "job": {"started_unix_seconds": int(time.time()),
                    "timeout_seconds": 350 * 60,
                    "cleanup_upload_seconds": 20 * 60},
            "runs": 3, "safety_timeout_seconds": 900,
            "memory_limits_bytes": {"runner": 1 << 20, "checker": 1 << 20,
                                    "engine": 1 << 20},
            "output_dir": "/not-created", "cgroup_root": "/not-used",
            "binaries": {}, "data": {},
            "construction_selection_approved": True,
        }

    def test_bad_schema_and_caps_refuse_before_source_and_output(self):
        config = self.config()
        changes = [({"schema": "unknown"}, "exact documented fields"),
                   ({"wall_seconds": 100}, "exact documented fields"),
                   ({"runs": 2}, "three fresh runs"),
                   ({"runs": True}, "positive runs"),
                   ({"safety_timeout_seconds": 0}, "positive"),
                   ({"base": {"main_wall_seconds": 9000}}, "side limits"),
                   ({"base": {"main_wall_seconds": -1,
                              "side_wall_seconds": 2700}}, "positive"),
                   ({"memory_limits_bytes": {"engine": 1}}, "three component"),
                   ({"memory_limits_bytes": {"runner": 1, "checker": 1,
                                             "engine": 1}}, "page-aligned")]
        for change, message in changes:
            with self.subTest(change=change), self.assertRaisesRegex(
                    RunError, message):
                validate_config({**config, **change}, Path("unopened.json"))

    def test_unapproved_source_or_platform_refuses_before_approval_io(self):
        expected = "source revision" if sys.platform == "linux" else "Linux CI"
        with self.assertRaisesRegex(RunError, expected):
            validate_config(self.config(), Path("unopened.json"))


class C2DecisionRegressionTests(unittest.TestCase):
    def test_identical_text_cannot_reject_or_create_a_read_conflict(self):
        control = observations()
        candidate = observations()
        for tier in candidate.values():
            for row in tier["runs"]:
                row["p95_ns"]["symbols"] = 150
                row["p95_ns"]["multi_symbols"] = 150
                row["p95_ns"]["in_calls"] = 90
        result = base_verdict(candidate, control, control)
        self.assertEqual(result["status"], "pass")
        self.assertFalse(any("symbols" in key for key in
                             result["read_directions"]))
        self.assertEqual(result["identical_code_reads"][
            "tier10/multi_symbols"]["ratios"], [1.5, 1.5, 1.5])
        self.assertNotEqual(choose_base(result, result,
                                       cpu_selection_approved=True)[1],
                            "ask_user_read_tradeoff")
        for row in candidate["tier10"]["runs"]:
            row["p95_ns"]["multi_name"] = 101
        self.assertEqual(base_verdict(candidate, control, control)["status"],
                         "miss")

    def test_pooled_count_cannot_admit_an_underfilled_pair_statistic(self):
        control = observations()
        candidate = observations()
        candidate["tier1"]["sample_counts"]["file_hit"] = 36
        for row in candidate["tier1"]["runs"]:
            row["sample_counts"]["file_hit"] = 12
        self.assertEqual(lookup_verdict(candidate, control)["status"],
                         "partial")
        candidate = observations()
        candidate["tier10"]["runs"][1]["sample_counts"]["multi_traverse2"] = 19
        result = base_verdict(candidate, control, control)
        self.assertEqual(result["status"], "partial")
        self.assertIn("tier10/multi_traverse2",
                      result["insufficient_read_strata"])

    def test_combined_name_group_does_not_set_a_lookup_direction(self):
        control = observations()
        candidate = observations()
        for row in candidate["tier1"]["runs"]:
            row["p95_ns"]["name"] = 10000
        result = base_verdict(candidate, control, control)
        self.assertEqual(result["status"], "pass")
        self.assertNotIn("tier1/name", result["read_directions"])
        self.assertEqual(result["read_directions"]["tier1/name_hit"], 0)
        self.assertEqual(result["read_directions"]["tier1/name_miss"], 0)

    def test_every_unexplained_runner_failure_blocks_a_semantic_miss(self):
        extra = ("input changed during run: data", "release failed",
                 "reset failed", "crash: unavailable", "unknown failure")
        with tempfile.TemporaryDirectory() as tmp:
            for index, message in enumerate(extra):
                root = Path(tmp) / str(index)
                report, receipt = terminal_fixture(root,
                                                    failure_kind="semantic")
                report["failures"].append(message)
                save_terminal(root, report, receipt)
                with self.subTest(message=message), self.assertRaises(
                        RunError) as caught:
                    collect_run(root, "parquet-base", {"sha256": "a" * 64},
                                full_protocol=False)
                self.assertIs(type(caught.exception), RunError)
                self.assertTrue((root / "run-01/queries.jsonl").exists())

    def test_failure_multiset_does_not_hide_a_duplicate_runner_message(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "duplicate"
            report, receipt = terminal_fixture(root, failure_kind="semantic")
            report["failures"] *= 2
            save_terminal(root, report, receipt)
            with self.assertRaises(RunError) as caught:
                collect_run(root, "parquet-base", {"sha256": "a" * 64},
                            full_protocol=False)
            self.assertIs(type(caught.exception), RunError)


class OrchestrationRegressionTests(unittest.TestCase):
    def test_semantic_branch_assembly_and_unknown_failure_receipts(self):
        scenarios = (("c", 200, "recommend_b"),
                     ("c", 602, "stop_and_amend"),
                     ("c_side", 200, "recommend_b"),
                     ("resource", 200, "comparison_unavailable_stop"))
        for failed_adapter, b_bytes, branch in scenarios:
            with self.subTest(case=failed_adapter, b_bytes=b_bytes):
                with tempfile.TemporaryDirectory() as tmp:
                    root = Path(tmp)
                    config, config_path, _ = pinned_inputs(root)
                    config["data"]["tiny"] = config["data"]["tier1"]
                    config["data"]["tier100"] = config["data"]["tier1"]
                    config.update(
                        source_revision="a" * 40, approval_sha256="a" * 64,
                        recipe_sha256="a" * 64, binaries={},
                        job={"started_unix_seconds": int(time.time()),
                             "timeout_seconds": 350 * 60,
                             "cleanup_upload_seconds": 20 * 60},
                        memory_limits_bytes={},
                        base={"main_wall_seconds": 9000,
                              "side_wall_seconds": 2700},
                        construction_selection_approved=True)

                    def process_boundary(config, output, label, adapter,
                                         data_label, deadline, *,
                                         full_protocol, pair_index=0):
                        if adapter == failed_adapter and full_protocol:
                            raise SemanticMismatch([({"checked": True}, {})])
                        if failed_adapter == "resource" and adapter == "c":
                            raise RunError("unclassified process failure")
                        if full_protocol:
                            return []
                        ns = (200 if failed_adapter == "c_side"
                              and adapter == "c" else 100)
                        groups = {key: [ns] * 20 for key in
                                  observations()["tier1"]["sample_counts"]}
                        inventory = {
                            "construction_cpu_ns": 100,
                            "raw_build_cpu_ns": 100,
                            "verification_cpu_ns": 0,
                            "bytes": {"graph": b_bytes if adapter == "b"
                                      else 200}, "total": 1000,
                            "verification": {str(i): {} for i in range(9)}}
                        return [({"pair_index": pair_index,
                                  "_inventory": [inventory]}, groups)]

                    with (
                        patch("measure.validate_config"),
                        patch("measure.measure_adapter", process_boundary),
                        patch.dict(os.environ),
                    ):
                        if failed_adapter == "resource":
                            with self.assertRaises(RunError):
                                run(config, config_path)
                        else:
                            run(config, config_path)
                    result_path = root / "output/result.json"
                    result = json.loads(result_path.read_text(
                        encoding="utf-8"))
                    self.assertEqual(result["branch_taken"], branch)
                    self.assertEqual(result["finalization"]["reserve_seconds"],
                                     60)
                    self.assertGreater(
                        result["finalization"]["protected_bytes"], 0)
                    if failed_adapter == "resource":
                        self.assertEqual(result["status"], "blocked")
                        self.assertIn("error", result)
                        self.assertIn("measured_value", result)
                    else:
                        self.assertEqual(result["check2"]["status"], "miss")
                        self.assertEqual(result["check5"]["b"]["passes"],
                                         b_bytes == 200)


class C3OutcomeRegressionTests(unittest.TestCase):
    def test_conclusive_disk_miss_survives_partial_latency_evidence(self):
        control = observations()
        b = base_verdict(control, control, control)
        for byte_count, status, branch in (
                (301, "miss", "recommend_b"),
                (300, "partial", "stop_no_recommendation")):
            candidate = observations(graph=byte_count)
            candidate["tier1"]["runs"][0]["sample_counts"]["file_miss"] = 19
            c = base_verdict(candidate, control, control)
            self.assertEqual(c["lookup"]["status"], "partial")
            self.assertEqual(c["status"], status)
            chosen = choose_base(b, c, cpu_selection_approved=True)
            self.assertEqual(chosen[1], branch)
        c = base_verdict(observations(graph=301), control, control)
        b_miss = base_verdict(observations(graph=301), control, control)
        self.assertEqual(choose_base(b_miss, c,
                                    cpu_selection_approved=True)[1],
                         "stop_and_amend")

    def test_omitted_detail_defaults_empty_but_extra_errors_block(self):
        with tempfile.TemporaryDirectory() as tmp:
            for index, extra in enumerate((None, "input changed during run")):
                root = Path(tmp) / str(index)
                report, receipt = terminal_fixture(root,
                                                    failure_kind="semantic")
                report["checks"][0].pop("detail")
                report["failures"] = ["wrong-edge: "]
                if extra is not None:
                    report["failures"].append(extra)
                save_terminal(root, report, receipt)
                with self.assertRaises(RunError) as caught:
                    collect_run(root, "parquet-base", {"sha256": "a" * 64},
                                full_protocol=False)
                self.assertIs(type(caught.exception),
                              SemanticMismatch if extra is None else RunError)


class AbsoluteDeadlineTests(unittest.TestCase):
    def job(self):
        return {"started_unix_seconds": 100000,
                "timeout_seconds": 350 * 60,
                "cleanup_upload_seconds": 20 * 60}

    def test_hash_reserve_has_a_floor_and_covers_pinned_bytes(self):
        for byte_count, seconds in ((0, 60), (1, 60),
                                    (516258893, 70), (436311531, 60),
                                    (952570424, 128), (4363115310, 586),
                                    (5315685734, 713)):
            with self.subTest(protected_bytes=byte_count):
                self.assertEqual(finalization_reserve(byte_count), seconds)

    def test_setup_is_counted_and_side_cannot_reset_job_clock(self):
        base = {"main_wall_seconds": 209 * 60,
                "side_wall_seconds": 60 * 60}
        # The join and evidence reserve come out of main, not extra runtime.
        main, total = phase_deadlines(
            base, self.job(), finalization_seconds=128,
            monotonic_now=5000, unix_now=103600)
        self.assertEqual(main, 5000 + 209 * 60 - 158)
        self.assertEqual(total, 5000 + 269 * 60 - 158)
        # Ten more setup minutes shorten the global cap, not the reserve.
        later_main, later_total = phase_deadlines(
            base, self.job(), finalization_seconds=128,
            monotonic_now=5600, unix_now=104200)
        self.assertEqual(later_main, 5600 + 209 * 60 - 158)
        self.assertEqual(later_total, total)
        self.assertEqual(100000 + 329 * 60 - 158,
                         104200 + later_total - 5600)
        side_start = later_main
        side_end = min(later_total, side_start + base["side_wall_seconds"])
        self.assertEqual(side_end - side_start, 50 * 60)
        self.assertEqual(base, {"main_wall_seconds": 12540,
                                "side_wall_seconds": 3600})

    def test_last_work_join_and_hashes_finish_before_wrapper_kill(self):
        base = {"main_wall_seconds": 12540, "side_wall_seconds": 3600}
        # Planned setup, a full phase sum, and a shortened absolute budget.
        for setup_minutes in (60, 0, 70):
            for reserve in (60, 128, 713):
                with self.subTest(setup=setup_minutes, reserve=reserve):
                    now = 100000 + setup_minutes * 60
                    main, total = phase_deadlines(
                        base, self.job(), finalization_seconds=reserve,
                        monotonic_now=5000, unix_now=now)
                    kill_at = (100000 + 350 * 60 - 20 * 60 - 60 + 30)
                    self.assertLess(now + total - 5000 + 30 + reserve,
                                    kill_at)
                    self.assertLessEqual(total + 30 + reserve,
                                         5000 + 269 * 60)
                    self.assertLessEqual(main + 30 + reserve,
                                         5000 + 209 * 60)

    def test_exhausted_clock_bad_reserve_and_missing_clock_refuse(self):
        base = {"main_wall_seconds": 12540, "side_wall_seconds": 3600}
        for changed in ({}, None, [], "job", 1,
                        {**self.job(), "started_unix_seconds": True},
                        {**self.job(), "started_unix_seconds": 200000},
                        {**self.job(), "timeout_seconds": 351 * 60},
                        {**self.job(), "cleanup_upload_seconds": 1199}):
            with self.subTest(job=changed), self.assertRaises(RunError):
                phase_deadlines(base, changed, finalization_seconds=128,
                                monotonic_now=1, unix_now=103600)
        for remaining in (0, 158):
            with self.subTest(remaining=remaining), self.assertRaisesRegex(
                    RunError, "exhausted"):
                phase_deadlines(
                    base, self.job(), finalization_seconds=128,
                    monotonic_now=1,
                    unix_now=100000 + 329 * 60 - remaining)
        with self.assertRaisesRegex(RunError, "cannot fit"):
            phase_deadlines(base, self.job(), finalization_seconds=12540,
                            monotonic_now=1, unix_now=100000)

    def test_policy_has_one_complete_sum_below_the_job_cap(self):
        policy = json.loads(Path(__file__).parents[1].joinpath(
            "run-policy.json").read_text())
        job = policy["jobs"]["base"]
        seconds = (policy["fetch"]["wall_seconds"]
                   + policy["tiny"]["wall_seconds"]
                   + policy["tier100"]["wall_seconds"]
                   + job["setup_seconds"]
                   + policy["base"]["main_wall_seconds"]
                   + policy["base"]["side_wall_seconds"]
                   + job["cleanup_upload_seconds"])
        reserve = 713
        main, total = phase_deadlines(
            policy["base"], self.job(), finalization_seconds=reserve,
            monotonic_now=0, unix_now=103600)
        self.assertEqual(main, 11797)
        self.assertEqual(60 * 60 + total + 30 + reserve
                         + job["cleanup_upload_seconds"], 349 * 60)
        self.assertEqual(seconds, 349 * 60)
        self.assertLess(seconds, job["timeout_minutes"] * 60)
        self.assertEqual(job["timeout_minutes"], 350)
        self.assertEqual(job["cleanup_upload_seconds"], 20 * 60)


if __name__ == "__main__":
    unittest.main()
