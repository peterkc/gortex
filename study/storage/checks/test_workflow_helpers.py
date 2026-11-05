"""Storage workflow regression tests. Run on CI, not on the Mac."""

import ast
import hashlib
import json
import os
import textwrap
from dataclasses import replace
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from workflow_helpers import (
    MeasurementHost,
    admit_dispatch,
    admit_measurement,
    base_capacity_record,
    collect_linkdelta_tiers,
    dispatch_request,
    require_idle_targets,
    unique_object,
    STEPS, BATCHES, selected_steps, measurement_jobs, base_step_spec,
    linkdelta_config_step,
)


class TierCollectionTests(unittest.TestCase):
    def test_all_tier_results_and_fixed_order(self):
        for codes in ((0, 0), (1, 0), (0, 2), (1, 2)):
            with self.subTest(codes=codes):
                invoked = []
                recorded = []
                commands = [("tier1", ["first"]), ("tier10", ["second"])]
                before = [(tier, list(command)) for tier, command in commands]

                def run(command):
                    invoked.append(command[0])
                    return codes[len(invoked) - 1]

                def idle():
                    return None

                if any(codes):
                    with self.assertRaises(RuntimeError):
                        collect_linkdelta_tiers(commands, run, idle,
                                                recorded.append)
                else:
                    collect_linkdelta_tiers(commands, run, idle,
                                            recorded.append)
                self.assertEqual(invoked, ["first", "second"])
                self.assertEqual(recorded, [
                    {"tier": "tier1", "exit_code": codes[0]},
                    {"tier": "tier10", "exit_code": codes[1]},
                ])
                self.assertEqual(commands, before)

    def test_launch_failure_stops_without_hiding_it(self):
        recorded = []
        invoked = []

        def run(command):
            invoked.append(command[0])
            raise FileNotFoundError(2, "missing test executable")

        def idle():
            self.fail("launch failure must not reach the idle check")

        with self.assertRaises(FileNotFoundError):
            collect_linkdelta_tiers(
                [("tier1", ["first"]), ("tier10", ["second"])],
                run, idle, recorded.append)
        self.assertEqual(invoked, ["first"])
        self.assertEqual(len(recorded), 1)
        self.assertEqual(recorded[0]["launch_errno"], 2)

    def test_signal_stops_without_starting_another_tier(self):
        invoked = []
        recorded = []

        def run(command):
            invoked.append(command[0])
            return -15

        def idle():
            self.fail("signal termination must stop before continuation")

        with self.assertRaises(RuntimeError):
            collect_linkdelta_tiers(
                [("tier1", ["first"]), ("tier10", ["second"])],
                run, idle, recorded.append)
        self.assertEqual(invoked, ["first"])
        self.assertEqual(recorded, [{"tier": "tier1", "exit_code": -15}])

    def test_unsafe_cleanup_stops_another_tier(self):
        invoked = []
        recorded = []

        def run(command):
            invoked.append(command[0])
            return 1

        def idle():
            raise RuntimeError("busy test target")

        with self.assertRaises(RuntimeError):
            collect_linkdelta_tiers(
                [("tier1", ["first"]), ("tier10", ["second"])],
                run, idle, recorded.append)
        self.assertEqual(invoked, ["first"])
        self.assertEqual(recorded[0]["continuation_error"], "busy test target")

    def test_real_idle_reader_and_identity_refusal(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "driver").mkdir()
            target = root / "target"
            target.mkdir()
            events = target / "cgroup.events"
            events.write_text("populated 0\nfrozen 0\n")
            stat = root.stat()
            identity = (stat.st_dev, stat.st_ino)
            require_idle_targets(root, identity)
            with self.assertRaises(RuntimeError):
                require_idle_targets(root, (stat.st_dev, stat.st_ino + 1))
            events.write_text("populated 1\nfrozen 0\n")
            with self.assertRaises(RuntimeError):
                require_idle_targets(root, identity)
            self.assertTrue(target.is_dir())


class BaseCapacityTests(unittest.TestCase):
    def tiers(self):
        return [{"bytes": 10, "nodes": 2, "edges": 9},
                {"bytes": 20, "nodes": 8, "edges": 1},
                {"bytes": 15, "nodes": 4, "edges": 5}]

    def formula(self):
        return {"serving_sets": 2, "input_multiplier": 8, "edge_bytes": 300,
                "node_bytes": 256, "reserve_bytes": 1073741824}

    def test_independent_maxima_and_exact_boundary(self):
        # Inputs already occupy disk; do not add their bytes again.
        required = 1073751640
        result = base_capacity_record(self.tiers(), self.formula(), required)
        self.assertEqual((result["S"], result["N"], result["E"]), (20, 8, 9))
        self.assertEqual(result["required_free_bytes"], required)
        self.assertEqual(result["status"], "admitted")
        blocked = base_capacity_record(
            self.tiers(), self.formula(), required - 1)
        self.assertEqual(blocked["status"], "blocked_capacity")

    def test_invalid_capacity_metadata_refuses(self):
        for value in (-1, True):
            tiers = self.tiers()
            tiers[0]["edges"] = value
            with self.assertRaises(RuntimeError):
                base_capacity_record(tiers, self.formula(), 0)
        formula = self.formula()
        formula["serving_sets"] = 0
        with self.assertRaises(RuntimeError):
            base_capacity_record(self.tiers(), formula, 0)


class AdmissionTests(unittest.TestCase):
    def test_dispatch_environment_mapping_is_exact(self):
        environ = {
            "GITHUB_EVENT_NAME": "workflow_dispatch",
            "REQUEST_APPROVAL": "batch-record", "REQUEST_COMMIT": "a" * 40,
            "REQUEST_POLICY": "b" * 64,
            "REQUEST_RELEASE": "storage-fixtures-v1",
            "GITHUB_SHA": "c" * 40, "UNRELATED": "ignored",
            "REQUEST_STEPS": "bytes", "REQUEST_C_VARIANT": "",
        }
        before = dict(environ)
        self.assertEqual(dispatch_request(environ), {
            "event": "workflow_dispatch", "approval_id": "batch-record",
            "fork_commit": "a" * 40, "policy_sha256": "b" * 64,
            "release_tag": "storage-fixtures-v1", "steps": "bytes",
            "c_variant": "",
        })
        self.assertEqual(environ, before)

    def test_dispatch_environment_requires_every_field(self):
        required = ("GITHUB_EVENT_NAME", "REQUEST_APPROVAL", "REQUEST_COMMIT",
                    "REQUEST_POLICY", "REQUEST_RELEASE", "REQUEST_STEPS",
                    "REQUEST_C_VARIANT")
        for missing in required:
            with self.subTest(missing=missing):
                environ = {key: "present" for key in required
                           if key != missing}
                environ["GITHUB_SHA"] = "c" * 40
                before = dict(environ)
                with self.assertRaises(KeyError) as raised:
                    dispatch_request(environ)
                self.assertEqual(raised.exception.args, (missing,))
                self.assertEqual(environ, before)

    def test_exact_and_push(self):
        admit_dispatch(self.request(), source="a" * 40, actual="a" * 40,
                       policy_pin="b" * 64, attempt="1")
        admit_dispatch({"event": "push"}, source="a" * 40,
                       actual="a" * 40, policy_pin="b" * 64, attempt="2")

    def request(self):
        return {"event": "workflow_dispatch", "approval_id": "batch-record",
                "fork_commit": "a" * 40, "policy_sha256": "b" * 64,
                "release_tag": "storage-fixtures-v1", "steps": "bytes",
                "c_variant": ""}

    def test_refusal_before_execution(self):
        cases = (("approval_id", ""), ("approval_id", "  "),
                 ("fork_commit", "a" * 39),
                 ("fork_commit", "c" * 40),
                 ("policy_sha256", "b" * 63),
                 ("policy_sha256", "c" * 64),
                 ("release_tag", "latest"), ("release_tag", "../other"))
        for field, value in cases:
            with self.subTest(field=field, value=value):
                request = {**self.request(), field: value}
                with self.assertRaises(RuntimeError):
                    admit_dispatch(request, source="a" * 40,
                                   actual="a" * 40, policy_pin="b" * 64,
                                   attempt="1")
        for actual, attempt in (("c" * 40, "1"), ("a" * 40, "2")):
            with self.assertRaises(RuntimeError):
                admit_dispatch(self.request(), source="a" * 40,
                               actual=actual, policy_pin="b" * 64,
                               attempt=attempt)

    def test_check5_missing_or_invalid_variant_fails_preflight(self):
        for variant in ("", "other", "PLAIN"):
            with (self.subTest(variant=variant),
                  self.assertRaises(RuntimeError)):
                admit_dispatch({**self.request(), "steps": "check5-tier1",
                                "c_variant": variant}, source="a" * 40,
                               actual="a" * 40, policy_pin="b" * 64,
                               attempt="1")
        for variant in ("plain", "side"):
            admit_dispatch({**self.request(), "steps": "check5-tier1",
                            "c_variant": variant}, source="a" * 40,
                           actual="a" * 40, policy_pin="b" * 64, attempt="1")

    def test_duplicate_policy_key(self):
        with self.assertRaises(RuntimeError):
            unique_object([("wall_seconds", 1), ("wall_seconds", 2)])

    def test_request_mapping_is_not_changed(self):
        for changes in ({}, {"approval_id": ""}):
            request = {**self.request(), **changes}
            before = dict(request)
            if changes:
                with self.assertRaises(RuntimeError):
                    admit_dispatch(request, source="a" * 40, actual="a" * 40,
                                   policy_pin="b" * 64, attempt="1")
            else:
                admit_dispatch(request, source="a" * 40, actual="a" * 40,
                               policy_pin="b" * 64, attempt="1")
            self.assertEqual(request, before)


class StepSelectionTests(unittest.TestCase):
    def test_every_single_step_and_only_its_measurement_jobs(self):
        for step in STEPS:
            with self.subTest(step=step):
                self.assertEqual(selected_steps(step), (step,))
                self.assertEqual(measurement_jobs((step,)), {
                    "base": [] if step == "check4" else [step],
                    "linkdelta": step == "check4", "vector": False,
                })

    def test_only_the_two_approved_batches_in_any_order(self):
        for batch in BATCHES:
            names = tuple(step for step in STEPS if step in batch)
            self.assertEqual(selected_steps(",".join(names)), names)
            self.assertEqual(selected_steps(", ".join(reversed(names))), names)
            jobs = measurement_jobs(names)
            self.assertEqual(jobs["base"], [
                step for step in names if step != "check4"])
            self.assertEqual(jobs["linkdelta"], "check4" in batch)
            self.assertFalse(jobs["vector"])

    def test_invalid_selections_refuse_before_measurement_jobs(self):
        invalid = ("", " ", "unknown", "check3", "check6", "bytes,check4",
                   "bytes,", "bytes,bytes", "check1-side,check4")
        for text in invalid:
            with self.subTest(text=text):
                with self.assertRaises(RuntimeError):
                    selected_steps(text)
                with self.assertRaises(RuntimeError):
                    measurement_jobs(text.split(","))

    def test_each_step_has_only_its_measurement_work(self):
        self.assertEqual(base_step_spec("bytes", ""), {
            "tiers": ["tier1"], "adapters": ["a", "b", "c", "c_side"],
            "mode": "build-only", "runs": 1})
        self.assertEqual(base_step_spec("check2", ""), {
            "tiers": ["tiny", "tier1"],
            "adapters": ["a", "b", "c", "c_side"],
            "mode": "full-protocol", "runs": 1})
        for step in ("check1-plain", "check1-side"):
            spec = base_step_spec(step, "")
            self.assertEqual(spec["tiers"], ["tier1", "tier10"])
            self.assertEqual(spec["mode"], "lookup-only")
            self.assertEqual(spec["adapters"], [
                "a", "b", "c" if step.endswith("plain") else "c_side"])
        for tier in ("tier1", "tier10", "tier100"):
            for variant in ("plain", "side"):
                spec = base_step_spec("check5-" + tier, variant)
                self.assertEqual(spec["tiers"], [tier])
                self.assertEqual(spec["mode"], "timing-only")
                self.assertEqual(spec["adapters"], [
                    "a", "b", "c" if variant == "plain" else "c_side"])
            for variant in ("", "other"):
                with self.assertRaises(RuntimeError):
                    base_step_spec("check5-" + tier, variant)

    def test_check4_sets_the_driver_step_without_check3(self):
        self.assertEqual(linkdelta_config_step("check4"), {"step": "check4"})
        for step in ("", "check3", "check6", "bytes"):
            with self.assertRaises(RuntimeError):
                linkdelta_config_step(step)


class WorkflowSourceTests(unittest.TestCase):
    def test_selected_jobs_and_embedded_python_compile(self):
        source = (Path(__file__).parents[3]
                  / ".github/workflows/storage-checks.yml").read_text()
        self.assertNotIn("\n  vector:\n", source)
        self.assertIn("step: ${{ fromJSON(needs.tests.outputs."
                      "measurement_jobs).base }}", source)
        self.assertIn("**linkdelta_config_step(os.environ[\"CHECK_STEP\"])",
                      source)
        self.assertIn("\"step\": step, \"c_variant\": variant", source)
        self.assertIn("fromJSON(needs.tests.outputs."
                      "measurement_jobs).linkdelta", source)
        blocks = []
        active = None
        for line in source.splitlines():
            if "python3.12 -B -c '" in line:
                active = []
            elif active is not None:
                if line.strip() == "'" or line.lstrip().startswith("' "):
                    blocks.append(textwrap.dedent("\n".join(active)))
                    active = None
                else:
                    active.append(line)
        self.assertEqual(len(blocks), 9)
        for block in blocks:
            ast.parse(block)

    def test_only_check2_generates_and_uses_the_tiny_fixture(self):
        source = (Path(__file__).parents[3]
                  / ".github/workflows/storage-checks.yml").read_text()
        blocks = []
        active = None
        for line in source.splitlines():
            if "python3.12 -B -c '" in line:
                active = []
            elif active is not None:
                if line.strip() == "'" or line.lstrip().startswith("' "):
                    blocks.append(ast.parse(textwrap.dedent(
                        "\n".join(active))))
                    active = None
                else:
                    active.append(line)
        preparation = next(
            node for block in blocks for node in block.body
            if isinstance(node, ast.If)
            and ast.unparse(node.test)
            == "role == 'base' and os.environ['CHECK_STEP'] == 'check2'")
        calls = []

        def generate(*args):
            calls.append(("generate", args))
            return "receipt-pin"

        def freeze(*args):
            calls.append(("freeze", args))

        common = {"source_revision": "a" * 40}
        tiny_policy = {"wall_seconds": 300}
        for step in ("check2", "check1-plain", "bytes"):
            calls.clear()
            namespace = {
                "os": os, "role": "base", "work": Path("/work"),
                "policy": {"tiny": tiny_policy}, "common": common,
                "builds": {"fixture": {"sha256": "b" * 64}},
                "launcher": Path("/work/bin/limited-command"),
                "cgroup": "test-scope", "freeze": Path("/work/freeze"),
                "generate": generate, "freeze_tier": freeze,
            }
            with patch.dict(os.environ, {"CHECK_STEP": step}):
                exec(compile(ast.Module(body=[preparation], type_ignores=[]),
                             "workflow-preparation", "exec"), namespace)
            if step != "check2":
                self.assertEqual(calls, [])
                continue
            self.assertEqual([row[0] for row in calls],
                             ["generate", "freeze"])
            args = calls[0][1]
            self.assertEqual(args[3:6], (
                "tiny", ["/work/bin/fixture", "-out", "/work/inputs/tiny",
                         "-source-revision", common["source_revision"]],
                tiny_policy))
            self.assertEqual(calls[1][1][:4], (
                Path("/work/inputs/tiny"), Path("/work/freeze/tiny"),
                "source-free tiny correctness", 1))
        directories = next(
            node for block in blocks for node in ast.walk(block)
            if isinstance(node, ast.Assign)
            and any(isinstance(target, ast.Name)
                    and target.id == "directories"
                    for target in node.targets))
        namespace = {"base_work": Path("/work"),
                     "selected": base_step_spec("check2", "")}
        exec(compile(ast.Module(body=[directories], type_ignores=[]),
                     "workflow-directories", "exec"), namespace)
        self.assertEqual(namespace["directories"],
                         {"tiny": Path("/work/inputs/tiny"),
                          "tier1": Path("/work/preparation/fetch/data/tier1")})
        self.assertIn('if [ "$CHECK_STEP" = check2 ]; then', source)
        self.assertIn('-o "$WORK/bin/fixture" ./cmd/fixture', source)


class Check4ProtocolTests(unittest.TestCase):
    def policy(self):
        return json.loads((Path(__file__).parent / "run-policy.json").read_text())

    def test_per_tier_budget_accepts_equality_rejects_one_more_second(self):
        source = (Path(__file__).parents[3]
                  / ".github/workflows/storage-checks.yml").read_text()
        start = source.index("        timeouts = {}")
        end = source.index('        with Path(os.environ["GITHUB_OUTPUT"])', start)
        budget = compile(textwrap.dedent(source[start:end]),
                         "workflow-preflight-budget", "exec")
        policy = self.policy()
        policy["linkdelta"]["wall_seconds"] = 16200
        policy["jobs"]["linkdelta"]["timeout_minutes"] = 320
        namespace = {"policy": policy}
        exec(budget, namespace)
        self.assertEqual(namespace["timeouts"]["linkdelta"], 320)
        policy["linkdelta"]["wall_seconds"] += 1
        with self.assertRaisesRegex(RuntimeError, "job budget.*linkdelta"):
            exec(budget, {"policy": policy})
        policy["linkdelta"]["wall_seconds"] = 16200
        policy["base"]["main_wall_seconds"] += 60
        with self.assertRaisesRegex(RuntimeError, "base needs a strict margin"):
            exec(budget, {"policy": policy})

    def run_preflight(self, steps):
        source = (Path(__file__).parents[3]
                  / ".github/workflows/storage-checks.yml").read_text()
        start = source.index("        python3.12 -B -c '")
        start = source.index("\n", start) + 1
        end = source.index("\n        ' 2>&1", start)
        code = compile(textwrap.dedent(source[start:end]),
                       "workflow-preflight", "exec")
        raw = (Path(__file__).parent / "run-policy.json").read_bytes()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "output"
            environment = {
                "GITHUB_WORKSPACE": str(Path(__file__).parents[3]),
                "GITHUB_SHA": "a" * 40, "GITHUB_RUN_ATTEMPT": "1",
                "GITHUB_EVENT_NAME": "workflow_dispatch",
                "GITHUB_OUTPUT": str(output), "WORK": temporary,
                "GITHUB_RUN_ID": "123", "GITHUB_ACTOR": "fixture",
                "REQUEST_APPROVAL": "fixture-only",
                "REQUEST_COMMIT": "a" * 40,
                "REQUEST_POLICY": hashlib.sha256(raw).hexdigest(),
                "REQUEST_RELEASE": "storage-fixtures-v1",
                "REQUEST_STEPS": steps, "REQUEST_C_VARIANT": "plain",
            }
            with (patch.dict(os.environ, environment),
                  patch("pathlib.Path.read_bytes", return_value=raw),
                  patch("subprocess.check_output", return_value="a" * 40 + "\n")):
                exec(code, {})
            return dict((name, json.loads(value)) for name, value in
                        (line.split("=", 1) for line in
                         output.read_text().splitlines()))

    def test_check4_only_has_two_tiers_and_two_parallel_slots(self):
        output = self.run_preflight("check4")
        self.assertEqual(output["measurement_jobs"], {
            "base": [], "linkdelta": True, "vector": False})
        self.assertEqual(output["linkdelta_jobs"], {
            "tiers": ["tier1", "tier10"], "max_parallel": 2})
        self.assertEqual(output["timeouts"]["linkdelta"], 320)

    def test_first_d12_batch_leaves_one_parallel_linkdelta_slot(self):
        output = self.run_preflight("check4,check1-plain,check2")
        self.assertEqual(output["measurement_jobs"], {
            "base": ["check2", "check1-plain"],
            "linkdelta": True, "vector": False})
        self.assertEqual(output["linkdelta_jobs"], {
            "tiers": ["tier1", "tier10"], "max_parallel": 1})
        self.assertEqual(output["timeouts"]["linkdelta"], 320)

    def test_non_check4_batches_keep_their_jobs_and_timeouts(self):
        for steps in ("bytes", "check1-side",
                      "check5-tier1,check5-tier10,check5-tier100"):
            with self.subTest(steps=steps):
                output = self.run_preflight(steps)
                self.assertEqual(output["measurement_jobs"], {
                    "base": steps.split(","), "linkdelta": False,
                    "vector": False})
                self.assertEqual(output["linkdelta_jobs"], {
                    "tiers": [], "max_parallel": 1})
                self.assertEqual(output["timeouts"], {
                    "base": 350, "linkdelta": 320, "vector": 40})

    def test_approved_policy_values_and_contract_hashes(self):
        policy = self.policy()
        self.assertEqual(policy["linkdelta"]["query_repeats"], 12)
        self.assertEqual(policy["linkdelta"]["delta_sizes"], [1, 2, 4, 8])
        self.assertEqual(policy["linkdelta"]["wall_seconds"], 16200)
        self.assertEqual(policy["jobs"]["linkdelta"], {
            "timeout_minutes": 320, "setup_seconds": 1200,
            "cleanup_upload_seconds": 1200})
        self.assertEqual(policy["contract"]["plan_sha256"],
                         "4c851d2451417303b5fe6191058c4ae3909053833d5ae820bd45409946d882dc")
        self.assertEqual(policy["contract"]["actions_sha256"],
                         "0f572f0f03b38f1dbc4b1d0ac3904324fc96c90447265893335ab7ab4458704e")

    def test_each_matrix_job_generates_only_its_tier_config(self):
        source = (Path(__file__).parents[3]
                  / ".github/workflows/storage-checks.yml").read_text()
        start = source.index('        elif role == "linkdelta":')
        end = source.index('        elif role == "vector":', start)
        branch = ast.parse(textwrap.dedent(source[start:end])
                           .replace("elif ", "if ", 1))
        loop = next(node for node in branch.body[0].body
                    if isinstance(node, ast.For))
        code = compile(ast.Module(body=[loop], type_ignores=[]),
                       "workflow-tier-config", "exec")
        spec = self.policy()["linkdelta"]
        for item in spec["tiers"]:
            with self.subTest(tier=item["tier"]):
                writes = []
                namespace = {
                    "os": os, "spec": spec, "work": Path("/work"),
                    "source": Path("/source"), "approval": Path("/approval"),
                    "common": {"source_revision": "a" * 40},
                    "sha256": lambda path: "b" * 64,
                    "write_json": lambda path, value: writes.append((path, value)),
                    "linkdelta_config_step": linkdelta_config_step,
                    "invocations": [],
                }
                with patch.dict(os.environ, {"CHECK_STEP": "check4",
                                             "CHECK_TIER": item["tier"]}):
                    exec(code, namespace)
                self.assertEqual(len(writes), 2)
                config = writes[0][1]
                self.assertEqual(config["data"],
                                 "/work/preparation/fetch/data/" + item["tier"])
                self.assertEqual(config["delta_repo"], item["delta_repo"])
                self.assertEqual(config["query_repeats"], 12)
                self.assertEqual(config["delta_sizes"], [1, 2, 4, 8])
                self.assertEqual(config["wall_seconds"], 16200)
                self.assertEqual([tier for tier, _ in namespace["invocations"]],
                                 [item["tier"]])

    def test_matrix_and_artifact_bindings_keep_base_names_unchanged(self):
        source = (Path(__file__).parents[3]
                  / ".github/workflows/storage-checks.yml").read_text()
        base = source.split("\n  base:\n", 1)[1].split("    steps:", 1)[0]
        linkdelta = source.split("\n  linkdelta:\n", 1)[1]
        self.assertIn("CHECK_ARTIFACT: ${{ matrix.step }}", base)
        self.assertIn("CHECK_TIER: ${{ matrix.tier }}", linkdelta)
        self.assertIn("CHECK_ARTIFACT: check4-${{ matrix.tier }}", linkdelta)
        self.assertIn("max-parallel: ${{ fromJSON(needs.tests.outputs."
                      "linkdelta_jobs).max_parallel }}", linkdelta)
        self.assertIn("tier: ${{ fromJSON(needs.tests.outputs."
                      "linkdelta_jobs).tiers }}", linkdelta)
        templates = [line.strip().removeprefix("name: ") for line in
                     source.splitlines() if "name: storage-checks-" in line
                     and "env.CHECK_ARTIFACT" in line]
        self.assertEqual(len(templates), 2)
        for template, prefix in zip(templates, ("storage-checks-freeze-",
                                                "storage-checks-")):
            def render(artifact):
                return (template.replace("${{ env.CHECK_ARTIFACT }}", artifact)
                        .replace("${{ github.run_id }}", "123")
                        .replace("${{ github.run_attempt }}", "1"))
            for step in STEPS:
                if step != "check4":
                    self.assertEqual(render(step), prefix + step + "-123-1")
            self.assertEqual([render("check4-" + tier) for tier in
                              ("tier1", "tier10")], [
                                  prefix + "check4-tier1-123-1",
                                  prefix + "check4-tier10-123-1"])


class MeasurementAdmissionTests(unittest.TestCase):
    def setUp(self):
        self.host = MeasurementHost(
            system="Linux", python_version=(3, 12), cpu_count=4,
            physical_bytes=16 * 1024 ** 3, private_repository=False,
            zstd_available=True)
        self.arguments = {
            "event": "workflow_dispatch", "role": "base",
            "checkout_status": b"", "host": self.host, "cpus": 4,
            "job_memory_bytes": 12 * 1024 ** 3,
            "host_reserve_bytes": 2 * 1024 ** 3, "price_cap_usd": 0,
        }

    def test_exact_public_host_and_memory_boundary(self):
        for role in ("base", "linkdelta", "vector"):
            with self.subTest(role=role):
                self.assertIsNone(admit_measurement(
                    **{**self.arguments, "role": role}))
        boundary = replace(self.host, physical_bytes=14 * 1024 ** 3)
        self.assertIsNone(admit_measurement(
            **{**self.arguments, "host": boundary}))

    def test_manual_only_clean_checkout_and_known_group(self):
        cases = (("event", "push"), ("event", "workflow_run"),
                 ("checkout_status", b" M source.py\n"),
                 ("role", "unknown"), ("role", ""))
        for field, value in cases:
            with self.subTest(field=field, value=value):
                with self.assertRaises(RuntimeError):
                    admit_measurement(**{**self.arguments, field: value})

    def test_platform_cpu_and_memory_refusals(self):
        cases = (
            ("system", replace(self.host, system="Darwin")),
            ("python", replace(self.host, python_version=(3, 11))),
            ("cpu_low", replace(self.host, cpu_count=3)),
            ("cpu_high", replace(self.host, cpu_count=5)),
            ("cpu_unknown", replace(self.host, cpu_count=None)),
            ("memory", replace(
                self.host, physical_bytes=14 * 1024 ** 3 - 1)),
        )
        for reason, host in cases:
            with self.subTest(reason=reason):
                with self.assertRaises(RuntimeError):
                    admit_measurement(**{**self.arguments, "host": host})

    def test_private_or_paid_venue_refusals(self):
        private = replace(self.host, private_repository=True)
        for changes in ({"host": private}, {"price_cap_usd": 1}):
            with self.subTest(changes=changes):
                with self.assertRaises(RuntimeError):
                    admit_measurement(**{**self.arguments, **changes})

    def test_zstd_requirement_tracks_fixture_consumers(self):
        host = replace(self.host, zstd_available=False)
        for role in ("base", "linkdelta"):
            with self.subTest(role=role):
                with self.assertRaises(RuntimeError):
                    admit_measurement(
                        **{**self.arguments, "host": host, "role": role})
        self.assertIsNone(admit_measurement(
            **{**self.arguments, "host": host, "role": "vector"}))


if __name__ == "__main__":
    unittest.main()
