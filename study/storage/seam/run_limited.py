"""Bind seam commands to the existing, unchanged limited-command launcher."""
import argparse
import hashlib
import json
import os
import socket
import subprocess
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path


HARD_LIMIT = 12884901888
HEADROOM = 3 * 1024 ** 3
SOFT_LIMIT = "10GiB"


def digest(path: Path) -> str:
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def write_json(path: Path, value: object) -> None:
    with path.open("x", encoding="utf-8") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")


def host_capacity(meminfo: str, processors: int) -> dict:
    entries = [line.split() for line in meminfo.splitlines()
               if line.startswith("MemTotal:")]
    if (len(entries) != 1 or len(entries[0]) != 3
            or entries[0][2] != "kB" or not entries[0][1].isdigit()
            or type(processors) is not int or processors < 1):
        raise ValueError("invalid MemTotal/nproc evidence")
    total = int(entries[0][1]) * 1024
    return {"memtotal_bytes": total, "nproc": processors,
            "memory_max_bytes": HARD_LIMIT, "headroom_bytes": HEADROOM,
            "sufficient": total >= HARD_LIMIT + HEADROOM}


def preflight(output: Path) -> None:
    meminfo = Path("/proc/meminfo").read_text()
    processors = int(subprocess.check_output(["nproc"], text=True).strip())
    capacity = host_capacity(meminfo, processors)
    write_json(output / "host-capacity.json", capacity)
    (output / "meminfo.txt").write_text(meminfo)
    (output / "swaps.txt").write_text(Path("/proc/swaps").read_text())
    (output / "runner-cgroup.txt").write_text(
        Path("/proc/self/cgroup").read_text())
    if not capacity["sufficient"]:
        raise RuntimeError("refuse: MemTotal is below 12 GiB + 3 GiB")
    if os.environ.get("GOMEMLIMIT") != SOFT_LIMIT:
        raise RuntimeError("refuse: GOMEMLIMIT must be 10GiB")


def facts(output: Path, root: Path, command: list[str]) -> None:
    membership = Path("/proc/self/cgroup").read_text().splitlines()
    rows = [row[3:] for row in membership if row.startswith("0::")]
    if len(rows) != 1:
        raise RuntimeError("expected one unified cgroup membership")
    scope = Path("/sys/fs/cgroup") / rows[0].lstrip("/")
    if scope.parent != root or not scope.name.startswith("gxstudy-external-"):
        raise RuntimeError("payload is not in its delegated launcher scope")
    values = {name: int((scope / name).read_text().strip()) for name in
              ("memory.max", "memory.swap.max", "memory.oom.group")}
    if values != {"memory.max": HARD_LIMIT, "memory.swap.max": 0,
                  "memory.oom.group": 1}:
        raise RuntimeError("kernel memory limit readback mismatch")
    write_json(output, {"schema": "seam-kernel-limits/v1", **values,
                        "cgroup": str(scope), "argv": command,
                        "gomemlimit": os.environ.get("GOMEMLIMIT")})


def launch(args: argparse.Namespace) -> int:
    capacity = json.loads((args.artifact / "host-capacity.json").read_text())
    if capacity.get("memtotal_bytes", 0) < HARD_LIMIT + HEADROOM:
        raise RuntimeError("refuse: insufficient preflight MemTotal")
    if os.environ.get("GOMEMLIMIT") != SOFT_LIMIT:
        raise RuntimeError("refuse: GOMEMLIMIT must be 10GiB")
    label = args.label
    script = args.artifact / (label + "-command.sh")
    kernel_path = args.artifact / (label + "-kernel.json")
    # All paths reach the allowlisted child through argv, not GitHub env.
    body = ('set -euo pipefail\n'
            'python=$1; helper=$2; kernel=$3; root=$4; artifact=$5\n'
            'shift 5\n'
            '"$python" -B "$helper" facts --output "$kernel" '
            '--root "$root" -- "$@"\n')
    if label == "analysis":
        body += ("\"$1\" list -deps -test -tags= -f '{{.ImportPath}}' ./... "
                 '> "$artifact/packages.txt" '
                 '2> "$artifact/package-list.stderr"\n'
                 'count=$(wc -l < "$artifact/packages.txt")\n'
                 'printf "listed_package_count=%s\\n" "$count" '
                 '>> "$artifact/receipt.txt"\n'
                 '"$@" 2> "$artifact/callgraph.stderr" '
                 '| cat > "$artifact/edges.tsv.partial"\n')
    else:
        body += 'exec "$@"\n'
    with script.open("x") as stream:
        stream.write(body)
    command = ["/usr/bin/bash", "--noprofile", "--norc", str(script),
               sys.executable, str(Path(__file__).resolve()), str(kernel_path),
               str(args.root), str(args.artifact), *args.command]
    seconds = 28 * 60 if label == "analysis" else 4 * 60
    deadline = datetime.now(timezone.utc) + timedelta(seconds=seconds)
    policy = args.artifact / (label + "-policy.json")
    write_json(policy, {"schema": "external-limits/v1", "deadline":
                        deadline.isoformat().replace("+00:00", "Z")})
    binding = args.artifact / (label + "-binding.json")
    write_json(binding, {
        "source_revision": args.commit,
        "harness_sha256": digest(args.launcher),
        "data_sha256": digest(script), "recipe_sha256": digest(policy),
        "host": socket.gethostname(), "command": command,
        # RunExternal launches only Engine; its shared schema requires all 3.
        "memory_limits_bytes": dict.fromkeys(
            ("runner", "checker", "engine"), HARD_LIMIT),
    })
    result = subprocess.run([
        str(args.launcher), "-binding", str(binding), "-binding-pin",
        digest(binding), "-policy", str(policy), "-data", str(script),
        "-cgroup-root", str(args.root), "-out",
        str(args.artifact / (label + "-launch")), "--", *command,
    ], cwd=args.cwd)
    return result.returncode


def cleanup(root: Path, identity: Path, run_id: str, attempt: str) -> None:
    expected = Path("/sys/fs/cgroup") / f"gxseam-{run_id}-{attempt}"
    entry = root.lstat()
    expected_id = tuple(map(int, identity.read_text().split()))
    if (root != expected or root.resolve() != root or not root.is_dir()
            or (entry.st_dev, entry.st_ino) != expected_id):
        raise RuntimeError("refuse cleanup of replaced/unowned cgroup root")
    if any(row == "0::/" + str(root.relative_to("/sys/fs/cgroup"))
           or row.startswith("0::/" + str(root.relative_to("/sys/fs/cgroup"))
                             + "/")
           for row in Path("/proc/self/cgroup").read_text().splitlines()):
        raise RuntimeError("cleanup must run outside the delegated root")
    # The launcher owns child cleanup. Never force-remove an unexpected scope.
    for path in (root / "owner", root):
        path.rmdir()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_subparsers(dest="mode", required=True)
    pre = modes.add_parser("preflight")
    pre.add_argument("--output", type=Path, required=True)
    child = modes.add_parser("facts")
    child.add_argument("--output", type=Path, required=True)
    child.add_argument("--root", type=Path, required=True)
    child.add_argument("command", nargs=argparse.REMAINDER)
    run = modes.add_parser("run")
    run.add_argument("--label", choices=("analysis", "filter"), required=True)
    for name in ("artifact", "launcher", "root", "cwd"):
        run.add_argument("--" + name, type=Path, required=True)
    run.add_argument("--commit", required=True)
    run.add_argument("command", nargs=argparse.REMAINDER)
    clean = modes.add_parser("cleanup")
    clean.add_argument("--root", type=Path, required=True)
    clean.add_argument("--identity", type=Path, required=True)
    clean.add_argument("--run-id", required=True)
    clean.add_argument("--attempt", required=True)
    args = parser.parse_args()
    if hasattr(args, "command"):
        if args.command[:1] == ["--"]:
            args.command = args.command[1:]
        if not args.command or not Path(args.command[0]).is_absolute():
            parser.error("absolute command and arguments required")
    if args.mode == "preflight":
        preflight(args.output)
    elif args.mode == "facts":
        facts(args.output, args.root, args.command)
    elif args.mode == "cleanup":
        cleanup(args.root, args.identity, args.run_id, args.attempt)
    else:
        return launch(args)
    return 0


if __name__ == "__main__":
    sys.exit(main())
