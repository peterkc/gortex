"""Create private memory cgroups at the unified root, then move one PID."""
import argparse
import json
import os
import shlex
from pathlib import Path


ROOT = Path("/sys/fs/cgroup")
GIB = 1024 ** 3


def positive(value: str) -> int:
    try:
        number = int(value)
    except ValueError as error:
        raise argparse.ArgumentTypeError(
            "positive integer required") from error
    if number <= 0:
        raise argparse.ArgumentTypeError("positive integer required")
    return number


def private_name(value: str) -> str:
    if (not value or not value.isascii() or not value[0].isalnum()
            or any(not (part.isalnum() or part in "._-") for part in value)):
        raise argparse.ArgumentTypeError("private basename required")
    return value


def identity(path: Path) -> str:
    status = path.stat()
    return f"{status.st_dev}:{status.st_ino}"


def unified_root() -> Path:
    if ROOT.resolve() != ROOT:
        raise ValueError("unified root must not use a path alias")
    mounts = []
    for line in Path("/proc/self/mountinfo").read_text().splitlines():
        location, filesystem = line.split(" - ", 1)
        fields = location.split()
        if fields[4] == str(ROOT):
            mounts.append((fields[3], filesystem.split()[0]))
    # The real root lacks these non-root interfaces. A namespace/delegated
    # mount must not pass merely because its cgroup.type says "domain".
    if (mounts != [("/", "cgroup2")]
            or (ROOT / "memory.max").exists()
            or (ROOT / "cgroup.type").exists()):
        raise ValueError("true cgroup v2 root mount required")
    if "memory" not in (ROOT / "cgroup.controllers").read_text().split():
        raise ValueError("memory controller unavailable at unified root")
    return ROOT


def membership(pid: int, root: Path) -> tuple[str, Path]:
    text = Path(f"/proc/{pid}/cgroup").read_text()
    line = text.strip()
    if not line.startswith("0::/") or "\n" in line:
        raise ValueError("PID must use a unified cgroup v2 membership")
    group = root / line[4:]
    if (group.resolve() != group
            or (group != root and root not in group.parents)):
        raise ValueError("PID cgroup escaped the unified root")
    return text, group


def read_limits(path: Path) -> dict:
    return {field: (path / field).read_text().strip() for field in
            ("memory.max", "memory.swap.max", "memory.oom.group")}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pid", type=positive, required=True)
    parser.add_argument("--name", type=private_name, required=True)
    parser.add_argument("--receipt", type=Path, required=True)
    parser.add_argument("--environment", type=Path, required=True)
    parser.add_argument("--parent-bytes", type=positive, default=26 * GIB)
    parser.add_argument("--owner-bytes", type=positive, default=GIB // 2)
    parser.add_argument("--bootstrap-bytes", type=positive, default=4 * GIB)
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("root required")
    if args.receipt.resolve() == args.environment.resolve():
        parser.error("receipt and environment must be different files")
    root = unified_root()
    original, original_group = membership(args.pid, root)
    parent = root / args.name
    if parent.exists():
        raise FileExistsError(f"private cgroup already exists: {parent}")
    facts = {"pid": args.pid, "original_cgroup": original,
             "original_cgroup_path": str(original_group),
             "unified_root": str(root), "root_identity": identity(root),
             "capacity_cgroup": str(root), "groups": {},
             "state": "initial"}
    # Reserve outputs before cgroup changes. Preserve old evidence.
    with args.receipt.open("x") as receipt, \
            args.environment.open("x") as environment:
        try:
            (root / "cgroup.subtree_control").write_text("+memory\n")
            facts["root_subtree_control"] = (
                root / "cgroup.subtree_control").read_text().strip()
            if "memory" not in facts["root_subtree_control"].split():
                raise ValueError("root memory controller did not enable")
            parent.mkdir(mode=0o700)
            parent_identity = identity(parent)
            facts["groups"][str(parent)] = {"identity": parent_identity}
            # Publish cleanup ownership before any PID migration.
            values = {"CGROOT": str(parent), "CAPACITY_GROUP": str(root),
                      "IDENTITY": parent_identity}
            environment.write("".join(
                f"{key}={shlex.quote(value)}\n"
                for key, value in values.items()))
            environment.flush()
            os.fsync(environment.fileno())
            (parent / "memory.max").write_text(f"{args.parent_bytes}\n")
            (parent / "memory.swap.max").write_text("0\n")
            (parent / "cgroup.subtree_control").write_text("+memory\n")
            facts["parent_subtree_control"] = (
                parent / "cgroup.subtree_control").read_text().strip()
            if "memory" not in facts["parent_subtree_control"].split():
                raise ValueError("private memory controller did not enable")
            for name, limit in (("owner", args.owner_bytes),
                                ("bootstrap", args.bootstrap_bytes)):
                leaf = parent / name
                leaf.mkdir(mode=0o700)
                facts["groups"][str(leaf)] = {"identity": identity(leaf)}
                (leaf / "memory.max").write_text(f"{limit}\n")
                (leaf / "memory.swap.max").write_text("0\n")
                (leaf / "memory.oom.group").write_text("1\n")
            for group, limit in ((parent, args.parent_bytes),
                                 (parent / "owner", args.owner_bytes),
                                 (parent / "bootstrap", args.bootstrap_bytes)):
                observed = read_limits(group)
                facts["groups"][str(group)]["limits"] = observed
                expected = {"memory.max": str(limit), "memory.swap.max": "0"}
                if group != parent:
                    expected["memory.oom.group"] = "1"
                if any(observed[key] != value
                       for key, value in expected.items()):
                    raise ValueError(f"memory limits did not apply: {group}")
            facts["state"] = "configured"
            # Apply limits and save cleanup ownership before migration.
            (parent / "owner/cgroup.procs").write_text(f"{args.pid}\n")
            facts["state"] = "moved"
            after, group = membership(args.pid, root)
            facts["pid_cgroup_after"] = after
            if group != parent / "owner":
                raise ValueError("PID did not enter private owner leaf")
            facts["state"] = "complete"
        finally:
            json.dump(facts, receipt, indent=2)
            receipt.write("\n")
            receipt.flush()
            os.fsync(receipt.fileno())
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
