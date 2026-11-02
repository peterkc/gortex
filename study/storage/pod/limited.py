"""Bind one sequential external command to the existing native limiter."""
import argparse
import json
import resource
import socket
import subprocess
from pathlib import Path


from artifacts import digest


def write(path: Path, value: dict) -> None:
    with path.open("x") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    for key in ("launcher", "data", "cgroup-root", "output"):
        parser.add_argument("--" + key, type=Path, required=True)
    parser.add_argument("--source", required=True)
    parser.add_argument("--deadline", required=True)
    parser.add_argument("--memory-bytes", type=int, required=True)
    parser.add_argument("--file-bytes", type=int, required=True)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command
    if command and command[0] == "--":
        command = command[1:]
    if not command or not Path(command[0]).is_absolute():
        parser.error("absolute command argv required")
    args.output.mkdir(mode=0o700)
    policy = args.output / "policy.json"
    binding = args.output / "binding.json"
    write(policy, {"schema": "external-limits/v1",
                   "deadline": args.deadline})
    write(binding, {
        "source_revision": args.source, "host": socket.gethostname(),
        "harness_sha256": digest(args.launcher),
        "data_sha256": digest(args.data), "recipe_sha256": digest(policy),
        "command": command,
        "memory_limits_bytes": {
            "runner": 536870912, "checker": 536870912,
            "engine": args.memory_bytes},
    })
    if args.file_bytes < 1:
        parser.error("positive file byte limit required")
    resource.setrlimit(resource.RLIMIT_FSIZE,
                       (args.file_bytes, args.file_bytes))
    write(args.output / "file-limit.json", {
        "rlimit_fsize": resource.getrlimit(resource.RLIMIT_FSIZE)})
    # The native limiter validates, places and cleans up the child.
    result = subprocess.run([
        str(args.launcher), "-binding", str(binding), "-binding-pin",
        digest(binding), "-policy", str(policy), "-data", str(args.data),
        "-cgroup-root", str(args.cgroup_root), "-out",
        str(args.output / "launch"), "--", *command,
    ], check=False)
    return result.returncode


if __name__ == "__main__":
    raise SystemExit(main())
