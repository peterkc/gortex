#!/usr/bin/env python3
"""Prepare/run the pinned public storage study. Python 3.10+, stdlib only."""

from __future__ import annotations

import argparse
from collections import Counter, deque
from collections.abc import Callable, Mapping, Sequence
from dataclasses import asdict, dataclass, field
from datetime import datetime
import ctypes
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path, PurePosixPath
import platform
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
from zoneinfo import ZoneInfo


COMMIT = "79af1c58f352c4eb3670045e31df84a476df9b19"
REPOSITORY = "https://github.com/peterkc/gortex.git"
BRANCH = "study/storage-harness"
GO_FILE = "go1.27.1.linux-amd64.tar.gz"
GO_SHA = "63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445"
USEARCH_SHA = (
    "96f1ed59f32303e612b2e72c393475696743b49e0832d33e65ad97aacf61d655"
)
USEARCH_URL = (
    "https://codeload.github.com/unum-cloud/USearch/tar.gz/refs/tags/v2.26.5"
)
GIB = 1024 ** 3
MEMORY_RESERVE = 16 * GIB
NODE_SCALE = 423567 / 81179
QUERY_SCALE = 811 / 694
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
UV_VERSION = "0.13.5"
UV_FILE = "uv-x86_64-unknown-linux-gnu.tar.gz"
UV_SHA = "f68d291c75783122dad58ab77def0e13f3233f358731db5d6d7e8715603b928f"
PYTHON_KEY = "cpython-3.12.15-linux-x86_64-gnu"
PYTHON_SHA = "6d7521138fd33afddc1ccd08abb007d9a2829e38117c2576f9efdc0ec73ce772"
# Sum of existing build/Open/query metric means*counts, not CLI wall time.
TIMED_LOWER_BOUND = (
    9.903676664 + 38.168377211
)
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
TIME_FORMAT = (
    '{"elapsed_seconds":%e,"user_seconds":%U,"system_seconds":%S,'
    '"max_rss_kib":%M,"exit_code":%x}'
)


class ReviewRequired(RuntimeError):
    """Stop without retrying or deleting evidence."""


class FatalSession(ReviewRequired):
    """Process, isolation or shared-resource failure; stop all blocks."""


# Absolute allocation minutes. These are limits, not measured predictions.
BLOCKS = (
    ("setup", 45, 0, 45), ("smoke", 25, 45, 70),
    ("calibration", 15, 70, 85), ("storage", 150, 85, 235),
    ("segment_timing", 120, 235, 355), ("catalog", 30, 355, 385),
    ("hybrid", 45, 385, 430), ("model_a4", 160, 430, 590),
    ("cpu_cost", 60, 590, 650), ("baseline_a7", 40, 650, 690),
    ("retrieval", 30, 690, 720),
)
INPUT_BLOCKS = frozenset({"smoke", "calibration", "model_a4", "cpu_cost",
                          "baseline_a7", "retrieval"})
APT_PACKAGES = (
    "build-essential", "cmake", "git", "ca-certificates", "curl", "zstd",
    "pkg-config", "binutils", "file", "time", "util-linux",
)
APT_PATHS = (
    "/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc", "/var/lib/apt",
    "/var/lib/dpkg", "/var/cache/apt", "/var/log",
)
CLEANUP_GRACE_SECONDS = 5.0
CLEANUP_RESERVE_SECONDS = 2 * CLEANUP_GRACE_SECONDS + 1


class DeadlineExceeded(ReviewRequired):
    """The owned work must stop; acceptance remains incomplete."""


def setup_recipe() -> dict:
    return {
        "owner": "pi-gcp", "host": "single-authorized-disposable-pod",
        "privilege": "root-or-sudo-n", "packages": list(APT_PACKAGES),
        "commands": ["apt-get update",
                     "apt-get install -y " + " ".join(APT_PACKAGES)],
        "allowed_system_paths": list(APT_PATHS),
        "dependencies": "package-manager-resolved-only",
        "study_root": "/work",
    }


def validate_recipe(recipe: Mapping) -> None:
    if not isinstance(recipe, Mapping):
        raise ReviewRequired("setup: malformed recipe")
    if recipe.get("owner") != "pi-gcp":
        raise ReviewRequired("setup: owner must be pi-gcp")
    if dict(recipe) != setup_recipe():
        raise ReviewRequired("setup: unlisted package, command or effect path")


def validate_procedure(procedure: Mapping) -> None:
    if not isinstance(procedure, Mapping):
        raise ReviewRequired("termination procedure: malformed")
    required = {
        "owner": "pi-gcp", "mechanism": "Mac launchd one-shot",
        "terminate_argv": ["prime", "pods", "terminate", "<pod-id>",
                           "--yes", "--plain"],
        "status_argv": ["prime", "pods", "status", "<pod-id>", "-o", "json"],
        "time_origin": "created_at",
        "existing_cli_login": True, "no_api_key_on_pod": True,
        "provider_ttl": False,
    }
    for key, value in required.items():
        if (type(procedure.get(key)) is not type(value)
                or procedure[key] != value):
            raise ReviewRequired(f"termination procedure: {key}")
    backstop = procedure.get("backstop_minute")
    if type(backstop) not in (int, float) or not math.isfinite(backstop):
        raise ReviewRequired("termination procedure: missing backstop_minute")
    if backstop < 690:
        raise ReviewRequired("backstop cuts a measurement window")
    if backstop > 720:
        raise ReviewRequired("backstop exceeds cap")
    awake = procedure.get("awake")
    if not isinstance(awake, str) or not awake.strip():
        raise ReviewRequired("termination procedure: awake")
    fields = {
        "job_label", "plist_path", "pod_id", "created_at", "deadline_pdt",
        "calendar_entry", "install_time_pdt", "awake", "launchctl_state",
        "setup_confirmed", "owner", "existing_cli_login",
    }
    provided = procedure.get("live_receipt_fields")
    if (not isinstance(provided, list)
            or any(not isinstance(field, str) for field in provided)
            or len(provided) != len(fields) or set(provided) != fields):
        raise ReviewRequired("termination procedure: live_receipt_fields")


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
                   "minutes": 45,
                   "receipt_path": str(root / "setup-complete.json")}],
        "storage": [{"id": job.name, "argv": command(job, root),
                     "minutes": 150,
                     "receipt_path": str(
                         root / "jobs" / job.name / "complete.json")}
                    for job in jobs() if job.stage != "timing"],
        "segment_timing": [{"id": job.name, "argv": command(job, root),
                            "minutes": 120,
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
                "id": name, "minutes": 2,
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
            "id": name, "minutes": 22.5, "argv": argv,
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


def preflight(bindings: Mapping, recipe: Mapping,
              procedure: Mapping, root: Path, *, concurrency: int = 1) -> dict:
    """Validate prospective inputs without a pod, host probe or side effect."""
    validate_recipe(recipe)
    validate_procedure(procedure)
    if not isinstance(bindings, Mapping):
        raise ReviewRequired("bindings: malformed object")
    if set(bindings) != INPUT_BLOCKS:
        missing = sorted(INPUT_BLOCKS - set(bindings))
        extra = sorted(set(bindings) - INPUT_BLOCKS)
        raise ReviewRequired(f"bindings: missing {missing}; unlisted {extra}")
    inventory = bound_jobs(root)
    inventory["setup"][0]["owner"] = recipe["owner"]
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
            if (block != "retrieval"
                    and Path(argv[0]).name in {"apt-get", "sudo"}):
                raise ReviewRequired(f"{block}: forbidden setup-owner command")
            for key, lengths in (("source_pin", (40, 64)),
                                 ("config_pin", (64,)), ("input_pin", (64,))):
                if not sha_pin(entry[key], lengths):
                    raise ReviewRequired(f"{block}: malformed {key}")
            if (block != "retrieval"
                    and not owned_path(entry["receipt_path"], root)):
                raise ReviewRequired(f"{block}: malformed receipt_path")
            if block == "retrieval" and (
                    not isinstance(entry["receipt_path"], str)
                    or not entry["receipt_path"]
                    or any(ord(c) < 32 for c in entry["receipt_path"])):
                raise ReviewRequired("retrieval: malformed receipt_path")
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
    for block, minutes, _, _ in BLOCKS:
        if (block not in {"storage", "segment_timing"}
                and sum(job["minutes"] for job in inventory[block]) > minutes):
            raise ReviewRequired(
                f"{block}: job allocations exceed {minutes} minutes")
    return {
        "phase1": "prepared-no-pod", "source": COMMIT,
        "enforcement_entrypoint": "study/storage/pod/run.py:main",
        "enforcement_argv": ["python3", "/work/control/run.py", "--execute",
                             "--root", "/work", "--inputs", "/work/inputs",
                             "--concurrency", str(concurrency),
                             "--bindings", "/work/control/bindings.json",
                             "--setup-recipe",
                             "/work/control/setup-recipe.json",
                             "--stop-procedure",
                             "/work/control/stop-procedure.json",
                             "--allocation", "/work/control/allocation.json",
                             "--termination-receipt",
                             "/work/control/termination.json"],
        "cleanup_reserve_seconds": CLEANUP_RESERVE_SECONDS,
        "blocks": [{"id": block, "minutes": minutes,
                    "latest_start_minute": latest, "deadline_minute": deadline,
                    "jobs": inventory[block],
                    "execution_scope": (
                        "external-owner" if block == "retrieval" else "pod")}
                   for block, minutes, latest, deadline in BLOCKS],
        "setup_recipe": dict(recipe), "termination_procedure": dict(procedure),
        "deletion_owner": "pi-gcp", "total_minutes": 720,
        "measurement_deadline_minute": 690,
        "backstop_minute": procedure["backstop_minute"],
        "retrieval_deadline_minute": min(720, procedure["backstop_minute"]),
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


def live_termination(allocation: Mapping, receipt: Mapping,
                     now_wall: float) -> tuple[float, float]:
    if not isinstance(allocation, Mapping) or not isinstance(receipt, Mapping):
        raise ReviewRequired(
            "A9/pi-gcp deletion: missing/malformed live receipt")
    pod = allocation.get("pod_id")
    if not isinstance(pod, str) or not pod:
        raise ReviewRequired("A9/pi-gcp deletion: allocation pod_id missing")
    if (not isinstance(allocation.get("job_label"), str)
            or not allocation["job_label"].strip()):
        raise ReviewRequired(
            "A9/pi-gcp deletion: allocation job_label missing")
    zero = timestamp(allocation.get("created_at"),
                     "A9/pi-gcp allocation.created_at")
    deadline = timestamp(allocation.get("deadline_pdt"), "backstop deadline")
    if deadline < zero + 690 * 60:
        raise ReviewRequired(
            "A9/pi-gcp deletion: backstop cuts a measurement window")
    if deadline > zero + 720 * 60:
        raise ReviewRequired("A9/pi-gcp deletion: backstop exceeds cap")
    if not zero <= now_wall < deadline:
        raise ReviewRequired("A9/pi-gcp deletion: absolute backstop expired")
    expected = {
        "owner": "pi-gcp", "pod_id": pod, "setup_confirmed": True,
        "existing_cli_login": True, "created_at": allocation["created_at"],
        "deadline_pdt": allocation.get("deadline_pdt"),
        "job_label": allocation.get("job_label"),
    }
    for key, value in expected.items():
        if (not value or type(receipt.get(key)) is not type(value)
                or receipt[key] != value):
            raise ReviewRequired(f"A9/pi-gcp deletion: termination {key}")
    plist = receipt.get("plist_path")
    if not isinstance(plist, str) or not plist.strip():
        raise ReviewRequired("A9/pi-gcp deletion: plist_path")
    zone = ZoneInfo("America/Los_Angeles")
    local = datetime.fromtimestamp(deadline, zone)
    received = parse_time(receipt["deadline_pdt"],
                          "A9/pi-gcp deletion: deadline_pdt")
    if received.utcoffset() != local.utcoffset():
        raise ReviewRequired(
            "A9/pi-gcp deletion: deadline must use local PDT/PST")
    expected_calendar = {"Month": local.month, "Day": local.day,
                         "Hour": local.hour, "Minute": local.minute}
    calendar = receipt.get("calendar_entry")
    if (not isinstance(calendar, Mapping)
            or set(calendar) != set(expected_calendar)
            or any(type(calendar[key]) is not int
                   or calendar[key] != value
                   for key, value in expected_calendar.items())
            or local.second or local.microsecond):
        raise ReviewRequired("A9/pi-gcp deletion: calendar_entry")
    installed = timestamp(receipt.get("install_time_pdt"),
                          "A9/pi-gcp install_time_pdt")
    if not zero <= installed <= now_wall:
        raise ReviewRequired("A9/pi-gcp deletion: install_time_pdt")
    awake = receipt.get("awake")
    if not isinstance(awake, Mapping):
        raise ReviewRequired("A9/pi-gcp deletion: awake")
    pid = awake.get("caffeinate_pid")
    if type(pid) is not int or pid <= 0:
        raise ReviewRequired("A9/pi-gcp deletion: awake.caffeinate_pid")
    flags = awake.get("flags")
    if (not isinstance(flags, list) or len(flags) != 5
            or flags[:4] != ["-i", "-m", "-s", "-t"]
            or not isinstance(flags[4], str) or not flags[4].isdecimal()
            or int(flags[4]) <= 0):
        raise ReviewRequired("A9/pi-gcp deletion: awake.flags")
    end = timestamp(awake.get("end_time_pdt"),
                    "A9/pi-gcp awake.end_time_pdt")
    if end < deadline + 15 * 60:
        raise ReviewRequired("A9/pi-gcp deletion: awake.end_time_pdt")
    if type(awake.get("ac_power")) is not bool:
        raise ReviewRequired("A9/pi-gcp deletion: awake.ac_power")
    for field, value in (("install_time_pdt", installed),
                         ("awake.end_time_pdt", end)):
        text = (receipt["install_time_pdt"] if field == "install_time_pdt"
                else awake["end_time_pdt"])
        if (parse_time(text, f"A9/pi-gcp deletion: {field}").utcoffset()
                != datetime.fromtimestamp(value, zone).utcoffset()):
            raise ReviewRequired(f"A9/pi-gcp deletion: {field} timezone")
    state = receipt.get("launchctl_state")
    if (not isinstance(state, Mapping) or state.get("loaded") is not True
            or state.get("disabled") is not False):
        raise ReviewRequired("A9/pi-gcp deletion: launchctl_state")
    return zero, deadline


def block_limit(block: str, now: float, zero: float, hard: float,
                outcomes: Mapping[str, str], *, a3_live: bool) -> float:
    position = next(i for i, row in enumerate(BLOCKS) if row[0] == block)
    _, allocation, latest, deadline = BLOCKS[position]
    for previous, _, _, _ in BLOCKS[:position]:
        if outcomes.get(previous) not in {
                "completed", "checks_failed", "dependency_blocked",
                "partial", "skipped"}:
            raise ReviewRequired(
                f"{block}: predecessor {previous} has no clean handoff")
    if block == "cpu_cost":
        if hard - now <= 70 * 60:
            raise DeadlineExceeded(
                "cpu_cost: preserve A7/deletion 70-minute reserve")
        for required in ("storage", "segment_timing", "catalog", "hybrid"):
            if outcomes.get(required) not in {"completed", "checks_failed"}:
                raise ReviewRequired(f"cpu_cost: dependency {required}")
        if outcomes.get("model_a4") not in {"completed", "dependency_blocked"}:
            raise ReviewRequired("cpu_cost: dependency model_a4")
    if block == "baseline_a7" and a3_live:
        raise ReviewRequired("baseline_a7: A3 is still live; A9 required")
    if block != "setup" and now > zero + latest * 60:
        raise DeadlineExceeded(f"{block}: latest start T+{latest} exceeded")
    # Setup includes startup from allocation; it never restarts a 45m clock.
    start = zero if block == "setup" else now
    later = sum(row[1] for row in BLOCKS[position + 1:]) * 60
    end = min(start + allocation * 60, zero + deadline * 60, hard - later)
    if end - now <= CLEANUP_RESERVE_SECONDS:
        raise DeadlineExceeded(
            f"{block}: no work time before absolute deadline")
    if block == "cpu_cost" and end - now < allocation * 60:
        raise DeadlineExceeded("cpu_cost: insufficient complete CPU window")
    return end


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


def schedule_minutes(concurrency: int, scale: float) -> float:
    """No CPU speedup; FIFO simulation, 180 GiB initially available."""
    pending = deque(SCREEN)
    active: list[tuple[float, int]] = []
    elapsed = 0.0
    capacity = 180 * GIB - MEMORY_RESERVE
    while pending or active:
        while pending and len(active) < concurrency:
            name = pending[0]
            need = projected_memory(Job("screen", name, "tier10"))
            if sum(memory for _, memory in active) + need > capacity:
                break
            pending.popleft()
            active.append((elapsed + OBSERVED[name][0] * scale, need))
        if not active:
            raise ReviewRequired("estimate cannot admit a correctness case")
        elapsed = min(end for end, _ in active)
        active = [(end, memory) for end, memory in active if end > elapsed]
    return elapsed / 60


def estimates() -> dict:
    lower = 3 * TIMED_LOWER_BOUND * (1 + NODE_SCALE) / 60
    rows = []
    for concurrency in (1, 2, 3):
        for label, scale in (
            ("node-scaled", NODE_SCALE),
            ("node-and-query-scaled", NODE_SCALE * QUERY_SCALE),
        ):
            oracle = OBSERVED["oracle"][0] * scale / 60
            screen = schedule_minutes(concurrency, scale)
            rows.append({
                "concurrency": concurrency, "model": label,
                "setup_minutes_assumed": 45, "oracle_minutes": oracle,
                "screen_minutes": screen,
                "timing_minutes_assumed": [2 * lower, 3 * lower],
                "total_hours_assumed": [
                    (45 + oracle + screen + factor * lower) / 60
                    for factor in (2, 3)
                ],
            })
    return {
        "node_scale": NODE_SCALE, "query_scale": QUERY_SCALE,
        "timing_raw_metric_lower_bound_minutes": lower,
        "timing_overhead_multiplier_assumed": [2, 3],
        "linux_or_timing_only_measurement": False, "models": rows,
        "scope": "SDK only; catalog/hybrid setup and timing are not measured",
        "complete_A1_upper_bound": False,
    }


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


def memory_available() -> int:
    fields = dict(line.split(":", 1) for line in
                  Path("/proc/meminfo").read_text().splitlines())
    return int(fields["MemAvailable"].split()[0]) * 1024


def process_groups() -> dict[int, dict[tuple[int, int], int]]:
    """PGID -> {(PID, start ticks): RSS bytes}; tolerate exiting processes."""
    groups: dict[int, dict[tuple[int, int], int]] = {}
    for entry in Path("/proc").iterdir():
        if not entry.name.isdecimal():
            continue
        try:
            fields = (entry / "stat").read_text().rsplit(")", 1)[1].split()
            pgid, start = int(fields[2]), int(fields[19])
            rss = int(fields[21]) * os.sysconf("SC_PAGE_SIZE")
            groups.setdefault(pgid, {})[(int(entry.name), start)] = rss
        except (FileNotFoundError, ProcessLookupError):
            continue
    return groups


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

    def __init__(self, root: Path, environment: Mapping[str, str], *,
                 allocation: Mapping, termination: Mapping):
        wall, mono = time.time(), time.monotonic()
        allocated, deadline = live_termination(allocation, termination, wall)
        self.zero = mono - (wall - allocated)
        self.hard_deadline = self.zero + 720 * 60
        self.backstop_deadline = mono + (deadline - wall)
        self.allocation, self.termination = allocation, termination
        self.block_deadline = 0.0
        self.launch_deadline = 0.0
        self.block_id = "unbound"
        self.job_minutes: dict[str, float] = {}
        signal.signal(signal.SIGALRM, alarm_expired)
        # Adopt orphaned descendants so PID 1 is not our reaping mechanism.
        libc = ctypes.CDLL(None, use_errno=True)
        if libc.prctl(36, 1, 0, 0, 0) != 0:  # PR_SET_CHILD_SUBREAPER
            raise ReviewRequired("cannot own descendant reaping; A9 required")
        self.root = root
        self.environment = dict(environment)
        self.active: list[OwnedProcess] = []
        self.serial = 0

    def begin_block(self, block: Mapping, outcomes: Mapping[str, str]) -> None:
        self.block_id = block["id"]
        self.block_deadline = block_limit(
            self.block_id, time.monotonic(), self.zero, self.hard_deadline,
            outcomes, a3_live=bool(self.active),
        )
        if self.block_id == "retrieval":
            self.block_deadline = min(
                self.block_deadline, self.backstop_deadline,
            )
        self.launch_deadline = self.block_deadline - CLEANUP_RESERVE_SECONDS
        self.job_minutes = {job["id"]: job["minutes"] for job in block["jobs"]}

    def job_limit(self, name: str, now: float) -> float:
        if name not in self.job_minutes:
            raise ReviewRequired(f"{self.block_id}: unlisted job {name}")
        self.launch_deadline = min(
            self.block_deadline - CLEANUP_RESERVE_SECONDS,
            now + self.job_minutes[name] * 60 - CLEANUP_RESERVE_SECONDS,
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
        """Reap owned children, including unseen adopted zombies."""
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
        try:
            live_termination(self.allocation, self.termination, time.time())
        except Exception as error:
            raise FatalSession(str(error)) from error
        if args and Path(args[0]).name in {"apt-get", "sudo"}:
            raise ReviewRequired(
                "driver cannot run apt-get or sudo; pi-gcp owns SSH setup")
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
        try:
            try:
                if time.monotonic() >= self.launch_deadline:
                    raise DeadlineExceeded(
                        f"{self.block_id}: expired before spawn")
                process = subprocess.Popen(
                    args, cwd=cwd, env=environment, stdout=log,
                    stderr=subprocess.STDOUT, start_new_session=True,
                    preexec_fn=lambda: signal.pthread_sigmask(
                        signal.SIG_SETMASK, previous_mask,
                    ),
                )
            except (OSError, DeadlineExceeded):
                log.close()
                raise
            owned = OwnedProcess(
                process, directory, time.monotonic(), log=log,
                deadline=self.launch_deadline,
            )
            self.active.append(owned)
            self.observe(process_groups())
            atomic_json(directory / "process.json", {
                "pid": process.pid, "pgid": process.pid,
                "boot_id": Path(
                    "/proc/sys/kernel/random/boot_id"
                ).read_text(),
                "known_pid_start_ticks": sorted(owned.known),
                "command": list(args), "cwd": str(cwd),
                "absolute_deadline": owned.deadline,
            })
            self.arm_guard()
            return owned
        finally:
            signal.pthread_sigmask(signal.SIG_SETMASK, previous_mask)

    def observe(self, groups: Mapping) -> None:
        for owned in self.active:
            members = groups.get(owned.process.pid, {})
            owned.known.update(members)
            rss = sum(members.values())
            owned.peak_rss = max(owned.peak_rss, rss)
            with (owned.directory / "rss.jsonl").open("a") as stream:
                stream.write(json.dumps({
                    "elapsed_seconds": time.monotonic() - owned.started,
                    "group_rss_bytes": rss,
                    "mem_available_bytes": memory_available(),
                    "loadavg": list(os.getloadavg()),
                }) + "\n")

    def close(self, owned: OwnedProcess) -> None:
        self.reap()
        members = process_groups().get(owned.process.pid, {})
        if members:
            raise FatalSession(
                "job exited with surviving session members; "
                "A9/pi-gcp deletion")
        owned.log.close()
        self.active.remove(owned)
        self.arm_guard()

    def stop_all(self) -> None:
        for sig in (signal.SIGTERM, signal.SIGKILL):
            groups = process_groups()
            for owned in self.active:
                members = groups.get(owned.process.pid, {})
                if not members:
                    continue
                if not owned.known.intersection(members):
                    raise FatalSession(
                        "unverified process identity; no kill; "
                        "A9/pi-gcp deletion"
                    )
                owned.known.update(members)
                try:
                    os.killpg(owned.process.pid, sig)
                except ProcessLookupError:
                    continue
            deadline = time.monotonic() + CLEANUP_GRACE_SECONDS
            while time.monotonic() < deadline:
                self.reap()
                groups = process_groups()
                if not any(groups.get(p.process.pid) for p in self.active):
                    break
                time.sleep(0.1)
        self.reap()
        groups = process_groups()
        survivors = [p.process.pid for p in self.active
                     if groups.get(p.process.pid)]
        for owned in self.active:
            owned.log.close()
            if owned.process.poll() is None:
                survivors.append(owned.process.pid)
            else:
                owned.process.wait(timeout=0)
        if survivors:
            raise FatalSession(
                f"owned sessions remain: {survivors}; A9/pi-gcp deletion")
        self.active.clear()

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
            status = owned.process.wait(
                timeout=max(0, owned.deadline - time.monotonic()),
            )
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


def host_record() -> dict:
    if platform.system() != "Linux" or platform.machine() != "x86_64":
        raise ReviewRequired("execution requires Linux x86_64")
    release = dict(line.split("=", 1) for line in
                   Path("/etc/os-release").read_text().splitlines()
                   if "=" in line)
    if (release.get("VERSION_ID", "").strip('"') != "22.04"
            or release.get("ID", "").strip('"') != "ubuntu"):
        raise ReviewRequired("execution requires Ubuntu 22.04 x86_64")
    if len(os.sched_getaffinity(0)) < 30:
        raise ReviewRequired("fewer than 30 visible CPUs")
    meminfo = Path("/proc/meminfo").read_text()
    total = next(int(line.split()[1]) * 1024 for line in meminfo.splitlines()
                 if line.startswith("MemTotal:"))
    if total < 180 * GIB:
        raise ReviewRequired("less than 180 GiB visible RAM")
    return {
        "cpuinfo": Path("/proc/cpuinfo").read_text(),
        "meminfo": meminfo,
        "kernel": platform.release(), "platform": platform.platform(),
        "euid": os.geteuid(),
        "affinity": sorted(os.sched_getaffinity(0)),
        "loadavg": list(os.getloadavg()), "os_release": release,
    }


def verify_package_versions(supervisor: Supervisor) -> dict[str, str]:
    """Read package metadata; pi-gcp owns all installation over SSH."""
    parsed = []
    malformed = False
    for field in ("${Version}", "${db:Status-Status}"):
        log = supervisor.run([
            "dpkg-query", "-W", "-f", "${Package} " + field + "\n",
            *APT_PACKAGES,
        ], accepted_exit_codes=(0, 1))
        rows = {}
        for line in log.read_text().splitlines():
            values = line.split()
            if (len(values) != 2 or values[0] not in APT_PACKAGES
                    or values[0] in rows):
                malformed = True
                continue
            rows[values[0]] = values[1]
        parsed.append(rows)
    versions, states = parsed
    for package in APT_PACKAGES:
        if package not in versions or package not in states:
            raise ReviewRequired(f"setup: missing required package {package}")
        if states[package] != "installed":
            raise ReviewRequired(f"setup: package not installed: {package}")
    if malformed:
        raise ReviewRequired("setup: malformed dpkg-query output")
    return versions


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


def prepare(supervisor: Supervisor, inputs: Path, identity: Mapping,
            recipe: Mapping) -> dict:
    validate_recipe(recipe)
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
        raise ReviewRequired("incomplete setup; operator review required")
    if not set(path.name for path in root.iterdir()) <= {
            "driver.lock", "control", "inputs", "session.json", "lost+found"}:
        raise ReviewRequired("new workspace permits only control/inputs/lock")
    if not inputs.resolve().is_relative_to(root.resolve()):
        raise ReviewRequired("pod inputs must be copied under /work")
    if shutil.disk_usage(root).free < 500 * GIB:
        raise ReviewRequired("require at least 500 GiB free study disk")
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
    atomic_json(root / "host.json", {
        **host_record(), "disk": shutil.disk_usage(root)._asdict(),
    })
    atomic_json(root / "setup-recipe.json", dict(recipe))
    package_versions = verify_package_versions(supervisor)
    atomic_json(root / "setup-packages.json", {
        "owner": "pi-gcp", "versions": package_versions,
        "verification": "read-only dpkg-query; no installation by driver",
    })
    go_archive = root / "downloads" / GO_FILE
    download("https://go.dev/dl/" + GO_FILE, go_archive, GO_SHA, 70553950)
    extract(go_archive, root / "toolchain", "go")
    uv_archive = root / "downloads" / UV_FILE
    download(f"https://github.com/astral-sh/uv/releases/download/"
             f"{UV_VERSION}/{UV_FILE}", uv_archive, UV_SHA, 64 * 1024**2)
    extract(uv_archive, root / "toolchain", "uv-x86_64-unknown-linux-gnu")
    uv = root / "toolchain/uv-x86_64-unknown-linux-gnu/uv"
    supervisor.run([str(uv), "--no-config", "python", "install", PYTHON_KEY,
                    "--install-dir", str(root / "python"), "--no-bin"])
    python = root / "python" / PYTHON_KEY / "bin/python3.12"
    version = supervisor.run([str(python), "--version"]).read_text().strip()
    if version != "Python 3.12.15" or not python.resolve().is_relative_to(root):
        raise ReviewRequired("managed Python version/path mismatch")
    atomic_json(root / "python-runtime.json", {
        "uv_version": UV_VERSION, "uv_archive_sha256": UV_SHA,
        "python_key": PYTHON_KEY, "python_archive_sha256": PYTHON_SHA,
        "version": version, "executable_sha256": digest(python),
        "metadata_source": "astral-sh/uv@0.13.5:"
        "crates/uv-python-managed/download-metadata.json",
    })
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
    for name in ("neon-eval", "pod-models", "pod-cost"):
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
            env["CGO_LDFLAGS"] = (
                f"-L{native} -Wl,-rpath,{native} -lstdc++ -lm -pthread"
            )
        binary = root / "bin" / name
        supervisor.run([
            "go", "build", "-p=1", "-o", str(binary), target,
        ], cwd=directory, env=env)
        description = supervisor.run(["file", str(binary)]).read_text()
        headers = supervisor.run([
            "readelf", "--program-headers", "--wide", str(binary),
        ]).read_text()
        dynamic = supervisor.run(["readelf", "-d", str(binary)]).read_text()
        interpreter = any(line.split()[:1] == ["INTERP"]
                          for line in headers.splitlines())
        needed = [line for line in dynamic.splitlines()
                  if "(NEEDED)" in line]
        dependencies = ""
        if interpreter or needed:
            dependencies = supervisor.run(["ldd", str(binary)]).read_text()
            if "=> not found" in dependencies:
                raise ReviewRequired(f"missing runtime library: {name}")
            for line in dependencies.splitlines():
                fields = line.split()
                library_path = None
                if len(fields) >= 3 and fields[1] == "=>":
                    library_path = fields[2]
                elif fields and fields[0].startswith("/"):
                    library_path = fields[0]
                if library_path and library_path.startswith("/"):
                    runtime_libraries[library_path] = digest(
                        Path(library_path)
                    )
        linking[name] = {
            "file": description, "program_headers": headers,
            "dynamic_section": dynamic, "ldd": dependencies,
            "fully_static_elf": not interpreter and not needed,
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
                     root / "toolchain" / "go" / "bin" / "go",
                     root / "linking.json", root / "tool-versions.json",
                     uv, python, root / "python-runtime.json",
                     root / "setup-recipe.json", root / "setup-packages.json"]
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
        "runtime_libraries": runtime_libraries,
        "package_versions": package_versions, "setup_owner": "pi-gcp",
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
        "DEBIAN_FRONTEND": "noninteractive", "GIT_TERMINAL_PROMPT": "0",
        "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
        "GOTOOLCHAIN": "local", "GOENV": "off", "GOWORK": "off",
        "GOMAXPROCS": "2",
        "GOFLAGS": "-mod=readonly", "GOPROXY": "https://proxy.golang.org",
        "GOSUMDB": "sum.golang.org",
        "GOCACHE": str(root / "cache" / "go-build"),
        "GOMODCACHE": str(root / "cache" / "go-mod"),
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
    }


def finished_case(root: Path, job: Job, expected: Mapping) -> dict:
    directory = root / "jobs" / job.name
    receipt = read_json(directory / "complete.json")
    if receipt["identity"] != expected:
        raise ReviewRequired(f"completed case identity changed: {job.name}")
    for relative, sha256 in receipt["files"].items():
        if digest(directory / relative) != sha256:
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
        "timing_medians": medians, "estimates": estimates(),
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
    root = supervisor.root
    pending = deque(selected)
    active: dict[str, tuple[Job, OwnedProcess, dict]] = {}
    while pending or active:
        groups = process_groups()
        supervisor.observe(groups)
        if memory_available() < MEMORY_RESERVE:
            raise FatalSession("memory stop: MemAvailable below 16 GiB")
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
            timing = read_json(directory / "time.json")
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
                "correctness_pass": (
                    job.stage != "timing" and verdict.state == "pass"
                ),
                "exit_code": code,
                "wall_seconds": timing["elapsed_seconds"],
                "user_seconds": timing["user_seconds"],
                "system_seconds": timing["system_seconds"],
                "peak_rss_bytes": max(
                    report["peak_rss_bytes"], owned.peak_rss,
                    timing["max_rss_kib"] * 1024,
                ),
                "sampled_group_peak_is_lower_bound": True,
                "metrics": report["metrics"],
                "failed_checks": report["failures"],
                "files": {
                    str(p.relative_to(directory)): digest(p)
                    for p in (path, directory / "time.json",
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
            gomaxprocs = 30 if job.stage != "screen" else 30 // concurrency
            identity = case_identity(job, root, setup, gomaxprocs)
            directory = root / "jobs" / job.name
            if directory.exists():
                if not (directory / "complete.json").exists():
                    raise ReviewRequired(f"incomplete attempt: {job.name}")
                receipt = finished_case(root, job, identity)
                receipts.append(receipt)
                pending.popleft()
                if job.stage == "oracle" and receipt["state"] != "pass":
                    raise ReviewRequired("saved reference oracle did not pass")
                print(job.name, "validated receipt; skipped", flush=True)
                continue
            reserve = sum(
                max(0, projected_memory(old_job)
                    - sum(groups.get(owned.process.pid, {}).values()))
                for old_job, owned, _ in active.values()
            )
            need = projected_memory(job) + reserve + MEMORY_RESERVE
            if need > memory_available():
                if not active:
                    raise FatalSession(
                        f"insufficient free memory: {job.name}"
                    )
                break
            directory.mkdir()
            atomic_json(directory / "started.json", identity)
            env = {**supervisor.environment, "GOMAXPROCS": str(gomaxprocs)}
            supervisor.job_limit(job.name, time.monotonic())
            owned = supervisor.start([
                "/usr/bin/time", "-q", "-f", TIME_FORMAT, "-o",
                str(directory / "time.json"), *command(job, root),
            ], directory, env, root)
            active[job.name] = (job, owned, identity)
            pending.popleft()
            # Refresh after launch; reserve the new job's pending RSS growth.
            groups = process_groups()
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
    env = {**supervisor.environment, "GOMAXPROCS": "30"}
    working_directory = cwd or root / "checkout"
    identity = {"source": COMMIT, "command": list(args),
                "cwd": str(working_directory),
                "environment": env, "selected_jobs": list(selection),
                "python_runtime": digest(root / "python-runtime.json")}
    if directory.exists():
        if not (directory / "complete.json").exists():
            raise ReviewRequired(f"incomplete auxiliary attempt: {name}")
        receipt = read_json(directory / "complete.json")
        if receipt["identity"] != identity:
            raise ReviewRequired("auxiliary identity drift")
        for relative, sha in receipt["files"].items():
            path = root / relative
            if not path.resolve().is_relative_to(root) or digest(path) != sha:
                raise ReviewRequired("auxiliary output drift")
        validate(directory)
        return receipt
    directory.mkdir()
    atomic_json(directory / "started.json", identity)
    if selection:
        atomic_json(directory / "selected-jobs.json", list(selection))
    supervisor.job_limit(name, time.monotonic())
    owned = supervisor.start(args, directory, env, working_directory)
    while owned.process.poll() is None:
        supervisor.observe(process_groups())
        if memory_available() < MEMORY_RESERVE:
            raise FatalSession("auxiliary memory stop; A9 required")
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
        if block["id"] == "retrieval":
            # Deletion uses pi-gcp's existing login on the Mac, never the pod.
            atomic_json(supervisor.root / "A9-handoff.json", {
                "state": "external-owner-required", "jobs": block["jobs"],
                "owner": "pi-gcp",
                "deadline_pdt": supervisor.allocation["deadline_pdt"],
                "no_prime_api_key_on_pod": True,
            })
            return "dependency_blocked"
        path = Path(job["receipt_path"])
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
                output = supervisor.root / relative
                if (not owned_path(str(output), supervisor.root)
                        or not output.resolve().is_relative_to(supervisor.root)
                        or not sha_pin(expected, (64,))
                        or digest(output) != expected):
                    raise ReviewRequired(
                        f"{block['id']}: result file identity")
                paths.append(output)
            return paths

        receipt = auxiliary(
            supervisor, job["id"], job["argv"], validate,
            cwd=supervisor.root, accepted_exit_codes=(0, 1),
        )
        result = read_json(path)
        if (result.get("exit_code") != receipt["exit_code"]
                or (receipt["exit_code"] == 1
                    and result["state"] not in {"checks_failed", "partial"})):
            raise ReviewRequired(f"{block['id']}: fatal exit/receipt mismatch")
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
    if block["id"] == "retrieval":
        # Its authoritative receipt belongs to the external owner.
        return paths
    for job in block["jobs"]:
        path = Path(job["receipt_path"])
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
    root = supervisor.root
    path = root / "session.json"
    session_identity = {
        "configuration": dict(admission), "setup": dict(identity),
        "allocation": dict(supervisor.allocation),
        "termination": dict(supervisor.termination),
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
        if saved:
            remaining = min(supervisor.zero + 690 * 60,
                            supervisor.backstop_deadline) - time.monotonic()
            if remaining <= 0:
                raise DeadlineExceeded("resume cutoff T+690; A9 required")
            signal.setitimer(signal.ITIMER_REAL, remaining)
            if saved["state"] == "started":
                raise ReviewRequired(
                    f"{block_id}: incomplete attempt; no replay")
            for relative, expected in saved["files"].items():
                target = root / relative
                if (not target.resolve().is_relative_to(root)
                        or digest(target) != expected):
                    raise ReviewRequired(f"{block_id}: completed output drift")
            outcomes[block_id] = saved["state"]
            if block_id == "setup" and saved["state"] != "completed":
                raise ReviewRequired("setup incomplete; A9 required")
            if block_id == "setup":
                setup = prepare(
                    supervisor, inputs, identity, admission["setup_recipe"])
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
                "state": "skipped" if block_id == "cpu_cost" else "partial",
                "reason": str(error), "reason_code": "time",
                "files": {}, "clean_handoff": True,
            }
            record["blocks"][block_id] = state
            outcomes[block_id] = state["state"]
            atomic_json(path, record)
            if block_id == "setup":
                raise ReviewRequired(
                    "setup: no time; A9/pi-gcp deletion required") from error
            continue
        except ReviewRequired as error:
            if block_id != "cpu_cost":
                raise
            state = {"state": "dependency_blocked", "reason": str(error),
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
        record["blocks"][block_id] = {"state": "started", "files": {}}
        atomic_json(path, record)
        signal.setitimer(signal.ITIMER_REAL, max(
            0.000001, supervisor.block_deadline - CLEANUP_RESERVE_SECONDS
            - time.monotonic(),
        ))
        try:
            state = "completed"
            if block_id == "setup":
                setup = prepare(
                    supervisor, inputs, identity, admission["setup_recipe"])
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
        except (FatalSession, MemoryError):
            raise
        except Exception as error:
            signal.setitimer(signal.ITIMER_REAL, 0)
            state = "partial"
            record["blocks"][block_id]["reason_code"] = type(error).__name__
            record["blocks"][block_id]["reason"] = str(error)
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
        # Any uncertain process/isolation failure propagates instead of
        # releasing an independent block. No work continues after minute 690.
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
                "setup incomplete; A9/pi-gcp deletion required")
    record["acceptance"] = (
        "complete-pending-independent-deletion"
        if all(outcomes.get(block) in {"completed", "checks_failed"}
               for block, _, _, _ in BLOCKS
               if block not in {"cpu_cost", "retrieval"})
        else "blocked-incomplete-required-evidence"
    )
    atomic_json(path, record)
    return record


def interrupted(signum: int, frame: object) -> None:
    raise FatalSession(f"interrupted by signal {signum}; no replay")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--dry-run", action="store_true")
    mode.add_argument("--execute", action="store_true")
    mode.add_argument("--preflight", action="store_true")
    parser.add_argument("--bindings", type=Path)
    parser.add_argument("--setup-recipe", type=Path)
    parser.add_argument("--stop-procedure", type=Path)
    parser.add_argument("--allocation", type=Path)
    parser.add_argument("--termination-receipt", type=Path)
    parser.add_argument("--root", type=Path,
                        default=Path("/work"))
    parser.add_argument("--inputs", type=Path,
                        default=Path("/work/inputs"))
    parser.add_argument(
        "--concurrency", type=int, choices=(1, 2, 3), default=1,
    )
    args = parser.parse_args()
    root, inputs = args.root.absolute(), args.inputs.absolute()
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
            "source": COMMIT, "go_archive": GO_FILE,
            "archives": ARCHIVES, "adapters_built": ADAPTERS,
            "correctness_concurrency": args.concurrency,
            "timing_concurrency": 1, "source_branch": BRANCH,
            "jobs": [{**asdict(job), "command": shlex.join(command(job, root)),
                      "admission_peak_gib": projected_memory(job) / GIB}
                     for job in planned],
            "estimates": estimates(),
            "absolute_schedule": BLOCKS,
            "mandatory_owner_bindings": sorted(INPUT_BLOCKS),
            "creation_admitted": False,
            "auxiliary": {"catalog_blocks": 15,
                          "hybrid_selected_jobs": {mode: hybrid_names(mode)
                                                   for mode in ("timing", "latency")},
                          "optional_1M": "skipped", "uv": UV_VERSION,
                          "managed_python": PYTHON_KEY},
        }, indent=2))
        return 0
    for field_name in ("bindings", "setup_recipe", "stop_procedure"):
        if getattr(args, field_name) is None:
            raise ReviewRequired(f"preflight: missing {field_name}")
    admission = preflight(
        read_json(args.bindings), read_json(args.setup_recipe),
        read_json(args.stop_procedure), root, concurrency=args.concurrency,
    )
    admission["driver_sha256"] = digest(Path(__file__))
    if args.preflight:
        print(json.dumps(admission, indent=2))
        return 0
    if args.allocation is None or args.termination_receipt is None:
        raise ReviewRequired(
            "A9/pi-gcp deletion: missing live termination receipt")
    allocation = read_json(args.allocation)
    termination = read_json(args.termination_receipt)
    live_termination(allocation, termination, time.time())
    if root != Path("/work"):
        raise ReviewRequired("execution requires the dedicated /work root")
    host = host_record()
    if root.is_symlink():
        raise ReviewRequired("workspace must not be a symlink")
    root.mkdir(parents=True, exist_ok=True)
    with (root / "driver.lock").open("a+") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise ReviewRequired(
                "another driver owns this workspace"
            ) from error
        identity = {
            "source": COMMIT, "driver": digest(Path(__file__)),
            "archives": ARCHIVES,
            "runtime": {
                "python": platform.python_version(), "kernel": host["kernel"],
                "cpu_identity": sorted({
                    line.strip() for line in host["cpuinfo"].splitlines()
                    if line.split(":", 1)[0].strip() in {
                        "vendor_id", "cpu family", "model", "model name",
                        "stepping", "microcode", "flags",
                    }
                }),
                "affinity": host["affinity"],
                "correctness_concurrency": args.concurrency,
            },
        }
        supervisor = Supervisor(
            root, environment(root), allocation=allocation,
            termination=termination,
        )
        for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            signal.signal(sig, interrupted)
        try:
            result = execute_session(
                supervisor, admission, inputs, identity, args.concurrency,
            )
            print(result["acceptance"], flush=True)
            if result["acceptance"] == "blocked-incomplete-required-evidence":
                return 2
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
                signal.signal(sig, signal.SIG_IGN)
            try:
                supervisor.stop_all()
            except Exception as error:
                atomic_json(root / "A9-handoff.json", {
                    "owner": "pi-gcp", "deletion_required": True,
                    "clean_handoff": False, "reason": str(error),
                    "deadline_pdt": allocation["deadline_pdt"],
                })
                raise
            handoff = root / "A9-handoff.json"
            atomic_json(handoff, {
                **(read_json(handoff) if handoff.exists() else {}),
                "owner": "pi-gcp", "deletion_required": True,
                "clean_handoff": sys.exc_info()[0] is None,
                "deadline_pdt": allocation["deadline_pdt"],
            })
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except ReviewRequired as error:
        print(f"STOP FOR OPERATOR REVIEW: {error}", file=sys.stderr)
        raise SystemExit(2) from error
