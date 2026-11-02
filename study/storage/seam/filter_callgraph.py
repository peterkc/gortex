"""Filter pinned x/tools callgraph TSV edges. Run only on approved CI."""
import argparse
import hashlib
import json
import sys
from collections import defaultdict
from collections.abc import Iterable, Mapping, Sequence
from dataclasses import asdict, dataclass
from pathlib import Path


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


def parse_edges(lines: Iterable[str], source: str) -> list[Edge] | Invalid:
    edges = []
    for number, line in enumerate(lines, 1):
        fields = line.rstrip("\n").split("\t")
        if (len(fields) != 11 or not fields[0] or not fields[4]
                or not fields[9].isascii() or not fields[9].isdigit()
                or fields[10] not in ("static", "dynamic")):
            return Invalid(f"edge line {number}: malformed 11-field TSV")
        caller = Node(fields[0], fields[1], filename(fields[2], source),
                      fields[3])
        callee = Node(fields[4], fields[5], filename(fields[6], source),
                      fields[7])
        edges.append(Edge(caller, callee, filename(fields[8], source),
                          int(fields[9]), fields[10]))
    return edges


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


def analyze(edges: Sequence[Edge],
            symbols: Sequence[Mapping]) -> dict | Invalid:
    names = [row["symbol"] for row in symbols]
    if not names or len(names) != len(set(names)):
        return Invalid("seam manifest: empty or duplicate symbols")
    unique = set(edges)
    nodes = {node for edge in unique for node in (edge.caller, edge.callee)}
    missing = sorted(set(names) - {node.name for node in nodes})
    if missing:
        return Invalid("symbols absent from edge graph (not zero): "
                       + ", ".join(missing))
    incoming = defaultdict(set)
    for edge in unique:
        incoming[edge.callee].add(edge.caller)
    reports, all_direct, all_depth3 = {}, set(), set()
    for row in symbols:
        seeds = {n for n in nodes if n.name == row["symbol"]}
        direct = {caller for n in seeds for caller in incoming[n]}
        seen, frontier, levels = set(seeds), set(seeds), []
        for _ in range(3):
            frontier = {c for n in frontier for c in incoming[n]} - seen
            seen.update(frontier)
            levels.append(sorted(frontier))
        depth3 = set(seen) - seeds
        all_direct.update(direct)
        all_depth3.update(depth3)
        reports[row["symbol"]] = {
            "why": row["why"], "direct": caller_set(direct),
            "depth3": caller_set(depth3),
            "by_shortest_depth": [caller_set(level) for level in levels],
        }
    selected = sorted((e for e in unique if e.callee.name in names),
                      key=lambda e: (e.callee, e.caller, e.file, e.line,
                                     e.dispatch))
    return {
        "edge_counts": {"raw": len(edges), "unique": len(unique),
                        "duplicates": len(edges) - len(unique),
                        "selected": len(selected)},
        "selected_edges": [asdict(e) for e in selected],
        "symbols": reports, "direct_union": caller_set(all_direct),
        "depth3_union": caller_set(all_depth3),
        "unresolved_nodes": [asdict(n) for n in sorted(nodes)
                             if not n.package or not n.file],
        "synthetic_nodes": [asdict(n) for n in sorted(nodes) if n.synthetic],
    }


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
            "## Counts", "", json.dumps(report["edge_counts"]), "",
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
        rows.append(f"- {key}: {len(report[key])}; full list in JSON.")
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
    args = parser.parse_args(argv)
    if args.print_format:
        print(FORMAT)
        return 0
    if any(getattr(args, name) is None for name in
           ("edges", "symbols", "source", "output", "go_version",
            "commit", "tree")):
        parser.error("all analysis arguments are required")
    manifest = json.loads(args.symbols.read_text())
    expected = {"schema": "seam-symbols/v1", "commit": COMMIT, "tree": TREE}
    if (any(manifest.get(k) != v for k, v in expected.items())
            or args.commit != COMMIT or args.tree != TREE
            or args.go_version != "go1.27.3"):
        parser.error("source/toolchain/manifest identity mismatch")
    with args.edges.open(encoding="utf-8") as stream:
        edges = parse_edges(stream, str(args.source))
    if isinstance(edges, Invalid):
        parser.error(edges.reason)
    report = analyze(edges, manifest["symbols"])
    if isinstance(report, Invalid):
        parser.error(report.reason)
    with args.edges.open("rb") as stream:
        raw_pin = hashlib.file_digest(stream, "sha256").hexdigest()
    report["identity"] = {
        "commit": COMMIT, "tree": TREE, "go": args.go_version,
        "tool": TOOL, "tool_version": "v0.52.0", "algorithm": "cha",
        "options": OPTIONS, "raw_sha256": raw_pin,
        "symbols_sha256": hashlib.sha256(
            args.symbols.read_bytes()).hexdigest(),
        "upstream_synthetic_policy": "DeleteSyntheticNodes before output",
    }
    report["non_callable_limits"] = manifest["non_callable_limits"]
    report["assertion_sites"] = manifest["assertion_sites"]
    args.output.mkdir()  # A fresh directory refuses stale/replayed success.
    outputs = {"seam-callers.json": json.dumps(report, indent=2) + "\n",
               "summary.md": markdown(report)}
    for name, text in outputs.items():
        (args.output / name).write_text(text)
    receipt = {"state": "completed", "identity": report["identity"],
               "outputs": {name: hashlib.sha256(text.encode()).hexdigest()
                           for name, text in outputs.items()}}
    (args.output / "receipt.json").write_text(
        json.dumps(receipt, indent=2) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
