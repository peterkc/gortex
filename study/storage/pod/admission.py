"""Check local inputs before the single create."""
import hashlib
import io
import json
import math
import os
import subprocess
import tarfile
from collections.abc import Mapping
from pathlib import Path

from accuracy import FREEZE, REVISION, verify_proofs
from artifacts import MAX_FILE, MAX_MEMBERS, MAX_PAYLOAD, TAR_OVERHEAD, digest

HERE = Path(__file__).resolve().parent
# Frozen quotas: 900 short, 48 medium, 10 long. Width >2048 is singleton
# cpu_cost.batches uses a 4M quadratic budget.
# Short batches pad to <=2048.
# Three flat int64 arrays use <=30 bytes/value at indent=2.
# Add 2 MiB for metadata.
TOKEN_JSON_BOUND = ((900 * 2048 + 48 * 4096 + 10 * 8192) * 3 * 30
                    + 2 * 1024 ** 2)
TOOLS = {
    "go.tar.gz":
        "65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40",
    "uv.tar.gz":
        "f68d291c75783122dad58ab77def0e13f3233f358731db5d6d7e8715603b928f",
}
PATHS = {"payload_path": "payload_sha256", "session_path": "session_sha256",
         "environment_path": "environment_sha256"}


def upload_seconds(size: int, mbps: float) -> int:
    if size < 0 or not math.isfinite(mbps) or mbps <= 0:
        raise ValueError("invalid upload size/rate")
    return math.ceil(size * 8 / (mbps * 1_000_000) * 1.25) + 30


def payload_check(bundle: Path,
                  expected_sources: Mapping | None = None) -> None:
    if bundle.stat().st_size > MAX_PAYLOAD + TAR_OVERHEAD:
        raise ValueError("input archive byte bound exceeded")
    hashes, metadata, seen, total = {}, {}, set(), 0
    wanted = {"corpus/freeze.json", "corpus/freeze-canonical.json",
              "model-limit.json", "runtime/receipt.json",
              "harness/study/storage/pod-cost/cpu-cost-v1.json",
              "file-manifest.json"}
    with tarfile.open(bundle, "r:") as archive:
        for member in archive:
            name = Path(member.name)
            if (not member.isfile() or name.is_absolute()
                    or name.parts[:1] != ("evidence",)
                    or len(name.parts) < 2 or ".." in name.parts
                    or "\\" in member.name or name.as_posix() != member.name
                    or member.name in seen
                    or not 0 <= member.size <= MAX_FILE):
                raise ValueError("unsafe input archive member")
            seen.add(member.name)
            total += member.size
            if (len(seen) > MAX_MEMBERS + 1
                    or total > MAX_PAYLOAD + TAR_OVERHEAD):
                raise ValueError("input archive exceeds bounds")
            relative = str(Path(*name.parts[1:]))
            stream = archive.extractfile(member)
            if relative in wanted:
                if member.size > 16 * 1024 ** 2:
                    raise ValueError("oversized input metadata")
                raw = stream.read()
                metadata[relative] = json.loads(raw)
                stream = io.BytesIO(raw)
            value = hashlib.sha256()
            for chunk in iter(lambda: stream.read(1024 ** 2), b""):
                value.update(chunk)
            hashes[relative] = value.hexdigest()
    if any(hashes.get(name) != pin for name, pin in TOOLS.items()):
        raise ValueError("missing or changed pinned Go/uv archive")
    if expected_sources is not None and any(
            hashes.get("harness/" + name) != pin
            for name, pin in expected_sources.items()):
        raise ValueError("payload is not the complete reviewed harness export")
    manifest = metadata["file-manifest.json"]
    if manifest["files"] != {k: v for k, v in hashes.items()
                             if k != "file-manifest.json"}:
        raise ValueError("input manifest does not cover exact input bytes")
    freeze = metadata["corpus/freeze.json"]
    canonical = metadata["corpus/freeze-canonical.json"]
    if (hashes["corpus/freeze-canonical.json"] != FREEZE
            or freeze["freeze_id"] != FREEZE
            or canonical != {k: v for k, v in freeze.items()
                             if k != "freeze_id"}
            or any(hashes.get("corpus/" + name) != pin
                   for name, pin in freeze["files"].items())):
        raise ValueError("wrong or incomplete frozen corpus")
    pins = metadata["harness/study/storage/pod-cost/cpu-cost-v1.json"]
    if pins["model_revision"] != REVISION:
        raise ValueError("wrong model revision")
    for name, pin in pins["inputs"].items():
        relative = name.removeprefix("data/cpu-cost/")
        if (relative.startswith(("model/", "tokenizer/"))
                and hashes.get(relative) != pin):
            raise ValueError("wrong model/tokenizer input")
    verify_proofs(pins, metadata["model-limit.json"],
                  metadata["runtime/receipt.json"],
                  hashes["runtime/libonnxruntime.so.1.31.0"])


def validate(config: Mapping, *, destination_ready: bool = False) -> int:
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=HERE,
                                   text=True).strip()
    dirty = subprocess.check_output(
        ["git", "status", "--porcelain", "--untracked-files=all"],
        cwd=HERE, text=True)
    if head != config["harness_commit"] or dirty:
        raise ValueError("helper checkout must be clean and reviewed")
    for name, pin in PATHS.items():
        path = Path(config[name])
        if (str(path) != config[name] or not path.is_absolute()
                or path.resolve() != path
                or not path.is_file()):
            raise ValueError(
                "transport inputs must be canonical regular files")
        if (name == "payload_path"
                and path.stat().st_size > MAX_PAYLOAD + TAR_OVERHEAD):
            raise ValueError("input archive byte bound exceeded")
        if digest(path) != config[pin]:
            raise ValueError("transport hash differs from approval")
    key = Path(config["ssh_key_path"])
    if (str(key) != config["ssh_key_path"] or not key.is_absolute()
            or key.resolve() != key or not key.is_file()
            or key.stat().st_mode & 0o077):
        raise ValueError("SSH key must be a canonical private file")
    destination = Path(config["destination"])
    if (str(destination) != config["destination"]
            or not destination.is_absolute()
            or destination.resolve() != destination
            or not destination.parent.is_dir()
            or not os.access(destination.parent, os.W_OK)
            or destination == HERE.parents[2]
            or HERE.parents[2] in destination.parents):
        raise ValueError("invalid private durable copy destination")
    if destination_ready:
        if not destination.is_dir() or any(destination.iterdir()):
            raise ValueError("copy destination must still be empty")
    elif destination.exists():
        raise ValueError("copy destination must be fresh")
    if destination.exists() and destination.stat().st_mode & 0o077:
        raise ValueError("copy destination must be private")
    payload = Path(config["payload_path"])
    if payload.name != "inputs.tar":
        raise ValueError("payload must be inputs.tar")
    sidecar = payload.with_suffix(".tar.sha256").read_text()
    if sidecar != config["payload_sha256"] + "  inputs.tar\n":
        raise ValueError("input archive external hash receipt differs")
    roots = ["study/storage/" + name for name in
             ("harness", "onnx", "neon-eval", "pod-cost", "pod")]
    names = subprocess.check_output(["git", "ls-files", "--", *roots],
                                   cwd=HERE.parents[2],
                                   text=True).splitlines()
    if not names:
        raise ValueError("reviewed harness file set is empty")
    expected_sources = {}
    for name in names:
        source = HERE.parents[2] / name
        if source.resolve() != source or not source.is_file():
            raise ValueError("reviewed source uses a nonregular/aliased path")
        expected_sources[name] = digest(source)
    payload_check(payload, expected_sources)
    transport_bytes = sum(Path(config[name]).stat().st_size for name in PATHS)
    support_names = ("artifacts.py", "preflight.py", "cgroup_setup.py")
    transport_bytes += sum((HERE / name).stat().st_size
                           for name in support_names)
    transport_bytes += len(json.dumps(config).encode("utf-8")) + 4096
    seconds = upload_seconds(transport_bytes, config["minimum_upload_mbps"])
    if (config["ready_seconds"] + config["bootstrap_seconds"] + seconds
            > config["setup_seconds"]):
        raise ValueError("upload/readiness/build do not fit approved setup")
    if not (TOKEN_JSON_BOUND <= config["token_json_max_bytes"]
            <= 256 * 1024 ** 2):
        raise ValueError("prepared token JSON exceeds approved per-file cap")
    return seconds
