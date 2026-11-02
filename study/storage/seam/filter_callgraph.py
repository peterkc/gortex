"""Filter pinned x/tools callgraph TSV edges. Run only on approved CI."""
import argparse
import hashlib
import json
import sys
from collections import defaultdict
from collections.abc import Iterable, Mapping, Sequence
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import TextIO

from run_limited import HARD_LIMIT, HEADROOM, SOFT_LIMIT


COMMIT = "7096db70df5ab7d83e20a28d9ccfd0994812acf9"
TREE = "829d2461eb61b73069b4206a137346fc338b02c6"
TOOL = "golang.org/x/tools/cmd/callgraph"
# Empty package/file fields preserve unresolved nodes instead of guessing.
FIELDS = (
    "{{.Caller}}", "{{if .Caller.Pkg}}{{.Caller.Pkg.Pkg.Path}}{{end}}",
    "{{(posn .Caller).Filename}}", "{{.Caller.Synthetic}}",
    "{{.Callee}}", "{{if .Callee.Pkg}}{{.Callee.Pkg.Pkg.Path}}{{end}}",
    "{{(posn .Callee).Filename}}", "{{.Callee.Synthetic}}",
    "{{.Filename}}", "{{.Line}}", "{{.Dynamic}}",
)
FORMAT = '{{printf "\\t"}}'.join(FIELDS)
OPTIONS = ["-algo=cha", "-test=true", "-tags=", "-cpuprofile=",
           "-memprofile=", "-format=" + FORMAT, "./..."]


@dataclass(frozen=True, slots=True)
class Invalid:
    reason: str


@dataclass(frozen=True, slots=True, order=True)
class Node:
    name: str
    package: str
    file: str
    synthetic: str


@dataclass(frozen=True, slots=True)
class Edge:
    caller: Node
    callee: Node
    file: str
    line: int
    dispatch: str


def filename(value: str, source: str) -> str:
    prefix = source.rstrip("/") + "/"
    return value[len(prefix):] if value.startswith(prefix) else value


@dataclass(frozen=True, slots=True)
class Frontier:
    edges: frozenset[Edge]
    symbols: Mapping[str, frozenset[Node]]
    total_edges: int
    packages: frozenset[str]
    raw_frontier_edges: int
    diagnostic_occurrences: int


def parse_edge(line: str, number: int, source: str) -> Edge | Invalid:
    fields = line.rstrip("\n").split("\t")
    if (len(fields) != 11 or not fields[0] or not fields[4]
            or not fields[9].isascii() or not fields[9].isdigit()
            or fields[10] not in ("static", "dynamic")):
        return Invalid(f"edge line {number}: malformed 11-field TSV")
    caller = Node(fields[0], fields[1], filename(fields[2], source), fields[3])
    callee = Node(fields[4], fields[5], filename(fields[6], source), fields[7])
    return Edge(caller, callee, filename(fields[8], source), int(fields[9]),
                fields[10])


def scan_frontier(stream: TextIO, names: Sequence[str], source: str,
                  diagnostics: TextIO) -> Frontier | Invalid:
    """Read all edges, but retain only the reverse depth-three frontier."""
    if not names or len(names) != len(set(names)):
        return Invalid("seam manifest: empty or duplicate symbols")
    symbols = {name: set() for name in names}
    retained, packages, seen, frontier = set(), set(), set(), set()
    total, raw_frontier, diagnostic_count = 0, 0, 0
    for depth in range(3):
        if depth and not frontier:
            break
        stream.seek(0)
        callers = set()
        for number, line in enumerate(stream, 1):
            edge = parse_edge(line, number, source)
            if isinstance(edge, Invalid):
                return edge
            if depth == 0:
                total += 1
                for node in (edge.caller, edge.callee):
                    if node.package:
                        packages.add(node.package)
                    if node.name in symbols:
                        symbols[node.name].add(node)
                    if not node.file or not node.package or node.synthetic:
                        diagnostic_count += 1
                        diagnostics.write(json.dumps({
                            "edge_line": number, "node": asdict(node),
                        }) + "\n")
            if ((depth == 0 and edge.callee.name in symbols)
                    or (depth > 0 and edge.callee in frontier)):
                retained.add(edge)
                raw_frontier += 1
                callers.add(edge.caller)
        seeds = {node for group in symbols.values() for node in group}
        seen.update(seeds)
        frontier = callers - seen
        seen.update(frontier)
    missing = [name for name, nodes in symbols.items() if not nodes]
    if missing:
        return Invalid("symbols absent from edge graph (not zero): "
                       + ", ".join(sorted(missing)))
    frozen_symbols = {name: frozenset(nodes)
                      for name, nodes in symbols.items()}
    return Frontier(frozenset(retained), frozen_symbols, total,
                    frozenset(packages), raw_frontier, diagnostic_count)


def caller_set(nodes: Iterable[Node]) -> dict:
    unique = sorted(set(nodes))
    groups = {"production": [], "test": [], "unresolved": []}
    for node in unique:
        if not node.package or not node.file:
            group = "unresolved"
        elif (node.file.endswith("_test.go")
              or node.package.endswith(("_test", ".test"))):
            group = "test"
        else:
            group = "production"
        groups[group].append(asdict(node))
    return {"count": len(unique), "nodes": [asdict(n) for n in unique],
            "files": sorted({n.file for n in unique if n.file}),
            "packages": sorted({n.package for n in unique if n.package}),
            "split": groups}


def analyze(graph: Frontier, symbols: Sequence[Mapping]) -> dict:
    names = {row["symbol"] for row in symbols}
    nodes = {node for edge in graph.edges
             for node in (edge.caller, edge.callee)}
    incoming = defaultdict(set)
    for edge in graph.edges:
        incoming[edge.callee].add(edge.caller)
    reports, all_direct, all_depth3 = {}, set(), set()
    for row in symbols:
        seeds = graph.symbols[row["symbol"]]
        direct = {caller for n in seeds for caller in incoming[n]}
        seen, frontier, levels = set(seeds), set(seeds), []
        for _ in range(3):
            frontier = {c for n in frontier for c in incoming[n]} - seen
            seen.update(frontier)
            levels.append(sorted(frontier))
        depth3 = seen - seeds
        all_direct.update(direct)
        all_depth3.update(depth3)
        reports[row["symbol"]] = {
            "why": row["why"], "direct": caller_set(direct),
            "depth3": caller_set(depth3),
            "by_shortest_depth": [caller_set(level) for level in levels],
        }
    selected = sorted((e for e in graph.edges if e.callee.name in names),
                      key=lambda e: (e.callee, e.caller, e.file, e.line,
                                     e.dispatch))
    return {
        "edge_counts": {"raw": graph.total_edges,
                        "frontier_unique": len(graph.edges),
                        "frontier_raw": graph.raw_frontier_edges,
                        "frontier_duplicates": graph.raw_frontier_edges
                        - len(graph.edges), "selected": len(selected)},
        "package_count": len(graph.packages),
        "diagnostic_endpoint_occurrences": graph.diagnostic_occurrences,
        "selected_edges": [asdict(e) for e in selected],
        "symbols": reports, "direct_union": caller_set(all_direct),
        "depth3_union": caller_set(all_depth3),
        "unresolved_nodes": [asdict(n) for n in sorted(nodes)
                             if not n.package or not n.file],
        "synthetic_nodes": [asdict(n) for n in sorted(nodes) if n.synthetic],
    }


def resource_facts(launcher: Mapping, kernel: Mapping,
                   host: Mapping) -> dict | Invalid:
    memory = launcher.get("memory", {})
    binding = launcher.get("binding", {})
    argv = kernel.get("argv", [])
    bound_command = binding.get("command", [])
    if (launcher.get("schema") != "external-command/v1"
            or launcher.get("state") != "completed"
            or launcher.get("removed") is not True
            or not launcher.get("deadline")
            or memory.get("status") != "completed"
            or memory.get("limit_bytes") != HARD_LIMIT
            or memory.get("oom_kills") != 0 or memory.get("exit_code") != 0
            or type(memory.get("cgroup_peak_bytes")) is not int
            or memory["cgroup_peak_bytes"] <= 0):
        return Invalid("analysis launcher did not complete within its limits")
    if (kernel.get("schema") != "seam-kernel-limits/v1"
            or kernel.get("memory.max") != HARD_LIMIT
            or kernel.get("memory.swap.max") != 0
            or kernel.get("memory.oom.group") != 1
            or kernel.get("gomemlimit") != SOFT_LIMIT
            or kernel.get("cgroup") != launcher.get("cgroup")):
        return Invalid("analysis kernel memory limit facts mismatch")
    if (not isinstance(argv, list) or len(argv) != 3 + len(OPTIONS)
            or argv[1:3] != ["run", TOOL] or argv[3:] != OPTIONS
            or not isinstance(bound_command, list)
            or bound_command[-len(argv):] != argv):
        return Invalid("analysis argv/options differ from launcher receipt")
    if (type(host.get("memtotal_bytes")) is not int
            or host["memtotal_bytes"] < HARD_LIMIT + HEADROOM
            or type(host.get("nproc")) is not int or host["nproc"] < 1):
        return Invalid("refuse: insufficient MemTotal or invalid nproc")
    return {"memory_max_bytes": kernel["memory.max"],
            "memory_swap_max_bytes": kernel["memory.swap.max"],
            "memory_oom_group": kernel["memory.oom.group"],
            "memory_peak_bytes": memory["cgroup_peak_bytes"],
            "oom_kills": memory["oom_kills"], "exit_code": memory["exit_code"],
            "deadline": launcher["deadline"], "gomemlimit": SOFT_LIMIT,
            "memtotal_bytes": host["memtotal_bytes"], "nproc": host["nproc"],
            "argv": argv, "options": argv[3:],
            "launcher_binary_sha256": binding.get("harness_sha256")}


def markdown(report: Mapping) -> str:
    rows = ["# Storage seam callers", "", "CHA is conservative, not runtime "
            "reachability. Depth 3 means shortest reverse paths of 1–3 edges.",
            "Self/cycle returns to a seed do not count as transitive callers.",
            "File/package unions use caller declarations, not call sites.",
            "Zero means the symbol appears in an edge but has no callers.",
            "Missing symbols fail. Isolated nodes cannot be proved by edges.",
            "x/tools deletes synthetic nodes before output; remaining "
            "synthetic and unresolved nodes are reported in JSON.", "",
            "## Run identity", "", "```json",
            json.dumps(report["identity"], indent=2), "```", "",
            "## Resources", "", "```json",
            json.dumps(report["resources"], indent=2), "```", "",
            "## Counts", "", json.dumps(report["edge_counts"]), "",
            "Raw/package counts cover the full graph. Unique/duplicate edge "
            "counts cover only the retained frontier. Full node diagnostics "
            "are streamed to node-diagnostics.jsonl.", "",
            "| Symbol | Direct | Depth 3 |", "| --- | ---: | ---: |"]
    for name, counts in report["symbols"].items():
        rows.append(f"| `{name}` | {counts['direct']['count']} | "
                    f"{counts['depth3']['count']} |")
    rows.extend(["", "## Unions and limits", ""])
    for name in ("direct_union", "depth3_union"):
        union = report[name]
        split = {key: len(value) for key, value in union["split"].items()}
        rows.append(f"- {name}: {union['count']} callers, "
                    f"{len(union['files'])} files, "
                    f"{len(union['packages'])} packages. Split: {split}.")
    for key in ("unresolved_nodes", "synthetic_nodes"):
        rows.append(f"- {key}: {len(report[key])} distinct frontier nodes; "
                    "full-graph occurrences in node-diagnostics.jsonl.")
    rows.extend("- " + text for text in report["non_callable_limits"])
    return "\n".join(rows) + "\n"


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--print-format", action="store_true")
    parser.add_argument("--edges", type=Path)
    parser.add_argument("--symbols", type=Path)
    parser.add_argument("--source", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--go-version")
    parser.add_argument("--commit")
    parser.add_argument("--tree")
    parser.add_argument("--launcher-receipt", type=Path)
    parser.add_argument("--kernel-limits", type=Path)
    parser.add_argument("--host-capacity", type=Path)
    parser.add_argument("--package-list", type=Path)
    args = parser.parse_args(argv)
    if args.print_format:
        print(FORMAT)
        return 0
    if any(getattr(args, name) is None for name in
           ("edges", "symbols", "source", "output", "go_version",
            "commit", "tree", "launcher_receipt", "kernel_limits",
            "host_capacity", "package_list")):
        parser.error("all analysis arguments are required")
    manifest = json.loads(args.symbols.read_text())
    expected = {"schema": "seam-symbols/v1", "commit": COMMIT, "tree": TREE}
    if (any(manifest.get(k) != v for k, v in expected.items())
            or args.commit != COMMIT or args.tree != TREE
            or args.go_version != "go1.27.3"):
        parser.error("source/toolchain/manifest identity mismatch")
    resources = resource_facts(
        json.loads(args.launcher_receipt.read_text()),
        json.loads(args.kernel_limits.read_text()),
        json.loads(args.host_capacity.read_text()))
    if isinstance(resources, Invalid):
        parser.error(resources.reason)
    with args.package_list.open(encoding="utf-8") as stream:
        listed_packages = sum(1 for line in stream if line.strip())
    if not listed_packages:
        parser.error("package inventory is empty")
    if args.output.exists():
        parser.error("output directory already exists")
    diagnostic_path = args.output.parent / "node-diagnostics.jsonl.partial"
    with args.edges.open(encoding="utf-8") as stream, \
            diagnostic_path.open("x", encoding="utf-8") as diagnostics:
        graph = scan_frontier(stream, [row["symbol"] for row in
                                      manifest["symbols"]],
                              str(args.source), diagnostics)
    if isinstance(graph, Invalid):
        parser.error(graph.reason)
    report = analyze(graph, manifest["symbols"])
    with args.edges.open("rb") as stream:
        raw_pin = hashlib.file_digest(stream, "sha256").hexdigest()
    report["identity"] = {
        "commit": COMMIT, "tree": TREE, "go": args.go_version,
        "tool": TOOL, "tool_version": "v0.52.0", "algorithm": "cha",
        "options": resources["options"], "raw_sha256": raw_pin,
        "symbols_sha256": hashlib.sha256(
            args.symbols.read_bytes()).hexdigest(),
        "upstream_synthetic_policy": "DeleteSyntheticNodes before output",
    }
    resources["launcher_receipt_sha256"] = hashlib.sha256(
        args.launcher_receipt.read_bytes()).hexdigest()
    resources["kernel_limits_sha256"] = hashlib.sha256(
        args.kernel_limits.read_bytes()).hexdigest()
    with args.package_list.open("rb") as stream:
        package_pin = hashlib.file_digest(stream, "sha256").hexdigest()
    report["listed_package_count"] = listed_packages
    report["listed_packages_sha256"] = package_pin
    report["resources"] = resources
    report["non_callable_limits"] = manifest["non_callable_limits"]
    report["assertion_sites"] = manifest["assertion_sites"]
    args.output.mkdir()  # A fresh directory refuses stale/replayed success.
    diagnostic_output = args.output / "node-diagnostics.jsonl"
    diagnostic_path.rename(diagnostic_output)
    with diagnostic_output.open("rb") as stream:
        diagnostic_pin = hashlib.file_digest(stream, "sha256").hexdigest()
    outputs = {"seam-callers.json": json.dumps(report, indent=2) + "\n",
               "summary.md": markdown(report)}
    for name, text in outputs.items():
        (args.output / name).write_text(text)
    receipt = {"state": "completed", "identity": report["identity"],
               "resources": resources,
               "package_count": report["package_count"],
               "listed_package_count": listed_packages,
               "listed_packages_sha256": package_pin,
               "total_edge_count": graph.total_edges,
               "frontier_edge_count": len(graph.edges),
               "frontier_raw_edge_count": graph.raw_frontier_edges,
               "frontier_duplicate_edge_count": graph.raw_frontier_edges
               - len(graph.edges),
               "diagnostic_endpoint_occurrences": graph.diagnostic_occurrences,
               "outputs": {name: hashlib.sha256(text.encode()).hexdigest()
                           for name, text in outputs.items()}}
    receipt["outputs"]["node-diagnostics.jsonl"] = diagnostic_pin
    (args.output / "receipt.json").write_text(
        json.dumps(receipt, indent=2) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
