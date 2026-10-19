from collections import Counter
from contextlib import redirect_stdout
import importlib.util
import io
from itertools import product
import json
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
        environment = mock.patch.dict(planner.os.environ)
        environment.start()
        self.addCleanup(environment.stop)
        for name in ("TEST_RACE", "TEST_WINDOWS"):
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
        self.assertEqual(planner.RACE_SHARDS,
                         {"store": 6, "indexer": 12, "mcp": 3})
        self.assertEqual(planner.PLAIN_SHARDS, {"indexer": 3})
        modes = ((True, planner.RACE_SHARDS), (False, planner.PLAIN_SHARDS))
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

    def test_workflow_partitions_match_each_mode_once_per_os(self):
        workflow = (Path(__file__).parents[1]
                    / ".github/workflows/ci.yml").read_text()
        modes = (
            ("test", planner.RACE_SHARDS, planner.RACE_PACKAGES,
             ["ubuntu-latest"]),
            ("test-platform", planner.PLAIN_SHARDS, planner.PLAIN_PACKAGES,
             ["macos-latest", "windows-latest"]),
        )
        for job_id, shards, packages, operating_systems in modes:
            with self.subTest(job=job_id):
                jobs = re.findall(
                    rf"^  {job_id}:\n(.*?)(?=^  [\w-]+:|\Z)",
                    workflow, re.MULTILINE | re.DOTALL)
                self.assertEqual(len(jobs), 1)
                job = jobs[0]
                valid = ({"remaining"} | (packages.keys() - shards.keys())
                         | {f"{family}-{index}"
                            for family, count in shards.items()
                            for index in range(count)})
                for key, expected in (("partition", valid),
                                      ("os", operating_systems)):
                    lists = re.findall(
                        rf"^        {key}: \[(.*?)\]$", job, re.MULTILINE)
                    self.assertEqual(len(lists), 1)
                    values = [value.strip().strip("'\"")
                              for value in lists[0].split(",")]
                    if key == "partition":
                        self.assertEqual(Counter(values), Counter(expected))
                    else:
                        self.assertEqual(values, expected)
                self.assertRegex(job, r"(?m)^    timeout-minutes: 60$")
                if job_id == "test":
                    steps = re.split(r"(?m)^      - ", job)[1:]
                    setup = [step for step in steps
                             if "uses: actions/setup-go@" in step]
                    self.assertEqual(len(setup), 1)
                    self.assertRegex(setup[0], r"(?m)^          cache: false$")
                    restore = [step for step in steps
                               if "uses: actions/cache/restore@" in step]
                    save = [step for step in steps
                            if "uses: actions/cache/save@" in step]
                    self.assertEqual(len(restore), 1)
                    self.assertEqual(len(save), 1)
                    conditions = re.findall(
                        r"(?m)^        if: (.*)$", save[0])
                    self.assertEqual(len(conditions), 1)
                    self.assertIn("matrix.partition == 'remaining'",
                                  conditions[0])
                    warm = [step for step in steps
                            if step.startswith("name: Warm race cache")]
                    self.assertEqual(len(warm), 1)
                    packages = " ".join(
                        "./" + path.lstrip("/")
                        for path in planner.RACE_PACKAGES.values())
                    for flags in ("-race -c", "-race -cover -c"):
                        self.assertRegex(
                            warm[0], rf"(?m)go test {flags} -o \S+ "
                            + re.escape(packages) + "$")

    def test_committed_durations_balance_every_race_package(self):
        timings = json.loads(planner.RACE_TIMINGS_PATH.read_text())
        expected = {"store": 793.93, "indexer": 855.49, "mcp": 710.42}
        for family, maximum in expected.items():
            package = "github.com/zzet/gortex" + planner.RACE_PACKAGES[family]
            entries = timings[package]
            names = sorted(entries)
            with self.subTest(family=family):
                assignments, predicted = planner.plan_duration_shards(
                    names, entries, planner.RACE_SHARDS[family])
                self.assertEqual(len(assignments),
                                 {"store": 6, "indexer": 12, "mcp": 3}[family])
                self.assertAlmostEqual(max(predicted), maximum, places=2)
                self.assertLessEqual(max(predicted),
                                     max(900, max(entries.values())))
                self.assertEqual(
                    Counter(name for shard in assignments for name in shard),
                    Counter(names))

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

    def test_main_records_census_seconds(self):
        package = "example.org/gortex/internal/indexer"
        names = [f"Test{letter}" for letter in "ABCDEFGHIJKL"]
        outputs = ["example.org/gortex", package, "\n".join(names)] + names
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "selection.json"
            argv = ["partition", "--partition", "indexer-0", "--dry-run",
                    "--manifest", str(path)]
            with mock.patch.object(planner, "capture", side_effect=outputs), \
                    mock.patch.object(planner.time, "monotonic",
                                      side_effect=[100, 101.25]), \
                    mock.patch.object(planner.sys, "argv", argv), \
                    mock.patch("builtins.print") as log:
                planner.main()
            manifest = json.loads(path.read_text())
            self.assertEqual(manifest["census_seconds"], 1.25)
            log.assert_any_call("census seconds: 1.250", flush=True)

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
            with mock.patch.object(planner, "capture", side_effect=capture), \
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
            with mock.patch.object(planner, "capture",
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
            self.assertIn("census_seconds", manifest)
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
        for race, partition in (("false", "store-0"), ("true", "mcp")):
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
