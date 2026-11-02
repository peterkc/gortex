"""CI-only Checks 1, 2 and 5 over the accepted separate-process runner."""

import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import signal
import shutil
import stat
import socket
import subprocess
import sys
import time


class RunError(RuntimeError):
    """A run cannot support the requested measurement claim."""


class SemanticMismatch(RunError):
    """Complete terminal evidence proves a candidate answer mismatch."""

    def __init__(self, reports):
        super().__init__("candidate returned a checked semantic mismatch")
        self.reports = reports


LOOKUP_MIN_SAMPLES = 20
LOOKUP_MISS_REPEATS = 20
TIMING_RUNS = 3


def digest(path):
    checksum = hashlib.sha256()
    with Path(path).open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            checksum.update(block)
    return checksum.hexdigest()


def read_json(path):
    with Path(path).open(encoding="utf-8") as source:
        return json.load(source)


def write_json(path, value):
    with Path(path).open("x", encoding="utf-8") as output:
        json.dump(value, output, indent=2, allow_nan=False)
        output.write("\n")
        output.flush()
        os.fsync(output.fileno())


def percentile(values):
    if not values or any(type(value) is not int or value <= 0
                         for value in values):
        raise RunError("missing or nonpositive engine timing")
    ordered = sorted(values)
    return ordered[math.ceil(0.95 * len(ordered)) - 1]


def latency_groups(rows):
    groups = {}
    for row in rows:
        if row["status"] != "passed":
            raise RunError("unsupported or failed timed sample")
        ns = row.get("elapsed_ns")
        if type(ns) is not int or ns <= 0:
            raise RunError("sample lacks positive engine elapsed_ns")
        if row["warmup"]:
            continue
        operation = row["operation"]
        if operation in ("in", "out", "multi_in"):
            operation += "_" + row["kind"]
        groups.setdefault(operation, []).append(ns)
        if operation in ("id", "name", "file"):
            suffix = "miss" if row["missing"] else "hit"
            groups.setdefault(f"{operation}_{suffix}", []).append(ns)
    return groups


def choose_base(b, c, *, cpu_selection_approved):
    if not cpu_selection_approved or b is None or c is None:
        return "comparison_unavailable", "stop_no_recommendation"
    if b["status"] in ("partial", "blocked") or c["status"] in (
            "partial", "blocked"):
        return "comparison_unavailable", "stop_no_recommendation"
    if c["passes"] and not b["passes"]:
        return "only_c_passes", "recommend_c"
    if b["passes"] and not c["passes"]:
        return "only_b_passes", "recommend_b"
    if not b["passes"] and not c["passes"]:
        return "neither_passes", "stop_and_amend"
    directions = c["read_directions"]
    if -1 in directions.values() and 1 in directions.values():
        return "both_pass", "ask_user_read_tradeoff"
    for key in ("construction_cpu_ns", "total_bytes"):
        if b[key] < c[key]:
            return "both_pass", "recommend_b"
        if c[key] < b[key]:
            return "both_pass", "recommend_c"
    return "both_pass", "ask_user_unresolved_tie"


def ratio_status(ratios, ceiling):
    if not ratios:
        raise RunError("missing paired ratio observations")
    if all(value <= ceiling for value in ratios):
        return "pass"
    if all(value > ceiling for value in ratios):
        return "miss"
    return "partial"


def combined_status(statuses):
    if "blocked" in statuses:
        return "blocked"
    if "miss" in statuses:
        return "miss"
    if "partial" in statuses:
        return "partial"
    return "pass"


def paired_ratios(candidate, reference, key):
    left = {row["pair_index"]: row for row in candidate["runs"]}
    right = {row["pair_index"]: row for row in reference["runs"]}
    expected = set(range(1, TIMING_RUNS + 1))
    if (set(left) != expected or set(right) != expected
            or len(candidate["runs"]) != TIMING_RUNS
            or len(reference["runs"]) != TIMING_RUNS):
        raise RunError("fresh-process pairs are incomplete or mismatched")
    ratios = []
    for index in sorted(left):
        numerator = left[index]["p95_ns"].get(key)
        denominator = right[index]["p95_ns"].get(key)
        if (type(numerator) is not int or numerator <= 0
                or type(denominator) is not int or denominator <= 0):
            raise RunError(f"missing paired read: {key}")
        ratios.append(numerator / denominator)
    return ratios


def checked_file(root, relative):
    relative = Path(relative)
    if relative.is_absolute() or ".." in relative.parts:
        raise RunError("input/output path escapes its declared directory")
    path = root / relative
    if not path.is_file() or path.is_symlink():
        raise RunError(f"nonregular input or output: {path}")
    if path.resolve() != path.absolute():
        raise RunError(f"input/output alias: {path}")
    return path


def protected_snapshot(config, config_path):
    root = Path(config["source_root"])
    directories = [root / name for name in (
        "study/storage/harness", "study/storage/adapters/sqlite-segment",
        "study/storage/adapters/pebble-bleve", "study/storage/checks/base")]
    directories.extend(Path(item["path"]) for item in config["data"].values())
    directories.extend({config_path.parent,
                        Path(config["approval_path"]).parent})
    files = {str(config_path): digest(config_path),
             str(Path(config["approval_path"])):
                 digest(config["approval_path"])}
    pin_path = root / "study/storage/checks/fixtures/fixture-pins.json"
    files[str(pin_path)] = digest(pin_path)
    listings = {}
    for directory in directories:
        names = []
        for current, dirs, entries in os.walk(directory, followlinks=False):
            for name in sorted(dirs + entries):
                path = Path(current) / name
                if path.is_symlink():
                    raise RunError("protected directory contains an alias")
                relative = str(path.relative_to(directory))
                names.append((relative, "directory" if path.is_dir()
                              else "file"))
                if path.is_file():
                    files[str(path)] = digest(path)
        listings[str(directory)] = sorted(names)
    return {"files": files, "directory_entries": listings}


def validate_fixture_pins(config):
    pins_path = (Path(config["source_root"])
                 / "study/storage/checks/fixtures/fixture-pins.json")
    pins = read_json(pins_path)
    if pins["schema"] != "design-c-fixtures/v1":
        raise RunError("unsupported reviewed fixture-pin schema")
    for label in ("tier1", "tier10"):
        item = config["data"][label]
        pinned = pins["tiers"][label]
        directory = Path(item["path"])
        manifest = read_json(directory / "manifest.json")
        if (item["sha256"] != pinned["manifest_sha256"]
                or digest(directory / "manifest.json")
                != pinned["manifest_sha256"]
                or manifest["repos"] != pinned["repos"]
                or manifest["counts"] != pinned["counts"]):
            raise RunError(f"fixture differs from reviewed pins: {label}")
        for repo, expected in pinned["part_manifests"].items():
            actual = digest(checked_file(directory, repo + "/manifest.json"))
            if actual != expected:
                raise RunError(f"part manifest differs from pins: {repo}")
    return digest(pins_path)


def canonical_output(config, config_path):
    output = Path(config["output_dir"])
    if not output.is_absolute() or output.exists() or output.is_symlink():
        raise RunError("explicit fresh output directory required")
    parent = output.parent.resolve(strict=True)
    canonical = parent / output.name
    if canonical != output:
        raise RunError("aliased or unresolved output path")
    protected = [Path(config["source_root"]),
                 *[Path(item["path"]) for item in config["data"].values()],
                 config_path.parent, Path(config["approval_path"]).parent]
    for directory in protected:
        if canonical.is_relative_to(directory.resolve(strict=True)):
            raise RunError("output is inside protected source/fixture/config")
    return canonical


def validate_config(config, config_path):
    required = {
        "schema", "source_root", "source_revision", "recipe_sha256",
        "approval_path", "approval_sha256", "base", "runs",
        "safety_timeout_seconds", "memory_limits_bytes", "output_dir",
        "cgroup_root", "binaries", "data", "construction_selection_approved",
    }
    if set(config) != required or config["schema"] != "design-c-base-run/v1":
        raise RunError("config requires the exact documented fields")
    for key in ("runs", "safety_timeout_seconds"):
        if type(config[key]) is not int or config[key] <= 0:
            raise RunError(f"explicit positive {key} required")
    if config["runs"] != TIMING_RUNS:
        raise RunError("paired protocol requires three fresh runs")
    if set(config["base"]) != {"main_wall_seconds", "side_wall_seconds"}:
        raise RunError("explicit base main and side limits required")
    if any(type(value) is not int or value <= 0
           for value in config["base"].values()):
        raise RunError("base phase wall limits must be positive integers")
    limits = config["memory_limits_bytes"]
    if set(limits) != {"runner", "checker", "engine"}:
        raise RunError("all three component memory limits are required")
    page_size = os.sysconf("SC_PAGE_SIZE")
    if any(type(value) is not int or value <= 0 or value % page_size
           for value in limits.values()):
        raise RunError("positive page-aligned memory limits required")
    if sys.platform != "linux":
        raise RunError("measurements require approved Linux CI, not this Mac")
    root = Path(config["source_root"])
    actual = subprocess.check_output(
        ["git", "rev-parse", "HEAD"], cwd=root, text=True).strip()
    if actual != config["source_revision"]:
        raise RunError("reviewed source revision does not match checkout")
    if subprocess.check_output(["git", "status", "--porcelain"], cwd=root):
        raise RunError("measurement source checkout is dirty")
    if digest(config["approval_path"]) != config["approval_sha256"]:
        raise RunError("missing or changed user/operator approval receipt")
    if type(config["construction_selection_approved"]) is not bool:
        raise RunError("construction selection approval must be explicit")
    for key in ("recipe_sha256", "approval_sha256"):
        if len(bytes.fromhex(config[key])) != 32:
            raise RunError(f"invalid {key}")
    if set(config["binaries"]) != {"a", "b", "c", "c_side"}:
        raise RunError("A, B, C and the bounded C side-index binary required")
    for binary in config["binaries"].values():
        path = Path(binary["path"])
        if not path.is_absolute() or digest(path) != binary["sha256"]:
            raise RunError("missing or changed executable")
    if set(config["data"]) != {"tiny", "tier1", "tier10", "tier100"}:
        raise RunError("tiny and all three scaling fixture paths required")
    for label, item in config["data"].items():
        path = Path(item["path"])
        if not path.is_absolute():
            raise RunError("absolute explicit data paths required")
        if digest(path / "manifest.json") != item["sha256"]:
            raise RunError(f"changed fixture: {label}")
        manifest = read_json(path / "manifest.json")
        if manifest["generator_version"] != "gxstudy-v2":
            raise RunError("not a harness dataset")
        if label != "tiny" and len(manifest["repos"]) != int(label[4:]):
            raise RunError("repository count does not match scaling point")
        for name, expected in manifest["sha256"].items():
            if digest(checked_file(path, name)) != expected:
                raise RunError(f"fixture hash mismatch: {label}/{name}")
    validate_fixture_pins(config)
    canonical_output(config, config_path)


def run_command(command, log_path, remaining):
    if remaining <= 0:
        raise RunError("approved wall-time budget exhausted")
    with log_path.open("xb") as log:
        process = subprocess.Popen(command, stdout=log, stderr=log)
        try:
            code = process.wait(timeout=remaining)
        finally:
            if process.poll() is None:
                # SIGTERM lets the accepted runner stop its owned scopes.
                process.terminate()
                try:
                    process.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
                    raise RunError(
                        "runner cleanup did not join; CI owner must clean "
                        "its proven-owned job cgroup")
        return code


def collect_run(directory, expected_adapter, data, *, full_protocol):
    reports = []
    semantic_failure = False
    for receipt_path in sorted(directory.glob("components-*.json")):
        receipt = read_json(receipt_path)
        for name, expected in receipt["output_sha256"].items():
            if digest(checked_file(directory, name)) != expected:
                raise RunError("component output hash mismatch")
        components = receipt["components"]
        if set(components) != {"runner", "checker", "engine"}:
            raise RunError("missing component accounting")
        for name, component in components.items():
            if (component["oom_kills"]
                    or component["status"] not in ("completed", "failed")
                    or (name != "runner" and (
                        component["status"] != "completed"
                        or component["exit_code"] != 0))):
                raise RunError("component resource/transport failure")
        result_paths = [directory / name
                        for name in receipt["output_sha256"]
                        if name.startswith(expected_adapter + "-")
                        and name.endswith(".json")]
        if len(result_paths) != 1:
            raise RunError("missing unique measured adapter result")
        report = read_json(result_paths[0])
        if report["manifest_sha256"] != data["sha256"]:
            raise RunError("unmatched result manifest")
        if bool(report.get("timing_only", False)) == full_protocol:
            raise RunError("incorrect correctness/timing-only boundary")
        checks = [*report.get("checks", []), *report.get("delta_checks", []),
                  *report.get("concurrent", {}).get("checks", [])]
        failed_checks = [check for check in checks
                         if check.get("mismatch") is True
                         or check.get("status") == "failed"]
        mismatch = bool(failed_checks) and all(
            check.get("mismatch") is True
            and check.get("failure_kind") == "semantic"
            for check in failed_checks)
        if receipt["success"] != report["success"]:
            raise RunError("inconsistent terminal success states")
        if not report["success"]:
            runner = components["runner"]
            if (not mismatch or runner["exit_code"] != 1
                    or runner["status"] != "failed"):
                raise RunError("unclassified runner/tool failure")
            semantic_failure = True
        elif any(item["status"] != "completed"
                 for item in components.values()):
            raise RunError("incomplete successful component receipt")
        if full_protocol and not semantic_failure and (
                not report["delta_checks"] or not report["crash"]):
            raise RunError("full correctness phases missing")
        if not report["builds"]:
            raise RunError("missing construction records")
        for build in report["builds"]:
            if (type(build.get("cpu_ns")) is not int
                    or build["cpu_ns"] <= 0):
                raise RunError("missing positive construction CPU")
        rows = []
        for name, expected in report.get("query_latency_sha256", {}).items():
            path = checked_file(directory / f"run-{report['run']:02d}", name)
            if digest(path) != expected:
                raise RunError("query latency hash mismatch")
            with path.open(encoding="utf-8") as source:
                rows.extend(json.loads(line) for line in source)
        if semantic_failure:
            reports.append((report, {}))
            continue
        if not rows:
            raise RunError("no hash-bound query timing samples")
        reports.append((report, latency_groups(rows)))
    if not reports:
        raise RunError("no terminal component receipts")
    if semantic_failure:
        raise SemanticMismatch(reports)
    return reports


def measure_adapter(config, output, label, adapter, data_label, deadline,
                    *, full_protocol, pair_index=0):
    binary = config["binaries"][adapter]
    fixture = config["data"][data_label]
    binding_path = output / f"{label}.binding.json"
    run_dir = output / label
    command = [binary["path"], "-data", fixture["path"], "-out", str(run_dir),
               "-runs", "1", "-warmup=false", "-crash-trials", "12",
               "-safety-timeout", f"{config['safety_timeout_seconds']}s",
               "-binding", str(binding_path), "-cgroup-root",
               config["cgroup_root"], "-lookup-miss-repeats",
               "1" if full_protocol else str(LOOKUP_MISS_REPEATS)]
    if not full_protocol:
        command.append("-timing-only")
    binding = {
        "source_revision": config["source_revision"],
        "harness_sha256": binary["sha256"],
        "data_sha256": fixture["sha256"],
        "recipe_sha256": config["recipe_sha256"],
        "host": socket.gethostname(), "command": command[1:],
        "memory_limits_bytes": config["memory_limits_bytes"],
    }
    write_json(binding_path, binding)
    controller_started = time.monotonic()
    code = run_command(command, output / f"{label}.log",
                       deadline - controller_started)
    names = {"a": "sqlite-segment", "b": "pebble-bleve",
             "c": "parquet-base", "c_side": "parquet-side"}
    reports = collect_run(run_dir, names[adapter], fixture,
                          full_protocol=full_protocol)
    if code != 0:
        raise RunError(f"unclassified adapter exit {code}")
    if not full_protocol:
        for report, _ in reports:
            inventory = []
            for build in report["builds"]:
                expected = (run_dir / "run-01" / "segments" / build["repo"])
                if Path(build["ref"]["path"]) != expected:
                    raise RunError("unexpected segment path in run result")
                inventory.append(artifact_inventory(build, report["adapter"]))
            report["_inventory"] = inventory
            report["pair_index"] = pair_index
            report["_controller_started"] = controller_started
            write_json(output / f"{label}.inventory.json", inventory)
            remove_serving(inventory, run_dir,
                           output / f"{label}.serving-removal.jsonl")
    return reports


def physical_category(adapter, name):
    if adapter == "pebble-bleve":
        first = Path(name).parts[0]
        if first == "vectors":
            return "vectors"
        if first in ("text-records", "symbols.bleve", "content.bleve"):
            return "text"
    elif adapter in ("parquet-base", "parquet-side"):
        if name.startswith("vectors."):
            return "vectors"
        if name.startswith(("symbols.", "content.")):
            return "text"
    return "graph"


def valid_sha256(value):
    try:
        return (type(value) is str and len(value) == 64
                and len(bytes.fromhex(value)) == 32)
    except ValueError:
        return False


def artifact_inventory(build, adapter):
    path = Path(build["ref"]["path"])
    identity = path.lstat()
    if not stat.S_ISDIR(identity.st_mode) or path.resolve() != path:
        raise RunError("serving directory is an alias or not a directory")
    files = {}
    for current, dirs, names in os.walk(path, followlinks=False):
        if any((Path(current) / name).is_symlink() for name in dirs):
            raise RunError("serving directory contains an alias")
        for name in names:
            item = checked_file(Path(current), name)
            files[str(item.relative_to(path))] = {
                "bytes": item.stat().st_size, "sha256": digest(item),
                "category": "graph"}
    verification_cpu = 0
    verification = {}
    if adapter in ("parquet-base", "parquet-side", "pebble-bleve"):
        seal = read_json(path / "seal.json")
        if digest(path / "seal.json") != build["ref"]["digest"]:
            raise RunError("seal no longer matches the checked reference")
        sealed_names = set()
        for entry in seal["files"]:
            name = entry["name"]
            if name in sealed_names or name not in files:
                raise RunError("duplicate or missing serving inventory entry")
            sealed_names.add(name)
            actual = files[name]
            if (actual["bytes"] != entry["size"]
                    or actual["sha256"] != entry["sha256"]
                    or entry["category"] != physical_category(adapter, name)):
                raise RunError("serving inventory hash/size/category mismatch")
            actual["category"] = entry["category"]
        if sealed_names | {"seal.json"} != set(files):
            raise RunError("unbound serving artifact")
        if adapter.startswith("parquet-"):
            verification = seal["verification"]
            families = {"nodes": "nodes", "names": "nodes", "paths": "nodes",
                        "out": "edges", "in": "edges", "files": "files",
                        "symbols": "symbols", "content": "content",
                        "vectors": "vectors"}
            if set(verification) != set(families):
                raise RunError("missing full-table verification witnesses")
            for name, witness in verification.items():
                if (not valid_sha256(witness["input_sha256"])
                        or witness["input_sha256"] != witness["output_sha256"]
                        or witness["rows"] != seal["meta"]["counts"][
                            families[name]]
                        or type(witness["cpu_ns"]) is not int
                        or witness["cpu_ns"] < 0):
                    raise RunError("invalid full-table verification witness")
                verification_cpu += witness["cpu_ns"]
    categories = {key: 0 for key in ("graph", "text", "vectors")}
    for item in files.values():
        categories[item["category"]] += item["bytes"]
    if sum(categories.values()) != build["artifact_bytes"]:
        raise RunError("artifact inventory is incomplete")
    raw_cpu = build["cpu_ns"]
    if not 0 <= verification_cpu < raw_cpu:
        raise RunError("impossible construction/verification CPU accounting")
    return {"repo": build["repo"], "path": str(path), "files": files,
            "bytes": categories, "total": build["artifact_bytes"],
            "identity": [identity.st_dev, identity.st_ino],
            "raw_build_cpu_ns": raw_cpu,
            "verification_cpu_ns": verification_cpu,
            "construction_cpu_ns": raw_cpu - verification_cpu,
            "verification": verification}


def remove_serving(directories, run_directory, receipt_path):
    removed = []
    with receipt_path.open("x", encoding="utf-8") as receipt:
        for inventory in directories:
            path = Path(inventory["path"])
            expected = (run_directory / "run-01" / "segments"
                        / inventory["repo"])
            if path != expected or path.resolve() != expected:
                raise RunError("refuse removal outside created segment path")
            actual = path.lstat()
            if ([actual.st_dev, actual.st_ino] != inventory["identity"]
                    or not stat.S_ISDIR(actual.st_mode)):
                raise RunError("refuse removal of replaced serving directory")
            if not shutil.rmtree.avoids_symlink_attacks:
                raise RunError("identity-safe directory removal unavailable")
            intent = {"path": str(path), "identity": inventory["identity"],
                      "files": inventory["files"], "state": "remove_intent"}
            receipt.write(json.dumps(intent) + "\n")
            receipt.flush()
            os.fsync(receipt.fileno())
            shutil.rmtree(path)
            done = {"path": str(path), "identity": inventory["identity"],
                    "state": "removed"}
            removed.append(done)
            receipt.write(json.dumps(done) + "\n")
            receipt.flush()
            os.fsync(receipt.fileno())
    return removed


def aggregate(reports, manifest):
    if not reports:
        raise RunError("no complete fresh-process observations")
    groups = {}
    runs = []
    inventories = []
    for report, samples in reports:
        inventory = report["_inventory"]
        if not inventory:
            raise RunError("no saved serving inventory")
        for name, values in samples.items():
            groups.setdefault(name, []).extend(values)
        cpu = sum(row["construction_cpu_ns"] for row in inventory)
        runs.append({"pair_index": report["pair_index"],
                     "p95_ns": {key: percentile(values)
                                for key, values in samples.items()},
                     "sample_counts": {key: len(values)
                                       for key, values in samples.items()},
                     "construction_cpu_ns": cpu,
                     "raw_build_cpu_ns": sum(row["raw_build_cpu_ns"]
                                             for row in inventory),
                     "verification_cpu_ns": sum(row["verification_cpu_ns"]
                                                for row in inventory),
                     "graph_bytes": sum(row["bytes"]["graph"]
                                        for row in inventory),
                     "total_bytes": sum(row["total"] for row in inventory),
                     "started": report.get("started"),
                     "uptime": report.get("uptime"),
                     "controller_started": report.get("_controller_started")})
        inventories.extend(inventory)
    if manifest["counts"]["edges"] <= 0:
        raise RunError("bytes/edge requires a non-empty edge fixture")
    cpus = sorted(row["construction_cpu_ns"] for row in runs)
    return {
        "p95_ns": {name: percentile(values)
                   for name, values in groups.items()},
        "sample_counts": {name: len(values)
                          for name, values in groups.items()},
        "runs": runs, "construction_cpu_ns": cpus[len(cpus) // 2],
        "graph_serving_bytes": max(row["graph_bytes"] for row in runs),
        "total_bytes": max(row["total_bytes"] for row in runs),
        "bytes_per_edge": max(row["graph_bytes"] for row in runs)
                          / manifest["counts"]["edges"],
        "inventory": inventories, "counts": manifest["counts"],
        "repositories": manifest["repos"],
    }


def lookup_verdict(candidate, reference):
    ratios = {}
    statuses = []
    insufficient = []
    for tier in ("tier1", "tier10"):
        for operation in ("id", "name", "file"):
            for kind in ("hit", "miss"):
                key = f"{operation}_{kind}"
                label = f"{tier}/{key}"
                if min(candidate[tier]["sample_counts"].get(key, 0),
                       reference[tier]["sample_counts"].get(key, 0)) < 20:
                    insufficient.append(label)
                    statuses.append("partial")
                    continue
                values = paired_ratios(candidate[tier], reference[tier], key)
                ratios[label] = values
                statuses.append(ratio_status(values, 1.5))
    return {"status": ("partial" if insufficient else
                       combined_status(statuses)),
            "target": "each paired lookup hit/miss p95 <= 1.5x same-host A",
            "minimum_samples": LOOKUP_MIN_SAMPLES,
            "insufficient_strata": insufficient, "measured_value": ratios}


def base_verdict(candidate, a, b):
    lookup = lookup_verdict(candidate, a)
    statuses = [lookup["status"]]
    reads = {}
    directions = {}
    observations = {}
    for tier in ("tier1", "tier10", "tier100"):
        required = ["name", "symbols", "in_calls", "traverse2", "traverse3"]
        if tier != "tier1":
            required += ["multi_name", "multi_symbols", "multi_in_calls",
                         "multi_traverse2", "multi_traverse3"]
        for operation in required:
            key = f"{tier}/{operation}"
            ratios = paired_ratios(candidate[tier], b[tier], operation)
            observations[key] = ratios
            directions[key] = (-1 if all(value < 1 for value in ratios) else
                               1 if all(value > 1 for value in ratios) else 0)
            if operation.startswith(("multi_", "traverse")):
                reads[key] = ratios
                statuses.append(ratio_status(ratios, 1.0))
    disk = {tier: row["bytes_per_edge"] for tier, row in candidate.items()}
    statuses.append("pass" if max(disk.values()) <= 300 else "miss")
    status = combined_status(statuses)
    return {
        "passes": status == "pass", "status": status, "lookup": lookup,
        "cross_repo_traversal_ratios": reads, "read_directions": directions,
        "read_p95_ratios": observations, "serving_bytes_per_edge": disk,
        "construction_cpu_ns": sum(row["construction_cpu_ns"]
                                   for row in candidate.values()),
        "total_bytes": sum(row["total_bytes"] for row in candidate.values()),
        "construction_scope": "storage-build CPU on neutral records, "
                              "equal adapter work; read-back CPU separate",
        "identical_code_reads": "included, judged by paired fresh-run rule",
    }


def output_hashes(directory):
    hashes = {}
    for current, dirs, files in os.walk(directory, followlinks=False):
        if any((Path(current) / name).is_symlink() for name in dirs):
            raise RunError("output directory contains a symlink")
        for name in files:
            path = checked_file(Path(current), name)
            hashes[str(path.relative_to(directory))] = digest(path)
    return hashes


def run(config, config_path):
    validate_config(config, config_path)
    output = canonical_output(config, config_path)
    before = protected_snapshot(config, config_path)
    output.mkdir(mode=0o700)
    isolated = output / "environment"
    isolated.mkdir()
    for key in list(os.environ):
        if key.startswith(("PRIME_", "RLM_", "GORTEX_")):
            del os.environ[key]
    for key in ("HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
                "XDG_STATE_HOME", "XDG_RUNTIME_DIR", "TMPDIR"):
        path = isolated / key.lower()
        path.mkdir()
        os.environ[key] = str(path)
    os.environ.pop("GOMEMLIMIT", None)
    os.environ.update(GOMAXPROCS="4", GOFLAGS="-mod=readonly", GOWORK="off")
    started = time.monotonic()
    main_deadline = started + config["base"]["main_wall_seconds"]
    total_deadline = main_deadline + config["base"]["side_wall_seconds"]
    results = {}
    reports = {}
    semantic_failures = {}
    receipt = {
        "schema": "design-c-base-result/v1", "status": "blocked",
        "branch_taken": "comparison_unavailable_stop",
        "target": "Checks 1, 2, 5", "host": socket.gethostname(),
        "source_revision": config["source_revision"],
        "config_sha256": digest(config_path),
        "script_sha256": digest(__file__),
        "approval_sha256": config["approval_sha256"],
        "recipe_sha256": config["recipe_sha256"],
        "fixture_pins_sha256": validate_fixture_pins(config),
        "binaries": config["binaries"], "data": config["data"],
        "memory_limits_bytes": config["memory_limits_bytes"],
        "base": config["base"],
        "lookup_protocol": {"minimum_samples": LOOKUP_MIN_SAMPLES,
                            "measured_miss_repeats": LOOKUP_MISS_REPEATS},
        "paired_protocol": {"fresh_runs": TIMING_RUNS,
                            "crossing_ceiling": "partial", "epsilon": 0},
        "whole_product_construction": {
            "status": "unmeasured", "target": "<= 0.50x same-host current "
            "main for equal whole cold-index work",
            "reason": "needs product integration (D29)"},
        "operations_exercised": ["B private build", "S full-payload seal",
                                 "O immutable open", "G Reader queries"],
        "not_established": ["product catalog/daemon parity", "product P/T/R/C",
                            "GraphAr SDK/wire compatibility"],
        "equal_work": {
            "a": "one neutral ingest, SQLite graph/FTS/vector indexes, "
                 "FULL sync and seal",
            "b": "one neutral ingest into physical graph/text/vector KV "
                 "stores plus Bleve, shared cache, sync/flush/hash/seal",
            "c": "one neutral ingest fanout, sort/index/compress, Bleve, "
                 "optional Pebble, sync/hash/seal; read-back CPU separate"},
        "retention": "successful index directories removed only after full "
                     "inventory/identity receipts; failed indexes retained",
    }

    def record_semantic(adapter, error, label):
        if adapter in ("a", "b"):
            raise RunError("reference control semantic mismatch") from error
        semantic_failures[adapter] = {
            "status": "miss", "failure_kind": "semantic", "label": label,
            "reports": [report for report, _ in error.reports]}

    def timing(adapter, tier, index, deadline):
        label = f"{adapter}-{tier}-pair{index}"
        try:
            observed = measure_adapter(
                config, output, label, adapter, tier, deadline,
                full_protocol=False, pair_index=index)
        except SemanticMismatch as error:
            record_semantic(adapter, error, label)
            return
        reports[adapter][tier].extend(observed)
        manifest = read_json(Path(config["data"][tier]["path"])
                             / "manifest.json")
        results[adapter][tier] = aggregate(reports[adapter][tier], manifest)

    try:
        receipt["fixture_metadata"] = {
            label: {
                "tier": read_json(Path(item["path"]) / "manifest.json"),
                "repositories": [
                    read_json(Path(item["path"]) / repo / "manifest.json")
                    for repo in read_json(
                        Path(item["path"]) / "manifest.json")["repos"]]}
            for label, item in config["data"].items()}
        receipt["tier100_label"] = "derived-100-repository scaling fixture"
        for adapter in ("a", "b", "c"):
            results[adapter] = {}
            reports[adapter] = {tier: [] for tier in (
                "tier1", "tier10", "tier100")}
            label = adapter + "-correctness"
            try:
                measure_adapter(config, output, label, adapter, "tiny",
                                main_deadline, full_protocol=True)
            except SemanticMismatch as error:
                record_semantic(adapter, error, label)
        orders = (("a", "b", "c"), ("b", "c", "a"), ("c", "a", "b"))
        for tier in ("tier1", "tier10", "tier100"):
            for index, order in enumerate(orders, 1):
                for adapter in order:
                    if adapter not in semantic_failures:
                        timing(adapter, tier, index, main_deadline)
        selected = "c"
        plain_lookup = None
        if "c" not in semantic_failures:
            plain_lookup = lookup_verdict(results["c"], results["a"])
        if plain_lookup is not None and plain_lookup["status"] == "miss":
            selected = "c_side"
            side_deadline = min(total_deadline, time.monotonic()
                                + config["base"]["side_wall_seconds"])
            results[selected] = {}
            reports[selected] = {tier: [] for tier in (
                "tier1", "tier10", "tier100")}
            label = "c-side-correctness"
            try:
                measure_adapter(config, output, label, selected, "tiny",
                                side_deadline, full_protocol=True)
            except SemanticMismatch as error:
                record_semantic(selected, error, label)
            for tier in ("tier1", "tier10", "tier100"):
                for index in range(1, TIMING_RUNS + 1):
                    if selected not in semantic_failures:
                        timing(selected, tier, index, side_deadline)
        b = base_verdict(results["b"], results["a"], results["b"])
        if selected in semantic_failures:
            c = {"passes": False, "status": "miss", "read_directions": {},
                 "reason": "checked semantic mismatch"}
            check2 = semantic_failures[selected]
            final_lookup = {"status": "partial",
                            "reason": "candidate semantic failure"}
        else:
            c = base_verdict(results[selected], results["a"], results["b"])
            final_lookup = lookup_verdict(results[selected], results["a"])
            witnesses = [item["verification"]
                         for item in results[selected]["tier1"]["inventory"]]
            if not witnesses or any(len(item) != 9 for item in witnesses):
                raise RunError("no complete tier1 round-trip verification")
            check2 = {"status": "pass", "verification": witnesses,
                      "target": "complete tier1 payload round trip and "
                                "equal checked answers"}
        if selected == "c_side" and selected not in semantic_failures:
            receipt["reused_control_age_seconds"] = {
                tier: [{
                    "pair_index": side["pair_index"],
                    "a": side["controller_started"]
                         - control_a["controller_started"],
                    "b": side["controller_started"]
                         - control_b["controller_started"]}
                    for side, control_a, control_b in zip(
                        results[selected][tier]["runs"],
                        results["a"][tier]["runs"],
                        results["b"][tier]["runs"])]
                for tier in ("tier1", "tier10", "tier100")}
        row, branch = choose_base(
            b, c, cpu_selection_approved=config[
                "construction_selection_approved"])
        receipt.update({
            "status": ("pass" if branch.startswith("recommend_") else
                       "miss" if row == "neither_passes" else "partial"),
            "check1": {**final_lookup, "plain_parquet": plain_lookup,
                       "branch_taken": ("bounded_pebble_side" if selected
                                        == "c_side" else "direct_parquet")},
            "check2": check2,
            "check5": {"b": b, "c": c, "selection_table_row": row,
                       "branch_taken": branch, "c_variant": selected},
            "branch_taken": branch})
    except BaseException as error:
        receipt.update(status="blocked", error=str(error),
                       error_type=type(error).__name__,
                       branch_taken="comparison_unavailable_stop")
        raise
    finally:
        receipt["measured_value"] = results
        receipt["semantic_failures"] = semantic_failures
        try:
            receipt["protected_inputs_unchanged"] = (
                protected_snapshot(config, config_path) == before)
            if not receipt["protected_inputs_unchanged"]:
                receipt.update(status="blocked",
                               branch_taken="protected_input_changed_stop")
            receipt["output_sha256"] = output_hashes(output)
        except (OSError, RunError) as error:
            receipt.update(status="blocked", evidence_error=str(error),
                           branch_taken="evidence_collection_failed_stop")
        write_json(output / "result.json", receipt)
    if receipt["status"] == "blocked":
        raise RunError("run cannot support a comparison")


def interrupted(signum, frame):
    raise RunError(f"run interrupted by signal {signum}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, required=True)
    args = parser.parse_args()
    config_path = args.config.resolve(strict=True)
    signal.signal(signal.SIGTERM, interrupted)
    run(read_json(config_path), config_path)


if __name__ == "__main__":
    main()
