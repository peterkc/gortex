from collections import Counter
from contextlib import redirect_stdout
import importlib.util
import io
from itertools import product
import json
import os
from pathlib import Path
import random
import re
import subprocess
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location(
    "ci_test_partition", Path(__file__).with_name("ci-test-partition.py"))
planner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(planner)


class PartitionTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        cwd = Path.cwd()
        self.addCleanup(os.chdir, cwd)
        os.chdir(directory.name)
        environment = mock.patch.dict(planner.os.environ)
        environment.start()
        self.addCleanup(environment.stop)
        for name in ("TEST_RACE", "TEST_WINDOWS", "TEST_SHARD_COUNT"):
            planner.os.environ.pop(name, None)

    def names(self, count=90):
        randomizer = random.Random(806)
        return sorted("Test" + "".join(randomizer.choices("ABCDEFGHIJKLMNOPQRSTUVWXYZ", k=40))
                      for _ in range(count))

    def test_oversized_windows_chunks_preserve_exact_balanced_assignment(self):
        names = self.names() + ["ExampleWindow", "FuzzWindow"]
        names.sort()
        census = ["C:\\Program Files\\Go 😀\\go.exe", "test"]
        tests = census + ["-v", "-timeout=45m"]
        package = "example.org/組織/internal/indexer"
        with mock.patch.object(planner, "WINDOWS_COMMAND_BUDGET", 650):
            assignments, chunks, bundle, _ = planner.plan_shard_commands(
                names, census, tests, package, windows_api=True)
            original, _ = planner.plan_shards(names, bundle)
            self.assertEqual(assignments, original)
            self.assertTrue(any(len(shard) > 1 for shard in chunks))
            self.assertLessEqual(max(map(len, assignments)) - min(map(len, assignments)), 1)
            for index, shard in enumerate(chunks):
                selected = [name for pattern in shard for name in names
                            if re.fullmatch(pattern, name)]
                self.assertEqual(sorted(selected), assignments[index])
                self.assertEqual(len(selected), len(set(selected)))
                for pattern in shard:
                    for prefix, flag in ((census, "-list"), (tests, "-run")):
                        self.assertLessEqual(planner.planned_command_units(
                            prefix + [flag, pattern, package]), 650)
            for name in names:
                self.assertEqual(sum(bool(re.fullmatch(pattern, name))
                                     for shard in chunks for pattern in shard), 1)

    def test_non_windows_keeps_one_command_and_original_membership(self):
        names = self.names()
        prefix = ["go", "test", "-race", "-timeout=45m", "-coverprofile=coverage.out"]
        with mock.patch.object(planner, "WINDOWS_COMMAND_BUDGET", 1):
            assignments, chunks, bundle, _ = planner.plan_shard_commands(
                names, ["go", "test", "-race"], prefix, "package", windows_api=False)
        original, patterns = planner.plan_shards(names)
        self.assertEqual(assignments, original)
        self.assertEqual(chunks, [[pattern] for pattern in patterns])
        self.assertEqual(bundle, planner.NAME_BUNDLE)

    def test_family_counts_preserve_exhaustive_windows_and_race_plans(self):
        names = self.names(91) + ["ExampleFamily", "FuzzFamily"]
        names.sort()
        self.assertEqual(planner.FALLBACK_RACE_SHARDS,
                         {"store": 6, "indexer": 12, "mcp": 3})
        self.assertEqual(planner.FALLBACK_PLAIN_SHARDS, {"indexer": 3})
        self.assertEqual(planner.FALLBACK_WINDOWS_SHARDS, {"indexer": 4})
        modes = ((True, planner.FALLBACK_RACE_SHARDS),
                 (False, planner.FALLBACK_PLAIN_SHARDS),
                 (False, planner.FALLBACK_WINDOWS_SHARDS))
        for (race, shards), windows in product(modes, (False, True)):
            for family, count in shards.items():
                with self.subTest(race=race, family=family, windows=windows):
                    census = ["go", "test"] + (["-race"] if race else [])
                    prefix = census + (["-v"] if windows else [])
                    with mock.patch.object(
                            planner, "WINDOWS_COMMAND_BUDGET", 650):
                        assignments, chunks, _, _ = (
                            planner.plan_shard_commands(
                                names, census, prefix + ["-timeout=45m"],
                                "package", windows_api=windows,
                                shard_count=count))
                    self.assertEqual(len(assignments), count)
                    self.assertEqual(
                        sorted(name for shard in assignments
                               for name in shard), names)
                    counts = [len(shard) for shard in assignments]
                    self.assertLessEqual(max(counts) - min(counts), 1)
                    for name in names:
                        self.assertEqual(
                            sum(bool(re.fullmatch(pattern, name))
                                for shard in chunks for pattern in shard),
                            1)
        self.assertEqual(planner.TEST_EXECUTION_BUDGET_SECONDS, 2700)

    def test_workflow_uses_plan_matrices_and_keeps_job_names(self):
        workflow = (Path(__file__).parents[1]
                    / ".github/workflows/ci.yml").read_text()
        for job_id, matrix in (("test", "linux"),
                               ("test-platform", "platform")):
            body = re.search(rf"^  {job_id}:\n(.*?)(?=^  [\w-]+:|\Z)",
                             workflow, re.MULTILINE | re.DOTALL).group(1)
            self.assertIn("    needs: plan", body)
            self.assertIn("fromJSON(needs.plan.outputs." + matrix + ")", body)
            self.assertIn("name: " + job_id + " (${{ matrix.os }}, "
                          "${{ matrix.go-version }}, ${{ matrix.partition }})",
                          body)
            self.assertIn("TEST_SHARD_COUNT: ${{ matrix.shard_count }}", body)
            self.assertIn("name: ci-timings-used", body)
            self.assertRegex(body, r"(?m)^    timeout-minutes: 60$")
            self.assertIn("- name: Test\n        id: test", body)
            steps = re.split(r"(?m)^      - ", body)[1:]
            timing_steps = [step for step in steps
                            if "name: Write shard timings" in step
                            or "path: ci-shard-timings/*.json" in step]
            self.assertEqual(len(timing_steps), 2)
            for step in timing_steps:
                condition = re.search(r"(?m)^        if: (.*)$", step).group(1)
                self.assertIn("steps.test.outcome == 'success'", condition)
                self.assertNotIn("!cancelled()", condition)
                self.assertIn("github.event_name", condition)
            if job_id == "test":
                setup = [step for step in steps
                         if "uses: actions/setup-go@" in step]
                self.assertEqual(len(setup), 1)
                self.assertIn("          cache: false", setup[0])
                restore = [step for step in steps
                           if "uses: actions/cache/restore@" in step]
                save = [step for step in steps
                        if "uses: actions/cache/save@" in step]
                self.assertEqual(len(restore), 1)
                self.assertEqual(len(save), 1)
                self.assertIn("matrix.partition == 'remaining'", save[0])
                self.assertIn("github.event_name", save[0])
                warm = [step for step in steps
                        if step.startswith("name: Warm race cache")]
                self.assertEqual(len(warm), 1)
                packages = " ".join("./" + path.lstrip("/")
                                    for path in planner.RACE_PACKAGES.values())
                for flags in ("-race -c", "-race -cover -c"):
                    self.assertRegex(warm[0], rf"(?m)go test {flags} -o \S+ "
                                     + re.escape(packages) + "$")
        self.assertIn("needs: [plan, test, test-platform]", workflow)
        self.assertNotIn("partition: [remaining", workflow)

    def test_committed_durations_balance_every_race_package(self):
        timings = json.loads(planner.RACE_TIMINGS_PATH.read_text())
        for family in planner.FALLBACK_RACE_SHARDS:
            package = "github.com/zzet/gortex" + planner.RACE_PACKAGES[family]
            entries = timings[package]
            names = sorted(entries)
            with self.subTest(family=family):
                assignments, predicted = planner.plan_duration_shards(
                    names, entries, planner.FALLBACK_RACE_SHARDS[family])
                self.assertEqual(len(assignments),
                                 {"store": 6, "indexer": 12, "mcp": 3}[family])
                weights = [max(0.005, seconds)
                           for seconds in entries.values()]
                self.assertAlmostEqual(sum(predicted), sum(weights))
                self.assertLessEqual(
                    max(predicted) - min(predicted), max(weights))
                self.assertEqual(
                    Counter(name for shard in assignments for name in shard),
                    Counter(names))

    def test_committed_plain_durations_balance_each_platform(self):
        timings = json.loads(planner.PLAIN_TIMINGS_PATH.read_text())
        entries = timings["github.com/zzet/gortex/internal/indexer"]
        names = sorted(entries)
        for count in (4, 3):
            with self.subTest(shards=count):
                assignments, predicted = planner.plan_duration_shards(
                    names, entries, count)
                self.assertEqual(len(assignments), count)
                weights = [max(0.005, seconds)
                           for seconds in entries.values()]
                self.assertAlmostEqual(sum(predicted), sum(weights))
                self.assertLessEqual(
                    max(predicted) - min(predicted), max(weights))
                self.assertEqual(
                    Counter(name for shard in assignments for name in shard),
                    Counter(names))

    def test_main_plain_timing_failures_warn_and_keep_count_balance(self):
        package = "example.org/gortex/internal/indexer"
        names = ["TestA", "TestB", "TestC", "TestD", "TestE", "TestF", "TestG"]
        cases = [
            ("missing", None, "missing timing file"),
            ("directory", None, "unreadable timing file"),
            ("malformed", "{", "malformed timing JSON"),
            ("package-less", "{}", "no entry for package in timing file"),
        ]
        platforms = [
            ("true", 4, [["TestA", "TestB"], ["TestC", "TestD"],
                         ["TestE", "TestF"], ["TestG"]]),
            ("false", 3, [["TestA", "TestB", "TestC"], ["TestD", "TestE"],
                          ["TestF", "TestG"]]),
        ]
        for (case, content, cause), (windows, count, shards) in product(
                cases, platforms):
            with self.subTest(case=case, windows=windows), \
                    tempfile.TemporaryDirectory() as directory:
                timing_path = Path(directory) / "timings.json"
                if case == "directory":
                    timing_path.mkdir()
                elif content is not None:
                    timing_path.write_text(content)
                path = Path(directory) / "selection.json"
                argv = ["partition", "--partition", "indexer-0",
                        "--manifest", str(path)]
                outputs = ["example.org/gortex", package, "\n".join(names)]
                outputs += ["\n".join(shard) for shard in shards]
                stdout = io.StringIO()
                with mock.patch.object(planner, "PLAIN_TIMINGS_PATH",
                                       timing_path), \
                        mock.patch.dict(planner.os.environ,
                                        {"TEST_WINDOWS": windows,
                                         "TEST_RACE": "false"}), \
                        mock.patch.object(planner, "capture",
                                          side_effect=outputs), \
                        mock.patch.object(planner.sys, "argv", argv), \
                        mock.patch.object(planner.subprocess, "run") as run, \
                        redirect_stdout(stdout):
                    planner.main()
                manifest = json.loads(path.read_text())
                self.assertEqual(manifest["assignment"], "count")
                self.assertNotIn("predicted_seconds", manifest)
                self.assertEqual(manifest["shard_count"], count)
                self.assertEqual(manifest["shards"], shards)
                counts = [len(shard) for shard in manifest["shards"]]
                self.assertLessEqual(max(counts) - min(counts), 1)
                warnings = [line for line in stdout.getvalue().splitlines()
                            if line.startswith("::warning::")]
                self.assertEqual(len(warnings), 1)
                prefix = f"::warning::{package}: {cause}"
                if case == "directory":
                    self.assertTrue(warnings[0].startswith(prefix + ": "))
                else:
                    self.assertEqual(warnings[0], prefix)
                run.assert_called_once_with(manifest["command"], check=True)

    def test_main_plain_duration_manifest_and_go_matcher(self):
        package = "example.org/gortex/internal/indexer"
        names = ["TestA", "TestB", "TestC", "TestD", "TestE"]

        def capture(command):
            if command == ["go", "list", "-m"]:
                return "example.org/gortex"
            if command == ["go", "list", "./..."]:
                return package
            pattern = command[command.index("-list") + 1]
            return "\n".join(name for name in names
                             if pattern == "." or re.fullmatch(pattern, name))

        platforms = [
            ("true", 4, "indexer-3",
             [["TestA"], ["TestD"], ["TestE"], ["TestB", "TestC"]],
             [8.12, 6, 5, 6]),
            ("false", 3, "indexer-2",
             [["TestA"], ["TestC", "TestD"], ["TestB", "TestE"]],
             [8.12, 8, 9]),
        ]
        for windows, count, partition, shards, predicted in platforms:
            with self.subTest(windows=windows), \
                    tempfile.TemporaryDirectory() as directory:
                timing_path = Path(directory) / "timings.json"
                timing_path.write_text(json.dumps({package: {
                    "TestA": 8.123, "TestB": 4, "TestC": 2, "TestD": 6,
                }}))
                path = Path(directory) / "selection.json"
                argv = ["partition", "--partition", partition,
                        "--manifest", str(path)]
                stdout = io.StringIO()
                with mock.patch.object(planner, "PLAIN_TIMINGS_PATH",
                                       timing_path), \
                        mock.patch.dict(planner.os.environ,
                                        {"TEST_WINDOWS": windows,
                                         "TEST_RACE": "false"}), \
                        mock.patch.object(planner, "capture",
                                          side_effect=capture) as census, \
                        mock.patch.object(planner.sys, "argv", argv), \
                        mock.patch.object(planner.subprocess, "run") as run, \
                        redirect_stdout(stdout):
                    planner.main()
                manifest = json.loads(path.read_text())
                self.assertEqual(manifest["assignment"], "duration")
                self.assertEqual(manifest["shard_count"], count)
                self.assertEqual(manifest["shards"], shards)
                self.assertEqual(manifest["predicted_seconds"], predicted)
                self.assertEqual(census.call_count, 3 + count)
                self.assertNotIn("::warning::", stdout.getvalue())
                run.assert_called_once_with(manifest["command"], check=True)

    def test_parse_pass_durations_ignores_subtests_and_non_pass_results(self):
        output = """2026-10-20T01:00:00.000Z === RUN   TestA
2026-10-20T01:00:01.000Z     --- PASS: TestA/child (8.00s)
2026-10-20T01:00:01.000Z --- PASS: TestA (1.25s)
2026-10-20T01:00:02.000Z --- SKIP: TestSkip (0.00s)
2026-10-20T01:00:02.000Z --- FAIL: TestFail (9.00s)
2026-10-20T01:00:02.000Z --- PASS: ExampleA (0.00s)
2026-10-20T01:00:02.000Z --- PASS: FuzzA (0.12s)
2026-10-20T01:00:02.000Z ok  \texample.org/a\t1.5s
--- PASS: TestB (2.50s)
ok  \texample.org/b\t2.5s
--- PASS: TestUnfinished (3.00s)
"""
        self.assertEqual(planner.parse_pass_durations(output), {
            "example.org/a": {"TestA": 1.25, "ExampleA": 0.0, "FuzzA": 0.12},
            "example.org/b": {"TestB": 2.5},
        })

    def test_main_verbose_only_for_race_timing_shards(self):
        package = "example.org/gortex/internal/mcp"
        names = ["TestA", "TestB", "TestC"]
        outputs = ["example.org/gortex", package, "\n".join(names)] + names
        with tempfile.TemporaryDirectory() as directory:
            argv = ["partition", "--partition", "mcp-0", "--dry-run",
                    "--manifest", str(Path(directory) / "selection.json")]
            with mock.patch.object(planner.sys, "argv", argv), \
                    mock.patch.object(planner, "capture",
                                      side_effect=outputs), \
                    mock.patch.object(planner, "read_race_timings",
                                      return_value="missing timing file"):
                planner.main()
            command = json.loads((Path(directory)
                                  / "selection.json").read_text())["command"]
            self.assertIn("-v", command)
            self.assertIn("-race", command)

    def test_extra_timing_names_do_not_enter_the_compiled_census(self):
        names = ["TestA", "TestB", "TestNew"]
        timings = {"TestA": 1.0, "TestB": 2.0, "TestRemoved": 3.0}
        assignments, predicted = planner.plan_duration_shards(
            names, timings, 2)
        self.assertEqual(
            sorted(name for shard in assignments for name in shard), names)
        self.assertEqual(sum(predicted), 5.0)

    def test_zero_and_tiny_timings_spread_tests_evenly(self):
        names = [f"Test{index:03}" for index in range(31)]
        for seconds in (0.0, 0.00001):
            with self.subTest(seconds=seconds):
                timings = {name: seconds for name in names}
                assignments, predicted = planner.plan_duration_shards(
                    names, timings, 3)
                counts = [len(shard) for shard in assignments]
                self.assertLessEqual(max(counts) - min(counts), 1)
                self.assertEqual(sorted(name for shard in assignments
                                        for name in shard), names)
                self.assertAlmostEqual(sum(predicted), 31 * 0.005)

    def test_posix_run_pattern_budget_counts_bytes_and_allows_boundary(self):
        with mock.patch.object(planner.os, "name", "posix"):
            planner.guard_command(["go", "test", "-run", "A" * (120 * 1024)])
            for pattern in ("A" * (120 * 1024 + 1), "組" * 42000):
                with self.subTest(bytes=len(pattern.encode("utf-8"))):
                    with self.assertRaisesRegex(ValueError,
                                                "exceeds 120 KiB budget"):
                        planner.guard_command(["go", "test", "-run", pattern])

    def test_main_rejects_oversized_pattern_before_matcher_or_execution(self):
        package = "example.org/gortex/internal/mcp"
        names = ["TestA", "TestB", "TestC"]
        outputs = ["example.org/gortex", package, "\n".join(names)]
        plan = ([[name] for name in names],
                [["A" * (120 * 1024 + 1)], ["TestB"], ["TestC"]],
                None, [1.0, 1.0, 1.0])
        argv = ["partition", "--partition", "mcp-0"]
        with mock.patch.object(planner.sys, "argv", argv), \
                mock.patch.object(planner, "capture",
                                  side_effect=outputs) as capture, \
                mock.patch.object(planner, "read_race_timings", return_value={
                    name: 1.0 for name in names}), \
                mock.patch.object(planner, "plan_shard_commands",
                                  return_value=plan), \
                mock.patch.object(planner.subprocess, "run") as run:
            with self.assertRaisesRegex(ValueError, "exceeds 120 KiB budget"):
                planner.main()
        self.assertEqual(capture.call_count, 3)
        run.assert_not_called()

    def test_parser_resets_failed_package_and_preserves_indentation(self):
        output = (
            "2026-10-20T01:00:00Z --- PASS: TestFailedPackage (9.00s)\r\n"
            "2026-10-20T01:00:00Z FAIL\texample.org/failed\t9.0s\r\n"
            "2026-10-20T01:00:00Z --- PASS: TestGood (1.25s)\r\n"
            "2026-10-20T01:00:00Z     --- PASS: TestIndented (7.00s)\r\n"
            "2026-10-20T01:00:00Z     ok  \texample.org/other\t7.0s\r\n"
            "2026-10-20T01:00:00Z ok  \texample.org/good\t1.3s\r\n")
        self.assertEqual(planner.parse_pass_durations(output), {
            "example.org/good": {"TestGood": 1.25},
        })

    def committed_plan(self):
        module = "github.com/zzet/gortex"
        race = {family: planner.read_race_timings(
            planner.RACE_TIMINGS_PATH, module + planner.RACE_PACKAGES[family])
                for family in planner.FALLBACK_RACE_SHARDS}
        plain = {"indexer": planner.read_race_timings(
            planner.PLAIN_TIMINGS_PATH, module + "/internal/indexer")}
        return planner.build_plan(race, plain, "1.27")

    def test_committed_timings_produce_ten_minute_layout(self):
        plan = self.committed_plan()
        expected = {
            "ubuntu-latest": {"store": 3, "indexer": 2, "mcp": 2},
            "windows-latest": {"indexer": 2},
            "macos-latest": {"indexer": 2},
        }
        rows = plan["linux"]["include"] + plan["platform"]["include"]
        for platform, counts in expected.items():
            selected = [row for row in rows if row["os"] == platform]
            realized = {family: sum(row["partition"].startswith(family + "-")
                                    for row in selected) for family in counts}
            self.assertEqual(realized, counts)
            self.assertEqual(len(selected), sum(counts.values())
                             + (1 if platform == "ubuntu-latest" else 2))
            self.assertTrue(all(row["go-version"] == "1.27"
                                for row in selected))
            self.assertEqual(len({row["partition"] for row in selected}),
                             len(selected))
        self.assertEqual(json.loads(json.dumps(plan)), plan)

    def test_plan_caps_shards_and_mac_jobs_and_keeps_one_minimum(self):
        race = {family: {"TestSlow": 10 ** 9}
                for family in planner.FALLBACK_RACE_SHARDS}
        plan = planner.build_plan(race, {"indexer": {"TestSlow": 10 ** 9}},
                                  "1.27")
        self.assertEqual(max(row["shard_count"]
                             for row in plan["linux"]["include"]),
                         planner.MAX_SHARDS)
        self.assertEqual(sum(row["os"] == "macos-latest"
                             for row in plan["platform"]["include"]), 5)
        counts = planner.family_counts({"indexer": {"TestZero": 0.0}},
                                       {"indexer": 4}, planner.TARGET_SECONDS)
        self.assertEqual(counts, {"indexer": 1})

    def test_plan_warns_and_uses_count_fallback_for_missing_or_bad_files(self):
        for content in (None, "{", "[]", '{"wrong": {}}'):
            with self.subTest(content=content), \
                    tempfile.TemporaryDirectory() as directory:
                base = Path(directory)
                source = base / "race.json"
                if content is not None:
                    source.write_text(content)
                stdout = io.StringIO()
                with mock.patch.object(planner, "RACE_TIMINGS_PATH", source), \
                        mock.patch.object(planner, "PLAIN_TIMINGS_PATH",
                                          base / "plain.json"), \
                        redirect_stdout(stdout):
                    planner.write_plan(base / "used", "example.org/gortex",
                                       "1.27", base / "outputs")
                plan = json.loads((base / "used/plan.json").read_text())
                self.assertEqual(len(plan["linux"]["include"]), 22)
                self.assertEqual(len(plan["platform"]["include"]), 11)
                self.assertIn("::warning::", stdout.getvalue())
                self.assertIn("count split", stdout.getvalue())

    def test_plan_uses_exact_cached_files_and_outputs_matrices(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            used = base / "used"
            used.mkdir()
            module = "example.org/gortex"
            race = {module + suffix: {"TestA": 2.0}
                    for suffix in planner.RACE_PACKAGES.values()}
            plain = {module + "/internal/indexer": {"TestB": 3.0}}
            for filename, timings in (("ci-race-timings.json", race),
                                      ("ci-plain-timings.json", plain)):
                (used / filename).write_text(json.dumps(timings))
            before = (used / "ci-race-timings.json").read_bytes()
            with redirect_stdout(io.StringIO()):
                planner.write_plan(used, module, "1.27", base / "outputs")
            outputs = dict(line.split("=", 1) for line in
                           (base / "outputs").read_text().splitlines())
            self.assertEqual(set(outputs), {"linux", "platform"})
            self.assertEqual(len(json.loads(outputs["linux"])["include"]), 4)
            self.assertEqual((used / "ci-race-timings.json").read_bytes(),
                             before)

    def test_partition_count_override_and_index_validation(self):
        self.assertEqual(planner.partition_shards(
            "indexer-13", {"indexer": 3}, 14), {"indexer": 14})
        for partition, count in (("indexer-3", 3), ("indexer--1", 3),
                                 ("indexer-0", 0), ("indexer-0", 17),
                                 ("remaining", 1)):
            with self.subTest(partition=partition, count=count):
                with self.assertRaises(ValueError):
                    planner.partition_shards(partition, {"indexer": 3}, count)

    def test_main_uses_supplied_shard_count_before_any_go_execution(self):
        package = "example.org/gortex/internal/mcp"
        names = ["TestA", "TestB", "TestC", "TestD"]
        outputs = ["example.org/gortex", package, "\n".join(names)] + names
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "selection.json"
            argv = ["partition", "--partition", "mcp-3", "--shard-count", "4",
                    "--manifest", str(path), "--dry-run"]
            with mock.patch.object(planner.sys, "argv", argv), \
                    mock.patch.object(planner, "capture",
                                      side_effect=outputs), \
                    mock.patch.object(planner, "read_race_timings",
                                      return_value=dict.fromkeys(names, 1.0)):
                planner.main()
            self.assertEqual(json.loads(path.read_text())["shard_count"], 4)
            argv[5] = "3"
            with mock.patch.object(planner.sys, "argv", argv), \
                    mock.patch.object(planner, "capture") as capture, \
                    mock.patch.object(planner.sys, "stderr"):
                with self.assertRaises(SystemExit):
                    planner.main()
            capture.assert_not_called()

    def test_timing_merge_averages_and_drops_only_for_complete_families(self):
        old = {"TestA": 2.0, "TestGone": 9.0}
        delivered = {"store-0": {"TestA": 4.02, "TestNew": 1.25}}
        expected = {"store-0", "store-1"}
        partial = planner.merge_family_timings(old, delivered, expected)
        self.assertEqual(partial, {"TestA": 3.01, "TestGone": 9.0,
                                   "TestNew": 1.25})
        complete = {**delivered, "store-1": {"TestOther": 0.0}}
        result = planner.merge_family_timings(old, complete, expected)
        self.assertEqual(result, {"TestA": 3.01, "TestNew": 1.25,
                                  "TestOther": 0.0})
        self.assertEqual(result, planner.merge_family_timings(
            old, dict(reversed(list(complete.items()))), expected))
        self.assertEqual(old, {"TestA": 2.0, "TestGone": 9.0})

    def test_merge_uses_counts_from_this_runs_plan(self):
        plan = planner.build_plan(
            {family: {"TestA": 1.0}
             for family in planner.FALLBACK_RACE_SHARDS},
            {"indexer": {"TestA": 1.0}}, "1.27")
        module = "example.org/gortex"
        package = module + "/internal/indexer"
        old = {"race": {}, "plain": {package: {"TestA": 2.0, "TestOld": 3.0}}}
        record = {"os": "windows-latest", "partition": "indexer-0",
                  "shard_count": 4, "package": package,
                  "timings": {"TestA": 6.0}}
        merged = planner.merge_run_timings(plan, old, [record], module)
        self.assertEqual(merged["plain"][package], old["plain"][package])
        record = {**record, "shard_count": 1}
        merged = planner.merge_run_timings(plan, old, [record], module)
        self.assertEqual(merged["plain"][package], {"TestA": 4.0})

    def test_merge_writes_json_and_preserves_missing_families(self):
        module = "example.org/gortex"
        race = {family: {"TestA": 1.0}
                for family in planner.FALLBACK_RACE_SHARDS}
        plan = planner.build_plan(race, {"indexer": {"TestA": 1.0}}, "1.27")
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            used, shards = base / "used", base / "shards"
            used.mkdir()
            shards.mkdir()
            (used / "plan.json").write_text(json.dumps(plan))
            old = {"TestA": 2.0, "TestGone": 9.0}
            for source, paths in ((planner.RACE_TIMINGS_PATH,
                                   planner.RACE_PACKAGES),
                                  (planner.PLAIN_TIMINGS_PATH,
                                   {"indexer": "/internal/indexer"})):
                timings = {module + suffix: old for suffix in paths.values()}
                (used / source.name).write_text(json.dumps(timings))
            record = {"os": "ubuntu-latest", "partition": "store-0",
                      "shard_count": 1,
                      "package": module + "/internal/graph/store_sqlite",
                      "timings": {"TestA": 4.0}}
            (shards / "store.json").write_text(json.dumps(record))
            planner.merge_timings(used, shards, module)
            text = (used / "ci-race-timings.json").read_text()
            merged = json.loads(text)
            self.assertEqual(merged[record["package"]], {"TestA": 3.0})
            self.assertEqual(merged[module + "/internal/indexer"], old)
            self.assertEqual(merged[module + "/internal/mcp"], old)
            self.assertEqual(text, json.dumps(merged, indent=2, sort_keys=True)
                             + "\n")

    def test_per_shard_writer_records_pass_results_without_go(self):
        Path("go.mod").write_text("module github.com/zzet/gortex\ngo 1.27.0\n")
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            log = base / "test.log"
            log.write_bytes(
                b"--- PASS: TestA (1.25s)\r\n"
                b"    --- PASS: TestA/child (0.25s)\r\n"
                b"ok  \tgithub.com/zzet/gortex/internal/mcp\t1.5s\r\n")
            argv = ["partition", "--write-timings", str(log),
                    "--partition", "mcp-1", "--shard-count", "2",
                    "--shard-timings-dir", str(base / "shards")]
            with mock.patch.object(planner.sys, "argv", argv), \
                    mock.patch.dict(planner.os.environ,
                                    {"TEST_OS": "ubuntu-latest"}), \
                    mock.patch.object(planner, "capture") as capture:
                planner.main()
            path = base / "shards/ubuntu-latest-mcp-1.json"
            record = json.loads(path.read_text())
            self.assertEqual(record["timings"], {"TestA": 1.25})
            self.assertEqual(record["shard_count"], 2)
            capture.assert_not_called()
            with self.assertRaisesRegex(ValueError, "no completed PASS"):
                planner.shard_timing_record("FAIL\tpkg\t1s", "ubuntu-latest",
                                            "mcp-1", 2,
                                            "github.com/zzet/gortex")

    def test_timing_cache_attempt_keys_and_artifact_lifecycle(self):
        workflow = (Path(__file__).parents[1]
                    / ".github/workflows/ci.yml").read_text()
        key = "key: ci-timings-${{ github.run_id }}-${{ github.run_attempt }}"
        self.assertEqual(workflow.count(key), 2)
        self.assertIn("restore-keys: ci-timings-", workflow)
        uploads = [step for step in re.split(r"(?m)^      - ", workflow)[1:]
                   if step.startswith("uses: actions/upload-artifact@")]
        self.assertEqual(len(uploads), 4)
        for step in uploads:
            self.assertIn("overwrite: true", step)
            retention = (7 if "path: ci-shard-timings/*.json" in step else 30)
            self.assertIn(f"retention-days: {retention}", step)
        self.assertNotIn("matrix.go-version == '1.27'", workflow)

    def test_main_prefers_cached_timings_and_falls_back_per_family(self):
        module = "example.org/gortex"
        package = module + "/internal/mcp"
        names = ["TestA", "TestB", "TestC", "TestD"]
        fallback = {"TestA": 100.0, "TestB": 10.0, "TestC": 10.0,
                    "TestD": 10.0}
        cached = {"TestA": 9.0, "TestB": 2.0, "TestC": 2.0, "TestD": 2.0}

        def capture(command):
            if command == ["go", "list", "-m"]:
                return module
            if command == ["go", "list", "./..."]:
                return package
            pattern = command[command.index("-list") + 1]
            return "\n".join(name for name in names
                             if pattern == "." or re.fullmatch(pattern, name))

        for missing in (False, True):
            with self.subTest(missing=missing), \
                    tempfile.TemporaryDirectory() as directory:
                base = Path(directory)
                used = base / "used"
                used.mkdir()
                source = base / "race.json"
                source.write_text(json.dumps({package: fallback}))
                (used / source.name).write_text(json.dumps(
                    {} if missing else {package: cached}))
                manifest = base / "selection.json"
                argv = ["partition", "--partition", "mcp-0", "--shard-count",
                        "2", "--timings-dir", str(used), "--dry-run",
                        "--manifest", str(manifest)]
                with mock.patch.object(planner.sys, "argv", argv), \
                        mock.patch.object(planner, "RACE_TIMINGS_PATH",
                                          source), \
                        mock.patch.object(planner, "capture",
                                          side_effect=capture), \
                        redirect_stdout(io.StringIO()):
                    planner.main()
                result = json.loads(manifest.read_text())
                self.assertEqual(result["assignment"], "duration")
                self.assertEqual(sum(result["predicted_seconds"]),
                                 130.0 if missing else 15.0)

    def test_main_plan_materializes_missing_cached_families(self):
        module = "example.org/gortex"
        Path("go.mod").write_text(f"module {module}\ngo 1.28.2\n")
        base = Path.cwd()
        source = base / "fallback"
        source.mkdir()
        used = base / "used"
        used.mkdir()
        race = {module + suffix: {"TestOld": 20.0}
                for suffix in planner.RACE_PACKAGES.values()}
        plain = {module + "/internal/indexer": {"TestPlain": 40.0}}
        race_path, plain_path = source / "race.json", source / "plain.json"
        race_path.write_text(json.dumps(race))
        plain_path.write_text(json.dumps(plain))
        store = module + "/internal/graph/store_sqlite"
        (used / race_path.name).write_text(json.dumps({
            store: {"TestCached": 30000.0}}))
        outputs = base / "outputs"
        argv = ["partition", "--plan", "--timings-dir", str(used)]
        with mock.patch.object(planner.sys, "argv", argv), \
                mock.patch.dict(planner.os.environ,
                                {"GITHUB_OUTPUT": str(outputs)}), \
                mock.patch.object(planner, "RACE_TIMINGS_PATH", race_path), \
                mock.patch.object(planner, "PLAIN_TIMINGS_PATH", plain_path), \
                mock.patch.object(planner, "capture") as capture, \
                redirect_stdout(io.StringIO()):
            planner.main()
        values = dict(line.split("=", 1)
                      for line in outputs.read_text().splitlines())
        matrix = json.loads(values["linux"])["include"]
        self.assertTrue(all(row["go-version"] == "1.28" for row in matrix))
        self.assertEqual(sum(row["partition"].startswith("store-")
                             for row in matrix), 16)
        materialized = json.loads((used / race_path.name).read_text())
        self.assertEqual(materialized[store], {"TestCached": 30000.0})
        self.assertEqual(materialized[module + "/internal/mcp"],
                         {"TestOld": 20.0})
        self.assertEqual(set(materialized), set(race))
        capture.assert_not_called()

    def test_incomplete_new_family_is_omitted_not_saved_as_partial_data(self):
        self.assertEqual(planner.merge_family_timings(
            {}, {"indexer-0": {"TestA": 2.0}},
            {"indexer-0", "indexer-1"}), {})
        module = "example.org/gortex"
        plan = planner.build_plan(
            {family: {"TestA": 1.0}
             for family in planner.FALLBACK_RACE_SHARDS},
            {"indexer": {"TestA": 2000.0}}, "1.27")
        record = {"os": "windows-latest", "partition": "indexer-0",
                  "shard_count": 2, "package": module + "/internal/indexer",
                  "timings": {"TestA": 2.0}}
        result = planner.merge_run_timings(
            plan, {"race": {}, "plain": {}}, [record], module)
        self.assertEqual(result, {"race": {}, "plain": {}})

    def test_main_merge_with_missing_or_empty_shard_directory(self):
        module = "example.org/gortex"
        Path("go.mod").write_text(f"module {module}\ngo 1.27.0\n")
        for present in (False, True):
            with self.subTest(present=present), \
                    tempfile.TemporaryDirectory() as directory:
                base = Path(directory)
                used = base / "used"
                used.mkdir()
                shards = base / "shards"
                if present:
                    shards.mkdir()
                race = {module + suffix: {"TestA": 2.0}
                        for suffix in planner.RACE_PACKAGES.values()}
                plain = {module + "/internal/indexer": {"TestA": 2.0}}
                (used / "ci-race-timings.json").write_text(json.dumps(race))
                (used / "ci-plain-timings.json").write_text(json.dumps(plain))
                plan = planner.build_plan(
                    {family: {"TestA": 2.0}
                     for family in planner.FALLBACK_RACE_SHARDS},
                    {"indexer": {"TestA": 2.0}}, "1.27")
                (used / "plan.json").write_text(json.dumps(plan))
                argv = ["partition", "--merge-timings", "--timings-dir",
                        str(used), "--shard-timings-dir", str(shards)]
                with mock.patch.object(planner.sys, "argv", argv), \
                        mock.patch.object(planner, "capture") as capture:
                    planner.main()
                self.assertEqual(json.loads(
                    (used / "ci-race-timings.json").read_text()), race)
                self.assertEqual(json.loads(
                    (used / "ci-plain-timings.json").read_text()), plain)
                capture.assert_not_called()

    def test_unknown_names_use_package_median_duration(self):
        timings = {"TestA": 8, "TestB": 4, "TestC": 2, "TestD": 6}
        names = ["TestA", "TestB", "TestC", "TestD", "TestE", "TestF"]
        assignments, predicted = planner.plan_duration_shards(
            names, timings, 3)
        self.assertEqual(assignments, [
            ["TestA", "TestC"], ["TestB", "TestD"], ["TestE", "TestF"],
        ])
        self.assertEqual(predicted, [10, 10, 10])
        self.assertEqual(timings,
                         {"TestA": 8, "TestB": 4, "TestC": 2, "TestD": 6})
        self.assertEqual(
            names, ["TestA", "TestB", "TestC", "TestD", "TestE", "TestF"])

    def test_duration_shards_keep_windows_command_budget(self):
        names = self.names() + ["ExampleDuration", "FuzzDuration"]
        names.sort()
        timings = {name: index + 1 for index, name in enumerate(names)}
        census = ["go", "test", "-race"]
        prefix = census + ["-timeout=45m", "-coverprofile=coverage.out"]
        with mock.patch.object(planner, "WINDOWS_COMMAND_BUDGET", 650):
            assignments, chunks, bundle, predicted = (
                planner.plan_shard_commands(
                    names, census, prefix, "package", windows_api=True,
                    shard_count=3, timings=timings))
            self.assertIsNone(bundle)
            self.assertEqual(predicted, [1427, 1426, 1425])
            self.assertTrue(any(len(shard) > 1 for shard in chunks))
            self.assertEqual(
                Counter(name for shard in assignments for name in shard),
                Counter(names))
            for index, shard in enumerate(chunks):
                matched = [name for pattern in shard for name in names
                           if re.fullmatch(pattern, name)]
                self.assertEqual(sorted(matched), assignments[index])
                for pattern in shard:
                    for command, flag in ((census, "-list"), (prefix, "-run")):
                        self.assertLessEqual(planner.planned_command_units(
                            command + [flag, pattern, "package"]), 650)

    def test_timing_errors_in_another_package_do_not_affect_this_one(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "timings.json"
            path.write_text(json.dumps({
                "example.org/a": {"TestA": "slow"},
                "example.org/b": {"TestB": 1.5},
            }))
            self.assertEqual(planner.read_race_timings(path, "example.org/b"),
                             {"TestB": 1.5})
            self.assertIn("wrong timing shape",
                          planner.read_race_timings(path, "example.org/a"))

    def test_main_race_timing_failures_warn_and_keep_count_balance(self):
        package = "example.org/gortex/internal/mcp"
        names = ["TestA", "TestB", "TestC", "TestD", "TestE", "TestF", "TestG"]
        cases = [
            ("missing", None, "missing timing file"),
            ("directory", None, "unreadable timing file"),
            ("malformed", "{", "malformed timing JSON"),
            ("top-level-list", "[]", "wrong timing shape"),
            ("package-list", json.dumps({package: []}), "wrong timing shape"),
            ("empty-package", json.dumps({package: {}}), "wrong timing shape"),
            ("package-less", "{}", "no entry for package"),
        ]
        for seconds in ("slow", -1, float("nan"), float("inf"), True,
                        10 ** 400):
            cases.append((str(seconds), json.dumps({
                package: {"TestA": seconds}}), "wrong timing shape"))
        for case, content, cause in cases:
            with self.subTest(case=case), \
                    tempfile.TemporaryDirectory() as directory:
                timing_path = Path(directory) / "timings.json"
                if case == "directory":
                    timing_path.mkdir()
                elif content is not None:
                    timing_path.write_text(content)
                path = Path(directory) / "selection.json"
                argv = ["partition", "--partition", "mcp-0",
                        "--manifest", str(path)]
                outputs = [
                    "example.org/gortex", package, "\n".join(names),
                    "TestA\nTestB\nTestC", "TestD\nTestE", "TestF\nTestG",
                ]
                stdout = io.StringIO()
                with mock.patch.object(planner, "RACE_TIMINGS_PATH",
                                       timing_path), \
                        mock.patch.object(planner, "capture",
                                          side_effect=outputs), \
                        mock.patch.object(planner.sys, "argv", argv), \
                        mock.patch.object(planner.subprocess, "run") as run, \
                        redirect_stdout(stdout):
                    planner.main()
                manifest = json.loads(path.read_text())
                self.assertEqual(manifest["assignment"], "count")
                self.assertNotIn("predicted_seconds", manifest)
                self.assertEqual(manifest["shard_count"], 3)
                self.assertEqual(manifest["shards"], [
                    ["TestA", "TestB", "TestC"], ["TestD", "TestE"],
                    ["TestF", "TestG"],
                ])
                counts = [len(shard) for shard in manifest["shards"]]
                self.assertLessEqual(max(counts) - min(counts), 1)
                warnings = [line for line in stdout.getvalue().splitlines()
                            if line.startswith("::warning::")]
                self.assertEqual(len(warnings), 1)
                self.assertIn(cause, warnings[0])
                self.assertIn(package, warnings[0])
                run.assert_called_once_with(manifest["command"], check=True)

    def test_main_race_duration_manifest_and_go_matcher(self):
        package = "example.org/gortex/internal/mcp"
        names = ["TestA", "TestB", "TestC", "TestD", "TestE", "TestF"]

        def capture(command):
            if command == ["go", "list", "-m"]:
                return "example.org/gortex"
            if command == ["go", "list", "./..."]:
                return package
            pattern = command[command.index("-list") + 1]
            return "\n".join(name for name in names
                             if pattern == "." or re.fullmatch(pattern, name))

        with tempfile.TemporaryDirectory() as directory:
            timing_path = Path(directory) / "timings.json"
            timing_path.write_text(json.dumps({package: {
                "TestA": 8.123, "TestB": 4, "TestC": 2, "TestD": 6,
            }}))
            path = Path(directory) / "selection.json"
            argv = ["partition", "--partition", "mcp-0",
                    "--manifest", str(path)]
            stdout = io.StringIO()
            with mock.patch.object(planner, "RACE_TIMINGS_PATH",
                                   timing_path), \
                    mock.patch.object(planner, "capture",
                                      side_effect=capture) as census, \
                    mock.patch.object(planner.sys, "argv", argv), \
                    mock.patch.object(planner.subprocess, "run") as run, \
                    redirect_stdout(stdout):
                planner.main()
            manifest = json.loads(path.read_text())
            self.assertEqual(manifest["assignment"], "duration")
            self.assertEqual(manifest["predicted_seconds"], [10.12, 10, 10])
            self.assertEqual(manifest["shards"], [
                ["TestA", "TestC"], ["TestB", "TestD"], ["TestE", "TestF"],
            ])
            self.assertEqual(census.call_count, 6)
            self.assertNotIn("::warning::", stdout.getvalue())
            run.assert_called_once_with(manifest["command"], check=True)

    def test_single_test_overflow_fails_without_dropping_it(self):
        with mock.patch.object(planner, "WINDOWS_COMMAND_BUDGET", 10):
            with self.assertRaisesRegex(ValueError, "one compiled test cannot fit"):
                planner.bounded_patterns(["TestOne"], ["go", "test"], ["go", "test"], "package")

    def test_units_count_quoted_utf16_not_characters(self):
        command = ["C:\\Go 😀\\go.exe", "test", "組織"]
        serialized = subprocess.list2cmdline(command)
        self.assertEqual(planner.command_units(command), len(serialized.encode("utf-16-le")) // 2)
        self.assertGreater(planner.command_units(command), len(serialized))

    def test_execution_chunks_share_deadline_and_record_effective_commands(self):
        commands = [["go", "test", "-timeout=45m", "-run", pattern, "package"]
                    for pattern in ("TestA", "TestB")]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "selection.json"
            manifest = {"commands": commands}
            with mock.patch.object(planner.time, "monotonic", side_effect=[100, 100, 700]), \
                    mock.patch.object(planner.subprocess, "run") as run:
                planner.run_commands(commands, manifest, path)
            self.assertEqual(run.call_args_list[0].kwargs["timeout"], 2700)
            self.assertEqual(run.call_args_list[1].kwargs["timeout"], 2100)
            self.assertIn("-timeout=2100.000s", run.call_args_list[1].args[0])
            written = json.loads(path.read_text())
            self.assertEqual(len(written["executed_commands"]), 2)
            self.assertEqual(commands[1][2], "-timeout=45m")

    def test_exhausted_deadline_does_not_start_another_chunk(self):
        commands = [["go", "test", "-timeout=45m", name] for name in ("A", "B")]
        with tempfile.TemporaryDirectory() as directory:
            with mock.patch.object(planner.time, "monotonic", side_effect=[0, 0, 2700]), \
                    mock.patch.object(planner.subprocess, "run") as run:
                with self.assertRaisesRegex(ValueError, "aggregate test execution budget"):
                    planner.run_commands(commands, {}, Path(directory) / "selection.json")
            self.assertEqual(run.call_count, 1)

    def test_chunk_failure_does_not_run_later_chunks(self):
        commands = [["go", "test", "-timeout=45m", name] for name in ("A", "B")]
        with tempfile.TemporaryDirectory() as directory:
            with mock.patch.object(planner.subprocess, "run", side_effect=subprocess.TimeoutExpired("go", 1)) as run:
                with self.assertRaises(subprocess.TimeoutExpired):
                    planner.run_commands(commands, {}, Path(directory) / "selection.json")
            self.assertEqual(run.call_count, 1)

    def test_single_command_preserves_non_windows_coverage_invocation(self):
        command = ["go", "test", "-race", "-timeout=45m", "-coverprofile=coverage.out", "package"]
        with tempfile.TemporaryDirectory() as directory:
            with mock.patch.object(planner.subprocess, "run") as run:
                planner.run_commands([command], {}, Path(directory) / "selection.json")
            run.assert_called_once_with(command, check=True)

    def test_main_verifies_each_go_chunk_and_manifests_all_commands(self):
        names = self.names()
        package = "example.org/gortex/internal/indexer"
        census_calls = []

        def capture(command):
            if command == ["go", "list", "-m"]:
                return "example.org/gortex\n"
            if command == ["go", "list", "./..."]:
                return package + "\n"
            self.assertIn("-list", command)
            pattern = command[command.index("-list") + 1]
            census_calls.append(pattern)
            matched = names if pattern == "." else [name for name in names if re.fullmatch(pattern, name)]
            return "\n".join(matched) + "\nok\t" + package

        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "selection.json"
            original_plan = planner.plan_shard_commands
            with mock.patch.object(planner, "PLAIN_TIMINGS_PATH",
                                   Path(directory) / "missing.json"), \
                    mock.patch.object(planner, "capture",
                                      side_effect=capture), \
                    mock.patch.object(planner, "WINDOWS_COMMAND_BUDGET", 650), \
                    mock.patch.object(planner, "plan_shard_commands", side_effect=
                                      lambda *args, **kwargs: original_plan(*args, windows_api=True, **kwargs)), \
                    mock.patch.dict(planner.os.environ, {"TEST_WINDOWS": "true"}), \
                    mock.patch.object(planner.sys, "argv", ["partition", "--partition", "indexer-1",
                                                           "--dry-run", "--manifest", str(path)]), \
                    mock.patch.object(planner.subprocess, "run") as run:
                planner.main()
            manifest = json.loads(path.read_text())
            chunks = manifest["pattern_chunks"]
            self.assertEqual(len(census_calls), 1 + sum(map(len, chunks)))
            self.assertEqual(len(manifest["commands"]), len(chunks[1]))
            self.assertGreater(len(manifest["commands"]), 1)
            self.assertTrue(all(units <= 650 for units in manifest["commands_utf16_units"]))
            self.assertEqual(manifest["aggregate_test_execution_budget_seconds"], 2700)
            run.assert_not_called()

    def test_main_plain_remaining_includes_store_and_other_packages(self):
        packages = [
            "example.org/gortex/cmd/gortex",
            "example.org/gortex/internal/graph/store_sqlite",
            "example.org/gortex/internal/indexer",
            "example.org/gortex/internal/mcp",
            "example.org/gortex/internal/parser",
        ]
        selected = [
            "example.org/gortex/cmd/gortex",
            "example.org/gortex/internal/graph/store_sqlite",
            "example.org/gortex/internal/parser",
        ]
        for windows in ("false", "true"):
            with self.subTest(windows=windows), \
                    tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / "selection.json"
                argv = ["partition", "--partition", "remaining",
                        "--manifest", str(path)]
                outputs = ["example.org/gortex", "\n".join(packages)]
                with mock.patch.object(planner, "capture",
                                       side_effect=outputs) as capture, \
                        mock.patch.dict(planner.os.environ,
                                        {"TEST_RACE": "false",
                                         "TEST_WINDOWS": windows}), \
                        mock.patch.object(planner.sys, "argv", argv), \
                        mock.patch.object(planner.subprocess, "run") as run:
                    planner.main()
                command = ["go", "test"]
                if windows == "true":
                    command += ["-v"]
                command += ["-timeout=45m"] + selected
                run.assert_called_once_with(command, check=True)
                manifest = json.loads(path.read_text())
                self.assertEqual(manifest["selected_packages"], selected)
                self.assertEqual(manifest["command"], command)
                self.assertEqual(capture.call_count, 2)

    def test_main_plain_mcp_runs_whole_package_without_census(self):
        packages = ["example.org/gortex/internal/indexer",
                    "example.org/gortex/internal/mcp",
                    "example.org/gortex/internal/graph/store_sqlite"]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "selection.json"
            argv = ["partition", "--partition", "mcp", "--manifest", str(path)]
            outputs = ["example.org/gortex", "\n".join(packages)]
            with mock.patch.object(planner, "capture",
                                   side_effect=outputs) as capture, \
                    mock.patch.dict(planner.os.environ,
                                    {"TEST_RACE": "false",
                                     "TEST_WINDOWS": "false"}), \
                    mock.patch.object(planner.sys, "argv", argv), \
                    mock.patch.object(planner.subprocess, "run") as run:
                planner.main()
            command = ["go", "test", "-timeout=45m",
                       "example.org/gortex/internal/mcp"]
            run.assert_called_once_with(command, check=True)
            manifest = json.loads(path.read_text())
            self.assertEqual(manifest["selected_packages"], [packages[1]])
            self.assertEqual(manifest["command"], command)
            self.assertNotIn("census", manifest)
            self.assertEqual(capture.call_args_list, [
                mock.call(["go", "list", "-m"]),
                mock.call(["go", "list", "./..."]),
            ])

    def test_main_plain_indexer_uses_three_shards_without_race(self):
        package = "example.org/gortex/internal/indexer"
        names = ["ExampleShard", "FuzzShard", "TestA", "TestB",
                 "TestC", "TestD"]
        outputs = ["example.org/gortex", package, "\n".join(names),
                   "ExampleShard\nFuzzShard", "TestA\nTestB", "TestC\nTestD"]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "selection.json"
            argv = ["partition", "--partition", "indexer-1",
                    "--manifest", str(path)]
            with mock.patch.object(planner, "PLAIN_TIMINGS_PATH",
                                   Path(directory) / "missing.json"), \
                    mock.patch.object(planner, "capture",
                                   side_effect=outputs) as capture, \
                    mock.patch.dict(planner.os.environ,
                                    {"TEST_RACE": "false",
                                     "TEST_WINDOWS": "false"}), \
                    mock.patch.object(planner.sys, "argv", argv), \
                    mock.patch.object(planner.subprocess, "run") as run:
                planner.main()
            manifest = json.loads(path.read_text())
            self.assertEqual(manifest["shards"], [
                ["ExampleShard", "FuzzShard"], ["TestA", "TestB"],
                ["TestC", "TestD"],
            ])
            self.assertEqual(manifest["shard_count"], 3)
            self.assertEqual(manifest["census"], names)
            command = manifest["command"]
            self.assertEqual(command[:4],
                             ["go", "test", "-timeout=45m", "-run"])
            self.assertEqual(command[5:], [package])
            self.assertEqual([name for name in names
                              if re.fullmatch(command[4], name)],
                             ["TestA", "TestB"])
            run.assert_called_once_with(command, check=True)
            self.assertEqual(capture.call_args_list[2], mock.call(
                ["go", "test", "-list", ".", package]))
            for call in capture.call_args_list:
                self.assertNotIn("-race", call.args[0])

    def test_main_rejects_partitions_from_the_other_mode(self):
        cases = (("false", "store-0"), ("true", "mcp"),
                 ("false", "indexer-3"))
        for race, partition in cases:
            with self.subTest(race=race, partition=partition):
                argv = ["partition", "--partition", partition]
                with mock.patch.dict(planner.os.environ,
                                     {"TEST_RACE": race}), \
                        mock.patch.object(planner.sys, "argv", argv), \
                        mock.patch.object(planner, "capture") as capture, \
                        mock.patch.object(planner.subprocess, "run") as run, \
                        mock.patch.object(planner.sys, "stderr"):
                    with self.assertRaises(SystemExit) as error:
                        planner.main()
                self.assertEqual(error.exception.code, 2)
                capture.assert_not_called()
                run.assert_not_called()

    def test_main_refuses_missing_go_matcher_results_before_execution(self):
        package = "example.org/gortex/internal/indexer"
        names = ["TestA", "TestB", "TestC", "TestD", "TestE", "TestF"]

        def capture(command):
            if command == ["go", "list", "-m"]:
                return "example.org/gortex"
            if command == ["go", "list", "./..."]:
                return package
            pattern = command[command.index("-list") + 1]
            if pattern == ".":
                return "\n".join(names)
            return "TestA"

        argv = ["partition", "--partition", "indexer-0"]
        with mock.patch.object(planner, "capture", side_effect=capture), \
                mock.patch.dict(planner.os.environ, {"TEST_RACE": "false"}), \
                mock.patch.object(planner.sys, "argv", argv), \
                mock.patch.object(planner.subprocess, "run") as run:
            with self.assertRaisesRegex(ValueError,
                                        "Go matcher disagrees with shard 0"):
                planner.main()
        run.assert_not_called()

    def test_main_refuses_duplicate_go_matcher_results_before_execution(self):
        names = [f"Test{letter}" for letter in "ABCDEFGHIJKL"]
        package = "example.org/gortex/internal/indexer"

        def capture(command):
            if command == ["go", "list", "-m"]:
                return "example.org/gortex"
            if command == ["go", "list", "./..."]:
                return package
            pattern = command[command.index("-list") + 1]
            if pattern == ".":
                return "\n".join(names)
            return "TestA\nTestA"

        with mock.patch.object(planner, "capture", side_effect=capture), \
                mock.patch.object(planner.sys, "argv", ["partition", "--partition", "indexer-0"]), \
                mock.patch.object(planner.subprocess, "run") as run:
            with self.assertRaisesRegex(ValueError, "nonempty and unique"):
                planner.main()
        run.assert_not_called()


if __name__ == "__main__":
    unittest.main()
