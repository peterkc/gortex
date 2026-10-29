#!/usr/bin/false
"""Storage study: 3.10+ fixtures; pinned CPython 3.14.7 native launcher."""

from __future__ import annotations

import argparse
from collections import Counter, deque
from collections.abc import Callable, Mapping, Sequence
from dataclasses import asdict, dataclass, field
from datetime import datetime
from functools import cache
import ctypes
import fcntl
import errno
import hashlib
import json
import math
import os
import platform
from pathlib import Path, PurePosixPath
import re
import shlex
import shutil
import signal
import statistics
import subprocess
import sys
import tarfile
import time
from typing import BinaryIO, Literal
import urllib.request
import uuid


COMMIT = "5bab54d2dd8b7165f7b813f9644857f694e28ede"
REPOSITORY = "https://github.com/peterkc/gortex.git"
BRANCH = "study/storage-harness"
USEARCH_SHA = (
    "96f1ed59f32303e612b2e72c393475696743b49e0832d33e65ad97aacf61d655"
)
USEARCH_URL = (
    "https://codeload.github.com/unum-cloud/USearch/tar.gz/refs/tags/v2.26.5"
)
GIB = 1024 ** 3
MEMORY_RESERVE = 16 * GIB
NODE_SCALE = 423567 / 81179
ARCHIVES = {
    "tier1": {
        "sha256": (
            "5ac6b1e83fc04b6e021c7a78ccdf52d002e06555f657ba1e9da02dd7b616b3fb"
        ),
        "bytes": 36857489,
        "queries": 694,
    },
    "tier10": {
        "sha256": (
            "5f2158772b3ebba975935d38cf06c52aab6a9dacc382bb3c31e08a090bd61c2c"
        ),
        "bytes": 143185077,
        "queries": 811,
    },
}
# Observed full-protocol Mac seconds / max(native, sampled-group) RSS bytes.
OBSERVED = {
    "oracle": (540.8674687, 17201381376),
    "sqlite-segment": (155.85307125, 3881385984),
    "libsql-segment": (681.61018054, 4211392512),
    "pebble-bleve": (214.78344771, 4745297920),
    "bbolt-bleve": (406.79129138, 4958240768),
    "badger-bleve": (91.69543958, 8472576000),
    "catalog-sqlite": (237.23913246, 15718940672),
    "catalog-refs": (232.38907992, 15452913664),
    "catalog-bbolt": (235.34039921, 16237494272),
    "catalog-pebble": (232.83216362, 16478257152),
    "catalog-manifest": (220.90304475, 16282402816),
    "sqlite-vecflat": (147.48933262, 3927752704),
    "sqlite-usearch": (154.86282621, 3905617920),
}
ADAPTERS = tuple(name for name in OBSERVED if name != "oracle")
SCREEN = tuple(name for name in ADAPTERS if name != "libsql-segment")
FINALISTS = ("sqlite-segment", "pebble-bleve")
CATALOGS = ("catalog-sqlite", "catalog-refs", "catalog-bbolt",
            "catalog-pebble", "catalog-manifest")
TIMING_BLOCKS = {"segment_timing", "catalog", "hybrid", "cpu_cost", "baseline_a7"}
PYTHON_KEY = "cpython-3.12.15-linux-x86_64-gnu"
STAGES = {
    "before-write", "after-write", "after-sync", "before-rename",
    "after-rename", "after-dirsync",
}
SKIPPED = ["delta", "concurrent_readers", "commit_race", "crash_trials"]
DELTA_IDS = {
    "delta-file", "delta-old-name", "delta-new-name", "delta-removed-id",
    "delta-added-id", "delta-removed-out", "delta-added-out",
    "delta-renamed-out", "delta-added-in", "delta-dangling-in",
    "delta-symbol", "delta-content", "delta-vector",
}
WINDOW_CLEANUP_SECONDS = 15 * 60
CASE_MINUTES = {"setup": 120, "smoke": 60, "evaluator": 30, "oracle": 30,
                "screen": 30, "segment": 45, "catalog": 15, "hybrid": 60,
                "coderank": 60, "baseline": 45}
BLOCK_KINDS = {"setup": "setup", "smoke": "smoke", "calibration": "evaluator",
               "segment_timing": "segment", "catalog": "catalog",
               "hybrid": "hybrid", "cpu_cost": "coderank",
               "baseline_a7": "baseline"}


class ReviewRequired(RuntimeError):
    """Stop without retrying or deleting evidence."""


class FatalSession(ReviewRequired):
    """Process, isolation or shared-resource failure; stop all blocks."""


INPUT_BLOCKS = frozenset({"smoke", "calibration", "cpu_cost",
                          "baseline_a7"})
CLEANUP_GRACE_SECONDS = 5.0
CLEANUP_RESERVE_SECONDS = 2 * CLEANUP_GRACE_SECONDS + 1


class DeadlineExceeded(ReviewRequired):
    """The owned work must stop; acceptance remains incomplete."""


def sha_pin(value: object, lengths: tuple[int, ...]) -> bool:
    return (isinstance(value, str) and len(value) in lengths
            and all(c in "0123456789abcdef" for c in value))


def owned_path(value: object, root: Path) -> bool:
    return (isinstance(value, str) and "\\" not in value
            and not any(ord(c) < 32 for c in value)
            and Path(value).is_absolute() and ".." not in Path(value).parts
            and Path(value).is_relative_to(root))


def bound_jobs(root: Path) -> dict[str, list[dict]]:
    result = {
        "setup": [{"id": "setup", "argv": ["run.py:prepare", str(root)],
                   "minutes": 120,
                   "receipt_path": str(root / "setup-complete.json")}],
        "storage": [{"id": job.name, "argv": command(job, root),
                     "minutes": 30,
                     "receipt_path": str(
                         root / "jobs" / job.name / "complete.json")}
                    for job in jobs() if job.stage != "timing"],
        "segment_timing": [{"id": job.name, "argv": command(job, root),
                            "minutes": 45,
                            "receipt_path": str(
                                root / "jobs" / job.name / "complete.json")}
                           for job in jobs() if job.stage == "timing"],
        "catalog": [], "hybrid": [],
    }
    for candidate in CATALOGS:
        for entries in (1, 10, 100):
            name = f"catalog-{candidate}-n{entries}"
            out = root / "aux" / name / "output"
            result["catalog"].append({
                "id": name, "minutes": 15,
                "argv": [str(root / "bin" / candidate), "catalog-bench",
                         "-candidate", candidate, "-entries", str(entries),
                         "-repeats", "30", "-seed", "20261025",
                         "-out", str(out)],
                "receipt_path": str(out.parent / "complete.json"),
            })
    for mode in ("timing", "latency"):
        name = "hybrid-" + mode
        argv = [str(root / "python" / PYTHON_KEY / "bin/python3.12"), "-B",
                str(root / "checkout/study/storage/vector"
                    / "hybrid-followup/run.py"),
                mode, "--execute", "--run-id", "pod-A1-" + mode, "--jobs-file",
                str(root / "aux" / name / "selected-jobs.json")]
        if mode == "latency":
            argv += ["--generated", "--native-dir", str(root / "native")]
        result["hybrid"].append({
            "id": name, "minutes": 60, "argv": argv,
            "selected_jobs": hybrid_names(mode),
            "receipt_path": str(root / "aux" / name / "complete.json"),
        })
    for block, entries in result.items():
        for job in entries:
            job.update(source_pin=COMMIT, config_pin=hashlib.sha256(
                json.dumps(job["argv"], separators=(",", ":")).encode()
            ).hexdigest(), input_pin=hashlib.sha256(
                json.dumps({"archives": ARCHIVES if block in {
                    "setup", "storage", "segment_timing"} else {},
                    "generated_seed": (
                        20261025 if block == "catalog" else 20261023),
                    "selected_jobs": job.get("selected_jobs", [])},
                    sort_keys=True).encode()
            ).hexdigest(), owner="study-harness")
    return result


def preflight(bindings: Mapping, root: Path) -> dict:
    """Validate prospective inputs without a pod, host probe or side effect."""
    if not isinstance(bindings, Mapping):
        raise ReviewRequired("bindings: malformed object")
    if set(bindings) != INPUT_BLOCKS:
        missing = sorted(INPUT_BLOCKS - set(bindings))
        extra = sorted(set(bindings) - INPUT_BLOCKS)
        raise ReviewRequired(f"bindings: missing {missing}; unlisted {extra}")
    inventory = bound_jobs(root)
    for block in sorted(INPUT_BLOCKS):
        entries = bindings[block]
        if not isinstance(entries, list) or not entries:
            raise ReviewRequired(f"{block}: missing jobs")
        if any(not isinstance(entry, Mapping) for entry in entries):
            raise ReviewRequired(f"{block}: malformed job")
        inventory[block] = [dict(entry) for entry in entries]
        seen = set()
        for entry in inventory[block]:
            fields = {"id", "argv", "source_pin", "config_pin", "input_pin",
                      "receipt_path", "minutes", "owner"}
            if set(entry) - fields:
                raise ReviewRequired(f"{block}: unlisted binding field")
            for key in sorted(fields):
                if key not in entry:
                    raise ReviewRequired(f"{block}: missing {key}")
            if (not isinstance(entry["id"], str) or not entry["id"]
                    or len(entry["id"]) > 128
                    or not entry["id"][0].isalnum()
                    or any(not (c.isascii() and (c.isalnum() or c in "-_."))
                           for c in entry["id"])
                    or entry["id"] in seen):
                raise ReviewRequired(f"{block}: malformed or duplicate id")
            seen.add(entry["id"])
            argv = entry["argv"]
            if (not isinstance(argv, list) or not argv
                    or any(not isinstance(arg, str) or not arg
                           or any(ord(c) < 32 for c in arg) for arg in argv)):
                raise ReviewRequired(f"{block}: malformed argv")
            for key, lengths in (("source_pin", (40, 64)),
                                 ("config_pin", (64,)), ("input_pin", (64,))):
                if not sha_pin(entry[key], lengths):
                    raise ReviewRequired(f"{block}: malformed {key}")
            if not owned_path(entry["receipt_path"], root):
                raise ReviewRequired(f"{block}: malformed receipt_path")
            minutes = entry["minutes"]
            if (type(minutes) not in (int, float) or not math.isfinite(minutes)
                    or minutes * 60 <= CLEANUP_RESERVE_SECONDS):
                raise ReviewRequired(f"{block}: malformed minutes")
            if (not isinstance(entry["owner"], str)
                    or not entry["owner"].strip()):
                raise ReviewRequired(f"{block}: malformed owner")
    all_ids = [job["id"] for entries in inventory.values() for job in entries]
    all_receipts = [job["receipt_path"] for entries in inventory.values()
                    for job in entries]
    if (len(set(all_ids)) != len(all_ids)
            or len(set(all_receipts)) != len(all_receipts)):
        raise ReviewRequired("bindings: duplicate job id or receipt_path")
    return {
        "phase1": "prepared-no-pod", "source": COMMIT,
        "enforcement_entrypoint": "study/storage/pod/run.py:main",
    }


def timestamp(value: object, label: str) -> float:
    return parse_time(value, label).timestamp()


def parse_time(value: object, label: str) -> datetime:
    """Parse an aware ISO time on Python 3.10 (Z and any fraction length)."""
    if not isinstance(value, str):
        raise ReviewRequired(f"{label}: missing timestamp")
    normalized = value[:-1] + "+00:00" if value.endswith("Z") else value
    fraction = re.fullmatch(
        r"(.*[T ]\d{2}:\d{2}:\d{2})\.(\d+)([+-]\d{2}:\d{2})", normalized)
    if fraction:
        clock, digits, zone = fraction.groups()
        normalized = f"{clock}.{digits[:6]:0<6}{zone}"
    try:
        parsed = datetime.fromisoformat(normalized)
    except ValueError as error:
        raise ReviewRequired(f"{label}: malformed timestamp") from error
    if parsed.utcoffset() is None:
        raise ReviewRequired(f"{label}: timezone required")
    return parsed


@dataclass(frozen=True, slots=True)
class Job:
    stage: Literal["oracle", "screen", "timing"]
    adapter: str
    tier: str
    repeat: int = 1

    @property
    def name(self) -> str:
        return f"{self.stage}-{self.tier}-{self.adapter}-r{self.repeat}"


def jobs() -> tuple[Job, ...]:
    priority = ("sqlite-segment", "pebble-bleve", "sqlite-vecflat",
                "sqlite-usearch", "catalog-sqlite", "catalog-refs")
    screen = (*priority, *(name for name in SCREEN if name not in priority))
    return (
        Job("oracle", "oracle", "tier10"),
        *(Job("screen", name, "tier10") for name in screen),
        *(Job("timing", name, tier, repeat)
          for repeat in range(1, 4)
          for tier in ARCHIVES for name in FINALISTS),
    )


def command(job: Job, root: Path) -> list[str]:
    args = [
        str(root / "bin" / job.adapter), "-data",
        str(root / "data" / job.tier), "-out",
        str(root / "jobs" / job.name / "output"), "-runs", "1",
        "-warmup=false", "-crash-trials", "12", "-safety-timeout", "15m",
    ]
    if job.stage == "timing":
        args.append("-timing-only")
    return args


def projected_memory(job: Job) -> int:
    scale = NODE_SCALE if job.tier == "tier10" else 1
    return math.ceil(OBSERVED[job.adapter][1] * scale * 1.3)


def digest(path: Path) -> str:
    hasher = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            hasher.update(chunk)
    return hasher.hexdigest()


def unique_keys(pairs: Sequence[tuple[str, object]]) -> dict:
    parsed = {}
    for key, value in pairs:
        if key in parsed:
            raise ReviewRequired(f"duplicate JSON key: {key}")
        parsed[key] = value
    return parsed


def reject_constant(value: str) -> None:
    raise ReviewRequired(f"non-finite JSON constant: {value}")


def read_json(path: Path) -> object:
    if path.stat().st_size > 512 * 1024 * 1024:
        raise ReviewRequired(f"oversized JSON: {path}")
    with path.open(encoding="utf-8") as stream:
        return json.load(
            stream, object_pairs_hook=unique_keys,
            parse_constant=reject_constant,
        )


def atomic_json(path: Path, value: object) -> None:
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("x", encoding="utf-8") as stream:
        json.dump(value, stream, indent=2, sort_keys=True, allow_nan=False)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)
    descriptor = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def native_output(args: Sequence[str], env: Mapping[str, str], *,
                  absent_ok: bool = False) -> str:
    try:
        result = subprocess.run(args, env=env, capture_output=True, text=True,
                                timeout=5, check=False)
    except (OSError, subprocess.SubprocessError) as error:
        raise FatalSession(f"native probe failed: {args[0]}") from error
    allowed = {0, 1} if absent_ok else {0}
    if (result.stderr or result.returncode not in allowed
            or (result.returncode == 1 and result.stdout)):
        raise FatalSession(f"unverified native probe: {args[0]}")
    return result.stdout


def parse_vm_stat(output: str) -> dict[str, int]:
    page_size = re.findall(r"page size of ([0-9]+) bytes", output)
    pairs = re.findall(r"^([^:\n]+):\s+([0-9]+)\.$", output, re.MULTILINE)
    fields = dict(pairs)
    if len(page_size) != 1 or len(fields) != len(pairs):
        raise FatalSession("unrecognized vm_stat measurement")
    try:
        # Free + inactive + speculative; purgeable is not added twice.
        available = sum(int(fields[k]) for k in (
            "Pages free", "Pages inactive", "Pages speculative"))
        return {"available_bytes": available * int(page_size[0]),
                "swapouts": int(fields["Swapouts"])}
    except KeyError as error:
        raise FatalSession("incomplete vm_stat measurement") from error


def native_resources(env: Mapping[str, str]) -> dict:
    sample = parse_vm_stat(native_output(["/usr/bin/vm_stat"], env))
    sample["level"] = int(native_output(
        ["/usr/sbin/sysctl", "-n", "kern.memorystatus_level"], env))
    sample["load1"] = os.getloadavg()[0]
    sample["ac_power"] = native_output(
        ["/usr/bin/pmset", "-g", "batt"], env).splitlines()[0] == (
            "Now drawing from 'AC Power'")
    return sample


def resource_limit(sample: Mapping, peak: int, previous_swapouts: int,
                   *, timing: bool, admission: bool) -> None:
    if (not math.isfinite(sample["load1"]) or sample["load1"] < 0
            or any(type(sample[k]) is not int or sample[k] < 0
                   for k in ("level", "swapouts", "available_bytes"))
            or sample["level"] > 100
            or sample["level"] < (30 if admission else 25)
            or sample["swapouts"] != previous_swapouts
            or sample["available_bytes"] < MEMORY_RESERVE + peak
            or not sample["ac_power"]):
        raise FatalSession("native memory/swap/AC stop")
    if admission and timing and sample["load1"] > 2.0:
        raise DeadlineExceeded("timing not-started: load1 exceeds 2.0")


class DarwinUsage(ctypes.Structure):
    # SDK sys/resource.h rusage_info_v0; libproc.h proc_pid_rusage.
    _fields_ = [("uuid", ctypes.c_uint8 * 16)] + [
        (name, ctypes.c_uint64) for name in (
            "user_time", "system_time", "pkg_idle_wkups", "interrupt_wkups",
            "pageins", "wired_size", "resident_size", "phys_footprint",
            "proc_start_abstime", "proc_exit_abstime")]


@cache
def libproc() -> ctypes.CDLL:
    library = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
    library.proc_pid_rusage.argtypes = [ctypes.c_int, ctypes.c_int,
                                      ctypes.c_void_p]
    library.proc_pid_rusage.restype = ctypes.c_int
    return library


def darwin_usage(pid: int) -> DarwinUsage | None:
    if sys.platform != "darwin":
        raise FatalSession("Darwin ownership required; no fallback")
    library = libproc()
    usage = DarwinUsage()
    if library.proc_pid_rusage(pid, 0, ctypes.byref(usage)) != 0:
        if ctypes.get_errno() in {errno.ESRCH, errno.ENOENT}:
            return None
        raise FatalSession(f"unverified Darwin process identity: {pid}")
    if not usage.proc_start_abstime:
        raise FatalSession(f"missing Darwin process birth: {pid}")
    return usage


def process_groups(env: Mapping[str, str],
                   owned: Sequence[OwnedProcess]) -> dict:
    """Owned PGID -> {(PID, birth): RSS bytes}; None is unproved."""
    if sys.platform != "darwin":
        raise FatalSession("Darwin ownership required; no fallback")
    try:
        groups: dict = {}
        pgids = {entry.process.pid for entry in owned}
        for line in native_output(["/bin/ps", "-A", "-o", "pid=,pgid="], env
                                  ).splitlines():
            pid, pgid = (int(field) for field in line.split())
            if pgid not in pgids:
                continue
            try:
                usage = darwin_usage(pid)
            except FatalSession:
                groups[pgid] = None
                continue
            if usage is not None and groups.get(pgid, {}) is not None:
                members = groups.setdefault(pgid, {})
                members[(pid, usage.proc_start_abstime)] = usage.resident_size
        return groups
    except (OSError, ValueError, TypeError) as error:
        raise FatalSession("Darwin process table unverified") from error


def daemon_off(config: Mapping, env: Mapping[str, str]) -> None:
    # Read only the v2 bare fields. Never evaluate the notes object.
    if config["schema"] != "installed-daemon-identities/v2":
        raise FatalSession("unsupported installed-daemon identity schema")
    job = config["launchd"]
    if job["service_target"] != job["domain"] + "/" + job["label"]:
        raise FatalSession("ambiguous launchd identity")
    paths = [config[k] for k in (
        "plist_path", "stop_marker_path", "socket_path")]
    paths.append(config["executable"]["path"])
    if (any(not isinstance(p, str) or not Path(p).is_absolute()
            or any(ord(c) < 32 for c in p) for p in paths)
            or not config["tcp_listeners"]
            or not isinstance(config["executable"]["argv"], list)
            or not config["executable"]["argv"]):
        raise FatalSession("incomplete bare daemon identities")
    executable = Path(config["executable"]["path"]).resolve()
    if (Path(config["executable"]["argv"][0]).resolve() != executable
            or Path(config["executable"]["path"]).is_symlink() is not
            config["executable"]["is_symlink"]):
        raise FatalSession("ambiguous installed executable identity")
    commands = native_output(["/bin/ps", "-A", "-ww", "-o", "command="], env)
    if " ".join(config["executable"]["argv"]) in commands.splitlines():
        raise FatalSession("installed daemon command is running")
    texts = native_output(["/usr/sbin/lsof", "-nP", "-d", "txt", "-Fn"], env,
                          absent_ok=True)
    if any(Path(line[1:].removesuffix(" (deleted)")).resolve() == executable
           for line in texts.splitlines() if line.startswith("n/")):
        raise FatalSession("installed executable is running")
    result = subprocess.run(
        ["/bin/launchctl", "print", job["service_target"]], env=env,
        capture_output=True, text=True, timeout=5, check=False)
    absent = f'Could not find service "{job["label"]}" in domain '
    if result.returncode == 0 or absent not in result.stderr:
        raise FatalSession("launchd job loaded or off-state unverified")
    if not Path(config["stop_marker_path"]).is_file():
        raise FatalSession("owner stop-intent marker missing")
    sockets = native_output(["/usr/sbin/lsof", "-nP", "-U", "-Fn"], env,
                            absent_ok=True)
    socket = Path(config["socket_path"]).resolve()
    if any(str(socket) in line[1:]
           or (line.startswith("n/") and Path(line[1:]).resolve() == socket)
           for line in sockets.splitlines() if line.startswith("n")):
        raise FatalSession("installed socket has a live holder")
    for endpoint in config["tcp_listeners"]:
        selector = f'TCP@{endpoint["host"]}:{endpoint["port"]}'
        if native_output(["/usr/sbin/lsof", "-nP", "-a", "-i" + selector,
                          "-sTCP:LISTEN", "-Fp"], env, absent_ok=True):
            raise FatalSession("installed TCP endpoint has a listener")


def window_limit(go: Mapping, manifest_sha: str, now: float) -> float:
    required = {"schema", "window_id", "input_manifest_sha256", "start", "end",
                "user_go", "migration_complete", "owner_stop_confirmed",
                "awake", "thermal_safe", "background_quiet"}
    if not required <= go.keys():
        raise FatalSession("incomplete current window go")
    if (go["schema"] != "mac-window-go/v1"
            or not isinstance(go["window_id"], str) or not go["window_id"]
            or go["input_manifest_sha256"] != manifest_sha
            or not all(go[k] is True for k in (
                "user_go", "migration_complete", "owner_stop_confirmed",
                "awake", "thermal_safe", "background_quiet"))):
        raise FatalSession("missing current window/migration/owner go")
    start = timestamp(go["start"], "window start")
    end = timestamp(go["end"], "window end")
    if not start <= now < end - WINDOW_CLEANUP_SECONDS:
        raise DeadlineExceeded("outside current window or cleanup reserve")
    return end - WINDOW_CLEANUP_SECONDS - now


def launcher_limit(expected: Mapping, executable: Path,
                   version: Sequence[int], sha256: str) -> None:
    if (tuple(version) != (3, 14, 7) or expected["version"] != list(version)
            or executable.resolve() != Path(expected["path"])
            or sha256 != expected["sha256"]):
        raise FatalSession("use the explicitly pinned CPython 3.14.7 launcher")


def bsd_time(output: str, code: int) -> dict:
    pairs = re.findall(r"^(real|user|sys) ([0-9]+(?:\.[0-9]+)?)$", output,
                       re.MULTILINE)
    rss = re.findall(r"^\s*([0-9]+)\s+maximum resident set size$", output,
                     re.MULTILINE)
    fields = dict(pairs)
    if (len(pairs) != 3 or set(fields) != {"real", "user", "sys"}
            or len(rss) != 1):
        raise FatalSession("unrecognized BSD time receipt")
    return {"elapsed_seconds": float(fields["real"]),
            "user_seconds": float(fields["user"]),
            "system_seconds": float(fields["sys"]),
            "max_rss_bytes": int(rss[0]), "exit_code": code,
            "rss_scope": "Darwin ru_maxrss bytes, not simultaneous group sum"}


@dataclass(slots=True)
class OwnedProcess:
    process: subprocess.Popen
    directory: Path
    started: float
    log: BinaryIO
    known: set[tuple[int, int]] = field(default_factory=set)
    peak_rss: int = 0
    deadline: float = 0


class Supervisor:
    """Sole owner of subprocess sessions, receipts and bounded shutdown."""

    def __init__(self, root: Path, environment: Mapping[str, str],
                 admission: Mapping):
        self.block_deadline = self.launch_deadline = 0.0
        self.block_id = "unbound"
        self.job_minutes: dict[str, float] = {}
        self.scratch = persistent_path(admission["scratch_root"],
                                       Path(admission["scratch_root"]))
        self.manifest_file = persistent_path(
            admission["manifest_file"], self.scratch)
        self.root = persistent_path(root, self.scratch)
        self.environment, self.bad_jobs = dict(environment), {"*"}
        self.active: list[OwnedProcess] = []
        self.serial = 0
        self.native_identity = {
            "input_manifest_sha256": admission["input_manifest_sha256"],
            "daemon_off_identities_sha256": admission["identities_sha256"],
            "launcher": dict(admission["launcher"]),
        }
        if admission["d11_limits"] != {
                "admission_level": 30, "stop_level": 25,
                "memory_reserve_bytes": MEMORY_RESERVE,
                "timing_load1_max": 2.0,
                "swap_observation_seconds": 30}:
            raise FatalSession("D11 limits differ from the approved policy")
        launcher_limit(admission["launcher"], Path(sys.executable),
                       sys.version_info[:3], digest(Path(sys.executable)))
        if (sys.implementation.name != "cpython" or sys.platform != "darwin"
                or platform.machine() != "arm64"):
            raise FatalSession("pinned CPython on darwin-arm64 required")
        self.go_file = persistent_path(admission["window_file"], self.scratch)
        self.go_sha = digest(self.go_file)
        self.off_file = persistent_path(
            admission["identities_file"], self.scratch)
        self.off = read_json(self.off_file)
        self.case: Mapping = {}
        self.swapouts: int | None = None
        self.current_rss = 0
        self.permissions()
        host = native_output([
            "/usr/sbin/sysctl", "-n", "machdep.cpu.brand_string", "hw.memsize",
            "hw.perflevel0.physicalcpu", "hw.perflevel1.physicalcpu",
            "hw.logicalcpu"], self.environment).splitlines()
        if host != ["Apple M1 Max", str(64 * GIB), "8", "2", "10"]:
            raise FatalSession("unverified M1 Max 64-GiB/8P+2E host")
        self.native_identity["host"] = host
        signal.signal(signal.SIGALRM, alarm_expired)

    def permissions(self, *, full: bool = True) -> float:
        if (digest(self.manifest_file) != self.native_identity[
                "input_manifest_sha256"] or digest(self.go_file) != self.go_sha
                or digest(self.off_file) != self.native_identity[
                    "daemon_off_identities_sha256"]):
            raise FatalSession("window go or daemon identities changed")
        if not full and time.monotonic() < self.next_d12_check:
            return self.block_deadline - time.monotonic()
        go = read_json(self.go_file)
        remaining = window_limit(
            go, self.native_identity["input_manifest_sha256"], time.time())
        daemon_off(self.off, self.environment)
        self.next_d12_check = time.monotonic() + 60
        self.window = {"id": go["window_id"], "go_sha256": self.go_sha,
                       "start": go["start"], "end": go["end"]}
        return remaining

    def guard(self, *, admission: bool = False) -> None:
        try:
            self.permissions(full=admission)
            sample = native_resources(self.environment)
            previous = sample["swapouts"] if self.swapouts is None else (
                self.swapouts)
            peak = max(0, self.case.get("estimated_peak_bytes", 0)
                       - (self.current_rss if self.active else 0))
            peak = peak if admission else 0
            resource_limit(sample, peak, previous,
                           timing=self.block_id in TIMING_BLOCKS,
                           admission=admission)
            self.swapouts, self.sample = sample["swapouts"], sample
        except ReviewRequired:
            self.stop_all()
            raise
        except (OSError, ValueError, KeyError, TypeError, IndexError,
                subprocess.SubprocessError) as error:
            self.stop_all()
            raise FatalSession("native admission unverified") from error

    def begin_block(self, block: Mapping, outcomes: Mapping[str, str]) -> None:
        self.block_id = block["id"]
        self.block_deadline = time.monotonic() + self.permissions()
        self.job_minutes = {}
        self.job_bindings = {job["id"]: dict(job) for job in block["jobs"]}
        for job in block["jobs"]:
            kind = job["kind"]
            expected = ({j.name: j.stage for j in jobs()}[job["id"]]
                        if self.block_id == "storage" else
                        BLOCK_KINDS[self.block_id])
            if (type(job["minutes"]) not in (int, float)
                    or not math.isfinite(job["minutes"]) or job["minutes"] <= 0
                    or kind != expected or any(type(job[k]) is not int
                    or job[k] <= 0 for k in (
                        "estimated_peak_bytes", "gomaxprocs"))
                    or (kind == "setup" and job["gomaxprocs"] != 2)):
                raise FatalSession("unbound case kind/memory/runtime profile")
            self.job_minutes[job["id"]] = min(
                job["minutes"], CASE_MINUTES[kind])
        if self.block_id == "setup":
            self.job_limit("setup", time.monotonic())

    def job_limit(self, name: str, now: float) -> float:
        if "*" in self.bad_jobs or name in self.bad_jobs:
            raise ReviewRequired(f"{name}: required input missing/changed")
        if name not in self.job_minutes or self.active:
            raise FatalSession(f"{self.block_id}: unlisted or competing case")
        duration = self.job_minutes[name] * 60
        if duration > self.block_deadline - now:
            raise DeadlineExceeded(f"{name}: window too short; not-started")
        self.case = self.job_bindings[name]
        self.launch_deadline = min(
            self.block_deadline - CLEANUP_RESERVE_SECONDS,
            now + duration - CLEANUP_RESERVE_SECONDS,
        )
        if now >= self.launch_deadline:
            raise DeadlineExceeded(f"{self.block_id}/{name}: job deadline")
        return self.launch_deadline

    def arm_guard(self) -> None:
        end = min([self.block_deadline - CLEANUP_RESERVE_SECONDS,
                   *(owned.deadline for owned in self.active)])
        signal.setitimer(signal.ITIMER_REAL,
                         max(0.000001, end - time.monotonic()))

    def reap(self) -> None:
        """Reap direct children; preserve statuses racing with poll()."""
        for owned in self.active:
            owned.process.poll()
        while True:
            try:
                pid, status = os.waitpid(-1, os.WNOHANG)
            except ChildProcessError:
                break
            if pid == 0:
                break
            for owned in self.active:
                if owned.process.pid == pid:
                    # Preserve a direct child exit racing with poll().
                    owned.process.returncode = (
                        os.waitstatus_to_exitcode(status))

    def start(self, args: Sequence[str], directory: Path,
              environment: Mapping[str, str], cwd: Path) -> OwnedProcess:
        if environment["GOMAXPROCS"] != str(self.case["gomaxprocs"]):
            raise FatalSession("unbound effective GOMAXPROCS")
        for path in (directory, cwd, *[environment[k] for k in (
                "HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
                "XDG_STATE_HOME", "TMPDIR", "TMP", "TEMP", "GOTMPDIR",
                "GOCACHE", "GOMODCACHE", "UV_CACHE_DIR", "HF_HOME",
                "PYTHONPYCACHEPREFIX", "PYTHONUSERBASE")]):
            persistent_path(path, self.scratch)
        self.guard(admission=True)
        observation_end = time.monotonic() + 30
        while time.monotonic() < observation_end:
            time.sleep(max(0, min(5, observation_end - time.monotonic())))
            self.guard()
            if time.monotonic() >= self.launch_deadline:
                raise DeadlineExceeded("admission reached case deadline")
        self.guard(admission=True)
        if time.monotonic() >= self.launch_deadline:
            raise DeadlineExceeded(
                f"{self.block_id}: absolute deadline; no spawn")
        log = (directory / "stdout-stderr.log").open("xb")
        # This controller has one thread. Defer interruption until ownership
        # is recorded; restore the original mask in the child before exec.
        previous_mask = signal.pthread_sigmask(
            signal.SIG_BLOCK, {signal.SIGINT, signal.SIGTERM, signal.SIGHUP,
                               signal.SIGALRM},
        )
        def prepare_child():
            signal.pthread_sigmask(signal.SIG_SETMASK, previous_mask)
        try:
            try:
                if time.monotonic() >= self.launch_deadline:
                    raise DeadlineExceeded(
                        f"{self.block_id}: expired before spawn")
                if digest(self.go_file) != self.go_sha:
                    raise FatalSession("go changed before spawn")
                window_limit(read_json(self.go_file), self.native_identity[
                    "input_manifest_sha256"], time.time())
                process = subprocess.Popen(
                    args, cwd=cwd, env=environment, stdout=log,
                    stderr=subprocess.STDOUT, start_new_session=True,
                    preexec_fn=prepare_child,
                )
            except (OSError, subprocess.SubprocessError, ReviewRequired):
                log.close()
                raise
            owned = OwnedProcess(
                process, directory, time.monotonic(), log=log,
                deadline=self.launch_deadline,
            )
            self.active.append(owned)
            usage = darwin_usage(process.pid)
            if usage is None:
                raise FatalSession("new child birth identity unavailable")
            owned.known.add((process.pid, usage.proc_start_abstime))
            self.arm_guard()
        except (ReviewRequired, OSError):
            self.stop_all()
            raise
        finally:
            signal.pthread_sigmask(signal.SIG_SETMASK, previous_mask)
        atomic_json(directory / "process.json", {
            "pid": process.pid, "pgid": process.pid,
            "identity_kind": "darwin-rusage-v0-start-abstime",
            "known_pid_birth": sorted(owned.known),
            "native": self.native_identity, "window": self.window,
            "resources": self.sample,
            "command": list(args), "cwd": str(cwd),
            "absolute_deadline": owned.deadline,
        })
        self.observe(process_groups(self.environment, self.active))
        return owned

    def observe(self, groups: Mapping) -> None:
        self.current_rss = 0
        for owned in self.active:
            members = groups.get(owned.process.pid, {})
            if (members is None or (members and (
                    (owned.known and not owned.known.intersection(members))
                    or (not owned.known and not any(
                        pid == owned.process.pid for pid, _ in members))))):
                self.stop_all()
                raise FatalSession("unverified group identity; no adoption")
            owned.known.update(members)
            self.current_rss += sum(members.values())
        self.guard()
        for owned in self.active:
            rss = sum(groups.get(owned.process.pid, {}).values())
            owned.peak_rss = max(owned.peak_rss, rss)
            with (owned.directory / "rss.jsonl").open("a") as stream:
                stream.write(json.dumps({
                    "elapsed_seconds": time.monotonic() - owned.started,
                    "group_rss_bytes": rss, **self.sample,
                }) + "\n")

    def close(self, owned: OwnedProcess) -> None:
        self.reap()
        groups = process_groups(self.environment, self.active)
        members = groups.get(owned.process.pid, {})
        if members is None or members:
            raise FatalSession("job exited with unproved/surviving members")
        owned.log.close()
        self.active.remove(owned)
        self.arm_guard()

    def stop_all(self) -> None:
        if not self.active:
            return
        uncertain = set()
        for sig in (signal.SIGTERM, signal.SIGKILL):
            groups = process_groups(self.environment, self.active)
            for owned in self.active:
                members = groups.get(owned.process.pid, {})
                if members is None or (members and not
                                       owned.known.intersection(members)):
                    uncertain.add(owned.process.pid)
                    continue
                if not members:
                    continue
                owned.known.update(members)
                try:
                    os.killpg(owned.process.pid, sig)
                except ProcessLookupError:
                    continue
            deadline = time.monotonic() + CLEANUP_GRACE_SECONDS
            while time.monotonic() < deadline:
                self.reap()
                groups = process_groups(self.environment, self.active)
                if not any(groups.get(p.process.pid) or p.process.pid in
                           uncertain for p in self.active):
                    break
                time.sleep(0.1)
        self.reap()
        groups = process_groups(self.environment, self.active)
        survivors = [p.process.pid for p in self.active
                     if groups.get(p.process.pid)
                     or p.process.pid in uncertain]
        for owned in self.active:
            owned.log.close()
            if owned.process.poll() is None:
                survivors.append(owned.process.pid)
            else:
                owned.process.wait(timeout=0)
        self.active = [p for p in self.active if p.process.pid in survivors]
        if survivors:
            raise FatalSession(f"unproved or surviving sessions: {survivors}")

    def run(self, args: Sequence[str], *, cwd: Path | None = None,
            env: Mapping[str, str] | None = None,
            accepted_exit_codes: Sequence[int] = (0,)) -> Path:
        self.serial += 1
        directory = self.root / "setup" / f"command-{self.serial:03d}"
        directory.mkdir()
        owned = self.start(
            args, directory, env or self.environment, cwd or self.root,
        )
        try:
            while owned.process.poll() is None:
                self.observe(process_groups(self.environment, self.active))
                remaining = owned.deadline - time.monotonic()
                if remaining <= 0:
                    raise DeadlineExceeded("absolute setup deadline")
                time.sleep(min(5, remaining))
            status = owned.process.returncode
        except FatalSession:
            signal.setitimer(signal.ITIMER_REAL, 0)
            self.stop_all()
            raise
        except (subprocess.TimeoutExpired, DeadlineExceeded) as error:
            signal.setitimer(signal.ITIMER_REAL, 0)
            self.stop_all()
            atomic_json(directory / "failure.json", {
                "state": "partial", "reason": "absolute deadline",
                "deadline": owned.deadline,
                "exit_code": owned.process.returncode,
                "whole_group_reaped": True,
            })
            raise DeadlineExceeded(
                f"{self.block_id}: absolute setup deadline"
            ) from error
        log = directory / "stdout-stderr.log"
        self.close(owned)
        atomic_json(directory / "complete.json", {"exit_code": status})
        if status not in accepted_exit_codes:
            raise ReviewRequired(f"setup command failed; inspect {log}")
        return log


def persistent_path(value: str | Path, root: Path) -> Path:
    path = Path(value).resolve()
    if (not Path(value).is_absolute() or path != Path(value).absolute()
            or ".." in Path(value).parts
            or any(path.is_relative_to(Path(p)) for p in
                   ("/tmp", "/private/tmp"))
            or not path.is_relative_to(root.resolve())):
        raise FatalSession("runtime path escaped persistent scratch")
    return path


def bad_jobs(path: Path, scratch: Path) -> set[str]:
    manifest = read_json(path)
    if (not isinstance(manifest, dict) or set(manifest) != {
            "schema", "source", "files", "jobs", "homebrew"}
            or manifest["schema"] != "storage-inputs/v1"
            or manifest["source"] != COMMIT
            or any(not isinstance(manifest[k], dict)
                   for k in ("files", "jobs", "homebrew"))):
        raise ReviewRequired("input manifest schema/source")
    files, bad = manifest["files"], set()
    if set(manifest["homebrew"]) - files.keys():
        raise ReviewRequired("unlisted managed file")
    for name, expected in files.items():
        target = Path(name).resolve()
        if str(target) != name or not sha_pin(expected, (64,)):
            raise ReviewRequired("aliased input or invalid hash")
        if name in manifest["homebrew"]:
            tool = manifest["homebrew"][name]
            if (not isinstance(tool, dict) or set(tool) != {
                    "formula", "version", "installed_at"} or any(
                    not isinstance(v, str) or not v or "/" in v
                    or v in {".", ".."} for v in (
                        tool["formula"], tool["version"]))):
                raise ReviewRequired("unnamed managed tool")
            timestamp(tool["installed_at"], "Homebrew install")
            persistent_path(target, Path("/opt/homebrew/Cellar") /
                            tool["formula"] / tool["version"])
        else:
            persistent_path(target, scratch)
        if not target.is_file() or digest(target) != expected:
            bad.add(name)
    for names in manifest["jobs"].values():
        if (not isinstance(names, list) or any(
                not isinstance(name, str) for name in names)
                or not set(names) <= files.keys()):
            raise ReviewRequired("unlisted job input")
    return {job for job, names in manifest["jobs"].items()
            if bad.intersection(names)}


def reusable(directory: Path, verify: Callable[[], dict]) -> dict | None:
    persistent_path(directory, directory.parents[1])
    try:
        return verify()
    except FatalSession:
        raise
    except (ReviewRequired, OSError, KeyError, TypeError, ValueError) as error:
        atomic_json(directory / "partial.json", {
            "state": "partial", "reason": str(error)})
        return None


def new_attempt(supervisor: Supervisor, directory: Path) -> None:
    root = supervisor.root
    persistent_path(directory, root)
    if directory.exists():
        previous = root / "attempts" / directory.parent.name / directory.name
        previous.mkdir(parents=True, exist_ok=True)
        directory.rename(previous / uuid.uuid4().hex)
    directory.mkdir()
    atomic_json(directory / "attempt.json", {
        "run_id": root.name, "job_id": directory.name,
        "attempt_id": uuid.uuid4().hex,
        "window_id": supervisor.window["id"]})


def download(url: str, path: Path, sha256: str, limit: int) -> None:
    with urllib.request.urlopen(url, timeout=120) as response:
        with path.open("xb") as stream:
            count = 0
            while chunk := response.read(1024 * 1024):
                count += len(chunk)
                if count > limit:
                    raise ReviewRequired("download exceeded expected bound")
                stream.write(chunk)
    if digest(path) != sha256:
        raise ReviewRequired(f"download checksum mismatch: {path.name}")


def extract(archive: Path, target: Path, prefix: str) -> None:
    """Python 3.10 equivalent of a stricter data-only extraction policy."""
    with tarfile.open(archive) as bundle:
        members = bundle.getmembers()
        seen: set[str] = set()
        total = 0
        if len(members) > 100000:
            raise ReviewRequired("archive member limit exceeded")
        for member in members:
            path = PurePosixPath(member.name)
            total += member.size
            if (path.is_absolute() or ".." in path.parts
                    or not path.parts or path.parts[0] != prefix
                    or "\\" in member.name
                    or any(ord(char) < 32 for char in member.name)
                    or str(path) in seen
                    or not (member.isfile() or member.isdir())
                    or member.size < 0 or total > 8 * GIB):
                raise ReviewRequired(f"unsafe archive member: {member.name}")
            seen.add(str(path))
        for member in members:
            destination = target / member.name
            if member.isdir():
                destination.mkdir(parents=True, exist_ok=True)
                continue
            destination.parent.mkdir(parents=True, exist_ok=True)
            with bundle.extractfile(member) as source:
                with destination.open("xb") as sink:
                    shutil.copyfileobj(source, sink)
            destination.chmod(member.mode & 0o777)


def copy_tree(root: Path, source: Path, destination: Path) -> dict[str, str]:
    if (source.is_symlink() or not source.is_dir()
            or destination.exists() or destination.is_symlink()
            or any(p.is_symlink() for path in (source, destination)
                   for p in (path, *path.parents)
                   if p.is_relative_to(root))):
        raise ReviewRequired(f"setup copy refused: {source} -> {destination}")
    files = {}
    for directory, subdirs, names in os.walk(source, followlinks=False):
        for name in (*subdirs, *names):
            path = Path(directory) / name
            if path.is_symlink() or not (path.is_dir() or path.is_file()):
                raise ReviewRequired(
                    f"setup copy refuses special path: {path}")
            if path.is_file():
                files[path.relative_to(source)] = digest(path)
    shutil.copytree(source, destination)
    artifacts = {}
    for relative, expected in files.items():
        target = destination / relative
        if digest(target) != expected:
            raise ReviewRequired(f"setup copied file changed: {target}")
        artifacts[str(target.relative_to(root))] = expected
    return artifacts


def prepare(supervisor: Supervisor, inputs: Path, identity: Mapping) -> dict:
    root = supervisor.root
    complete = root / "setup-complete.json"
    if complete.exists():
        receipt = read_json(complete)
        if receipt["identity"] != identity:
            raise ReviewRequired("setup identity changed; no reuse")
        for relative, expected in receipt["artifacts"].items():
            if digest(root / relative) != expected:
                raise ReviewRequired(f"setup artifact changed: {relative}")
        for path, expected in receipt["runtime_libraries"].items():
            if digest(Path(path)) != expected:
                raise ReviewRequired(f"runtime library changed: {path}")
        head = (root / "checkout" / ".git" / "HEAD").read_text().strip()
        if head != COMMIT:
            raise ReviewRequired("checkout no longer has the detached pin")
        return receipt
    if (root / "setup-started.json").exists():
        previous = root / "attempts" / "setup" / uuid.uuid4().hex
        previous.mkdir(parents=True)
        for path in root.iterdir():
            if path.name not in {"control", "inputs", "driver.lock",
                                 "session.json", "python-runtime.json",
                                 "checkpoint.json", "attempts"}:
                path.rename(previous / path.name)
        for source in (root / "control/neon-eval", root / "control/pod-cost"):
            if source.exists():
                source.rename(previous / source.name)
    if not set(path.name for path in root.iterdir()) <= {
            "driver.lock", "control", "inputs", "session.json", "lost+found",
            "python-runtime.json", "attempts", "checkpoint.json"}:
        raise ReviewRequired("new workspace permits only control/inputs/lock")
    if not inputs.resolve().is_relative_to(supervisor.scratch):
        raise ReviewRequired("pod inputs must be copied under /work")
    metadata = read_json(inputs / "tiers-summary.json")
    for tier, expected in ARCHIVES.items():
        provided = metadata[tier]
        archive = inputs / f"{tier}.tar.zst"
        if (provided["archive_sha256"] != expected["sha256"]
                or provided["archive_bytes"] != expected["bytes"]
                or provided["queries"] != expected["queries"]
                or archive.stat().st_size != expected["bytes"]
                or digest(archive) != expected["sha256"]):
            raise ReviewRequired(f"approved archive mismatch: {tier}")
    atomic_json(root / "setup-started.json", dict(identity))
    for name in ("setup", "downloads", "toolchain", "native", "bin",
                 "cache", "data", "jobs", "home", "xdg", "python", "aux"):
        (root / name).mkdir()
    for name in ("tmp", "go-build", "go-mod", "hf", "uv"):
        (root / "cache" / name).mkdir()
    for name in ("config", "cache", "data", "state"):
        (root / "xdg" / name).mkdir()
    copied = {}
    for name in ("evaluator", "cpu-cost"):
        copied.update(copy_tree(root, inputs / name, root / "data" / name))
    tool_versions = {}
    for tool in ("go", "cmake", "c++", "git", "/usr/bin/time"):
        option = "version" if tool == "go" else "--version"
        tool_versions[tool] = supervisor.run([tool, option]).read_text()
    atomic_json(root / "tool-versions.json", tool_versions)
    checkout = root / "checkout"
    supervisor.run([
        "git", "clone", "--branch", BRANCH, "--single-branch",
        "--no-checkout", REPOSITORY, str(checkout),
    ])
    supervisor.run(["git", "checkout", "--detach", COMMIT], cwd=checkout)
    revision = supervisor.run(["git", "rev-parse", "HEAD"], cwd=checkout)
    if revision.read_text().strip() != COMMIT:
        raise ReviewRequired("source revision mismatch")
    for name in ("neon-eval", "pod-cost"):
        copied.update(copy_tree(root, checkout / "study/storage" / name,
                                root / "control" / name))
    source_archive = root / "downloads" / "usearch-2.26.5.tar.gz"
    download(USEARCH_URL, source_archive, USEARCH_SHA, 128 * 1024 * 1024)
    extract(source_archive, root / "native", "USearch-2.26.5")
    source = root / "native" / "USearch-2.26.5"
    native = root / "native"
    build = native / "build"
    supervisor.run([
        "cmake", "-S", str(source), "-B", str(build),
        "-DCMAKE_BUILD_TYPE=Release", "-DUSEARCH_BUILD_LIB_C=ON",
        "-DUSEARCH_BUILD_TEST_CPP=OFF", "-DUSEARCH_BUILD_BENCH_CPP=OFF",
        "-DUSEARCH_BUILD_TEST_C=OFF", "-DUSEARCH_USE_NUMKONG=OFF",
        "-DUSEARCH_USE_OPENMP=OFF",
    ])
    supervisor.run([
        "cmake", "--build", str(build), "--target", "usearch_static_c",
        "--parallel", "1",
    ])
    library = build / "libusearch_static_c.a"
    if not library.exists():
        library = build / "c" / "libusearch_static_c.a"
    shutil.copyfile(library, native / "libusearch_c.a")
    shutil.copyfile(source / "c" / "usearch.h", native / "usearch.h")
    storage = checkout / "study" / "storage"
    targets = {
        "gen": (storage / "harness", "./cmd/gen"),
        "oracle": (storage / "harness", "./cmd/oracle"),
        **{name: (storage / "adapters" / name, ".")
           for name in ADAPTERS if not name.startswith("sqlite-v")
           and name != "sqlite-usearch"},
        **{name: (storage / "vector" / "adapters" / name, ".")
           for name in ("sqlite-vecflat", "sqlite-usearch")},
    }
    linking = {}
    runtime_libraries = {}
    for name, (directory, target) in targets.items():
        env = dict(supervisor.environment)
        env["CGO_ENABLED"] = "1" if name in {
            "sqlite-usearch", "libsql-segment",
        } else "0"
        if name == "sqlite-usearch":
            env["CGO_CFLAGS"] = f"-I{native}"
        binary = root / "bin" / name
        supervisor.run([
            "go", "build", "-p=1", "-o", str(binary), target,
        ], cwd=directory, env=env)
        description = supervisor.run(["file", str(binary)]).read_text()
        linking[name] = {
            "file": description,
            "usearch_static_c_archive": name == "sqlite-usearch",
            "build_environment": env,
        }
    atomic_json(root / "linking.json", linking)
    supervisor.run(["git", "diff", "--exit-code"], cwd=checkout)
    for tier in ARCHIVES:
        archive = inputs / f"{tier}.tar.zst"
        unpacked = root / "downloads" / f"{tier}.tar"
        # Keep decompression separate from extraction and preserve the input.
        supervisor.run([
            "zstd", "--decompress", "--output", str(unpacked), str(archive),
        ])
        extract(unpacked, root / "data", tier)
    artifacts = {
        str(path.relative_to(root)): digest(path)
        for path in [*sorted((root / "bin").iterdir()),
                     native / "libusearch_c.a", native / "usearch.h",
                     root / "linking.json", root / "tool-versions.json"]
    }
    artifacts.update(copied)
    for tier in ARCHIVES:
        manifest_path = root / "data" / tier / "manifest.json"
        manifest = read_json(manifest_path)
        for relative, expected in manifest["sha256"].items():
            path = root / "data" / tier / relative
            if digest(path) != expected:
                raise ReviewRequired(f"sealed tier checksum mismatch: {tier}")
            artifacts[str(path.relative_to(root))] = expected
        artifacts[str(manifest_path.relative_to(root))] = digest(manifest_path)
    receipt = {
        "identity": dict(identity), "artifacts": artifacts,
        "window_id": supervisor.window["id"],
        "runtime_libraries": runtime_libraries,
    }
    atomic_json(complete, receipt)
    return receipt


@dataclass(frozen=True, slots=True)
class Verdict:
    state: Literal["pass", "checks_failed", "stop"]
    reason: str


def check_result(report: Mapping, job: Job, code: int,
                 manifest: Mapping, queries: Sequence) -> Verdict:
    """Fixed SDK contract, not a classifier of arbitrary error text."""
    stop = "incomplete or inconsistent structured result"
    if (report.get("adapter") != job.adapter
            or report.get("tier") != job.tier or report.get("run") != 1
            or not isinstance(report.get("success"), bool)
            or report.get("timing_only", False) is not (job.stage == "timing")
            or code not in (0, 1)):
        return Verdict("stop", stop)
    if Counter(b["repo"] for b in report.get("builds", [])) != Counter(
            manifest["repos"]):
        return Verdict("stop", "base build coverage incomplete")
    checks = report.get("checks", [])
    required_ids = {q["id"] for q in queries}
    required_ids.update("native_" + q["id"] for q in queries
                        if q["op"] in {"traverse2", "traverse3"})
    if not required_ids.issubset({check["query_id"] for check in checks}):
        return Verdict("stop", "base query coverage incomplete")
    metrics = report.get("metrics", {})
    if (report.get("peak_rss_bytes", 0) <= 0
            or any(metrics.get(name, {}).get("count", 0) <= 0 for name in
                   ("build_seal", "open_first_no_cache_flush", "open_warm"))):
        return Verdict("stop", "missing lifecycle timing or RSS")
    all_checks = list(checks)
    if job.stage == "timing":
        if (report.get("skipped_phases") != SKIPPED
                or any(key in report for key in
                       ("delta", "delta_checks", "concurrent", "crash"))
                or "delta_build" in metrics):
            return Verdict("stop", "timing invocation ran correctness phases")
    else:
        delta = report.get("delta_checks", [])
        concurrent = report.get("concurrent", {})
        windows = concurrent.get("commit_race_windows", {})
        samples = concurrent.get("new_samples_per_reader", [])
        crash = report.get("crash", [])
        if ({check["query_id"] for check in delta} != DELTA_IDS
                or len(delta) != len(DELTA_IDS)
                or set(windows) != STAGES
                or min(windows.values(), default=0) < 2
                or concurrent.get("commit_race_minimum", 0) < 2
                or len(samples) != 8 or min(samples, default=0) < 2
                or concurrent.get("after_publish", {}).get("count", 0) < 16
                or not concurrent.get("checks")
                or len(crash) != 24
                or not all(c.get("ok") is True and c.get("killed") is True
                           for c in crash)
                or Counter(c["mode"] for c in crash) != Counter({
                    "checkpoint": 12, "external-time": 12,
                })
                or Counter(c["checkpoint"] for c in crash
                           if c["mode"] == "checkpoint")
                != Counter({stage: 2 for stage in STAGES})):
            return Verdict("stop", "full correctness coverage incomplete")
        all_checks.extend(delta)
        all_checks.extend(concurrent["checks"])
    named = []
    for check in all_checks:
        if (not check.get("query_id")
                or check.get("status") not in {
                    "passed", "failed", "unsupported",
                }
                or check.get("mismatch") is not (check["status"] == "failed")):
            return Verdict("stop", "invalid named check")
        if check["mismatch"]:
            if not check.get("detail"):
                return Verdict("stop", "failed check has no detail")
            named.append(check["query_id"] + ": " + check["detail"])
    if Counter(report.get("failures", [])) != Counter(named):
        return Verdict("stop", "unaccounted fatal/cleanup/harness error")
    if report["success"] and not named and code == 0:
        return Verdict("pass", "selected protocol completed")
    if not report["success"] and named and code == 1:
        return Verdict("checks_failed", "complete named check failures only")
    return Verdict("stop", "success/exit/check failures disagree")


def dry_run_contract_checks() -> list[str]:
    """Pure rejection examples; no host, files, processes or network."""
    job = Job("screen", "sqlite-segment", "tier10")
    check = {"query_id": "q", "operation": "id", "status": "passed",
             "mismatch": False}
    base = {
        "adapter": job.adapter, "tier": job.tier, "run": 1,
        "success": True, "builds": [{"repo": "fixture"}],
        "checks": [check], "peak_rss_bytes": 1, "failures": [],
        "metrics": {key: {"count": 1} for key in (
            "build_seal", "open_first_no_cache_flush", "open_warm",
        )},
        "delta_checks": [{**check, "query_id": key} for key in DELTA_IDS],
        "concurrent": {
            "commit_race_windows": {stage: 2 for stage in STAGES},
            "commit_race_minimum": 2, "new_samples_per_reader": [2] * 8,
            "after_publish": {"count": 16}, "checks": [check],
        },
        "crash": [
            {"ok": True, "killed": True, "mode": mode, "checkpoint": stage}
            for mode in ("checkpoint", "external-time")
            for stage in sorted(STAGES) for _ in range(2)
        ],
    }
    failed = {
        **base, "success": False, "failures": ["q: example mismatch"],
        "checks": [{**check, "status": "failed", "mismatch": True,
                    "detail": "example mismatch"}],
    }
    timing = {
        **{key: value for key, value in base.items()
           if key not in {"delta_checks", "concurrent", "crash"}},
        "timing_only": True, "skipped_phases": list(SKIPPED),
    }
    examples = [
        ("full pass", base, job, 0, "pass"),
        ("complete named failure", failed, job, 1, "checks_failed"),
        ("extra fatal error", {**failed, "failures": [
            *failed["failures"], "context deadline exceeded",
        ]}, job, 1, "stop"),
        ("missing crash coverage", {**failed, "crash": []}, job, 1, "stop"),
        ("missing reader coverage", {
            **failed, "concurrent": {**base["concurrent"],
                                     "new_samples_per_reader": []},
        }, job, 1, "stop"),
        ("killed process", failed, job, -9, "stop"),
        ("false success", {**failed, "success": True}, job, 0, "stop"),
        ("timing cannot pass correctness", timing, job, 0, "stop"),
        ("timing pass", timing,
         Job("timing", job.adapter, job.tier), 0, "pass"),
    ]
    checked = []
    for name, report, selected, code, expected in examples:
        verdict = check_result(
            report, selected, code, {"repos": ["fixture"]},
            [{"id": "q", "op": "id"}],
        )
        if verdict.state != expected:
            raise ReviewRequired(f"dry-run contract example failed: {name}")
        checked.append(name)
    return checked


def environment(root: Path) -> dict[str, str]:
    # Do not forward credentials, proxies or private module settings.
    return {
        "PATH": str(root / "toolchain" / "go" / "bin")
        + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
        "HOME": str(root / "home"), "LANG": "C.UTF-8", "LC_ALL": "C",
        "XDG_CONFIG_HOME": str(root / "xdg/config"),
        "XDG_CACHE_HOME": str(root / "xdg/cache"),
        "XDG_DATA_HOME": str(root / "xdg/data"),
        "XDG_STATE_HOME": str(root / "xdg/state"),
        "HF_HOME": str(root / "cache/hf"),
        "UV_CACHE_DIR": str(root / "cache/uv"),
        "GIT_TERMINAL_PROMPT": "0",
        "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
        "GOTOOLCHAIN": "local", "GOENV": "off", "GOWORK": "off",
        "GOMAXPROCS": "2",
        "GOFLAGS": "-mod=readonly", "GOPROXY": "https://proxy.golang.org",
        "GOSUMDB": "sum.golang.org",
        "GOCACHE": str(root / "cache" / "go-build"),
        "GOMODCACHE": str(root / "cache" / "go-mod"),
        "TMP": str(root / "cache/tmp"), "TEMP": str(root / "cache/tmp"),
        "PYTHONPYCACHEPREFIX": str(root / "cache/pycache"),
        "PYTHONUSERBASE": str(root / "python"),
        "TMPDIR": str(root / "cache" / "tmp"),
        "GOTMPDIR": str(root / "cache" / "tmp"),
    }


def result_path(root: Path, job: Job) -> Path:
    return (root / "jobs" / job.name / "output"
            / f"{job.adapter}-{job.tier}-01.json")


def case_identity(job: Job, root: Path, setup: Mapping,
                  gomaxprocs: int) -> dict:
    return {
        "job": asdict(job), "source": COMMIT,
        "driver": setup["identity"]["driver"],
        "archives": {tier: record["sha256"]
                     for tier, record in ARCHIVES.items()},
        "binary": setup["artifacts"][f"bin/{job.adapter}"],
        "command": command(job, root), "GOMAXPROCS": gomaxprocs,
        "runtime": setup["identity"]["runtime"],
        "runtime_libraries": setup["runtime_libraries"],
        "setup_identity": dict(setup["identity"]),
    }


def finished_case(root: Path, job: Job, expected: Mapping) -> dict:
    directory = root / "jobs" / job.name
    receipt = read_json(directory / "complete.json")
    if receipt["identity"] != expected:
        raise ReviewRequired(f"completed case identity changed: {job.name}")
    for relative, sha256 in receipt["files"].items():
        if digest(persistent_path(directory / relative, directory)) != sha256:
            raise ReviewRequired(
                f"completed case artifact changed: {job.name}"
            )
    report = read_json(result_path(root, job))
    verdict = check_result(
        report, job, receipt["exit_code"],
        read_json(root / "data" / job.tier / "manifest.json"),
        read_json(root / "data" / job.tier / "queries.json"),
    )
    if verdict.state == "stop" or verdict.state != receipt["state"]:
        raise ReviewRequired(f"invalid completed case: {job.name}")
    return receipt


def summary(root: Path, receipts: Sequence[Mapping]) -> None:
    medians = []
    for tier in ARCHIVES:
        for adapter in FINALISTS:
            group = [row for row in receipts
                     if row["identity"]["job"]["stage"] == "timing"
                     and row["identity"]["job"]["tier"] == tier
                     and row["identity"]["job"]["adapter"] == adapter]
            if len(group) != 3:
                continue
            screen_passed = any(
                row["identity"]["job"]["stage"] == "screen"
                and row["identity"]["job"]["adapter"] == adapter
                and row["state"] == "pass" for row in receipts
            )
            valid = screen_passed and all(r["state"] == "pass" for r in group)
            metrics = {}
            if valid:
                operations = set(group[0]["metrics"])
                if any(set(row["metrics"]) != operations for row in group):
                    raise ReviewRequired("timing operation coverage changed")
                metrics = {
                    operation: {
                        stat: statistics.median(row["metrics"][operation][stat]
                                                for row in group)
                        for stat in ("p50_ns", "p95_ns", "p99_ns", "mean_ns",
                                     "count")
                    } for operation in sorted(operations)
                }
            medians.append({
                "adapter": adapter, "tier": tier,
                "valid_timing_comparison": valid,
                "correctness_pass": False,
                "wall_seconds": statistics.median(r["wall_seconds"]
                                                  for r in group) if valid
                else None,
                "peak_rss_bytes": statistics.median(r["peak_rss_bytes"]
                                                    for r in group) if valid
                else None,
                "metrics": metrics,
            })
    atomic_json(root / "summary.json", {
        "source": COMMIT, "jobs_expected": len(jobs()),
        "scope": "SDK correctness/segment timing; auxiliary in aux-summary.json",
        "jobs_completed": len(receipts), "cases": list(receipts),
        "timing_medians": medians,
    })
    table = [
        "| Stage | Tier | Adapter | Repeat | Result | Wall s | Peak GiB |",
        "| --- | --- | --- | --- | --- | ---: | ---: |",
    ]
    for receipt in receipts:
        job = receipt["identity"]["job"]
        table.append(
            f"| {job['stage']} | {job['tier']} | {job['adapter']} | "
            f"{job['repeat']} | {receipt['state']} | "
            f"{receipt['wall_seconds']:.2f} | "
            f"{receipt['peak_rss_bytes'] / GIB:.2f} |"
        )
    table.extend([
        "", "| Timing tier | Adapter | Valid | Median s | Median GiB |",
        "| --- | --- | --- | ---: | ---: |",
    ])
    for median in medians:
        wall = median["wall_seconds"]
        rss = median["peak_rss_bytes"]
        table.append(
            f"| {median['tier']} | {median['adapter']} | "
            f"{median['valid_timing_comparison']} | "
            + (f"{wall:.2f} | {rss / GIB:.2f} |" if wall is not None
               else "not valid | not valid |")
        )
    temporary = root / "summary.md.tmp"
    temporary.write_text("\n".join(table) + "\n")
    os.replace(temporary, root / "summary.md")


def eligible_timings(receipts: Sequence[Mapping]) -> list[Job]:
    passed = {row["identity"]["job"]["adapter"] for row in receipts
              if row["identity"]["job"]["stage"] == "screen"
              and row["state"] == "pass"}
    return [job for job in jobs()
            if job.stage == "timing" and job.adapter in passed]


def execute_stage(supervisor: Supervisor, selected: Sequence[Job],
                  setup: Mapping, concurrency: int,
                  receipts: list[dict]) -> None:
    if concurrency != 1:
        raise FatalSession("whole-Mac correctness/timing cases must be serial")
    root = supervisor.root
    pending = deque(selected)
    active: dict[str, tuple[Job, OwnedProcess, dict]] = {}
    while pending or active:
        groups = process_groups(supervisor.environment, supervisor.active)
        supervisor.observe(groups)
        for name, (job, owned, identity) in list(active.items()):
            code = owned.process.poll()
            if time.monotonic() >= owned.deadline:
                raise DeadlineExceeded(f"{job.stage}/{name}: job deadline")
            if code is None:
                continue
            directory = root / "jobs" / name
            supervisor.close(owned)
            path = result_path(root, job)
            report = read_json(path)
            timing = bsd_time(
                (directory / "native-time.txt").read_text(), code)
            atomic_json(directory / "time.json", timing)
            if timing["exit_code"] != code:
                raise ReviewRequired("time receipt/exit disagreement")
            manifest_path = root / "data" / job.tier / "manifest.json"
            if report.get("manifest_sha256") != digest(manifest_path):
                raise ReviewRequired("result manifest digest mismatch")
            verdict = check_result(
                report, job, code, read_json(manifest_path),
                read_json(root / "data" / job.tier / "queries.json"),
            )
            if verdict.state == "stop":
                raise ReviewRequired(f"{name}: {verdict.reason}")
            receipt = {
                "identity": identity, "state": verdict.state,
                "attempt": read_json(directory / "attempt.json"),
                "correctness_pass": (
                    job.stage != "timing" and verdict.state == "pass"
                ),
                "exit_code": code,
                "wall_seconds": timing["elapsed_seconds"],
                "user_seconds": timing["user_seconds"],
                "system_seconds": timing["system_seconds"],
                "peak_rss_bytes": max(
                    report["peak_rss_bytes"], owned.peak_rss,
                    timing["max_rss_bytes"],
                ),
                "sampled_group_peak_is_lower_bound": True,
                "metrics": report["metrics"],
                "failed_checks": report["failures"],
                "files": {
                    str(p.relative_to(directory)): digest(p)
                    for p in (path, directory / "native-time.txt",
                              directory / "attempt.json",
                              directory / "time.json",
                              directory / "process.json",
                              directory / "rss.jsonl",
                              directory / "stdout-stderr.log")
                },
            }
            atomic_json(directory / "complete.json", receipt)
            receipts.append(receipt)
            summary(root, receipts)
            del active[name]
            print(name, verdict.state, flush=True)
            if job.stage == "oracle" and verdict.state != "pass":
                raise ReviewRequired("reference oracle did not pass")
        while pending and len(active) < concurrency:
            job = pending[0]
            gomaxprocs = supervisor.job_bindings[job.name]["gomaxprocs"]
            identity = {**case_identity(job, root, setup, gomaxprocs),
                        "inputs_verified": job.name not in supervisor.bad_jobs}
            directory = root / "jobs" / job.name
            receipt = reusable(directory, lambda: finished_case(
                root, job, identity)) if directory.exists() else None
            if receipt is not None:
                receipts.append(receipt)
                pending.popleft()
                if job.stage == "oracle" and receipt["state"] != "pass":
                    raise ReviewRequired("saved reference oracle did not pass")
                print(job.name, "validated receipt; skipped", flush=True)
                continue
            new_attempt(supervisor, directory)
            atomic_json(directory / "started.json", identity)
            env = {**supervisor.environment, "GOMAXPROCS": str(gomaxprocs)}
            supervisor.job_limit(job.name, time.monotonic())
            owned = supervisor.start([
                "/usr/bin/time", "-p", "-l", "-o",
                str(directory / "native-time.txt"), *command(job, root),
            ], directory, env, root)
            active[job.name] = (job, owned, identity)
            pending.popleft()
            # Refresh after launch; reserve the new job's pending RSS growth.
            groups = process_groups(supervisor.environment, supervisor.active)
        if active:
            time.sleep(5)
    summary(root, receipts)


def hybrid_commands(root: Path, mode: str) -> dict[str, tuple[str, ...]]:
    work = root / "home/.prime/research/gortex/storage-architecture/tmp"
    work = work / "vector/hybrid-followup"
    binary = str(work / "bin/linux-amd64" / ("timing" if mode == "timing" else "ann"))
    planned = {}
    if mode == "timing":
        for dimensions in (384, 1024):
            for n in (10000, 100000):
                label = f"n{n}-d{dimensions}"
                base = (binary, "-n", str(n), "-dimensions", str(dimensions),
                        "-data-dir", str(work / "timing-data"))
                planned[label + "-build"] = base + ("-phase", "build")
                for repeat in (1, 2, 3):
                    for workers in (1, 4, 8):
                        planned[f"{label}-w{workers}-rep{repeat}"] = base + (
                            "-phase", "query", "-workers", str(workers),
                            "-repeat", str(repeat), "-queries", "256")
        return planned
    for n in (10000, 100000):
        directory = work / f"generated-n{n}"
        label = f"hybrid-followup-generated-n{n}"
        base = (binary, "-dataset", str(directory))
        planned[label + "-generate"] = base + ("-phase", "generate", "-n", str(n))
        planned[label + "-int8"] = base + ("-phase", "int8-build")
        planned[label + "-native"] = base + ("-phase", "build", "-m", "32")
        for method, m, ef, k, workers in (
            ("exact", 16, 512, 10, 1), ("int8", 16, 512, 128, 1),
            ("int8", 16, 512, 128, 4), ("int8", 16, 512, 128, 8),
            ("f32", 32, 1024, 10, 1),
        ):
            for repeat in (1, 2, 3):
                name = (f"{label}-{method}-m{m}-e{ef}-k{k}-w{workers}"
                        f"-block{repeat}")
                planned[name] = base + ("-phase", "latency", "-method", method,
                                       "-m", str(m), "-ef", str(ef),
                                       "-workers", str(workers))
    return planned


def hybrid_names(mode: str) -> list[str]:
    return list(hybrid_commands(Path("/work"), mode))


def check_catalog_report(report: Mapping, candidate: str, entries: int) -> None:
    if (report.get("success") is not True
            or report.get("candidate") != candidate
            or report.get("entries") != entries
            or report.get("repeats") != 30
            or len(report.get("fixture_sha256", {})) != 4 * entries
            or len(report.get("footprints", [])) != 30
            or len(report.get("recoveries", [])) != 30):
        raise ReviewRequired("incomplete/wrong catalog micro-benchmark")
    for op in ("publish", "publish_set", "pin_resolve",
               "reopen_recover_pin_resolve"):
        if report.get("latency", {}).get(op, {}).get("count") != 30:
            raise ReviewRequired("catalog operation was not measured 30 times")
    for row in report["recoveries"]:
        if (row.get("candidate") != candidate or row.get("pid", 0) <= 0
                or row.get("factory") != "*main.catalog"
                or row.get("ns", 0) <= 0):
            raise ReviewRequired("wrong/uninvoked recovery factory")
    if any(row.get("files", 0) <= 0 or row.get("bytes", 0) <= 0
           for row in report["footprints"]):
        raise ReviewRequired("missing catalog footprint")


def check_hybrid_recipe(method: str, result: Mapping,
                        native: Mapping | None = None) -> None:
    if (result.get("method") != method or result.get("phase") != "latency"
            or result.get("queries") != 256
            or result.get("latency_ns", {}).get("count") != 256):
        raise ReviewRequired("hybrid recipe/result substitution or coverage")
    if method == "int8":
        if (result.get("candidate_k") != 128
                or result.get("required_file_components_bytes", {}).get(
                    "int8_with_original_f32", 0) <= 0):
            raise ReviewRequired("int8 scan must retain original F32/top128")
    elif method == "f32":
        cfg = (native or {}).get("config", {})
        if (result.get("M") != 32 or result.get("ef_search") != 1024
                or cfg.get("Connectivity") != 32
                or cfg.get("ExpansionAdd") != 256
                or cfg.get("Quantization") != 1
                or cfg.get("Dimensions") != 1024
                or (native or {}).get("native_version") != "2.26.5"):
            raise ReviewRequired("native F32 requires M32/efAdd256/efSearch1024")
    elif method != "exact":
        raise ReviewRequired("unsupported generated timing recipe")


def output_files(roots: Sequence[Path]) -> list[Path]:
    paths = []
    for root in roots:
        if root.is_file():
            paths.append(root)
        elif root.is_dir():
            for here, _, files in os.walk(root, followlinks=False):
                paths.extend(Path(here) / name for name in files
                             if name != "complete.json")
        else:
            raise ReviewRequired(f"missing auxiliary output: {root}")
    return sorted(set(paths))


def auxiliary(supervisor: Supervisor, name: str, args: Sequence[str],
              validate: Callable[[Path], Sequence[Path]], *,
              selection: Sequence[str] = (),
              cwd: Path | None = None,
              accepted_exit_codes: Sequence[int] = (0,)) -> dict:
    root = supervisor.root
    directory = root / "aux" / name
    env = {**supervisor.environment,
           "GOMAXPROCS": str(supervisor.job_bindings[name]["gomaxprocs"])}
    working_directory = cwd or root / "checkout"
    identity = {"source": COMMIT, "command": list(args),
                "inputs_verified": name not in supervisor.bad_jobs,
                "cwd": str(working_directory),
                "environment": env, "selected_jobs": list(selection),
                "python_runtime": digest(root / "python-runtime.json")}
    def verify_cached():
        receipt = read_json(directory / "complete.json")
        if receipt["identity"] != identity:
            raise ReviewRequired("auxiliary identity drift")
        for relative, sha in receipt["files"].items():
            path = root / relative
            if not path.resolve().is_relative_to(root) or digest(path) != sha:
                raise ReviewRequired("auxiliary output drift")
        validate(directory)
        return receipt
    cached = reusable(directory, verify_cached) if directory.exists() else None
    if cached is not None:
        return cached
    new_attempt(supervisor, directory)
    atomic_json(directory / "started.json", identity)
    if selection:
        atomic_json(directory / "selected-jobs.json", list(selection))
    supervisor.job_limit(name, time.monotonic())
    owned = supervisor.start(args, directory, env, working_directory)
    while owned.process.poll() is None:
        supervisor.observe(process_groups(supervisor.environment,
                                          supervisor.active))
        if time.monotonic() >= owned.deadline:
            raise DeadlineExceeded(f"{name}: absolute auxiliary deadline")
        time.sleep(5)
    supervisor.close(owned)
    paths = output_files([directory, *validate(directory)])
    if owned.process.returncode not in accepted_exit_codes:
        raise ReviewRequired(f"auxiliary command failed: {name}")
    for path in paths:
        if not path.resolve().is_relative_to(root):
            raise FatalSession("auxiliary output escaped /work")
    receipt = {"identity": identity, "correctness_pass": False,
               "attempt": read_json(directory / "attempt.json"),
               "exit_code": owned.process.returncode,
               "wall_seconds": time.monotonic()-owned.started,
               "sampled_group_peak_rss_bytes_lower_bound": owned.peak_rss,
               "memory_scope": "whole block including build/preparation",
               "files": {str(p.relative_to(root)): digest(p) for p in paths},
               "file_bytes": {str(p.relative_to(root)): p.stat().st_size
                              for p in paths}}
    atomic_json(directory / "complete.json", receipt)
    return receipt


def hybrid_outputs(root: Path, mode: str) -> list[Path]:
    work = root / "home/.prime/research/gortex/storage-architecture/tmp"
    work = work / "vector/hybrid-followup"
    runs = work / "runs" / mode / ("pod-A1-" + mode)
    for name, expected_command in hybrid_commands(root, mode).items():
        receipt = read_json(runs / (name + ".json"))
        if tuple(receipt.get("command", ())) != expected_command:
            raise ReviewRequired("hybrid selected recipe/command substitution")
        if (receipt.get("exit_code") != 0 or receipt.get("platform") != "linux"
                or receipt.get("architecture") != "amd64"):
            raise ReviewRequired("incomplete/wrong-platform hybrid result")
        if receipt["kind"] != "timing":
            continue
        if not receipt["timing_clean"]:
            raise ReviewRequired("noisy hybrid timing cannot be accepted")
        result = receipt["result"]
        flags = dict(zip(receipt["command"][1::2],
                         receipt["command"][2::2]))
        if mode == "timing":
            if (result.get("queries") != 256
                    or result.get("n") != int(flags["-n"])
                    or result.get("dimensions") != int(flags["-dimensions"])
                    or result.get("workers") != int(flags["-workers"])
                    or result.get("candidate_pool") !=
                    "global quantized top128, then original F32 rerank"):
                raise ReviewRequired("int8 timing recipe/input drift")
        else:
            folder = Path(flags["-dataset"])
            native = None
            if flags["-method"] == "f32":
                native = read_json(folder / "native-M32-f32-rep1/build.json")
            check_hybrid_recipe(flags["-method"], result, native)
            if (receipt.get("input_receipt_sha256")
                    != digest(folder / "input-receipt.json")):
                raise ReviewRequired("generated input receipt mismatch")
            for file, fp in read_json(folder / "input-receipt.json").items():
                if file not in {"document.f32", "query.f32", "records.json",
                                "query-records.json"}:
                    raise ReviewRequired("unexpected generated input path")
                if digest(folder / file) != fp["sha256"]:
                    raise ReviewRequired("generated input file drift")
    binary = work / "bin/linux-amd64" / ("timing" if mode == "timing" else "ann")
    data = ([work / "timing-data"] if mode == "timing" else
            [work / f"generated-n{n}" for n in (10000, 100000)])
    return [runs, binary, *data]


def run_catalog_hybrid(supervisor: Supervisor,
                       sdk: Sequence[Mapping], block: str) -> None:
    root = supervisor.root
    passed = {row["identity"]["job"]["adapter"] for row in sdk
              if row["identity"]["job"]["stage"] == "screen"
              and row["state"] == "pass"}
    receipts = []
    for candidate in CATALOGS if block == "catalog" else ():
        if candidate not in passed:
            continue
        for entries in (1, 10, 100):
            name = f"catalog-{candidate}-n{entries}"
            out = root / "aux" / name / "output"
            args = [str(root / "bin" / candidate), "catalog-bench",
                    "-candidate", candidate, "-entries", str(entries),
                    "-repeats", "30", "-seed", "20261025", "-out", str(out)]
            def validate(directory: Path) -> list[Path]:
                check_catalog_report(read_json(directory / "output/result.json"),
                                     candidate, entries)
                return []
            receipts.append(auxiliary(supervisor, name, args, validate))
    controller = (root / "checkout/study/storage/vector/hybrid-followup/run.py")
    python = root / "python" / PYTHON_KEY / "bin/python3.12"
    for mode in ("timing", "latency") if block == "hybrid" else ():
        name = "hybrid-" + mode
        args = [str(python), "-B", str(controller), mode, "--execute",
                "--run-id", "pod-A1-" + mode, "--jobs-file",
                str(root / "aux" / name / "selected-jobs.json")]
        if mode == "latency":
            args += ["--generated", "--native-dir", str(root / "native")]
        receipts.append(auxiliary(
            supervisor, name, args, lambda _: hybrid_outputs(root, mode),
            selection=hybrid_names(mode), cwd=controller.parent))
    atomic_json(root / (block + "-summary.json"), {
        "source": COMMIT, "blocks": receipts,
        "skipped_catalogs_failed_correctness": sorted(set(CATALOGS)-passed),
        "skipped_optional": ["1M exact/int8 timing and generated latency"],
        "data_scope": "generated vectors; bypass segment adapters; no real-vector timing",
    })


def binding_block(supervisor: Supervisor, block: Mapping) -> str:
    states = []
    for job in block["jobs"]:
        directory = supervisor.root / "aux" / job["id"]
        path = directory / "receipt.json"
        if not path.resolve().is_relative_to(supervisor.root):
            raise FatalSession(f"{block['id']}: receipt path escaped /work")

        def validate(directory: Path) -> list[Path]:
            receipt = read_json(path)
            if not isinstance(receipt, Mapping):
                raise ReviewRequired(f"{block['id']}: malformed receipt")
            if receipt.get("clean_handoff") is not True:
                raise FatalSession(
                    f"{block['id']}: unclean receipt; A9 required")
            for key in ("source_pin", "config_pin", "input_pin"):
                if receipt.get(key) != job[key]:
                    raise ReviewRequired(f"{block['id']}: receipt {key}")
            if receipt.get("state") not in {
                    "completed", "checks_failed", "partial",
                    "dependency_blocked", "skipped"}:
                raise ReviewRequired(
                    f"{block['id']}: incomplete/fatal receipt")
            if receipt["state"] == "skipped" and (
                    block["id"] != "cpu_cost" or receipt.get("reason_code")
                    not in {"check", "library", "gate", "time", "identity"}):
                raise ReviewRequired(f"{block['id']}: unlisted skip reason")
            paths = [path]
            files = receipt.get("files")
            if not isinstance(files, dict) or not files:
                raise ReviewRequired(f"{block['id']}: missing result files")
            for relative, expected in files.items():
                output = directory / relative
                if (not owned_path(str(output), directory)
                        or not output.resolve().is_relative_to(directory)
                        or not sha_pin(expected, (64,))
                        or digest(output) != expected):
                    raise ReviewRequired(
                        f"{block['id']}: result file identity")
                paths.append(output)
            return paths

        receipt = auxiliary(
            supervisor, job["id"], [str(path) if arg == job["receipt_path"]
                                    else arg for arg in job["argv"]], validate,
            cwd=directory, accepted_exit_codes=(0, 1),
        )
        result = read_json(path)
        if (result.get("exit_code") != receipt["exit_code"]
                or (receipt["exit_code"] == 1
                    and result["state"] not in {"checks_failed", "partial"})):
            raise ReviewRequired(f"{block['id']}: fatal exit/receipt mismatch")
        if result["state"] in {"partial", "dependency_blocked", "skipped"}:
            (directory / "complete.json").rename(
                directory / "partial-complete.json")
        states.append(result["state"])
    if "partial" in states:
        return "partial"
    if "dependency_blocked" in states:
        return "dependency_blocked"
    if "checks_failed" in states:
        return "checks_failed"
    if "skipped" in states:
        return "skipped"
    return "completed"


def block_files(root: Path, block: Mapping) -> dict[str, str]:
    paths = {}
    for job in block["jobs"]:
        path = (root / "setup-complete.json" if block["id"] == "setup" else
                root / ("jobs" if block["id"] in {"storage", "segment_timing"}
                        else "aux") / job["id"] / "complete.json")
        if not path.exists():
            continue  # Measured correctness rejection can omit dependent jobs.
        paths[str(path.relative_to(root))] = digest(path)
        receipt = read_json(path)
        global_paths = block["id"] in {"catalog", "hybrid"} | INPUT_BLOCKS
        base = root if global_paths else path.parent
        if block["id"] == "setup":
            base = root
        files = receipt.get("files", receipt.get("artifacts", {}))
        for relative, expected in files.items():
            target = base / relative
            if (not target.resolve().is_relative_to(root)
                    or digest(target) != expected):
                raise ReviewRequired(f"{block['id']}: completed output drift")
            paths[str(target.relative_to(root))] = expected
    reports = {
        "segment_timing": ("summary.json", "summary.md"),
        "catalog": ("catalog-summary.json",),
        "hybrid": ("hybrid-summary.json",),
    }
    for relative in reports.get(block["id"], ()):
        target = root / relative
        if target.exists():
            paths[relative] = digest(target)
    return paths


def alarm_expired(signum: int, frame: object) -> None:
    raise DeadlineExceeded("absolute block guard expired")


def execute_session(supervisor: Supervisor, admission: Mapping, inputs: Path,
                    identity: Mapping, concurrency: int) -> dict:
    supervisor.bad_jobs = bad_jobs(supervisor.manifest_file,
                                   supervisor.scratch)
    if "*" in supervisor.bad_jobs:
        raise ReviewRequired("common input missing/changed")
    root = supervisor.root
    path = root / "session.json"
    session_identity = {
        "configuration": dict(admission), "setup": dict(identity),
        "native": supervisor.native_identity,
    }
    record = {"identity": session_identity, "blocks": {}}
    if path.exists():
        record = read_json(path)
        if record["identity"] != session_identity:
            raise ReviewRequired("session identity changed; no resume")
    outcomes = {}
    sdk = []
    setup = None
    signal.signal(signal.SIGALRM, alarm_expired)
    for block in admission["blocks"]:
        block_id = block["id"]
        saved = record["blocks"].get(block_id)
        if saved and saved.get("terminal") == "fatal":
            raise FatalSession("known fatal attempt needs diagnosis")
        if (saved and saved["state"] in {"completed", "checks_failed"}
                and not any(j["id"] in supervisor.bad_jobs
                            for j in block["jobs"])
                and all((root / relative).resolve().is_relative_to(root)
                        and (root / relative).is_file()
                        and digest(root / relative) == expected
                        for relative, expected in saved["files"].items())):
            outcomes[block_id] = saved["state"]
            if block_id == "setup" and saved["state"] != "completed":
                raise ReviewRequired("setup incomplete; A9 required")
            if block_id == "setup":
                setup = prepare(
                    supervisor, inputs, identity)
            elif block_id == "storage":
                if any(not (root / "jobs" / job["id"] / "complete.json")
                       .is_file() for job in block["jobs"]):
                    raise ReviewRequired(
                        "storage: partial receipts; no resume")
                sdk = [read_json(root / "jobs" / job["id"] / "complete.json")
                       for job in block["jobs"]]
            signal.setitimer(signal.ITIMER_REAL, 0)
            continue
        try:
            supervisor.begin_block(block, outcomes)
        except DeadlineExceeded as error:
            state = {
                "state": "partial",
                "reason": str(error), "reason_code": "time",
                "files": {}, "clean_handoff": True,
            }
            record["blocks"][block_id] = state
            outcomes[block_id] = state["state"]
            atomic_json(path, record)
            if block_id == "setup":
                raise ReviewRequired(
                    "setup: no time") from error
            continue
        except FatalSession:
            supervisor.stop_all()
            raise
        except ReviewRequired as error:
            if block_id != "cpu_cost":
                raise
            state = {"state": "dependency_blocked",
                     "reason": str(error),
                     "files": {}, "clean_handoff": True}
            record["blocks"][block_id] = state
            outcomes[block_id] = state["state"]
            atomic_json(path, record)
            continue
        if (block_id in {"segment_timing", "catalog", "hybrid"}
                and not any(row["identity"]["job"]["stage"] == "oracle"
                            and row["state"] == "pass" for row in sdk)):
            record["blocks"][block_id] = {
                "state": "dependency_blocked",
                "reason": "required oracle incomplete",
                "files": {}, "clean_handoff": True,
            }
            outcomes[block_id] = "dependency_blocked"
            atomic_json(path, record)
            continue
        record["blocks"][block_id] = {
            "state": "started", "files": {}}
        atomic_json(path, record)
        signal.setitimer(signal.ITIMER_REAL, max(
            0.000001, supervisor.block_deadline - CLEANUP_RESERVE_SECONDS
            - time.monotonic(),
        ))
        try:
            state = "completed"
            if block_id == "setup":
                setup = prepare(
                    supervisor, inputs, identity)
            elif block_id == "storage":
                planned = jobs()
                execute_stage(supervisor, planned[:1], setup, 1, sdk)
                execute_stage(
                    supervisor, planned[1:12], setup, concurrency, sdk)
                if any(row["state"] == "checks_failed" for row in sdk):
                    state = "checks_failed"
            elif block_id == "segment_timing":
                execute_stage(supervisor, eligible_timings(sdk), setup, 1, sdk)
            elif block_id in {"catalog", "hybrid"}:
                run_catalog_hybrid(supervisor, sdk, block_id)
            else:
                state = binding_block(supervisor, block)
        except (FatalSession, MemoryError) as error:
            signal.setitimer(signal.ITIMER_REAL, 0)
            try:
                record["blocks"][block_id].update(
                    terminal="fatal", reason=str(error))
                atomic_json(path, record)
            finally:
                supervisor.stop_all()
            raise
        except Exception as error:
            signal.setitimer(signal.ITIMER_REAL, 0)
            state = "partial"
            record["blocks"][block_id]["reason_code"] = type(error).__name__
            record["blocks"][block_id]["reason"] = str(error)
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
        # Any uncertain process/isolation failure propagates instead of
        # releasing an independent block.
        supervisor.stop_all()
        files = {}
        remaining = supervisor.block_deadline - time.monotonic()
        if remaining > 0:
            signal.setitimer(signal.ITIMER_REAL, remaining)
            try:
                files = block_files(root, block)
            except (FatalSession, MemoryError):
                raise
            except Exception as error:
                state = "partial"
                record["blocks"][block_id]["reason_code"] = "receipt"
                record["blocks"][block_id]["reason"] = (
                    f"receipt collection: {error}")
            finally:
                signal.setitimer(signal.ITIMER_REAL, 0)
        else:
            state = "partial"
        if state == "partial":
            record["blocks"][block_id].setdefault("reason_code", "runner")
            record["blocks"][block_id].setdefault(
                "reason", "partial runner outcome; see job receipt")
        record["blocks"][block_id].update(
            state=state, files=files, clean_handoff=True,
            absolute_deadline=supervisor.block_deadline,
        )
        outcomes[block_id] = state
        atomic_json(path, record)
        if block_id == "setup" and state != "completed":
            raise ReviewRequired(
                "setup incomplete")
    atomic_json(path, record)
    return record


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--dry-run", action="store_true")
    mode.add_argument("--native", type=Path, metavar="CONFIG")
    mode.add_argument("--resume", type=Path, metavar="CONFIG")
    parser.add_argument("--root", type=Path,
                        default=Path("/work"))
    parser.add_argument(
        "--concurrency", type=int, choices=(1, 2, 3), default=1,
    )
    args = parser.parse_args()
    root = args.root.absolute()
    planned = jobs()
    if args.dry_run:
        # Return before any directory write, subprocess, network or host probe.
        if (len(planned) != 24
                or sum(j.stage == "timing" for j in planned) != 12
                or any(j.adapter == "libsql-segment" for j in planned)
                or any(("-timing-only" in command(j, root))
                       != (j.stage == "timing") for j in planned)):
            raise ReviewRequired("invalid job matrix")
        print(json.dumps({
            "dry_run": True, "external_commands_executed": 0,
            "network_requests": 0, "directories_written": 0,
            "pure_contract_checks": dry_run_contract_checks(),
            "source": COMMIT,
            "archives": ARCHIVES, "adapters_built": ADAPTERS,
            "correctness_concurrency": args.concurrency,
            "timing_concurrency": 1, "source_branch": BRANCH,
            "jobs": [{**asdict(job), "command": shlex.join(command(job, root)),
                      "admission_peak_gib": projected_memory(job) / GIB}
                     for job in planned],
            "mandatory_owner_bindings": sorted(INPUT_BLOCKS),
            "creation_admitted": False,
            "auxiliary": {"catalog_blocks": 15,
                          "hybrid_selected_jobs": {mode: hybrid_names(mode)
                                                   for mode in ("timing", "latency")},
                          "optional_1M": "skipped"},
        }, indent=2))
        return 0
    config_path = args.native or args.resume
    config = read_json(config_path)
    scratch = persistent_path(config["scratch_root"],
                              Path(config["scratch_root"]))
    root = persistent_path(args.root, scratch)
    if root.parent != scratch / "mac-run" or args.concurrency != 1:
        raise ReviewRequired("serial run-id below scratch/mac-run required")
    inputs = persistent_path(config["inputs"], scratch)
    manifest_path = persistent_path(config["manifest_file"], scratch)
    config["input_manifest_sha256"] = digest(manifest_path)
    manifest = read_json(manifest_path)
    required = {str(Path(__file__).resolve()), str(config_path.resolve()),
                config["launcher"]["path"]}
    if not required <= set(manifest["jobs"].get("*", [])):
        raise ReviewRequired("unbound common driver/config/launcher")
    required.update(str(Path(d) / f) for d, dirs, names in os.walk(inputs)
                    for f in (*names, *(n for n in dirs
                                        if (Path(d) / n).is_symlink())))
    if not required <= manifest["files"].keys():
        raise ReviewRequired("unmanifested input/config/driver/launcher")
    if root.exists() != bool(args.resume):
        raise ReviewRequired("new run-id or explicit resume required")
    root.mkdir(parents=True, exist_ok=bool(args.resume))
    with (root / "driver.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        owner = Supervisor(root, environment(root), config)
        atomic_json(root / "python-runtime.json", owner.native_identity)
        identity = {"driver": digest(Path(__file__)),
                    "runtime": owner.native_identity}
        try:
            execute_session(owner, config, inputs, identity, 1)
        finally:
            try:
                owner.stop_all()
            finally:
                paths = output_files([root / name for name in (
                    "attempts", "jobs", "aux", "setup", "data", "native",
                    "bin", "downloads", "linking.json", "tool-versions.json")
                    if (root / name).exists()])
                atomic_json(root / "checkpoint.json", {
                    "run_id": root.name, "files": {
                        str(p.relative_to(root)): digest(p) for p in paths}})
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except ReviewRequired as error:
        print(f"STOP FOR OPERATOR REVIEW: {error}", file=sys.stderr)
        raise SystemExit(2) from error
