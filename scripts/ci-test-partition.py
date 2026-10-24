#!/usr/bin/env python3
"""Run one exhaustive package/test partition without changing test budgets."""

import argparse
import json
import math
import os
import re
import shutil
import subprocess
import sys
import time
from pathlib import Path
from statistics import median

FALLBACK_RACE_SHARDS = {"store": 6, "indexer": 12, "mcp": 3}
RACE_TIMINGS_PATH = Path(__file__).with_name("ci-race-timings.json")
FALLBACK_PLAIN_SHARDS = {"indexer": 3}
FALLBACK_WINDOWS_SHARDS = {"indexer": 4}
PLAIN_TIMINGS_PATH = Path(__file__).with_name("ci-plain-timings.json")
MAX_SHARDS = 16
TARGET_SECONDS = 10 * 60
NAME_BUNDLE = 32
WINDOWS_COMMAND_BUDGET = 30000
TEST_EXECUTION_BUDGET_SECONDS = 45 * 60
RACE_PACKAGES = {
    "store": "/internal/graph/store_sqlite",
    "indexer": "/internal/indexer",
    "mcp": "/internal/mcp",
}
PLAIN_PACKAGES = {
    "indexer": "/internal/indexer",
    "mcp": "/internal/mcp",
}


def capture(command):
    return subprocess.check_output(command, text=True, encoding="utf-8")


def test_names(output):
    names = []
    for line in output.splitlines():
        if line.startswith(("Test", "Example", "Fuzz")):
            if not line.isidentifier():
                raise ValueError(f"unexpected test census line: {line!r}")
            names.append(line)
    if not names or len(names) != len(set(names)):
        raise ValueError("compiled test census must be nonempty and unique")
    return sorted(names)


def exact_pattern(names):
    # Factoring shared prefixes keeps Windows' process command line below its
    # limit. Anchors apply only to top-level names; Go runs all their subtests.
    trie = {}
    for name in names:
        node = trie
        for char in name:
            node = node.setdefault(char, {})
        node[""] = {}

    def render(node):
        suffixes = {}
        for char, child in sorted(node.items()):
            if char:
                suffixes.setdefault(render(child), []).append(char)
        alternatives = []
        for suffix, chars in suffixes.items():
            prefix = (re.escape(chars[0]) if len(chars) == 1 else
                      "[" + "".join(re.escape(char) for char in chars) + "]")
            alternatives.append(prefix + suffix)
        if not alternatives:
            return ""
        body = alternatives[0] if len(alternatives) == 1 else "(?:" + "|".join(alternatives) + ")"
        return "(?:" + body + ")?" if "" in node else body

    return "^(?:" + render(trie) + ")$"


def plan_shards(names, bundle_size=NAME_BUNDLE, shard_count=3):
    # Small adjacent bundles share enough prefixes for Windows command lines,
    # while round-robin assignment spreads name families across every runner.
    # Capacity limits keep the top-level case counts balanced to one case.
    targets = [len(names) // shard_count + (index < len(names) % shard_count)
               for index in range(shard_count)]
    assignments = [[] for _ in range(shard_count)]
    index = 0
    for start in range(0, len(names), bundle_size):
        bundle = names[start:start + bundle_size]
        while bundle:
            count = min(len(bundle), targets[index] - len(assignments[index]))
            assignments[index].extend(bundle[:count])
            bundle = bundle[count:]
            index = (index + 1) % shard_count
    assignments = [sorted(assignment) for assignment in assignments]
    if any(not assignment for assignment in assignments):
        raise ValueError("every shard must have at least one compiled test")
    patterns = [exact_pattern(assignment) for assignment in assignments]
    # Fail closed on omissions, overlaps, or a regex that selects a different
    # shard. This also covers Example functions and Fuzz seed corpora.
    for name in names:
        matches = [index for index, pattern in enumerate(patterns)
                   if re.fullmatch(pattern, name)]
        expected = [index for index, assignment in enumerate(assignments)
                    if name in assignment]
        if len(matches) != 1 or matches != expected:
            raise ValueError(f"non-exhaustive or overlapping shard assignment: {name}")
    return assignments, patterns


def read_race_timings(path, package):
    try:
        timings = json.loads(path.read_text())
    except FileNotFoundError:
        return "missing timing file"
    except OSError as error:
        return f"unreadable timing file: {error.strerror}"
    except (ValueError, UnicodeError):
        return "malformed timing JSON"
    if not isinstance(timings, dict):
        return "wrong timing shape: expected package object"
    if package not in timings:
        return "no entry for package in timing file"
    entries = timings[package]
    if not isinstance(entries, dict) or not entries:
        return "wrong timing shape: expected nonempty test object"
    for seconds in entries.values():
        if (type(seconds) not in (int, float) or seconds < 0
                or seconds > sys.float_info.max
                or not math.isfinite(seconds)):
            return "wrong timing shape: expected finite nonnegative seconds"
    return entries


def read_family_timings(path, fallback, package):
    timings = read_race_timings(path, package)
    missing = ("missing timing file", "no entry for package in timing file")
    if timings in missing:
        return read_race_timings(fallback, package), fallback
    return timings, path


def plan_duration_shards(names, timings, shard_count):
    default_seconds = median(timings.values())
    # Go reports centiseconds; rounded-zero cases still need a weight.
    weights = {name: max(0.005, timings.get(name, default_seconds))
               for name in names}
    assignments = [[] for _ in range(shard_count)]
    predicted = [0.0] * shard_count
    for name in sorted(names, key=lambda name: (-weights[name], name)):
        index = min(range(shard_count),
                    key=lambda index: (predicted[index],
                                       len(assignments[index])))
        assignments[index].append(name)
        predicted[index] += weights[name]
    if any(not assignment for assignment in assignments):
        raise ValueError("every shard must have at least one compiled test")
    return [sorted(shard) for shard in assignments], predicted


def command_units(command):
    return len(subprocess.list2cmdline(command).encode("utf-16-le")) // 2


def planned_command_units(command):
    # Leave room for the longer remaining-budget flag used by split commands.
    return command_units(["-timeout=2700.000s" if arg == "-timeout=45m" else arg
                          for arg in command])


def plan_shard_commands(names, census_prefix, test_prefix, package,
                        windows_api=None, shard_count=3, timings=None):
    # Keep existing membership when it fits. Larger adjacent bundles retain
    # exact coverage and balance while factoring more shared name prefixes.
    if windows_api is None:
        windows_api = os.name == "nt"
    if timings is not None:
        assignments, predicted = plan_duration_shards(
            names, timings, shard_count)
        chunks = [
            bounded_patterns(shard, census_prefix, test_prefix, package)
            if windows_api else [exact_pattern(shard)]
            for shard in assignments
        ]
        return assignments, chunks, None, predicted
    bundle_size = NAME_BUNDLE
    while True:
        assignments, patterns = plan_shards(names, bundle_size, shard_count)
        commands = [prefix + [flag, pattern, package]
                    for pattern in patterns
                    for prefix, flag in ((census_prefix, "-list"), (test_prefix, "-run"))]
        if not windows_api or all(planned_command_units(command) <= WINDOWS_COMMAND_BUDGET
                                  for command in commands):
            return (assignments, [[pattern] for pattern in patterns],
                    bundle_size, None)
        if bundle_size >= len(names):
            # Preserve this balanced assignment. Splitting invocations does not
            # move tests between runners or increase their execution budget.
            chunks = [bounded_patterns(assignment, census_prefix, test_prefix, package)
                      for assignment in assignments]
            return assignments, chunks, bundle_size, None
        bundle_size = min(bundle_size * 2, len(names))


def bounded_patterns(names, census_prefix, test_prefix, package):
    pattern = exact_pattern(names)
    commands = [prefix + [flag, pattern, package]
                for prefix, flag in ((census_prefix, "-list"), (test_prefix, "-run"))]
    if all(planned_command_units(command) <= WINDOWS_COMMAND_BUDGET for command in commands):
        return [pattern]
    if len(names) == 1:
        raise ValueError("one compiled test cannot fit Windows command budget: " + names[0])
    middle = len(names) // 2
    return (bounded_patterns(names[:middle], census_prefix, test_prefix, package)
            + bounded_patterns(names[middle:], census_prefix, test_prefix, package))


def write_manifest(path, manifest):
    path.write_text(json.dumps(manifest, indent=2) + "\n")


def run_commands(commands, manifest, manifest_path):
    # Only oversized Windows shards have multiple commands. One deadline covers
    # all chunks, including Go startup between them; no chunk receives a new 45m.
    deadline = time.monotonic() + TEST_EXECUTION_BUDGET_SECONDS
    manifest["executed_commands"] = []
    try:
        for command in commands:
            actual = list(command)
            remaining = deadline - time.monotonic()
            if len(commands) > 1:
                if remaining <= 0:
                    raise ValueError("aggregate test execution budget exhausted")
                actual[actual.index("-timeout=45m")] = f"-timeout={remaining:.3f}s"
            units = guard_command(actual)
            manifest["executed_commands"].append({"command": actual, "utf16_units": units})
            write_manifest(manifest_path, manifest)
            if len(commands) > 1:
                subprocess.run(actual, check=True, timeout=remaining)
            else:
                subprocess.run(actual, check=True)
    finally:
        write_manifest(manifest_path, manifest)


def guard_command(command):
    # Reserve room for Go's own test-binary flags and path below CreateProcess's
    # 32,767 UTF-16 code-unit limit on Windows. Other platforms do not use
    # CreateProcess. Never drop names to make an argument fit.
    units = command_units(command)
    if os.name == "nt" and units > WINDOWS_COMMAND_BUDGET:
        raise ValueError(f"factored test command exceeds Windows budget: {units}")
    # Leave headroom below Linux's 128 KiB single-argument limit.
    if os.name != "nt" and "-run" in command:
        pattern = command[command.index("-run") + 1]
        size = len(os.fsencode(pattern))
        if size > 120 * 1024:
            raise ValueError(
                f"test -run argument exceeds 120 KiB budget: {size} bytes")
    return units


# Actions prefixes each output line with an RFC3339 timestamp.
LOG_TIMESTAMP = re.compile(r"^\ufeff?\d{4}-\d\d-\d\dT\S+ ")
PASS_LINE = re.compile(
    r"--- PASS: ((?:Test|Example|Fuzz)\w+) \((\d+(?:\.\d+)?)s\)")


def parse_pass_durations(output):
    """Group top-level PASS durations by package, taking duplicate maxima."""
    packages = {}
    pending = {}
    for line in output.splitlines():
        text = LOG_TIMESTAMP.sub("", line)
        passed = PASS_LINE.fullmatch(text)
        if passed:
            name, seconds = passed.groups()
            pending[name] = max(pending.get(name, 0.0), float(seconds))
        summary = re.match(r"^(ok|FAIL)[ \t]+(\S+)\t", text)
        if summary:
            status, package = summary.groups()
            if status == "ok":
                entries = packages.setdefault(package, {})
                for name, seconds in pending.items():
                    entries[name] = max(entries.get(name, 0.0), seconds)
            pending = {}
    return packages


def family_counts(entries, fallback, target):
    counts = dict(fallback)
    for family, timings in entries.items():
        if isinstance(timings, dict):
            seconds = sum(max(0.005, value) for value in timings.values())
            counts[family] = min(MAX_SHARDS,
                                 max(1, math.ceil(seconds / target)))
    return counts


def build_plan(race_entries, plain_entries, go_version):
    race_counts = family_counts(
        race_entries, FALLBACK_RACE_SHARDS, TARGET_SECONDS)
    windows_counts = family_counts(
        plain_entries, FALLBACK_WINDOWS_SHARDS, TARGET_SECONDS)
    mac_counts = {family: min(3, count)
                  for family, count in windows_counts.items()}
    matrices = {}
    for key, platforms, counts_by_os, paths in (
            ("linux", ["ubuntu-latest"], {"ubuntu-latest": race_counts},
             RACE_PACKAGES),
            ("platform", ["macos-latest", "windows-latest"],
             {"macos-latest": mac_counts, "windows-latest": windows_counts},
             PLAIN_PACKAGES)):
        include = []
        for platform in platforms:
            counts = counts_by_os[platform]
            partitions = [("remaining", 0)]
            for family in paths:
                if family in counts:
                    partitions += [(f"{family}-{index}", counts[family])
                                   for index in range(counts[family])]
                else:
                    partitions.append((family, 0))
            include += [{"os": platform, "go-version": go_version,
                         "partition": partition, "shard_count": count}
                        for partition, count in partitions]
        matrices[key] = {"include": include}
    return matrices


def write_plan(directory, module, go_version, output_path):
    directory.mkdir(parents=True, exist_ok=True)
    entries = {}
    for mode, source, families in (
            ("race", RACE_TIMINGS_PATH, FALLBACK_RACE_SHARDS),
            ("plain", PLAIN_TIMINGS_PATH, FALLBACK_WINDOWS_SHARDS)):
        target = directory / source.name
        chosen = target if target.exists() else source
        print(f"{mode} timing source: {chosen}", flush=True)
        if chosen.exists() and chosen != target:
            shutil.copyfile(chosen, target)
        paths = RACE_PACKAGES if mode == "race" else PLAIN_PACKAGES
        entries[mode] = {}
        additions = {}
        for family in families:
            package = module + paths[family]
            timings, used = read_family_timings(chosen, source, package)
            print(f"{mode}/{family} timing source: {used}", flush=True)
            if (used == source and chosen != source
                    and isinstance(timings, dict)):
                additions[package] = timings
            entries[mode][family] = timings
            if isinstance(timings, str):
                print(f"::warning::{mode}/{family}: {timings}; "
                      "using fallback count and count split", flush=True)
        if additions:
            materialized = json.loads(target.read_text(encoding="utf-8"))
            materialized.update(additions)
            encoded = json.dumps(materialized, indent=2, sort_keys=True) + "\n"
            target.write_text(encoded, encoding="utf-8")
    plan = build_plan(entries["race"], entries["plain"], go_version)
    (directory / "plan.json").write_text(
        json.dumps(plan, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    with output_path.open("a", encoding="utf-8") as output:
        for name, matrix in plan.items():
            encoded = json.dumps(matrix, separators=(",", ":"))
            output.write(f"{name}={encoded}\n")
            print(f"{name}={encoded}", flush=True)


def partition_shards(partition, fallback, count):
    shards = dict(fallback)
    family, separator, index = (partition or "").rpartition("-")
    if separator and family in shards:
        actual = shards[family] if count is None else count
        if not 1 <= actual <= MAX_SHARDS:
            raise ValueError(f"shard count must be between 1 and {MAX_SHARDS}")
        if not index.isdecimal() or int(index) >= actual:
            raise ValueError("partition index must be below shard count")
        shards[family] = actual
    elif count not in (None, 0):
        raise ValueError("unsharded partitions require shard count 0")
    return shards


def passed_durations(output):
    """Return every top-level PASS duration, even without a package summary."""
    durations = {}
    for line in output.splitlines():
        passed = PASS_LINE.fullmatch(LOG_TIMESTAMP.sub("", line))
        if passed:
            name, seconds = passed.groups()
            durations[name] = max(durations.get(name, 0.0), float(seconds))
    return durations


def shard_timing_record(output, platform, partition, count, module,
                        complete=True):
    """Build one shard's timing record.

    A failed or timed-out shard still reports the tests that passed, marked
    incomplete: the merge adds those times but never drops a test because of
    them. A shard owns exactly one package, so its PASS lines need no package
    summary.
    """
    race = platform == "ubuntu-latest"
    fallback = FALLBACK_RACE_SHARDS if race else FALLBACK_WINDOWS_SHARDS
    paths = RACE_PACKAGES if race else PLAIN_PACKAGES
    family, separator, _ = partition.rpartition("-")
    if (platform not in ("ubuntu-latest", "windows-latest")
            or not separator or family not in fallback):
        raise ValueError("timings require a Linux race or Windows plain shard")
    counts = partition_shards(partition, fallback, count)
    count = counts[family]
    package = module + paths[family]
    timings = (parse_pass_durations(output).get(package) if complete
               else passed_durations(output))
    if not timings:
        if not complete:
            return None
        raise ValueError(f"no completed PASS results for {package}")
    return {"os": platform, "partition": partition, "shard_count": count,
            "package": package, "timings": timings, "complete": complete}


def merge_family_timings(old, delivered, expected, finished):
    """Average this run's times with the old ones.

    A test is dropped only when every expected shard finished; times from
    failed shards are added but never drop a test.
    """
    measured = {}
    for partition in sorted(expected & delivered.keys()):
        for name, seconds in delivered[partition].items():
            measured[name] = max(measured.get(name, 0.0), seconds)
    complete = bool(expected) and expected <= finished
    if not complete and not old:
        return {}
    names = measured.keys() if complete else old.keys() | measured.keys()
    return {
        name: (round((old[name] + measured[name]) / 2, 2)
               if name in old and name in measured else
               measured[name] if name in measured else old[name])
        for name in sorted(names)
    }


def merge_run_timings(plan, old, records, module):
    merged = {}
    for mode, matrix, platform, families, paths in (
            ("race", "linux", "ubuntu-latest", FALLBACK_RACE_SHARDS,
             RACE_PACKAGES),
            ("plain", "platform", "windows-latest", FALLBACK_WINDOWS_SHARDS,
             PLAIN_PACKAGES)):
        merged[mode] = {}
        for family in families:
            package = module + paths[family]
            expected_rows = [row for row in plan[matrix]["include"]
                             if row["os"] == platform
                             and row["partition"].startswith(family + "-")]
            expected = {row["partition"]: row["shard_count"]
                        for row in expected_rows}
            delivered = {}
            finished = set()
            for record in records:
                partition = record["partition"]
                if (record["os"] == platform and record["package"] == package
                        and partition in expected
                        and record["shard_count"] == expected[partition]):
                    delivered[partition] = record["timings"]
                    if record.get("complete") is True:
                        finished.add(partition)
            timings = merge_family_timings(
                old[mode].get(package, {}), delivered, set(expected),
                finished)
            if timings:
                merged[mode][package] = timings
    return merged


def merge_timings(directory, shard_directory, module):
    plan = json.loads((directory / "plan.json").read_text(encoding="utf-8"))
    old = {}
    for mode, source, families, paths in (
            ("race", RACE_TIMINGS_PATH, FALLBACK_RACE_SHARDS, RACE_PACKAGES),
            ("plain", PLAIN_TIMINGS_PATH, FALLBACK_WINDOWS_SHARDS,
             PLAIN_PACKAGES)):
        old[mode] = {}
        for family in families:
            package = module + paths[family]
            timings = read_race_timings(directory / source.name, package)
            old[mode][package] = timings if isinstance(timings, dict) else {}
    records = [json.loads(path.read_text(encoding="utf-8"))
               for path in sorted(shard_directory.glob("*.json"))]
    merged = merge_run_timings(plan, old, records, module)
    for mode, source in (("race", RACE_TIMINGS_PATH),
                         ("plain", PLAIN_TIMINGS_PATH)):
        (directory / source.name).write_text(
            json.dumps(merged[mode], indent=2, sort_keys=True) + "\n",
            encoding="utf-8")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--partition", default=os.environ.get("TEST_PARTITION"), required=False)
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--manifest", type=Path, default=Path("test-selection.json"))
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument("--plan", action="store_true")
    modes.add_argument("--merge-timings", action="store_true")
    modes.add_argument("--write-timings", type=Path, metavar="TEST_LOG")
    parser.add_argument("--timings-dir", type=Path, default=Path("ci-timings"))
    parser.add_argument("--shard-timings-dir", type=Path,
                        default=Path("ci-shard-timings"))
    parser.add_argument("--shard-count", type=int,
                        default=os.environ.get("TEST_SHARD_COUNT"))
    args = parser.parse_args()
    if args.plan or args.merge_timings or args.write_timings is not None:
        go_mod = Path("go.mod").read_text(encoding="utf-8").splitlines()
        module = next(line.split()[1] for line in go_mod
                      if line.startswith("module "))
        if args.plan:
            go_version = next(".".join(line.split()[1].split(".")[:2])
                              for line in go_mod if line.startswith("go "))
            write_plan(args.timings_dir, module, go_version,
                       Path(os.environ["GITHUB_OUTPUT"]))
        elif args.merge_timings:
            merge_timings(args.timings_dir, args.shard_timings_dir, module)
        else:
            platform = os.environ["TEST_OS"]
            outcome = os.environ.get("TEST_OUTCOME")
            if outcome not in ("success", "failure"):
                raise ValueError("TEST_OUTCOME must be success or failure")
            output = args.write_timings.read_text(
                encoding="utf-8", errors="replace")
            record = shard_timing_record(
                output, platform, args.partition, args.shard_count, module,
                complete=outcome == "success")
            if record is None:
                print("no passed tests to record", flush=True)
                return
            args.shard_timings_dir.mkdir(parents=True, exist_ok=True)
            path = args.shard_timings_dir / f"{platform}-{args.partition}.json"
            path.write_text(json.dumps(record, indent=2, sort_keys=True)
                            + "\n", encoding="utf-8")
        return
    windows = os.environ.get("TEST_WINDOWS", "false") == "true"
    race = (os.environ.get("TEST_RACE", "false" if windows else "true")
            == "true")
    plain_shards = (FALLBACK_WINDOWS_SHARDS if windows
                    else FALLBACK_PLAIN_SHARDS)
    fallback = FALLBACK_RACE_SHARDS if race else plain_shards
    try:
        shards = partition_shards(args.partition, fallback, args.shard_count)
    except ValueError as error:
        parser.error(str(error))
    paths = RACE_PACKAGES if race else PLAIN_PACKAGES
    valid = ({"remaining"} | (paths.keys() - shards.keys())
             | {f"{family}-{index}" for family, count in shards.items()
                for index in range(count)})
    if args.partition not in valid:
        parser.error(f"partition must be one of {sorted(valid)}")
    module = capture(["go", "list", "-m"]).strip()
    packages = capture(["go", "list", "./..."]).splitlines()
    if not packages or len(packages) != len(set(packages)):
        raise ValueError("package census must be nonempty and unique")
    package_partitions = {package: next((name for name, suffix in paths.items()
                                        if package == module + suffix), "remaining")
                          for package in packages}
    family = args.partition.split("-", 1)[0]
    selected = [package for package, partition in package_partitions.items() if partition == family]
    if not selected:
        raise ValueError(f"no packages selected for {args.partition}")
    manifest = {"partition": args.partition, "packages": package_partitions, "selected_packages": selected}
    command = ["go", "test"]
    if race:
        command += ["-race", "-timeout=45m", "-coverprofile=coverage.out"]
    else:
        command += (["-v"] if windows else []) + ["-timeout=45m"]
    if race and family in shards:
        command += ["-v"]
    if family in shards:
        if len(selected) != 1:
            raise ValueError("a test shard must own exactly one package")
        census_command = (["go", "test"] + (["-race"] if race else [])
                          + ["-list", ".", selected[0]])
        names = test_names(capture(census_command))
        timing_path = RACE_TIMINGS_PATH if race else PLAIN_TIMINGS_PATH
        timings, _ = read_family_timings(
            args.timings_dir / timing_path.name, timing_path, selected[0])
        manifest["assignment"] = "count"
        if isinstance(timings, str):
            print(f"::warning::{selected[0]}: {timings}", flush=True)
        if not isinstance(timings, dict):
            timings = None
        shard_plan = plan_shard_commands(
            names, census_command[:-3], command, selected[0],
            shard_count=shards[family], timings=timings)
        assignments, pattern_chunks, bundle_size, predicted = shard_plan
        if predicted is not None:
            manifest.update(assignment="duration", predicted_seconds=[
                round(seconds, 2) for seconds in predicted])
        # Verify the Python-generated expressions with Go's own test matcher.
        # The per-platform compiled census is the authority for every shard.
        for index, chunks in enumerate(pattern_chunks):
            matched = []
            for pattern in chunks:
                guard_command(census_command[:-3] + ["-list", pattern, selected[0]])
                guard_command(command + ["-run", pattern, selected[0]])
                matched.extend(test_names(capture(census_command[:-3] + ["-list", pattern, selected[0]])))
            if sorted(matched) != assignments[index] or len(matched) != len(set(matched)):
                raise ValueError(f"Go matcher disagrees with shard {index}")
        index = int(args.partition.rsplit("-", 1)[1])
        commands = [command + ["-run", pattern] + selected for pattern in pattern_chunks[index]]
        manifest.update(census=names, shards=assignments, pattern_chunks=pattern_chunks,
                        name_bundle=bundle_size, shard_count=shards[family])
        if all(len(chunks) == 1 for chunks in pattern_chunks):
            manifest["patterns"] = [chunks[0] for chunks in pattern_chunks]
        print(f"{selected[0]}: {len(names)} compiled tests/examples/fuzz seeds; "
              f"shards={[len(shard) for shard in assignments]}; selected={index}", flush=True)
    else:
        commands = [command + selected]
    units = [guard_command(command) for command in commands]
    manifest["commands"] = commands
    manifest["commands_utf16_units"] = units
    if len(commands) > 1:
        manifest["aggregate_test_execution_budget_seconds"] = TEST_EXECUTION_BUDGET_SECONDS
    if len(commands) == 1:
        manifest["command"] = commands[0]
        manifest["command_utf16_units"] = units[0]
    write_manifest(args.manifest, manifest)
    print(f"Running {len(selected)} packages in {args.partition}; command units={units}", flush=True)
    if not args.dry_run:
        run_commands(commands, manifest, args.manifest)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError,
            subprocess.TimeoutExpired) as error:
        print(f"CI test partition failed: {error}", file=sys.stderr)
        sys.exit(1)
