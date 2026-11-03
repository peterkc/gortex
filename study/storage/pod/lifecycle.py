"""Own one approved Prime CLI 0.17.0 rental and cutoff."""
import argparse
import ipaddress
import json
import math
import os
import select
import signal
import shlex
import shutil
import subprocess
import sys
import threading
import time
import uuid
from collections.abc import Mapping
from dataclasses import dataclass
from decimal import Decimal, InvalidOperation
from datetime import datetime, timezone
from pathlib import Path
from typing import assert_never


VERSION = "0.17.0"
OWNER_SIGNALS = (signal.SIGINT, signal.SIGTERM, signal.SIGHUP, signal.SIGQUIT)


def save(path: Path, value: object) -> None:
    temporary = path.with_suffix(path.suffix + "." + uuid.uuid4().hex + ".tmp")
    temporary.write_text(json.dumps(value, indent=2) + "\n")
    temporary.replace(path)


@dataclass(frozen=True, slots=True)
class Invalid:
    reason: str


class CreateRefused(Exception):
    """No create subprocess was started."""


@dataclass(frozen=True, slots=True)
class NotCreated:
    pass


@dataclass(frozen=True, slots=True)
class Unidentified:
    pass


@dataclass(frozen=True, slots=True)
class Identified:
    pod_id: str


@dataclass(frozen=True, slots=True)
class Attempted:
    pod_id: str


Lease = NotCreated | Unidentified | Identified | Attempted


def needs_owner(lease: Lease) -> bool:
    match lease:
        case Unidentified() | Identified():
            return True
        case NotCreated() | Attempted():
            return False
        case _:
            assert_never(lease)


def execution_pins(value: Mapping, now: float) -> Mapping | Invalid:
    from artifacts import MAX_PAYLOAD, TAR_OVERHEAD

    required = ("cli_context", "offer_id", "cloud_id", "image", "disk_gb",
                "quoted_total_hourly", "quote_checked_epoch",
                "disk_hourly_per_gb", "disk_price_reference",
                "harness_commit", "quote_reference", "gpu_label", "provider",
                "vcpus", "memory_gb", "max_seconds", "cutoff_seconds",
                "budget", "hourly_ceiling", "contingency_seconds",
                "setup_seconds", "minimum_free_bytes", "minimum_memory_bytes",
                "payload_sha256", "session_sha256", "environment_sha256",
                "location", "gpu_count", "ready_seconds",
                "analysis_seconds", "copy_seconds", "bootstrap_seconds",
                "minimum_upload_mbps", "upload_reference",
                "approval_reference",
                "token_json_max_bytes", "token_size_reference",
                "payload_path", "session_path", "environment_path",
                "ssh_key_path", "destination", "minimum_download_mbps",
                "download_reference", "expected_evidence_bytes",
                "evidence_size_reference", "cgroup_root")
    if any(key not in value or value[key] in (None, "") for key in required):
        return Invalid("every execution pin is mandatory; no defaults")
    texts = ("cli_context", "offer_id", "cloud_id", "image",
             "harness_commit", "quote_reference", "gpu_label", "provider",
             "location", "upload_reference", "approval_reference",
             "token_size_reference", "payload_path", "session_path",
             "environment_path", "ssh_key_path", "destination",
             "download_reference", "evidence_size_reference",
             "disk_price_reference")
    if any(not isinstance(value[key], str) or not value[key].strip()
           for key in texts):
        return Invalid("text execution pins must be nonempty strings")
    if not isinstance(value["disk_hourly_per_gb"], str):
        return Invalid("disk hourly rate must be a decimal string")
    try:
        disk_rate = Decimal(value["disk_hourly_per_gb"])
        price = Decimal(str(value["quoted_total_hourly"]))
        ceiling = Decimal(str(value["hourly_ceiling"]))
        budget = Decimal(str(value["budget"]))
    except InvalidOperation:
        return Invalid("quoted price must be a finite decimal")
    if not disk_rate.is_finite() or disk_rate < 0:
        return Invalid("disk hourly rate must be finite and nonnegative")
    checked = value["quote_checked_epoch"]
    if (not all(n.is_finite() for n in (price, ceiling, budget))
            or type(checked) not in (int, float)
            or not math.isfinite(checked)):
        return Invalid("price and quote timestamp must be finite")
    integers = ("disk_gb", "vcpus", "memory_gb", "max_seconds",
                "cutoff_seconds", "contingency_seconds", "setup_seconds",
                "minimum_free_bytes", "minimum_memory_bytes", "gpu_count",
                "ready_seconds", "analysis_seconds", "copy_seconds",
                "bootstrap_seconds", "token_json_max_bytes",
                "expected_evidence_bytes")
    if any(type(value[k]) is not int or value[k] < 1 for k in integers):
        return Invalid("resource and time pins must be positive integers")
    uplink = value["minimum_upload_mbps"]
    if (type(uplink) not in (int, float) or not math.isfinite(uplink)
            or uplink <= 0):
        return Invalid("minimum upload rate must be finite and positive")
    downlink = value["minimum_download_mbps"]
    if (type(downlink) not in (int, float) or not math.isfinite(downlink)
            or downlink <= 0):
        return Invalid("minimum download rate must be finite and positive")
    if value["expected_evidence_bytes"] > MAX_PAYLOAD + TAR_OVERHEAD:
        return Invalid("expected evidence exceeds the archive byte bound")
    if not isinstance(value["cgroup_root"], dict):
        return Invalid("cgroup root evidence must be a JSON object")
    if (value["provider"] != "nebius" or value["vcpus"] != 8
            or value["image"] != "ubuntu_22_cuda_12"
            or value["disk_gb"] != 100
            or value["memory_gb"] != 32 or value["gpu_count"] != 1
            or value["max_seconds"] > 10800 or value["cutoff_seconds"] > 10680
            or value["max_seconds"] - value["cutoff_seconds"] < 120
            or value["setup_seconds"] > 1800
            or value["analysis_seconds"] > 9600
            or not value["ready_seconds"] < value["setup_seconds"]
            < value["analysis_seconds"]
            or value["cutoff_seconds"] - value["analysis_seconds"]
            < 450 + value["copy_seconds"]
            or value["copy_seconds"] > 420
            or value["minimum_memory_bytes"] < 28 * 1024 ** 3
            or value["minimum_free_bytes"] < 32 * 1024 ** 3
            or budget > 2 or ceiling > Decimal("0.50")
            or value["token_json_max_bytes"] > 256 * 1024 ** 2):
        return Invalid("D14 profile/time/rate/copy refusal")
    worst_hours = Decimal(value["max_seconds"]
                          + value["contingency_seconds"]) / 3600
    if (not 0 < price <= ceiling
            or not 0 < worst_hours * ceiling <= budget
            or not value["setup_seconds"] < value["cutoff_seconds"]
            < value["max_seconds"]
            or not 0 <= now - checked <= 900
            or len(value["harness_commit"]) != 40
            or any(c not in "0123456789abcdef"
                   for c in value["harness_commit"])):
        return Invalid("disk/price/freshness/harness pin refusal")
    for name in ("payload_sha256", "session_sha256", "environment_sha256"):
        pin = value[name]
        if (not isinstance(pin, str) or len(pin) != 64
                or any(c not in "0123456789abcdef" for c in pin)):
            return Invalid("transport inputs require exact SHA-256 pins")
    return dict(value)


def configuration(path: Path) -> Mapping:
    value = json.loads(path.read_text())
    if not isinstance(value, dict):
        raise ValueError("execution file must be a JSON object")
    checked = execution_pins(value, time.time())
    if isinstance(checked, Invalid):
        raise ValueError(checked.reason)
    return checked


def within_budget(elapsed: float, estimate: Decimal, config: Mapping) -> bool:
    """Check time/rate estimates, not history billing."""
    return (0 <= elapsed <= config["max_seconds"] <= 10800
            and 0 < Decimal(str(config["quoted_total_hourly"]))
            <= Decimal(str(config["hourly_ceiling"])) <= Decimal("0.50")
            and 0 <= estimate <= Decimal(str(config["budget"])) <= 2)


def command(config: Mapping, *arguments: str,
            namespace: str = "pods") -> list[str]:
    return [config["cli_executable"], "--context", config["cli_context"],
            "--plain", namespace,
            *arguments]


def call(config: Mapping, state: Path, label: str, arguments: list[str],
         timeout: float = 30, *, not_after: float | None = None,
         cancel_descriptor: int | None = None,
         namespace: str = "pods") -> subprocess.CompletedProcess:
    if label == "terminate":
        # Issue deletion before journal writes, even if logging fails.
        result = subprocess.run(
            command(config, *arguments, namespace=namespace),
            stdin=subprocess.DEVNULL, capture_output=True,
            text=True, timeout=timeout, start_new_session=True)
        (state / "terminate.stdout").write_text(result.stdout)
        (state / "terminate.stderr").write_text(result.stderr)
        return result
    with (state / (label + ".stdout")).open("x") as stdout, \
            (state / (label + ".stderr")).open("x") as stderr:
        if not_after is not None:
            remaining = not_after - time.monotonic()
            cancelled = (cancel_descriptor is not None and select.select(
                [cancel_descriptor], [], [], 0)[0])
            if remaining <= 0 or cancelled:
                raise CreateRefused("create expired/cancelled before mutation")
            timeout = min(timeout, remaining)
        return subprocess.run(command(config, *arguments, namespace=namespace),
                              stdin=subprocess.DEVNULL,
                              stdout=stdout, stderr=stderr, timeout=timeout,
                              start_new_session=True)


def pages(config: Mapping, state: Path, mode: str, label: str,
          deadline: float | None = None) -> list[dict]:
    rows, offset = [], 0
    total = None
    seen = set()
    read_label = label + "-" + uuid.uuid4().hex
    deadline = min(time.monotonic() + 30,
                   deadline if deadline is not None else float("inf"))
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("bounded paginated read did not finish")
        page_label = f"{read_label}-{offset}"
        result = call(config, state, page_label,
                      [mode, "--output", "json", "--limit", "100",
                       "--offset", str(offset)], min(15, remaining))
        result.check_returncode()
        document = json.loads((state / (page_label + ".stdout")).read_text())
        chunk = document["pods" if mode == "list" else "history"]
        count = document["total_count"]
        if (type(document["offset"]) is not int
                or document["offset"] != offset or not isinstance(chunk, list)
                or type(count) is not int or count < 0
                or (total is not None and count != total)
                or offset + len(chunk) > count):
            raise ValueError("unexpected or changing CLI pagination schema")
        total = count
        for row in chunk:
            pod_id = row["id"]
            if (not isinstance(pod_id, str) or not pod_id
                    or pod_id.startswith("-")
                    or any(c.isspace() for c in pod_id) or pod_id in seen):
                raise ValueError("invalid or duplicate pod identity")
            seen.add(pod_id)
        rows.extend(chunk)
        offset += len(chunk)
        if offset == total:
            return rows
        if not chunk:
            raise ValueError("incomplete CLI result set")


def hourly_rate(value: object) -> Decimal:
    """Reject malformed or nonfinite rate fields at the CLI boundary."""
    if type(value) not in (str, int, float):
        raise ValueError("hourly rate must be a finite decimal")
    try:
        rate = Decimal(str(value))
    except InvalidOperation as error:
        raise ValueError("hourly rate is malformed") from error
    if not rate.is_finite() or rate < 0:
        raise ValueError("hourly rate must be finite and nonnegative")
    return rate


def disk_charge(config: Mapping) -> Decimal:
    return (Decimal(config["disk_gb"])
            * Decimal(config["disk_hourly_per_gb"]))


def total_hourly(compute: object, config: Mapping) -> Decimal:
    """Availability reports compute; disk units are an approved assumption."""
    return hourly_rate(compute) + disk_charge(config)


def conservative_hourly(config: Mapping) -> Decimal:
    # Status may already include disk. Bound either interpretation.
    return min(Decimal(str(config["hourly_ceiling"])),
               Decimal(str(config["quoted_total_hourly"]))
               + disk_charge(config))


def rate_receipt(status: Mapping, config: Mapping) -> dict:
    return {
        "raw_status_hourly": status.get("cost_per_hour"),
        "basis": "unknown, conservative",
        "quoted_expected_total_hourly": config["quoted_total_hourly"],
        "conservative_hourly_bound": str(conservative_hourly(config)),
        "disk_hourly_per_gb": config["disk_hourly_per_gb"],
        "disk_price_reference": config["disk_price_reference"],
    }


def mismatches(status: Mapping, config: Mapping) -> list[str]:
    expected = {"provider": config["provider"], "image": config["image"],
                "offer_id": config["offer_id"],
                "gpu": config["gpu_label"], "cloud_id": config["cloud_id"],
                "vcpus": config["vcpus"], "memory_gb": config["memory_gb"],
                "disk_gb": config["disk_gb"]}
    wrong = [key for key, value in expected.items()
             if key in status and status[key] is not None
             and status[key] != value]
    if ("is_spot" in status and status["is_spot"] is not None
            and status["is_spot"] is not False):
        wrong.append("is_spot")
    if "cost_per_hour" in status:
        rate = hourly_rate(status["cost_per_hour"])
        if (rate > Decimal(str(config["quoted_total_hourly"]))
                or rate + disk_charge(config)
                > Decimal(str(config["hourly_ceiling"]))):
            wrong.append("cost_per_hour")
    if status.get("team_id") is not None:
        wrong.append("team_id")
    return wrong


def status_read(config: Mapping, state: Path, pod_id: str, label: str) -> dict:
    result = call(config, state, label,
                  ["status", pod_id, "--output", "json"], 15)
    result.check_returncode()
    value = json.loads((state / (label + ".stdout")).read_text())
    if value["id"] != pod_id:
        raise ValueError("status returned another pod")
    return value


def stop_request(state: Path) -> None:
    if (state / "done.json").exists():
        done = json.loads((state / "done.json").read_text())
        if done.get("state") in ("deleted", "not-created"):
            return
    descriptor = os.open(state / "stop.fifo", os.O_WRONLY | os.O_NONBLOCK)
    try:
        os.write(descriptor, b"stop\n")
    finally:
        os.close(descriptor)


def request_cleanup(state: Path, primary: BaseException | None = None) -> None:
    """Ask owner to stop; attach failure to the supplied exception."""
    try:
        stop_request(state)
    except (OSError, ValueError) as error:
        if primary is None:
            raise
        primary.add_note("Cleanup request failed; escalate: " + repr(error))
        print("CLEANUP REQUEST FAILED: " + repr(error), file=sys.stderr)


def select_offer(document: Mapping, config: Mapping) -> dict:
    rows = document["gpu_resources"]
    if (not isinstance(rows, list) or not rows
            or type(document["total_count"]) is not int
            or document["total_count"] != len(rows)
            or document["filters"]["group_similar"] is not False):
        raise ValueError("empty/incomplete/grouped availability; no retry")
    matches = []
    for row in rows:
        if (row.get("cloud_id") == config["cloud_id"]
                and row.get("provider") == config["provider"]
                and row.get("location") == config["location"]
                and row.get("stock_status") == "Available"
                and row.get("gpu_type") == "CPU NODE"
                and type(row.get("gpu_count")) is int
                and row["gpu_count"] == config["gpu_count"]
                and row.get("vcpus") == str(config["vcpus"])
                and row.get("memory_gb") == str(config["memory_gb"])
                and row.get("disk_gb") in (str(config["disk_gb"]),
                                           str(config["disk_gb"]) + "+")
                and "is_spot" in row
                and (row["is_spot"] is None or row["is_spot"] is False)
                and total_hourly(row["price_value"], config)
                == Decimal(str(config["quoted_total_hourly"]))):
            matches.append(row)
    if len(matches) != 1 or matches[0]["id"] != config["offer_id"]:
        raise ValueError(
            "approved availability row is absent/ambiguous/changed")
    return matches[0]


def verify_deletion(config: Mapping, state: Path, pod_id: str,
                    started: float, deadline: float) -> None:
    attempt = 0
    while True:
        if time.monotonic() >= deadline:
            raise TimeoutError(
                "deletion unproved; use read-only verify and escalate")
        attempt += 1
        try:
            active = pages(config, state, "list", "post-terminate", deadline)
            history = pages(config, state, "history", "history", deadline)
        except (subprocess.CalledProcessError, ValueError, TimeoutError,
                subprocess.TimeoutExpired) as error:
            save(state / ("deletion-read-error-" + uuid.uuid4().hex + ".json"),
                 {"attempt": attempt, "observed_epoch": time.time(),
                  "monotonic": time.monotonic(), "error": repr(error)})
            remaining = deadline - time.monotonic()
            if remaining > 0:
                threading.Event().wait(min(5, remaining))
            continue
        if time.monotonic() >= deadline:
            raise TimeoutError("deletion observation exceeded reserve")
        entry = [row for row in history if row["id"] == pod_id]
        if (not any(row["id"] == pod_id for row in active)
                and len(entry) == 1 and entry[0].get("terminated_at")):
            try:
                ended = datetime.strptime(entry[0]["terminated_at"],
                    "%Y-%m-%d %H:%M:%S UTC"
                ).replace(tzinfo=timezone.utc).timestamp()
                # The CLI renders whole seconds; use the upper edge.
                elapsed = ended + 1 - started
            except (TypeError, ValueError):
                elapsed = -1  # Deletion remains recorded; time is unproved.
            estimate = (Decimal(str(max(0, elapsed))) / 3600
                        * conservative_hourly(config))
            accepted = within_budget(elapsed, estimate, config)
            save(state / "done.json", {
                "state": "deleted", "pod": entry[0],
                "elapsed_seconds": elapsed,
                "rate_time_estimate": str(estimate),
                "rate_basis": "unknown, conservative",
                "quoted_expected_total_hourly":
                    config["quoted_total_hourly"],
                "conservative_hourly_bound":
                    str(conservative_hourly(config)),
                "disk_price_reference": config["disk_price_reference"],
                "termination_time": ("unproved" if elapsed < 0
                                     else "UTC-second-upper-edge"),
                "within_time_and_approved_rate": accepted,
                "billed_cost": "not-verified; history total units unknown",
                "approval_reference": config["approval_reference"]})
            if not accepted:
                raise ValueError("deleted but time/rate proof failed")
            return
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            continue
        threading.Event().wait(min(5, remaining))


def root_facts_confirmed(value: object) -> bool:
    """Accept only actual D14 host/runtime facts from the first root check."""
    from admission import PYTHON_VERSION

    expected = {
        "root": "/sys/fs/cgroup", "writable": True,
        "memory_controller": True, "euid": 0, "machine": "x86_64",
        "os_id": "ubuntu", "os_version": "22.04",
        "python_implementation": "CPython", "python_version": PYTHON_VERSION,
    }
    if (not isinstance(value, dict)
            or set(value) != set(expected) | {"glibc_version"}
            or any(value[key] != fact for key, fact in expected.items())
            or type(value["euid"]) is not int
            or value["writable"] is not True
            or value["memory_controller"] is not True):
        return False
    version = value["glibc_version"]
    parts = version.split(".") if isinstance(version, str) else []
    return (len(parts) == 2
            and all(part.isascii() and part.isdecimal() for part in parts)
            and tuple(int(part) for part in parts) >= (2, 28))


def root_capability_confirmed(state: Path) -> bool:
    try:
        with (state / "root-capability.json").open("rb") as receipt:
            raw = receipt.read(1025)
        value = json.loads(raw) if len(raw) <= 1024 else None
    except (OSError, ValueError):
        return False
    return root_facts_confirmed(value)


def first_ssh_contact(state: Path, config: Mapping,
                      owner: Mapping) -> tuple[float, float] | None:
    """Return proved first-contact clocks, or None for unproved contact."""
    def unique_fields(pairs):
        value = dict(pairs)
        if len(value) != len(pairs):
            raise ValueError("duplicate SSH contact field")
        return value

    try:
        with (state / "ssh-contact.json").open("rb") as receipt:
            raw = receipt.read(1025)
        value = (json.loads(raw, object_pairs_hook=unique_fields)
                 if len(raw) <= 1024 else None)
        if (not isinstance(value, dict)
                or set(value) != {"connected_epoch", "connected_monotonic"}):
            return None
        epoch = value["connected_epoch"]
        monotonic = value["connected_monotonic"]
        started_epoch = owner.get("started_epoch")
        started_monotonic = owner.get("started_monotonic")
        ready_seconds = config.get("ready_seconds")
        clocks = (epoch, monotonic, started_epoch, started_monotonic,
                  ready_seconds)
        if any(type(clock) not in (int, float) or not math.isfinite(clock)
               for clock in clocks):
            return None
        if (not started_epoch <= epoch <= started_epoch + ready_seconds
                or not started_monotonic <= monotonic
                <= started_monotonic + ready_seconds):
            return None
        return float(epoch), float(monotonic)
    except (OSError, ValueError, OverflowError):
        return None


def connect(state: Path) -> None:
    config = json.loads((state / "config.json").read_text())
    session = json.loads((state / "session.json").read_text())
    try:
        refused = (state / "rental-error.json").exists()
        if (refused or not (state / "validated.json").exists()
                or (state / "done.json").exists()):
            raise ValueError("rental refused or already deleted")
        if time.time() - session["started_epoch"] >= config["setup_seconds"]:
            raise ValueError("paid setup allowance has expired")
        ready_deadline = session["started_epoch"] + config["ready_seconds"]
        attempt = 0
        while True:
            if time.time() >= ready_deadline:
                raise ValueError("approved readiness allowance expired")
            status = status_read(config, state, session["pod_id"],
                                 "connection-" + str(attempt))
            if mismatches(status, config):
                raise ValueError("reported rental pin mismatch")
            if time.time() >= ready_deadline:
                raise ValueError("approved readiness allowance expired")
            if status["status"] == "ACTIVE":
                break
            remaining = ready_deadline - time.time()
            if remaining <= 0:
                raise ValueError("pod not ACTIVE within setup allowance")
            threading.Event().wait(min(15, remaining))
            attempt += 1
        if any(status.get(key) is None for key in
               ("provider", "image", "gpu", "cost_per_hour")):
            raise ValueError("missing mandatory active-pod fact")
        save(state / "active-rate.json", rate_receipt(status, config))
        parts = shlex.split(status["ssh"])
        if (len(parts) != 3 or parts[1] != "-p"
                or not parts[0].startswith("root@")):
            raise ValueError("unsupported pinned CLI SSH connection format")
        ipaddress.ip_address(parts[0][5:].strip("[]"))
        if not parts[2].isdigit() or not 0 < int(parts[2]) < 65536:
            raise ValueError("invalid SSH port")
        save(state / "connection.json", {"target": parts[0], "port": parts[2]})
        (state / "connection.env").write_text(
            "export POD_SSH_TARGET=" + shlex.quote(parts[0]) + "\n"
            "export POD_SSH_PORT=" + shlex.quote(parts[2]) + "\n")
        save(state / "session-inputs.json", {
            **{key: config[key] for key in
               ("analysis_seconds", "approval_reference")},
            "harness_commit": config["harness_commit"],
            "max_seconds": config["max_seconds"],
            "setup_seconds": config["setup_seconds"],
            "budget": config["budget"],
            "hourly_ceiling": config["hourly_ceiling"],
            "provider": config["provider"],
            "started_epoch": session["started_epoch"],
            "cutoff_epoch": session["started_epoch"]
            + config["cutoff_seconds"],
            "vcpus": config["vcpus"], "memory_gb": config["memory_gb"],
            "minimum_free_bytes": config["minimum_free_bytes"],
            "minimum_memory_bytes": config["minimum_memory_bytes"],
        })
        save(state / "unavailable-status-fields.json", [
            key for key in ("offer_id", "cloud_id", "is_spot", "vcpus",
                            "memory_gb", "disk_gb")
            if key not in status])
    except BaseException as error:
        request_cleanup(state, error)
        raise


def rent(config_path: Path, state: Path) -> None:
    from admission import root_probe_seconds, validate

    approved = configuration(config_path)
    # No account operation precedes local refusals.
    upload = validate(approved)
    probe_seconds = (root_probe_seconds(approved)
                     if approved["cgroup_root"].get("mode")
                     == "first-in-pod-check" else None)
    executable = shutil.which("prime")
    if executable is None:
        raise FileNotFoundError("pinned Prime CLI is not installed")
    config = {**approved, "cli_executable": str(Path(executable).resolve()),
              "upload_seconds": upload}
    version = subprocess.check_output(
        [config["cli_executable"], "--version"], text=True,
        start_new_session=True)
    if version.strip() != "Prime CLI version: " + VERSION:
        raise ValueError("Prime CLI must be exactly " + VERSION)
    state.mkdir(mode=0o700)
    save(state / "config.json", config)
    available = call(config, state, "availability", [
        "list", "--gpu-type", "CPU_NODE", "--no-group-similar",
        "--output", "json"], namespace="availability")
    available.check_returncode()
    selected = select_offer(
        json.loads((state / "availability.stdout").read_text()), config)
    save(state / "selected-offer.json", selected)
    checked = execution_pins(config, time.time())
    if isinstance(checked, Invalid):
        raise ValueError(checked.reason)
    destination = Path(config["destination"])
    destination.mkdir(mode=0o700)
    identity = destination.stat()
    save(state / "destination-identity.json", {
        "path": str(destination), "device": identity.st_dev,
        "inode": identity.st_ino})
    name = "storage-" + uuid.uuid4().hex
    os.mkfifo(state / "stop.fifo", 0o600)
    descriptor = os.open(state / "stop.fifo", os.O_RDWR | os.O_NONBLOCK)
    started, monotonic_start = time.time(), time.monotonic()
    owner = {"pid": os.getpid(), "name": name,
             "started_epoch": started, "started_monotonic": monotonic_start,
             "cutoff_epoch": started + config["cutoff_seconds"]}
    save(state / "owner.json", owner)
    lease: Lease = Unidentified()
    lock = threading.Lock()
    errors: list[Exception] = []

    def record_cleanup_error(error: Exception, reason: str,
                             prefix: str) -> None:
        errors.append(error)
        try:
            save(state / ("owner-error-" + uuid.uuid4().hex + ".json"), {
                "reason": reason, "observed_epoch": time.time(),
                "monotonic": time.monotonic(), "error": repr(error)})
        except OSError as journal_error:
            error.add_note(
                "Owner error journal failed: " + repr(journal_error))
        try:
            print(prefix + repr(error), file=sys.stderr, flush=True)
        except OSError as stderr_error:
            error.add_note("Owner stderr failed: " + repr(stderr_error))

    def shutdown(reason: str) -> None:
        nonlocal lease
        with lock:
            match lease:
                case NotCreated() | Attempted():
                    return
                case Unidentified():
                    matches = [pod for pod in pages(
                        config, state, "list", "reconcile")
                               if pod["name"] == name]
                    if len(matches) != 1:
                        raise ValueError(
                            "rental identity unproved; "
                            "owner retained; escalate")
                    pod_id = matches[0]["id"]
                case Identified(pod_id=pod_id):
                    pass
                case _:
                    assert_never(lease)
            # No effect replay, including on journal IO failure.
            lease = Attempted(pod_id)
            deadline = min(time.monotonic() + 120,
                           monotonic_start + config["max_seconds"])
            try:
                result = call(config, state, "terminate",
                              ["terminate", pod_id, "--yes"], 30)
                save(state / "terminate-exit.json",
                     {"exit": result.returncode})
            except subprocess.TimeoutExpired:
                save(state / "terminate-exit.json",
                     {"exit": "timeout-uncertain"})
            save(state / "terminate-command.json", {
                "pod_id": pod_id, "reason": reason,
                "argv": command(config, "terminate", pod_id, "--yes")})
            verify_deletion(config, state, pod_id, started, deadline)

    def owner_deadlines() -> tuple[float, float]:
        epoch = started + config["cutoff_seconds"]
        monotonic = monotonic_start + config["cutoff_seconds"]
        if probe_seconds is None or root_capability_confirmed(state):
            return epoch, monotonic
        contact = first_ssh_contact(state, config, owner)
        if contact is None:
            return (min(epoch, started + config["ready_seconds"]),
                    min(monotonic, monotonic_start + config["ready_seconds"]))
        return (min(epoch, contact[0] + probe_seconds),
                min(monotonic, contact[1] + probe_seconds))

    def cutoff() -> None:
        try:
            shutdown("hard-cutoff")
        except Exception as error:
            record_cleanup_error(error, "hard-cutoff", "CUTOFF FAILED: ")
        finally:
            os.write(descriptor, b"cutoff\n")

    timer = threading.Timer(config["cutoff_seconds"], cutoff)
    timer.daemon = False
    handlers = {number: signal.signal(
        number, lambda *_: os.write(descriptor, b"host-signal\n"))
        for number in OWNER_SIGNALS}
    primary: BaseException | None = None
    timer.start()
    try:
        create = ["create", "--id", selected["id"], "--name", name,
                  "--vcpus", str(config["vcpus"]),
                  "--memory", str(config["memory_gb"]),
                  "--disk-size", str(config["disk_gb"]),
                  "--image", config["image"], "--yes"]
        save(state / "create-command.json", command(config, *create))
        try:
            result = call(config, state, "create", create, 90,
                          not_after=monotonic_start + 90,
                          cancel_descriptor=descriptor)
        except CreateRefused:
            with lock:
                lease = NotCreated()
            save(state / "done.json", {"state": "not-created"})
            raise
        result.check_returncode()
        matches = [pod for pod in pages(config, state, "list", "identify")
                   if pod["name"] == name]
        if len(matches) != 1:
            raise ValueError("ambiguous create; never create again")
        pod_id = matches[0]["id"]
        with lock:
            if not isinstance(lease, Unidentified):
                raise RuntimeError("cutoff already owns this rental")
            lease = Identified(pod_id)
        # Identity permits recovery, not connection yet.
        save(state / "session.json", {"pod_id": pod_id, "name": name,
                                     "started_epoch": started,
                                     "approval_reference":
                                     config["approval_reference"]})
        summary = [line.strip() for line in
                   (state / "create.stdout").read_text().splitlines()]
        request = {"cloudId": config["cloud_id"],
                   "vcpus": str(config["vcpus"]),
                   "memory": str(config["memory_gb"]),
                   "diskSize": str(config["disk_gb"]),
                   "image": config["image"], "provider": config["provider"],
                   "gpuCount": str(config["gpu_count"]), "team": "None"}
        if any(key + ": " + value not in summary
               for key, value in request.items()):
            raise ValueError("CLI request differs from approved offer")
        status = status_read(config, state, pod_id, "initial-status")
        if mismatches(status, config):
            raise ValueError("reported rental differs from approved pins")
        save(state / "initial-rate.json", rate_receipt(status, config))
        save(state / "unavailable-initial-fields.json", [key for key in
             ("offer_id", "cloud_id", "is_spot", "vcpus",
              "memory_gb", "disk_gb")
             if key not in status])
        save(state / "validated.json", {"pod_id": pod_id,
                                        "approval_reference":
                                        config["approval_reference"]})
        print("RENTED; state=" + str(state), flush=True)
    except BaseException as error:
        primary = error
        try:
            save(state / "rental-error.json", {"error": repr(error)})
        except OSError as journal_error:
            error.add_note("Rental journal failed: " + repr(journal_error))
    finally:
        # Keep this deletion owner, its handlers and FIFO after uncertain
        # create or failed early reconciliation (C-CUB-2).
        urgent = primary is not None
        while needs_owner(lease):
            epoch_deadline, monotonic_deadline = owner_deadlines()
            remaining = min(epoch_deadline - time.time(),
                            monotonic_deadline - time.monotonic())
            timeout = 0 if urgent else 15
            if remaining > 0:
                timeout = min(timeout, remaining)
            elif isinstance(lease, Identified):
                timeout = 0
            ready = select.select([descriptor], [], [], timeout)[0]
            # A receipt arriving during select takes effect before cleanup.
            epoch_deadline, monotonic_deadline = owner_deadlines()
            expired = (time.time() >= epoch_deadline
                       or time.monotonic() >= monotonic_deadline)
            if (not ready and not expired and not urgent
                    and not isinstance(lease, Unidentified)):
                continue
            reason = (os.read(descriptor, 4096).decode() if ready
                      else "owner-refusal-or-cutoff")
            urgent = False
            try:
                shutdown(reason)
            except Exception as error:
                record_cleanup_error(
                    error, reason,
                    "DELETION UNPROVED; owner retained if lease live: ")
        timer.cancel()
        timer.join()
        os.close(descriptor)
        for number, handler in handlers.items():
            signal.signal(number, handler)
    if primary is not None:
        for error in errors:
            primary.add_note("Cleanup observation: " + repr(error))
            for note in tuple(getattr(error, "__notes__", ())):
                primary.add_note(note)
        raise primary
    if errors:
        raise errors[-1]

def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("rent", "stop", "verify"))
    parser.add_argument("--state", type=Path, required=True)
    parser.add_argument("--config", type=Path)
    args = parser.parse_args()
    if not args.state.is_absolute():
        parser.error("state must be an absolute private host directory")
    if args.mode == "rent":
        if args.config is None:
            parser.error("approved config required")
        rent(args.config, args.state)
    elif args.mode == "verify":
        config = json.loads((args.state / "config.json").read_text())
        session = json.loads((args.state / "session.json").read_text())
        verify_deletion(config, args.state, session["pod_id"],
                        session["started_epoch"], time.monotonic() + 120)
    else:
        stop_request(args.state)


if __name__ == "__main__":
    main()
