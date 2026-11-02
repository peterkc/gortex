"""Bind seam analysis and fixture tests to the native hard-limit launcher."""
import argparse
import hashlib
import json
import os
import resource
import shutil
import socket
import subprocess
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path


GIB = 1024 ** 3
PROFILES = {
    "pod": {"hard": 52 * GIB, "soft": "46GiB", "headroom": 4 * GIB,
            "file_max": 4 * GIB, "disk_headroom": 2 * GIB,
            "analysis": 1620, "filter": 360},
    "ci-fixture": {"hard": 2 * GIB, "soft": "1536MiB", "headroom": GIB,
                   "file_max": 16 * 1024 ** 2,
                   "disk_headroom": 128 * 1024 ** 2,
                   "analysis": 300, "filter": 120},
    "ci-oom": {"hard": 128 * 1024 ** 2, "soft": "64MiB",
               "headroom": 256 * 1024 ** 2,
               "file_max": 1024 ** 2, "disk_headroom": 128 * 1024 ** 2,
               "oom": 30},
}
# Production defaults are separate from explicitly selected test profiles.
HARD_LIMIT = PROFILES["pod"]["hard"]
HEADROOM = PROFILES["pod"]["headroom"]
SOFT_LIMIT = PROFILES["pod"]["soft"]
CI_JOB_MINUTES = 25
FIXTURE_FILES = ("go.mod", "go.sum", "seam.go")


def digest(path: Path) -> str:
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def fixture_digest(source: Path) -> str:
    if {p.name for p in source.iterdir()} != {*FIXTURE_FILES, "symbols.json"}:
        raise ValueError("fixture directory differs from its closed file set")
    files = {name: digest(source / name) for name in FIXTURE_FILES}
    encoded = json.dumps(files, sort_keys=True).encode()
    return hashlib.sha256(encoded).hexdigest()


def write_json(path: Path, value: object) -> None:
    with path.open("x", encoding="utf-8") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")


def host_capacity(meminfo: str, processors: int, profile: str = "pod") -> dict:
    limits = PROFILES[profile]
    entries = [line.split() for line in meminfo.splitlines()
               if line.startswith("MemTotal:")]
    if (len(entries) != 1 or len(entries[0]) != 3
            or entries[0][2] != "kB" or not entries[0][1].isdigit()
            or type(processors) is not int or processors < 1):
        raise ValueError("invalid MemTotal/nproc evidence")
    total = int(entries[0][1]) * 1024
    return {"profile": profile, "memtotal_bytes": total, "nproc": processors,
            "memory_max_bytes": limits["hard"],
            "headroom_bytes": limits["headroom"],
            "sufficient": total >= limits["hard"] + limits["headroom"]}


def preflight(output: Path, profile: str = "pod") -> None:
    limits = PROFILES[profile]
    meminfo = Path("/proc/meminfo").read_text()
    processors = int(subprocess.check_output(["nproc"], text=True).strip())
    capacity = host_capacity(meminfo, processors, profile)
    capacity["disk_free_bytes"] = shutil.disk_usage(output).free
    capacity["file_max_bytes"] = limits["file_max"]
    write_json(output / "host-capacity.json", capacity)
    (output / "meminfo.txt").write_text(meminfo)
    (output / "swaps.txt").write_text(Path("/proc/swaps").read_text())
    (output / "runner-cgroup.txt").write_text(
        Path("/proc/self/cgroup").read_text())
    with (output / "df.txt").open("x") as stream:
        subprocess.run(["df", "-Pk", str(output)], stdout=stream, check=True)
    validate_capacity(capacity, profile)
    if os.environ.get("GOMEMLIMIT") != limits["soft"]:
        raise RuntimeError("refuse: GOMEMLIMIT differs from fixed profile")


def validate_capacity(capacity: dict, profile: str) -> None:
    limits = PROFILES[profile]
    if (capacity.get("profile") != profile
            or capacity.get("memtotal_bytes", 0)
            < limits["hard"] + limits["headroom"]):
        raise RuntimeError("refuse: insufficient preflight MemTotal/profile")
    if capacity.get("disk_free_bytes", 0) < (
            2 * limits["file_max"] + limits["disk_headroom"]):
        raise RuntimeError("refuse: insufficient preflight free disk")


def facts(output: Path, root: Path, command: list[str],
          profile: str = "pod") -> None:
    limits = PROFILES[profile]
    membership = Path("/proc/self/cgroup").read_text().splitlines()
    rows = [row[3:] for row in membership if row.startswith("0::")]
    if len(rows) != 1:
        raise RuntimeError("expected one unified cgroup membership")
    scope = Path("/sys/fs/cgroup") / rows[0].lstrip("/")
    if scope.parent != root or not scope.name.startswith("gxstudy-external-"):
        raise RuntimeError("payload is not in its delegated launcher scope")
    values = {name: int((scope / name).read_text().strip()) for name in
              ("memory.max", "memory.swap.max", "memory.oom.group")}
    if values != {"memory.max": limits["hard"], "memory.swap.max": 0,
                  "memory.oom.group": 1}:
        raise RuntimeError("kernel memory limit readback mismatch")
    file_limits = resource.getrlimit(resource.RLIMIT_FSIZE)
    if file_limits != (limits["file_max"], limits["file_max"]):
        raise RuntimeError("kernel file-size limit readback mismatch")
    write_json(output, {"schema": "seam-kernel-limits/v1", **values,
                        "profile": profile, "file_max_bytes": file_limits[0],
                        "cgroup": str(scope), "argv": command,
                        "gomemlimit": os.environ.get("GOMEMLIMIT")})


def launch(args: argparse.Namespace) -> int:
    profile = getattr(args, "profile", "pod")
    limits = PROFILES[profile]
    capacity = json.loads((args.artifact / "host-capacity.json").read_text())
    validate_capacity(capacity, profile)
    if os.environ.get("GOMEMLIMIT") != limits["soft"]:
        raise RuntimeError("refuse: GOMEMLIMIT differs from fixed profile")
    label = args.label
    if label not in limits:
        raise RuntimeError("command label is not permitted by this profile")
    script = args.artifact / (label + "-command.sh")
    kernel_path = args.artifact / (label + "-kernel.json")
    # Explicit argv survives the native launcher's environment allowlist.
    body = ('set -euo pipefail\n'
            'python=$1; helper=$2; kernel=$3; root=$4; artifact=$5\n'
            'profile=$6; blocks=$7; shift 7\n'
            'ulimit -f "$blocks"\n'
            '"$python" -B "$helper" facts --output "$kernel" '
            '--root "$root" --profile "$profile" -- "$@"\n')
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
               str(args.root), str(args.artifact), profile,
               str(limits["file_max"] // 1024), *args.command]
    deadline = datetime.now(timezone.utc) + timedelta(seconds=limits[label])
    policy = args.artifact / (label + "-policy.json")
    write_json(policy, {"schema": "external-limits/v1", "deadline":
                        deadline.isoformat().replace("+00:00", "Z")})
    binding = args.artifact / (label + "-binding.json")
    write_json(binding, {
        "source_revision": args.commit,
        "harness_sha256": digest(args.launcher),
        "data_sha256": digest(script), "recipe_sha256": digest(policy),
        "host": socket.gethostname(), "command": command,
        # The external launcher runs only Engine; the schema requires all 3.
        "memory_limits_bytes": dict.fromkeys(
            ("runner", "checker", "engine"), limits["hard"]),
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
    run.add_argument("--label", choices=("analysis", "filter", "oom"),
                     required=True)
    for name in ("artifact", "launcher", "root", "cwd"):
        run.add_argument("--" + name, type=Path, required=True)
    run.add_argument("--commit", required=True)
    run.add_argument("command", nargs=argparse.REMAINDER)
    for command in (pre, child, run):
        command.add_argument("--profile", choices=PROFILES, default="pod")
    clean = modes.add_parser("cleanup")
    clean.add_argument("--root", type=Path, required=True)
    clean.add_argument("--identity", type=Path, required=True)
    clean.add_argument("--run-id", required=True)
    clean.add_argument("--attempt", required=True)
    modes.add_parser("budgets")
    args = parser.parse_args()
    if hasattr(args, "command"):
        if args.command[:1] == ["--"]:
            args.command = args.command[1:]
        if not args.command or not Path(args.command[0]).is_absolute():
            parser.error("absolute command and arguments required")
    if args.mode == "budgets":
        seconds = (PROFILES["ci-fixture"]["analysis"]
                   + PROFILES["ci-fixture"]["filter"]
                   + PROFILES["ci-oom"]["oom"] + 120)
        print("json=" + json.dumps({"pipeline_minutes": (seconds + 59) // 60,
                                    "job_minutes": CI_JOB_MINUTES}))
    elif args.mode == "preflight":
        preflight(args.output, args.profile)
    elif args.mode == "facts":
        facts(args.output, args.root, args.command, args.profile)
    elif args.mode == "cleanup":
        cleanup(args.root, args.identity, args.run_id, args.attempt)
    else:
        return launch(args)
    return 0


if __name__ == "__main__":
    sys.exit(main())
