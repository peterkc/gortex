#!/usr/bin/env python3
"""Fetch the unchanged public study pair. Execution is CI-only."""

from __future__ import annotations

import argparse
from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import socket
import subprocess
import sys
import tarfile
import urllib.request


RECORDS = (
    "nodes.jsonl", "edges.jsonl", "files.jsonl", "symbols.jsonl",
    "content.jsonl", "vectors.bin",
)


class FetchError(RuntimeError):
    """Refuse this attempt without rewriting an input or retrying."""


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def unique_object(pairs: list[tuple[str, object]]) -> dict:
    result = {}
    for key, value in pairs:
        if key in result:
            raise FetchError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def read_json(path: Path) -> dict:
    with path.open(encoding="utf-8") as stream:
        value = json.load(stream, object_pairs_hook=unique_object)
    if not isinstance(value, dict):
        raise FetchError(f"JSON object required: {path.name}")
    return value


def write_json(path: Path, value: dict) -> None:
    with path.open("x", encoding="utf-8") as stream:
        json.dump(value, stream, indent=2, allow_nan=False)
        stream.write("\n")


def expected_files(spec: dict) -> set[str]:
    return {"manifest.json", "queries.json", "expected.json"} | {
        f"{repo}/{name}"
        for repo in spec["repos"] for name in (*RECORDS, "manifest.json")
    }


def regular_files(root: Path) -> dict[str, Path]:
    files = {}
    for folder, directories, names in os.walk(root, followlinks=False):
        for name in (*directories, *names):
            path = Path(folder) / name
            if path.is_symlink():
                raise FetchError(f"symlink input: {path.name}")
        for name in names:
            path = Path(folder) / name
            if not path.is_file():
                raise FetchError(f"nonregular input: {path.name}")
            files[path.relative_to(root).as_posix()] = path
    return files


def verify_tier(root: Path, spec: dict, pins: dict) -> dict:
    if root.is_symlink() or not root.is_dir():
        raise FetchError("tier must be a real directory")
    files = regular_files(root)
    if files.keys() != expected_files(spec):
        raise FetchError("tier file inventory differs")
    manifest_path = root / "manifest.json"
    if sha256(manifest_path) != spec["manifest_sha256"]:
        raise FetchError("tier manifest hash differs")
    manifest = read_json(manifest_path)
    for key, expected in (
        ("generator_version", pins["generator_version"]),
        ("seed", pins["seed"]), ("repos", spec["repos"]),
        ("counts", spec["counts"]),
    ):
        if manifest.get(key) != expected:
            raise FetchError(f"tier manifest {key} differs")
    if manifest.get("cross_repo_policy"):
        raise FetchError("unexpected synthetic links in the unchanged pair")
    if set(manifest["sha256"]) != files.keys() - {"manifest.json"}:
        raise FetchError("tier checksum inventory differs")
    checked = {"manifest.json": spec["manifest_sha256"]}
    for name, expected in manifest["sha256"].items():
        if sha256(files[name]) != expected:
            raise FetchError(f"file hash differs: {name}")
        checked[name] = expected
    totals = {key: 0 for key in spec["counts"]}
    for repo in spec["repos"]:
        part = read_json(root / repo / "manifest.json")
        source = pins["sources"][repo]
        if checked[f"{repo}/manifest.json"] != spec["part_manifests"][repo]:
            raise FetchError("repository manifest pin differs")
        for key, expected in (
            ("repo", repo), ("namespace", repo), ("origin", "real"),
            ("generator_version", pins["generator_version"]),
            ("seed", pins["seed"]),
            ("source_revision", source["revision"]),
            ("source_sha256", source["source_sha256"]),
        ):
            if part.get(key) != expected:
                raise FetchError(f"repository {repo} {key} differs")
        if set(part["sha256"]) != set(RECORDS):
            raise FetchError("repository checksum inventory differs")
        for name in RECORDS:
            if part["sha256"][name] != checked[f"{repo}/{name}"]:
                raise FetchError("repository/tier checksum disagreement")
        for key in totals:
            totals[key] += part["counts"][key]
    total_bytes = sum(path.stat().st_size for path in files.values())
    if totals != spec["counts"] or total_bytes != spec["bytes"]:
        raise FetchError("tier count sum or byte total differs")
    return {
        "manifest_sha256": spec["manifest_sha256"], "counts": totals,
        "bytes": total_bytes, "files": len(files), "sha256": checked,
    }


def extract_tier(tar_path: Path, data: Path, tier: str, spec: dict) -> None:
    destination = data / tier
    if destination.exists() or destination.is_symlink():
        raise FetchError("existing tier output refused")
    allowed_files = {f"{tier}/{name}" for name in expected_files(spec)}
    allowed_dirs = {tier, *(f"{tier}/{repo}" for repo in spec["repos"])}
    seen = set()
    total = 0
    with tarfile.open(tar_path, "r:") as archive:
        members = []
        for member in archive:
            name = member.name.rstrip("/")
            if name in seen:
                raise FetchError("duplicate archive member")
            seen.add(name)
            if len(seen) > len(allowed_files) + len(allowed_dirs):
                raise FetchError("too many archive members")
            if PurePosixPath(name).as_posix() != name:
                raise FetchError("noncanonical archive path")
            if member.isdir() and name in allowed_dirs:
                members.append(member)
                continue
            if not member.isfile() or name not in allowed_files:
                raise FetchError("unsafe or unexpected archive member")
            total += member.size
            if total > spec["bytes"]:
                raise FetchError("archive expansion exceeds pinned bytes")
            members.append(member)
        if not allowed_files.issubset(seen) or total != spec["bytes"]:
            raise FetchError("incomplete archive inventory")
        archive.extractall(data, members=members, filter="data")


def decompress(archive: Path, tar_path: Path) -> None:
    # The CI runner's zstd rejects --output; use the short -o form.
    subprocess.run(
        ["zstd", "--decompress", "--keep", "-q", "-o", str(tar_path),
         str(archive)], check=True,
    )


def download(url: str, path: Path, spec: dict) -> None:
    partial = path.with_suffix(path.suffix + ".partial")
    total = 0
    with urllib.request.urlopen(url, timeout=60) as response:
        with partial.open("xb") as stream:
            while chunk := response.read(1 << 20):
                total += len(chunk)
                if total > spec["archive_bytes"]:
                    raise FetchError("download exceeds pinned bytes")
                stream.write(chunk)
    if total != spec["archive_bytes"]:
        raise FetchError("download length differs")
    if sha256(partial) != spec["archive_sha256"]:
        raise FetchError("download hash differs")
    partial.rename(path)


def worker(args: argparse.Namespace) -> None:
    if platform.system() != "Linux" or sys.version_info < (3, 12):
        raise FetchError("Linux and Python 3.12+ required; no Mac execution")
    if sha256(args.pins) != args.pins_sha256:
        raise FetchError("fixture pin file changed")
    if sha256(Path(__file__)) != args.controller_sha256:
        raise FetchError("fetch source changed")
    pins = read_json(args.pins)
    args.data_dir.mkdir()
    downloads = args.out / "downloads"
    downloads.mkdir()
    verified = {}
    for tier, spec in pins["tiers"].items():
        archive = downloads / spec["asset"]
        url = (
            "https://github.com/peterkc/gortex/releases/download/"
            f"{args.release_tag}/{spec['asset']}"
        )
        download(url, archive, spec)
        tar_path = downloads / f"{tier}.tar"
        # zstd and extraction run inside the launcher's process-tree cap.
        decompress(archive, tar_path)
        if tar_path.stat().st_size != spec["tar_bytes"]:
            raise FetchError("decompressed tar length differs")
        extract_tier(tar_path, args.data_dir, tier, spec)
        verified[tier] = verify_tier(args.data_dir / tier, spec, pins)
    write_json(args.out / "verified.json", verified)


def run(args: argparse.Namespace) -> None:
    if platform.system() != "Linux" or sys.version_info < (3, 12):
        raise FetchError("Linux and Python 3.12+ required; no Mac execution")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", args.release_tag):
        raise FetchError("explicit versioned release tag required")
    if args.release_tag == "latest":
        raise FetchError("moving latest tag refused")
    if not re.fullmatch(r"[0-9a-f]{40}", args.source_revision):
        raise FetchError("full reviewed source revision required")
    limits = {
        "runner": args.runner_memory_bytes,
        "checker": args.checker_memory_bytes,
        "engine": args.engine_memory_bytes,
    }
    if args.wall_seconds <= 0 or any(
        value <= 0 or value % os.sysconf("SC_PAGE_SIZE")
        for value in limits.values()
    ):
        raise FetchError("positive deadline and page-aligned caps required")
    if args.out.exists() or args.out.is_symlink():
        raise FetchError("existing output refused")
    if args.data_dir.parent != args.out or args.data_dir.name != "data":
        raise FetchError("data-dir must be the output directory's data child")
    args.out.mkdir()
    pins = Path(__file__).with_name("fixture-pins.json")
    controller = Path(__file__).resolve()
    evidence = {
        "schema": "design-c-fixture-input/v1",
        "source_revision": args.source_revision,
        "pins_sha256": sha256(pins),
        "controller_sha256": sha256(controller),
        "python_sha256": sha256(Path(sys.executable)),
        "python_version": sys.version,
        "approval_sha256": sha256(args.approval),
        "release_tag": args.release_tag,
    }
    write_json(args.out / "input.json", evidence)
    policy = args.out / "policy.json"
    deadline = (
        datetime.now(timezone.utc) + timedelta(seconds=args.wall_seconds)
    )
    write_json(policy, {
        "schema": "external-limits/v1", "deadline": deadline.isoformat(),
    })
    command = [
        str(Path(sys.executable).resolve()), str(controller), "worker",
        "--pins", str(pins), "--pins-sha256", evidence["pins_sha256"],
        "--controller-sha256", evidence["controller_sha256"],
        "--release-tag", args.release_tag, "--out", str(args.out),
        "--data-dir", str(args.data_dir),
    ]
    binding = args.out / "binding.json"
    write_json(binding, {
        "source_revision": args.source_revision,
        "harness_sha256": sha256(args.launcher),
        "data_sha256": sha256(args.out / "input.json"),
        "recipe_sha256": sha256(policy), "host": socket.gethostname(),
        "command": command, "memory_limits_bytes": limits,
    })
    environment = {"PATH": os.defpath, "LANG": "C.UTF-8"}
    for name, folder in (
        ("HOME", "home"), ("TMPDIR", "tmp"),
        ("XDG_CONFIG_HOME", "config"), ("XDG_DATA_HOME", "state"),
        ("XDG_CACHE_HOME", "cache"), ("XDG_STATE_HOME", "xdg-state"),
        ("XDG_RUNTIME_DIR", "runtime"),
    ):
        directory = args.out / folder
        directory.mkdir(mode=0o700)
        environment[name] = str(directory)
    argv = [
        str(args.launcher), "-binding", str(binding),
        "-binding-pin", sha256(binding), "-policy", str(policy),
        "-data", str(args.out / "input.json"),
        "-cgroup-root", str(args.cgroup_root),
        "-out", str(args.out / "limited"), "--", *command,
    ]
    write_json(args.out / "command.json", {"argv": argv})
    completed = subprocess.run(argv, env=environment, check=False)
    receipt_path = args.out / "limited/receipt.json"
    verified_path = args.out / "verified.json"
    evidence_error = ""
    try:
        receipt = read_json(receipt_path)
        verified = read_json(verified_path)
    except (OSError, FetchError, json.JSONDecodeError) as error:
        receipt, verified = {}, None
        evidence_error = str(error)
    accepted = (
        completed.returncode == 0 and receipt.get("state") == "completed"
        and receipt.get("removed") is True and verified is not None
    )
    outputs = {
        str(path.relative_to(args.out)): sha256(path)
        for path in (receipt_path, verified_path) if path.is_file()
    }
    write_json(args.out / "result.json", {
        "target": "unchanged public tier1/tier10 fixture identity",
        "measured_value": verified if accepted else None,
        "status": "pass" if accepted else "blocked",
        "branch": "reuse-unchanged-assets" if accepted else "stop-no-retry",
        "input_sha256": sha256(args.out / "input.json"),
        "output_sha256": outputs, "launcher_exit_code": completed.returncode,
        "evidence_error": evidence_error,
        "operations": "fixture preparation only; no daemon or target timing",
    })
    if not accepted:
        raise FetchError("attempt incomplete; keep partial evidence and stop")


def parser() -> argparse.ArgumentParser:
    cli = argparse.ArgumentParser(description=__doc__)
    modes = cli.add_subparsers(dest="mode", required=True)
    outer = modes.add_parser("run")
    for flag in ("release-tag", "source-revision"):
        outer.add_argument(f"--{flag}", required=True)
    for flag in ("launcher", "approval", "cgroup-root", "out", "data-dir"):
        outer.add_argument(f"--{flag}", type=Path, required=True)
    for flag in (
        "runner-memory-bytes", "checker-memory-bytes", "engine-memory-bytes",
        "wall-seconds",
    ):
        outer.add_argument(f"--{flag}", type=int, required=True)
    inner = modes.add_parser("worker")
    for flag in ("pins", "out", "data-dir"):
        inner.add_argument(f"--{flag}", type=Path, required=True)
    for flag in ("pins-sha256", "controller-sha256", "release-tag"):
        inner.add_argument(f"--{flag}", required=True)
    return cli


if __name__ == "__main__":
    arguments = parser().parse_args()
    for field in ("out", "data_dir", "launcher", "approval", "cgroup_root"):
        if hasattr(arguments, field):
            setattr(arguments, field, getattr(arguments, field).absolute())
    try:
        if arguments.mode == "worker":
            worker(arguments)
        else:
            run(arguments)
    except (FetchError, OSError, subprocess.SubprocessError) as error:
        sys.exit(str(error))
