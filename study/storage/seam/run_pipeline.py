"""Run the pinned pipeline; CI uses only the closed fixture and OOM case."""
import argparse
import json
import os
import subprocess
import sys
from pathlib import Path

import filter_callgraph as cg
import run_limited as limited


HERE = Path(__file__).resolve().parent


def output(command: list[str], cwd: Path) -> str:
    return subprocess.check_output(command, cwd=cwd, text=True,
                                   timeout=15).strip()


def run(args: argparse.Namespace, label: str, command: list[str],
        artifact: Path, profile: str) -> int:
    return subprocess.run([
        sys.executable, "-B", str(HERE / "run_limited.py"), "run",
        "--profile", profile, "--label", label, "--artifact", str(artifact),
        "--launcher", str(args.launcher), "--root", str(args.root),
        "--cwd", str(args.source), "--commit", args.commit, "--", *command,
    ]).returncode


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--profile", choices=("pod", "ci-fixture"),
                        required=True)
    for name in ("source", "artifact", "launcher", "root", "go"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--tree", required=True)
    args = parser.parse_args()
    for name in ("source", "artifact", "launcher", "root", "go"):
        path = getattr(args, name)
        if not path.is_absolute() or path.resolve() != path:
            parser.error("absolute, resolved paths required")
    limited.validate_capacity(json.loads(
        (args.artifact / "host-capacity.json").read_text()), args.profile)
    version = output([str(args.go), "env", "GOVERSION"], args.source)
    if version != "go1.27.3":
        parser.error("Go version mismatch")
    tool_version = output([str(args.go), "list", "-m", "-mod=readonly", "-f",
                           "{{.Version}}", "golang.org/x/tools"], args.source)
    if tool_version != "v0.52.0":
        parser.error("x/tools in-module version mismatch")
    if output(["git", "rev-parse", "HEAD"], args.source) != args.commit:
        parser.error("source checkout commit mismatch")
    if args.profile == "pod":
        if (args.commit != cg.COMMIT or args.tree != cg.TREE
                or output(["git", "rev-parse", "HEAD^{tree}"], args.source)
                != cg.TREE
                or output(["git", "status", "--porcelain"], args.source)):
            parser.error("production source/tree/worktree mismatch")
        symbols = HERE / "seam_symbols.json"
    else:
        if args.tree != limited.fixture_digest(args.source):
            parser.error("fixture digest mismatch")
        symbols = args.source / "symbols.json"
    (args.artifact / "identity.txt").write_text(
        f"profile={args.profile}\ncommit={args.commit}\ntree={args.tree}\n"
        f"go={version}\nx/tools=v0.52.0\n")
    command = [str(args.go), "run", cg.TOOL, *cg.OPTIONS]
    limited.write_json(args.artifact / "requested-argv.json", command)
    status = run(args, "analysis", command, args.artifact, args.profile)
    if status:
        return status
    raw = args.artifact / "edges.tsv"
    (args.artifact / "edges.tsv.partial").rename(raw)
    report = args.artifact / "report"
    command = [sys.executable, "-B", str(HERE / "filter_callgraph.py"),
               "--profile", args.profile, "--edges", str(raw),
               "--symbols", str(symbols), "--source", str(args.source),
               "--output", str(report), "--go-version", version,
               "--commit", args.commit, "--tree", args.tree,
               "--launcher-receipt",
               str(args.artifact / "analysis-launch/receipt.json"),
               "--kernel-limits", str(args.artifact / "analysis-kernel.json"),
               "--host-capacity", str(args.artifact / "host-capacity.json"),
               "--package-list", str(args.artifact / "packages.txt")]
    status = run(args, "filter", command, args.artifact, args.profile)
    if status or args.profile == "pod":
        return status
    # Exercise real tool -> parser -> caller closure, not fabricated TSV.
    value = json.loads((report / "seam-callers.json").read_text())
    callers = value["symbols"]["example.com/seamfixture.Seam"]
    expected = {"example.com/seamfixture." + name
                for name in ("Direct", "Second", "Third")}
    if ({n["name"] for n in callers["depth3"]["nodes"]} != expected
            or callers["direct"]["count"] != 1):
        raise RuntimeError("real fixture caller closure mismatch")
    oom = args.artifact / "oom"
    oom.mkdir()
    previous = os.environ["GOMEMLIMIT"]
    try:
        os.environ["GOMEMLIMIT"] = limited.PROFILES["ci-oom"]["soft"]
        limited.preflight(oom, "ci-oom")
        status = run(args, "oom", [sys.executable, "-B",
                                   str(HERE / "oom_fixture.py")],
                     oom, "ci-oom")
    finally:
        os.environ["GOMEMLIMIT"] = previous
    receipt = json.loads((oom / "oom-launch/receipt.json").read_text())
    kernel = json.loads((oom / "oom-kernel.json").read_text())
    memory = receipt["memory"]
    if (not status or receipt["state"] != "oom" or not receipt["removed"]
            or memory["oom_kills"] < 1 or memory["cgroup_peak_bytes"] <= 0
            or memory["limit_bytes"] != limited.PROFILES["ci-oom"]["hard"]
            or kernel["memory.max"] != memory["limit_bytes"]
            or kernel["memory.swap.max"] != 0):
        raise RuntimeError("expected a real contained kernel OOM")
    limited.write_json(args.artifact / "ci-test-receipt.json", {
        "state": "completed", "evidence_scope": "CI fixture tests only",
        "not_evidence_for": ["FR-010", "AC-011"],
        "full_source_evidence": "study/storage/seam/POD-RUNBOOK.md",
        "fixture_sha256": args.tree,
        "report_receipt_sha256": limited.digest(report / "receipt.json"),
        "oom_receipt_sha256": limited.digest(oom / "oom-launch/receipt.json"),
    })
    return 0


if __name__ == "__main__":
    sys.exit(main())
