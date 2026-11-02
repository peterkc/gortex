"""Given/When/Then cases for the pinned callgraph filter."""
import hashlib
import io
import json
import os
import subprocess
import sys
import tempfile
import tracemalloc
import unittest
from argparse import Namespace
from pathlib import Path
from unittest.mock import patch

import filter_callgraph as cg
import run_limited as limited


SYMBOLS = [{"symbol": "p.Seam", "why": "fixture seam"}]


def edge(caller, callee, file="caller.go", package="p", synthetic=""):
    caller_file = "seam.go" if caller == "p.Seam" else file
    callee_file = "seam.go" if callee == "p.Seam" else "caller.go"
    return "\t".join((caller, package, "/src/" + caller_file, synthetic,
                      callee, "p", "/src/" + callee_file, "", "/src/" + file,
                      "12", "static")) + "\n"


def resource_fixture():
    argv = ["/usr/local/go/bin/go", "run", cg.TOOL, *cg.OPTIONS]
    return ({
        "schema": "external-command/v1", "state": "completed",
        "removed": True, "cgroup": "/sys/fs/cgroup/fixture/analysis",
        "deadline": "2026-11-02T06:00:00Z",
        "binding": {"command": ["/bin/bash", "/script", *argv],
                    "harness_sha256": "1" * 64,
                    "source_revision": cg.COMMIT},
        "memory": {"status": "completed", "limit_bytes": limited.HARD_LIMIT,
                   "cgroup_peak_bytes": 123456789, "oom_kills": 0,
                   "exit_code": 0},
    }, {"schema": "seam-kernel-limits/v1", "memory.max": limited.HARD_LIMIT,
        "memory.swap.max": 0, "memory.oom.group": 1,
        "cgroup": "/sys/fs/cgroup/fixture/analysis",
        "gomemlimit": limited.SOFT_LIMIT, "profile": "pod",
        "file_max_bytes": limited.PROFILES["pod"]["file_max"],
        "argv": argv}, {"profile": "pod", "memtotal_bytes": 64 * 1024 ** 3,
                        "nproc": 4})


def report(text):
    graph = cg.scan_frontier(io.StringIO(text), ["p.Seam"], "/src",
                             io.StringIO())
    if isinstance(graph, cg.Invalid):
        return graph
    return cg.analyze(graph, SYMBOLS)


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
                         {"raw": 4, "frontier_unique": 3, "frontier_raw": 4,
                          "frontier_duplicates": 1, "selected": 1})
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
                     edge("p.A", "p.Seam").replace("static", "unknown"),
                     edge("p.A", "p.Seam") + "bad unrelated line\n"):
            with self.subTest(text=text):
                self.assertIsInstance(cg.scan_frontier(
                    io.StringIO(text), ["p.Seam"], "/src", io.StringIO()),
                    cg.Invalid)

    def test_resource_receipt_requires_success_and_actual_kernel_limits(self):
        launcher, kernel, host = resource_fixture()
        value = cg.resource_facts(launcher, kernel, host)
        self.assertEqual(value["memory_max_bytes"], limited.HARD_LIMIT)
        self.assertEqual(value["memory_peak_bytes"], 123456789)
        self.assertEqual(value["memory_swap_max_bytes"], 0)
        self.assertEqual(value["memory_oom_group"], 1)
        self.assertEqual(value["deadline"], "2026-11-02T06:00:00Z")
        self.assertIsInstance(cg.resource_facts({}, kernel, host), cg.Invalid)
        for field, bad in (("state", "oom"), ("removed", False),
                           ("cgroup", "/wrong")):
            with self.subTest(field=field):
                self.assertIsInstance(cg.resource_facts(
                    {**launcher, field: bad}, kernel, host), cg.Invalid)
        for field, bad in (("oom_kills", 1), ("exit_code", 2),
                           ("limit_bytes", 1), ("cgroup_peak_bytes", 0)):
            with self.subTest(field=field):
                altered = {**launcher, "memory": {
                    **launcher["memory"], field: bad}}
                self.assertIsInstance(cg.resource_facts(altered, kernel, host),
                                      cg.Invalid)
        for field, bad in (("memory.max", 1), ("memory.swap.max", 1),
                           ("memory.oom.group", 0)):
            with self.subTest(field=field):
                self.assertIsInstance(cg.resource_facts(
                    launcher, {**kernel, field: bad}, host), cg.Invalid)

    def test_options_must_match_the_bound_actual_argv(self):
        self.assertEqual(cg.OPTIONS, [
            "-algo=cha", "-test=true", "-tags=", "-cpuprofile=",
            "-memprofile=", "-format=" + cg.FORMAT, "./..."])
        launcher, kernel, host = resource_fixture()
        for bad in ("-test=false", "-algo=rta", "./internal/graph/..."):
            argv = [*kernel["argv"][:-1], bad]
            altered = {**launcher, "binding": {
                **launcher["binding"],
                "command": ["/bin/bash", "/script", *argv]}}
            self.assertIsInstance(cg.resource_facts(
                altered, {**kernel, "argv": argv}, host), cg.Invalid)
        altered_binding = {**launcher, "binding": {
            **launcher["binding"], "command": ["/bin/false"]}}
        self.assertIsInstance(cg.resource_facts(
            altered_binding, kernel, host), cg.Invalid)
        self.assertIsInstance(cg.resource_facts(
            launcher, kernel, {**host, "memtotal_bytes": 56 * 1024 ** 3 - 1}),
            cg.Invalid)

    def test_preflight_refuses_capacity_below_cap_plus_headroom(self):
        boundary = 56 * 1024 ** 3
        for total, expected in ((boundary - 1024, False), (boundary, True)):
            value = limited.host_capacity(f"MemTotal: {total // 1024} kB\n", 4)
            self.assertEqual(value["sufficient"], expected)
            self.assertEqual(value["nproc"], 4)
        with self.assertRaises(ValueError):
            limited.host_capacity("MemTotal: unknown kB\n", 4)
        with self.assertRaises(ValueError):
            limited.host_capacity("MemTotal: 16000000 kB\n", 0)

    def test_preflight_refusal_happens_before_command_launch(self):
        documents = {
            "/proc/meminfo": "MemTotal: 15000000 kB\n",
            "/proc/swaps": "Filename Type Size Used Priority\n",
            "/proc/self/cgroup": "0::/fixture\n",
        }

        def read_document(path, *args, **kwargs):
            return documents[str(path)]

        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            with patch.object(Path, "read_text", read_document), \
                    patch.object(limited.subprocess, "check_output",
                                 return_value="4\n"), \
                    patch.dict(os.environ, {"GOMEMLIMIT": limited.SOFT_LIMIT}):
                with self.assertRaisesRegex(RuntimeError, "MemTotal"):
                    limited.preflight(output)
            self.assertFalse(json.loads(
                (output / "host-capacity.json").read_text())["sufficient"])

    def test_preflight_rejects_wrong_soft_limit_after_capacity_passes(self):
        documents = {"/proc/meminfo": "MemTotal: 67108864 kB\n",
                     "/proc/swaps": "", "/proc/self/cgroup": "0::/outside"}
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(Path, "read_text",
                             lambda path: documents[str(path)]), \
                patch.object(limited.subprocess, "check_output",
                             return_value="4\n"), \
                patch.object(limited.subprocess, "run"), \
                patch.object(limited.shutil, "disk_usage",
                             return_value=Namespace(free=20 * limited.GIB)), \
                patch.dict(os.environ, {"GOMEMLIMIT": "off"}):
            with self.assertRaisesRegex(RuntimeError, "GOMEMLIMIT"):
                limited.preflight(Path(directory))

    def test_launcher_failure_is_not_converted_to_success(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            (output / "host-capacity.json").write_text(json.dumps({
                "profile": "pod", "memtotal_bytes": 64 * 1024 ** 3,
                "disk_free_bytes": 20 * limited.GIB, "nproc": 4}))
            args = Namespace(artifact=output, label="analysis",
                             launcher=Path("/bin/false"), root=output,
                             cwd=output, commit=cg.COMMIT,
                             command=["/bin/true"])
            with patch.dict(os.environ, {"GOMEMLIMIT": limited.SOFT_LIMIT}):
                self.assertEqual(limited.launch(args), 1)
            binding = json.loads(
                (output / "analysis-binding.json").read_text())
            self.assertEqual(binding["command"][-1], "/bin/true")
            self.assertEqual(binding["memory_limits_bytes"]["engine"],
                             limited.HARD_LIMIT)
            self.assertEqual(binding["harness_sha256"],
                             limited.digest(Path("/bin/false")))
            script = (output / "analysis-command.sh").read_text()
            self.assertNotIn("GITHUB_", script)
            self.assertNotIn("RUNNER_", script)
            self.assertIn("set -euo pipefail", script)
            self.assertIn('ulimit -f "$blocks"', script)
            self.assertIn('list -deps -test -tags=', script)
            self.assertIn('| cat > "$artifact/edges.tsv.partial"', script)
            self.assertLess(script.index("listed_package_count="),
                            script.index('| cat >'))

    def test_launch_refuses_bad_capacity_soft_limit_and_label(self):
        valid = {"profile": "pod", "memtotal_bytes": 64 * limited.GIB,
                 "disk_free_bytes": 20 * limited.GIB}
        cases = [({**valid, "memtotal_bytes": 1}, limited.SOFT_LIMIT,
                  "pod", "analysis"),
                 ({**valid, "disk_free_bytes": 1}, limited.SOFT_LIMIT,
                  "pod", "analysis"),
                 (valid, "off", "pod", "analysis"),
                 ({**valid, "profile": "ci-oom"}, "64MiB", "ci-oom",
                  "analysis")]
        for capacity, soft, profile, label in cases:
            with self.subTest(profile=profile, label=label, soft=soft), \
                    tempfile.TemporaryDirectory() as directory:
                artifact = Path(directory)
                (artifact / "host-capacity.json").write_text(
                    json.dumps(capacity))
                args = Namespace(artifact=artifact, profile=profile,
                                 label=label, launcher=Path("/bin/false"))
                with patch.dict(os.environ, {"GOMEMLIMIT": soft}), \
                        patch.object(limited.subprocess, "run") as launch:
                    with self.assertRaises(RuntimeError):
                        limited.launch(args)
                    launch.assert_not_called()

    def test_kernel_facts_refuse_wrong_membership_or_limits(self):
        root = Path("/sys/fs/cgroup/unit")
        baseline = {"/proc/self/cgroup": "0::/unit/gxstudy-external-x\n",
                    str(root / "gxstudy-external-x/memory.max"):
                    str(limited.HARD_LIMIT),
                    str(root / "gxstudy-external-x/memory.swap.max"): "0",
                    str(root / "gxstudy-external-x/memory.oom.group"): "1"}
        changes = [{"/proc/self/cgroup": "1:memory:/legacy\n"},
                   {"/proc/self/cgroup": "0::/wrong/gxstudy-external-x\n"},
                   {str(root / "gxstudy-external-x/memory.max"): "1"},
                   {str(root / "gxstudy-external-x/memory.swap.max"): "1"},
                   {str(root / "gxstudy-external-x/memory.oom.group"): "0"}]
        for change in changes:
            documents = {**baseline, **change}
            with tempfile.TemporaryDirectory() as directory, \
                    patch.object(Path, "read_text",
                                 lambda path: documents[str(path)]):
                with self.assertRaises(RuntimeError):
                    limited.facts(Path(directory) / "facts.json", root,
                                  ["/bin/true"])

    def test_facts_reads_back_the_actual_file_limit(self):
        root = Path("/sys/fs/cgroup/unit")
        scope = root / "gxstudy-external-x"
        documents = {"/proc/self/cgroup": "0::/unit/gxstudy-external-x\n",
                     str(scope / "memory.max"): str(limited.HARD_LIMIT),
                     str(scope / "memory.swap.max"): "0",
                     str(scope / "memory.oom.group"): "1"}
        bound = limited.PROFILES["pod"]["file_max"]
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(Path, "read_text",
                             lambda path: documents[str(path)]):
            target = Path(directory) / "facts.json"
            with patch.object(limited.resource, "getrlimit",
                              return_value=(bound, bound)):
                limited.facts(target, root, ["/bin/true"])
            with patch.object(limited.resource, "getrlimit",
                              return_value=(-1, -1)):
                with self.assertRaisesRegex(RuntimeError, "file-size"):
                    limited.facts(Path(directory) / "bad.json", root,
                                  ["/bin/true"])

    def test_cleanup_refuses_replaced_root_and_active_membership(self):
        root = Path("/sys/fs/cgroup/gxseam-unit-1")
        for tested_root, inode, membership in (
                (root, 3, "0::/outside\n"),
                (root, 2, "0::/gxseam-unit-1/owner\n"),
                (Path("/sys/fs/cgroup/gxseam-other-1"), 2, "0::/outside\n")):
            documents = {"/identity": "1 2",
                         "/proc/self/cgroup": membership}
            entry = Namespace(st_dev=1, st_ino=inode)
            with patch.object(Path, "lstat", return_value=entry), \
                    patch.object(Path, "is_dir", return_value=True), \
                    patch.object(Path, "resolve", lambda path: path), \
                    patch.object(Path, "read_text",
                                 lambda path: documents[str(path)]), \
                    patch.object(Path, "rmdir") as remove:
                with self.assertRaises(RuntimeError):
                    limited.cleanup(tested_root, Path("/identity"),
                                    "unit", "1")
                remove.assert_not_called()

    def test_profiles_and_fixture_digest_cannot_bless_production(self):
        launcher, kernel, host = resource_fixture()
        self.assertIsInstance(cg.resource_facts(
            launcher, kernel, host, "ci-fixture"), cg.Invalid)
        fixture = Path(__file__).parent / "fixture"
        expected = json.loads((fixture / "symbols.json").read_text())
        self.assertEqual(limited.fixture_digest(fixture),
                         expected["fixture_sha256"])
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory)
            for name in [*limited.FIXTURE_FILES, "symbols.json"]:
                (source / name).write_bytes((fixture / name).read_bytes())
            (source / "seam.go").write_text("changed fixture")
            self.assertNotEqual(limited.fixture_digest(source),
                                expected["fixture_sha256"])
            (source / "extra.go").write_text("package extra")
            with self.assertRaises(ValueError):
                limited.fixture_digest(source)

    def test_ci_budget_fits_job_and_has_no_full_source_analysis(self):
        workflow = Path(__file__).resolve().parents[3] / (
            ".github/workflows/seam-callgraph.yml")
        text = workflow.read_text()
        self.assertIn(f"timeout-minutes: {limited.CI_JOB_MINUTES}", text)
        self.assertIn("fromJSON(steps.budgets.outputs.json)", text)
        seconds = (limited.PROFILES["ci-fixture"]["analysis"]
                   + limited.PROFILES["ci-fixture"]["filter"]
                   + limited.PROFILES["ci-oom"]["oom"] + 120)
        self.assertLess(seconds + 10 * 60, limited.CI_JOB_MINUTES * 60)
        self.assertIn("study/storage/harness/**", text)
        self.assertNotIn("https://github.com/zzet/gortex.git", text)
        self.assertIn("--profile ci-fixture", text)
        self.assertIn("NOT FR-010/AC-011 evidence", text)
        self.assertIn("POD-RUNBOOK.md", text)

    def test_streaming_retains_only_frontier_and_bounds_memory(self):
        class EdgeStream:
            def seek(self, offset):
                if offset != 0:
                    raise AssertionError("only full streaming passes allowed")

            def __iter__(self):
                yield edge("p.A", "p.Seam")
                yield edge("p.B", "p.A")
                yield edge("p.C", "p.B")
                for index in range(50000):
                    yield edge(f"p.Other{index}", f"p.Unrelated{index}",
                               package="outside/pkg")

            def read(self, *args):
                raise AssertionError("whole-file read is forbidden")

            def readlines(self, *args):
                raise AssertionError("whole-file readlines is forbidden")

        tracemalloc.start()
        try:
            graph = cg.scan_frontier(EdgeStream(), ["p.Seam"], "/src",
                                     io.StringIO())
            _, peak = tracemalloc.get_traced_memory()
        finally:
            tracemalloc.stop()
        self.assertEqual(graph.total_edges, 50003)
        self.assertEqual(len(graph.edges), 3)
        self.assertEqual(len(graph.packages), 2)
        # Below even 50k materialized raw lines, above the tiny frontier.
        self.assertLess(peak, 4 * 1024 ** 2)

    def test_multiple_seams_keep_each_depth_three_closure(self):
        text = (edge("p.A", "p.Seam") + edge("p.Second", "p.A")
                + edge("p.B", "p.Second") + edge("p.C", "p.B")
                + edge("p.D", "p.C"))
        symbols = [*SYMBOLS, {"symbol": "p.Second", "why": "second seam"}]
        graph = cg.scan_frontier(io.StringIO(text), ["p.Seam", "p.Second"],
                                 "/src", io.StringIO())
        value = cg.analyze(graph, symbols)
        self.assertEqual([node["name"] for node in
                          value["symbols"]["p.Seam"]["depth3"]["nodes"]],
                         ["p.A", "p.B", "p.Second"])
        self.assertEqual([node["name"] for node in
                          value["symbols"]["p.Second"]["depth3"]["nodes"]],
                         ["p.B", "p.C", "p.D"])

    def test_global_unresolved_diagnostics_stream_outside_the_frontier(self):
        text = (edge("p.A", "p.Seam")
                + edge("unknown", "p.Other", package="", synthetic="thunk"))
        diagnostics = io.StringIO()
        graph = cg.scan_frontier(io.StringIO(text), ["p.Seam"], "/src",
                                 diagnostics)
        self.assertEqual(len(graph.edges), 1)
        self.assertEqual(graph.diagnostic_occurrences, 1)
        self.assertEqual(json.loads(diagnostics.getvalue())["node"]["name"],
                         "unknown")
        self.assertEqual(cg.analyze(graph, SYMBOLS)["unresolved_nodes"], [])

    def test_failure_evidence_steps_keep_always_conditions(self):
        workflow = Path(__file__).resolve().parents[3] / (
            ".github/workflows/seam-callgraph.yml")
        text = workflow.read_text()
        markers = ("name: Compress full or explicitly partial raw evidence",
                   "name: Release only the delegated seam root",
                   "name: Upload final evidence")
        for marker in markers:
            block = text.split(marker, 1)[1].split("\n      - ", 1)[0]
            self.assertIn("if: always()", block)
        self.assertNotIn("continue-on-error:", text)

    def test_artifact_manifest_rejects_changed_or_unlisted_payload(self):
        packer = Path(__file__).parent / "package_artifacts.py"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            payload = root / "payload"
            payload.mkdir()
            raw = payload / "edges.tsv"
            raw.write_text("edge bytes")
            bundle = root / "evidence.tar.gz"
            command = [sys.executable, "-B", str(packer), "--source",
                       str(payload), "--bundle", str(bundle)]
            result = subprocess.run(command, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            manifest = payload / "file-manifest.json"
            entries = json.loads(manifest.read_text())["files"]
            self.assertEqual(set(entries), {"edges.tsv"})
            self.assertEqual(entries["edges.tsv"], limited.digest(raw))
            self.assertEqual(bundle.with_suffix(".gz.sha256").read_text(),
                             limited.digest(bundle) + "  evidence.tar.gz\n")
            command = [sys.executable, "-B", str(packer), "--verify",
                       str(manifest)]
            self.assertEqual(subprocess.run(
                command, capture_output=True).returncode, 0)
            raw.write_text("changed")
            self.assertNotEqual(subprocess.run(
                command, capture_output=True).returncode, 0)
            raw.write_text("edge bytes")
            (payload / "unlisted.txt").write_text("extra")
            self.assertNotEqual(subprocess.run(
                command, capture_output=True).returncode, 0)

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
            launcher, kernel, host = resource_fixture()
            launcher_path = root / "launcher.json"
            kernel_path, host_path = root / "kernel.json", root / "host.json"
            host_path.write_text(json.dumps(host))
            launcher_path.write_text(json.dumps(launcher))
            kernel_path.write_text(json.dumps(kernel))
            package_list = root / "packages.txt"
            package_list.write_text("p\n")
            output = root / "out"
            command = [sys.executable, "-B", cg.__file__, "--edges", str(raw),
                       "--symbols", str(manifest), "--source", "/src",
                       "--output", str(output), "--go-version", "go1.27.3",
                       "--commit", cg.COMMIT, "--tree", cg.TREE,
                       "--launcher-receipt", str(launcher_path),
                       "--kernel-limits", str(kernel_path),
                       "--host-capacity", str(host_path),
                       "--package-list", str(package_list)]
            completed = subprocess.run(command, capture_output=True)
            self.assertEqual(completed.returncode, 0, completed.stderr)
            value = json.loads((output / "seam-callers.json").read_text())
            receipt = json.loads((output / "receipt.json").read_text())
            self.assertEqual(receipt["state"], "completed")
            self.assertEqual(receipt["resources"]["memory_peak_bytes"],
                             123456789)
            self.assertEqual(receipt["resources"]["memory_swap_max_bytes"], 0)
            self.assertEqual(receipt["resources"]["memory_oom_group"], 1)
            self.assertEqual(value["identity"]["algorithm"], "cha")
            self.assertEqual(receipt["package_count"], 1)
            self.assertEqual(receipt["listed_package_count"], 1)
            self.assertEqual(receipt["total_edge_count"], 1)
            self.assertEqual(receipt["frontier_edge_count"], 1)
            self.assertEqual(receipt["frontier_raw_edge_count"], 1)
            self.assertEqual(receipt["frontier_duplicate_edge_count"], 0)
            self.assertEqual(value["identity"]["raw_sha256"],
                             hashlib.sha256(raw.read_bytes()).hexdigest())
            for name, expected in receipt["outputs"].items():
                self.assertEqual(hashlib.sha256(
                    (output / name).read_bytes()).hexdigest(), expected)
            self.assertIn("`p.Seam` | 1 | 1 |",
                          (output / "summary.md").read_text())
            self.assertNotEqual(subprocess.run(
                command, capture_output=True).returncode, 0)
            self.assertFalse(
                (root / "node-diagnostics.jsonl.partial").exists())
            original_manifest = json.loads(manifest.read_text())
            for index, field in enumerate(("arg-commit", "arg-tree", "schema",
                                           "commit", "tree")):
                changed = list(command)
                changed[changed.index(str(output))] = str(root / str(index))
                altered_manifest = dict(original_manifest)
                altered_launcher = json.loads(json.dumps(launcher))
                if field == "arg-commit":
                    changed[changed.index("--commit") + 1] = "0" * 40
                    altered_launcher["binding"]["source_revision"] = "0" * 40
                elif field == "arg-tree":
                    changed[changed.index("--tree") + 1] = "0" * 40
                else:
                    altered_manifest[field] = "wrong"
                manifest.write_text(json.dumps(altered_manifest))
                launcher_path.write_text(json.dumps(altered_launcher))
                self.assertNotEqual(subprocess.run(
                    changed, capture_output=True).returncode, 0, field)
                self.assertFalse((root / str(index)).exists())
            manifest.write_text(json.dumps(original_manifest))
            launcher_path.write_text(json.dumps(launcher))
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
