"""CI-only Checks 1, 2 and 5 over the accepted separate-process runner."""

import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import time


class RunError(RuntimeError):
    """A run cannot support the requested measurement claim."""


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
    # The five rows are closed plan rules, not a model-generated ranking.
    if not cpu_selection_approved or b is None or c is None:
        return "comparison_unavailable", "stop_no_recommendation"
    if c["passes"] and not b["passes"]:
        return "only_c_passes", "recommend_c"
    if b["passes"] and not c["passes"]:
        return "only_b_passes", "recommend_b"
    if not b["passes"] and not c["passes"]:
        return "neither_passes", "stop_and_amend"
    b_reads = b["read_p95_ns"]
    c_reads = c["read_p95_ns"]
    common = b_reads.keys() & c_reads.keys()
    b_faster = any(b_reads[key] < c_reads[key] for key in common)
    c_faster = any(c_reads[key] < b_reads[key] for key in common)
    if b_faster and c_faster:
        return "both_pass", "ask_user_read_tradeoff"
    for key in ("construction_cpu_ns", "total_bytes"):
        if b[key] < c[key]:
            return "both_pass", "recommend_b"
        if c[key] < b[key]:
            return "both_pass", "recommend_c"
    return "both_pass", "ask_user_unresolved_tie"


def checked_file(root, relative):
    path = root / relative
    if not path.is_file() or path.is_symlink():
        raise RunError(f"nonregular input or output: {path}")
    if path.resolve() != path.absolute():
        raise RunError(f"input/output alias: {path}")
    return path


def protected_snapshot(config, config_path):
    root = Path(config["source_root"])
    command = ["git", "ls-files", "-z", "--", "study/storage/harness",
               "study/storage/adapters/sqlite-segment",
               "study/storage/adapters/pebble-bleve",
               "study/storage/checks/base"]
    names = subprocess.check_output(command, cwd=root).split(b"\0")
    files = [checked_file(root, os.fsdecode(name))
             for name in names if name]
    files.extend([config_path, Path(config["approval_path"])])
    for item in config["data"].values():
        directory = Path(item["path"])
        manifest = read_json(directory / "manifest.json")
        files.append(checked_file(directory, "manifest.json"))
        files.extend(checked_file(directory, name)
                     for name in manifest["sha256"])
    return {str(path): digest(path) for path in files}


def validate_config(config, config_path):
    required = {
        "schema", "source_root", "source_revision", "recipe_sha256",
        "approval_path", "approval_sha256", "wall_seconds", "runs",
        "safety_timeout_seconds", "memory_limits_bytes", "output_dir",
        "cgroup_root", "binaries", "data", "construction_selection_approved",
    }
    if set(config) != required or config["schema"] != "design-c-base-run/v1":
        raise RunError("config requires the exact documented fields")
    if sys.platform != "linux":
        raise RunError("measurements require approved Linux CI, not this Mac")
    root = Path(config["source_root"])
    actual = subprocess.check_output(
        ["git", "rev-parse", "HEAD"], cwd=root, text=True).strip()
    if actual != config["source_revision"]:
        raise RunError("reviewed source revision does not match checkout")
    if subprocess.check_output(["git", "status", "--porcelain"], cwd=root):
        raise RunError("measurement source checkout is dirty")
    for key in ("wall_seconds", "runs", "safety_timeout_seconds"):
        if type(config[key]) is not int or config[key] <= 0:
            raise RunError(f"explicit positive {key} required")
    limits = config["memory_limits_bytes"]
    if set(limits) != {"runner", "checker", "engine"}:
        raise RunError("all three component memory limits are required")
    page_size = os.sysconf("SC_PAGE_SIZE")
    if any(type(value) is not int or value <= 0 or value % page_size
           for value in limits.values()):
        raise RunError("positive page-aligned memory limits required")
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
    output = Path(config["output_dir"])
    if not output.is_absolute() or output.exists():
        raise RunError("explicit fresh output directory required")
    # Evidence must not write into protected inputs or the checkout.
    for protected in [root, *[Path(x["path"])
                             for x in config["data"].values()]]:
        if output.is_relative_to(protected.resolve()):
            raise RunError("output is inside protected source or fixture")
    if config_path.is_relative_to(output):
        raise RunError("run config is inside its fresh output directory")


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
        if code != 0:
            raise RunError(f"adapter failed with exit code {code}")


def collect_run(directory, expected_adapter, data, *, full_protocol):
    reports = []
    for receipt_path in sorted(directory.glob("components-*.json")):
        receipt = read_json(receipt_path)
        if not receipt["success"]:
            raise RunError("component receipt did not complete")
        for name, expected in receipt["output_sha256"].items():
            if digest(checked_file(directory, name)) != expected:
                raise RunError("component output hash mismatch")
        for component in receipt["components"].values():
            if component["status"] != "completed" or component["oom_kills"]:
                raise RunError("component failed or exceeded memory limit")
        result_paths = [directory / name
                        for name in receipt["output_sha256"]
                        if name.startswith(expected_adapter + "-")
                        and name.endswith(".json")]
        if len(result_paths) != 1:
            raise RunError("missing unique measured adapter result")
        report = read_json(result_paths[0])
        if (not report["success"]
                or report["manifest_sha256"] != data["sha256"]):
            raise RunError("failed or unmatched result")
        if bool(report.get("timing_only", False)) == full_protocol:
            raise RunError("incorrect correctness/timing-only boundary")
        if full_protocol and (not report["delta_checks"]
                              or not report["crash"]):
            raise RunError("full correctness phases missing")
        for build in report["builds"]:
            if build.get("cpu_ns", 0) <= 0:
                raise RunError("missing positive construction CPU")
        rows = []
        for name, expected in report.get("query_latency_sha256", {}).items():
            path = checked_file(directory / f"run-{report['run']:02d}", name)
            if digest(path) != expected:
                raise RunError("query latency hash mismatch")
            with path.open(encoding="utf-8") as source:
                rows.extend(json.loads(line) for line in source)
        if not rows:
            raise RunError("no hash-bound query timing samples")
        reports.append((report, latency_groups(rows)))
    if not reports:
        raise RunError("no terminal component receipts")
    return reports


def measure_adapter(config, output, label, adapter, data_label, deadline,
                    *, full_protocol):
    binary = config["binaries"][adapter]
    fixture = config["data"][data_label]
    binding_path = output / f"{label}.binding.json"
    run_dir = output / label
    command = [binary["path"], "-data", fixture["path"], "-out", str(run_dir),
               "-runs", "1" if full_protocol else str(config["runs"]),
               "-warmup=false", "-crash-trials", "12", "-safety-timeout",
               f"{config['safety_timeout_seconds']}s", "-binding",
               str(binding_path), "-cgroup-root", config["cgroup_root"]]
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
    run_command(command, output / f"{label}.log", deadline - time.monotonic())
    names = {"a": "sqlite-segment", "b": "pebble-bleve",
             "c": "parquet-base", "c_side": "parquet-side"}
    return collect_run(run_dir, names[adapter], fixture,
                       full_protocol=full_protocol)


def aggregate(reports, manifest):
    groups = {}
    cpu = []
    graph_bytes = []
    total_bytes = []
    inventories = []
    round_trip = True
    for report, samples in reports:
        for name, values in samples.items():
            groups.setdefault(name, []).extend(values)
        cpu.append(sum(build["cpu_ns"] for build in report["builds"]))
        graph = total = 0
        for build in report["builds"]:
            path = Path(build["ref"]["path"])
            total += build["artifact_bytes"]
            if report["adapter"].startswith("parquet-"):
                seal = read_json(path / "seal.json")
                round_trip = round_trip and seal["round_trip_verified"]
                categories = {"graph": (path / "seal.json").stat().st_size,
                              "vectors": 0, "text": 0}
                for item in seal["files"]:
                    categories[item["category"]] += item["size"]
                if sum(categories.values()) != build["artifact_bytes"]:
                    raise RunError("C artifact inventory is incomplete")
            else:
                # B's graph KV mixes vector/text primary records. Count the
                # whole directory, never subtract an estimated payload cost.
                text = sum(
                    (Path(current) / name).stat().st_size
                    for current, _, files in os.walk(path, followlinks=False)
                    if any(part.endswith(".bleve")
                           for part in Path(current).parts)
                    for name in files
                )
                categories = {"graph": build["artifact_bytes"] - text,
                              "vectors": None, "text": text}
            graph += categories["graph"]
            inventories.append({"repo": build["repo"], "run": report["run"],
                                "bytes": categories,
                                "total": build["artifact_bytes"]})
        graph_bytes.append(graph)
        total_bytes.append(total)
    if manifest["counts"]["edges"] <= 0:
        raise RunError("bytes/edge requires a non-empty edge fixture")
    return {
        "p95_ns": {name: percentile(values)
                   for name, values in groups.items()},
        "sample_counts": {name: len(values)
                          for name, values in groups.items()},
        "construction_cpu_ns": sorted(cpu)[len(cpu) // 2],
        "graph_serving_bytes": max(graph_bytes),
        "total_bytes": max(total_bytes),
        "bytes_per_edge": max(graph_bytes) / manifest["counts"]["edges"],
        "inventory": inventories,
        "full_payload_round_trip": (
            round_trip if reports[0][0]["adapter"].startswith("parquet-")
            else None),
        "counts": manifest["counts"], "repositories": manifest["repos"],
    }


def lookup_verdict(candidate, reference):
    ratios = {}
    for tier in ("tier1", "tier10"):
        for operation in ("id", "name", "file"):
            for kind in ("hit", "miss"):
                key = f"{operation}_{kind}"
                value = candidate[tier]["p95_ns"].get(key)
                base = reference[tier]["p95_ns"].get(key)
                if value is None or base is None or base <= 0:
                    raise RunError(f"missing lookup stratum: {tier}/{key}")
                ratios[f"{tier}/{key}"] = value / base
    return {"status": "pass" if max(ratios.values()) <= 1.5 else "miss",
            "target": "each lookup hit/miss p95 <= 1.5x same-host A",
            "measured_value": ratios}


def base_verdict(candidate, a, b):
    lookup = lookup_verdict(candidate, a)
    reads = {}
    observations = {}
    for tier in ("tier1", "tier10", "tier100"):
        metrics = candidate[tier]["p95_ns"]
        required = ["name", "symbols", "in_calls", "traverse2", "traverse3"]
        if tier != "tier1":
            required += ["multi_name", "multi_symbols", "multi_in_calls",
                         "multi_traverse2", "multi_traverse3"]
        for operation in required:
            if operation not in metrics or operation not in b[tier]["p95_ns"]:
                raise RunError(f"missing base read: {tier}/{operation}")
            observations[f"{tier}/{operation}"] = metrics[operation]
            if operation.startswith(("multi_", "traverse")):
                reads[f"{tier}/{operation}"] = (
                    metrics[operation] / b[tier]["p95_ns"][operation])
    disk = {tier: row["bytes_per_edge"] for tier, row in candidate.items()}
    passes = (lookup["status"] == "pass" and max(reads.values()) <= 1.0
              and max(disk.values()) <= 300)
    return {
        "passes": passes, "status": "pass" if passes else "miss",
        "lookup": lookup, "cross_repo_traversal_ratios": reads,
        "serving_bytes_per_edge": disk, "read_p95_ns": observations,
        "construction_cpu_ns": sum(row["construction_cpu_ns"]
                                   for row in candidate.values()),
        "total_bytes": sum(row["total_bytes"] for row in candidate.values()),
        "construction_scope": "storage-build CPU on neutral records, "
                              "equal adapter work",
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
    output = Path(config["output_dir"])
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
    os.environ["GOMAXPROCS"] = "4"
    os.environ["GOFLAGS"] = "-mod=readonly"
    os.environ["GOWORK"] = "off"
    deadline = time.monotonic() + config["wall_seconds"]
    receipt = {
        "schema": "design-c-base-result/v1", "status": "blocked",
        "target": "Checks 1, 2, 5", "host": socket.gethostname(),
        "source_revision": config["source_revision"],
        "config_sha256": digest(config_path),
        "script_sha256": digest(__file__),
        "approval_sha256": config["approval_sha256"],
        "recipe_sha256": config["recipe_sha256"],
        "binaries": config["binaries"], "data": config["data"],
        "fixture_metadata": {
            label: {
                "tier": read_json(Path(item["path"]) / "manifest.json"),
                "repositories": [
                    read_json(Path(item["path"]) / repo / "manifest.json")
                    for repo in read_json(
                        Path(item["path"]) / "manifest.json")["repos"]
                ],
            }
            for label, item in config["data"].items()
        },
        "memory_limits_bytes": config["memory_limits_bytes"],
        "wall_seconds": config["wall_seconds"],
        "whole_product_construction": {
            "status": "unmeasured", "target": "<= 0.50x same-host current "
            "main for equal whole cold-index work",
            "reason": "needs product integration (D18, stage 1)",
        },
        "operations_exercised": ["B private build", "S full-payload seal",
                                 "O immutable open", "G Reader queries"],
        "not_established": ["product catalog/daemon parity", "product P/T/R/C",
                            "GraphAr SDK/wire compatibility"],
    }
    try:
        results = {}
        for adapter in ("a", "b", "c"):
            measure_adapter(config, output, adapter + "-correctness", adapter,
                            "tiny", deadline, full_protocol=True)
            results[adapter] = {}
            for tier in ("tier1", "tier10", "tier100"):
                reports = measure_adapter(config, output, adapter + "-" + tier,
                                          adapter, tier, deadline,
                                          full_protocol=False)
                manifest = read_json(Path(config["data"][tier]["path"])
                                     / "manifest.json")
                results[adapter][tier] = aggregate(reports, manifest)
        check1 = lookup_verdict(results["c"], results["a"])
        selected = "c"
        if check1["status"] == "miss":
            selected = "c_side"
            measure_adapter(config, output, "c-side-correctness", selected,
                            "tiny", deadline, full_protocol=True)
            results[selected] = {}
            for tier in ("tier1", "tier10", "tier100"):
                reports = measure_adapter(
                    config, output, selected + "-" + tier,
                                          selected, tier, deadline,
                                          full_protocol=False)
                manifest = read_json(Path(config["data"][tier]["path"])
                                     / "manifest.json")
                results[selected][tier] = aggregate(reports, manifest)
        branch1 = ("direct_parquet" if selected == "c"
                   else "bounded_pebble_side")
        final_lookup = lookup_verdict(results[selected], results["a"])
        b = base_verdict(results["b"], results["a"], results["b"])
        c = base_verdict(results[selected], results["a"], results["b"])
        row, branch = choose_base(
            b, c, cpu_selection_approved=config[
                "construction_selection_approved"])
        receipt.update({
            "status": ("pass" if branch.startswith("recommend_") else
                       "miss" if row == "neither_passes" else
                       "blocked" if row == "comparison_unavailable" else
                       "partial"),
            "measured_value": results,
            "check1": {**final_lookup, "plain_parquet": check1,
                       "branch_taken": branch1},
            "check2": {"status": "pass", "target": "full tier1 payload "
                       "round trip and equal checker answers",
                       "measured_value": results[selected]["tier1"][
                           "full_payload_round_trip"]},
            "check5": {"b": b, "c": c, "selection_table_row": row,
                       "branch_taken": branch, "c_variant": selected},
            "branch_taken": branch,
        })
        if not results[selected]["tier1"]["full_payload_round_trip"]:
            raise RunError("full-payload round trip was not established")
    except (OSError, ValueError, RunError,
            subprocess.SubprocessError) as error:
        receipt["error"] = str(error)
        receipt["status"] = "blocked"
        receipt["branch_taken"] = "comparison_unavailable_stop"
        raise
    finally:
        try:
            receipt["protected_inputs_unchanged"] = (
                protected_snapshot(config, config_path) == before)
            if not receipt["protected_inputs_unchanged"]:
                receipt["status"] = "blocked"
                receipt["branch_taken"] = "protected_input_changed_stop"
            receipt["output_sha256"] = output_hashes(output)
        except (OSError, RunError) as error:
            receipt["status"] = "blocked"
            receipt["branch_taken"] = "evidence_collection_failed_stop"
            receipt["evidence_error"] = str(error)
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
