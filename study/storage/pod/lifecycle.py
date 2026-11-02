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
from pathlib import Path
from typing import assert_never


VERSION = "0.17.0"


def save(path: Path, value: object) -> None:
    temporary = path.with_suffix(path.suffix + ".tmp")
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


def execution_pins(value: Mapping, now: float) -> Mapping | Invalid:
    required = ("cli_context", "offer_id", "cloud_id", "image", "disk_gb",
                "quoted_total_hourly", "quote_checked_epoch",
                "harness_commit", "quote_reference", "gpu_label", "provider",
                "vcpus", "memory_gb", "max_seconds", "cutoff_seconds",
                "budget", "hourly_ceiling", "contingency_seconds",
                "setup_seconds", "minimum_free_bytes", "minimum_memory_bytes",
                "payload_sha256", "session_sha256", "environment_sha256")
    if any(key not in value or value[key] in (None, "") for key in required):
        return Invalid("every execution pin is mandatory; no defaults")
    texts = ("cli_context", "offer_id", "cloud_id", "image",
             "harness_commit", "quote_reference", "gpu_label", "provider")
    if any(not isinstance(value[key], str) or not value[key].strip()
           for key in texts):
        return Invalid("text execution pins must be nonempty strings")
    try:
        price = Decimal(str(value["quoted_total_hourly"]))
        ceiling = Decimal(str(value["hourly_ceiling"]))
        budget = Decimal(str(value["budget"]))
    except InvalidOperation:
        return Invalid("quoted price must be a finite decimal")
    checked = value["quote_checked_epoch"]
    if (not all(n.is_finite() for n in (price, ceiling, budget))
            or type(checked) not in (int, float)
            or not math.isfinite(checked)):
        return Invalid("price and quote timestamp must be finite")
    integers = ("disk_gb", "vcpus", "memory_gb", "max_seconds",
                "cutoff_seconds", "contingency_seconds", "setup_seconds",
                "minimum_free_bytes", "minimum_memory_bytes")
    if any(type(value[k]) is not int or value[k] < 1 for k in integers):
        return Invalid("resource and time pins must be positive integers")
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


def within_budget(elapsed: float, billed: Decimal, config: Mapping) -> bool:
    return (0 <= elapsed <= config["max_seconds"]
            and 0 <= billed <= Decimal(str(config["budget"])))


def command(config: Mapping, *arguments: str) -> list[str]:
    return [config["cli_executable"], "--context", config["cli_context"],
            "--plain", "pods",
            *arguments]


def call(config: Mapping, state: Path, label: str, arguments: list[str],
         timeout: float = 30, *, not_after: float | None = None,
         cancel_descriptor: int | None = None) -> subprocess.CompletedProcess:
    if label == "terminate":
        # Issue deletion before journal writes, even if logging fails.
        result = subprocess.run(command(config, *arguments),
                                stdin=subprocess.DEVNULL, capture_output=True,
                                text=True, timeout=timeout)
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
        return subprocess.run(command(config, *arguments),
                              stdin=subprocess.DEVNULL,
                              stdout=stdout, stderr=stderr, timeout=timeout)


def pages(config: Mapping, state: Path, mode: str, label: str) -> list[dict]:
    rows, offset = [], 0
    total = None
    seen = set()
    read_label = label + "-" + uuid.uuid4().hex
    deadline = time.monotonic() + 30
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


def mismatches(status: Mapping, config: Mapping) -> list[str]:
    expected = {"provider": config["provider"], "image": config["image"],
                "offer_id": config["offer_id"], "is_spot": False,
                "gpu": config["gpu_label"], "cloud_id": config["cloud_id"],
                "vcpus": config["vcpus"], "memory_gb": config["memory_gb"],
                "disk_gb": config["disk_gb"]}
    wrong = [key for key, value in expected.items()
             if key in status and status[key] is not None
             and status[key] != value]
    if "cost_per_hour" in status and (
            Decimal(str(status["cost_per_hour"]))
            != Decimal(str(config["quoted_total_hourly"]))):
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
        return
    descriptor = os.open(state / "stop.fifo", os.O_WRONLY | os.O_NONBLOCK)
    try:
        os.write(descriptor, b"stop\n")
    finally:
        os.close(descriptor)


def connect(state: Path) -> None:
    config = json.loads((state / "config.json").read_text())
    session = json.loads((state / "session.json").read_text())
    try:
        refused = (state / "rental-error.json").exists()
        if refused or (state / "done.json").exists():
            raise ValueError("rental refused or already deleted")
        if time.time() - session["started_epoch"] >= config["setup_seconds"]:
            raise ValueError("paid setup allowance has expired")
        ready_deadline = session["started_epoch"] + 180
        attempt = 0
        while True:
            status = status_read(config, state, session["pod_id"],
                                 "connection-" + str(attempt))
            if mismatches(status, config):
                raise ValueError("reported rental pin mismatch")
            if status["status"] == "ACTIVE":
                break
            remaining = ready_deadline - time.time()
            if remaining <= 0:
                raise ValueError("pod not ACTIVE within setup allowance")
            threading.Event().wait(min(15, remaining))
            attempt += 1
        if any(status.get(key) is None for key in
               ("provider", "image", "cost_per_hour")):
            raise ValueError("missing mandatory active-pod fact")
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
    except BaseException:
        stop_request(state)
        raise


def rent(config_path: Path, state: Path) -> None:
    approved = configuration(config_path)
    executable = shutil.which("prime")
    if executable is None:
        raise FileNotFoundError("pinned Prime CLI is not installed")
    config = {**approved, "cli_executable": str(Path(executable).resolve())}
    version = subprocess.check_output(
        [config["cli_executable"], "--version"], text=True)
    if version.strip() != "Prime CLI version: " + VERSION:
        raise ValueError("Prime CLI must be exactly " + VERSION)
    state.mkdir(mode=0o700)
    save(state / "config.json", config)
    name = "storage-" + uuid.uuid4().hex
    os.mkfifo(state / "stop.fifo", 0o600)
    descriptor = os.open(state / "stop.fifo", os.O_RDWR | os.O_NONBLOCK)
    started = time.time()
    started_monotonic = time.monotonic()
    save(state / "owner.json", {"pid": os.getpid(), "name": name,
                               "started_epoch": started,
                               "cutoff_epoch": started
                               + config["cutoff_seconds"]})
    lease: Lease = Unidentified()
    lock = threading.Lock()
    cutoff_errors: list[Exception] = []

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
                        raise ValueError("rental identity unproved; escalate")
                    pod_id = matches[0]["id"]
                case Identified(pod_id=pod_id):
                    pass
                case _:
                    assert_never(lease)
            lease = Attempted(pod_id)
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
            # A lost response does not authorize another mutation.
            active = pages(config, state, "list", "post-terminate")
            history = pages(config, state, "history", "history")
            entry = [row for row in history if row["id"] == pod_id]
            if (any(row["id"] == pod_id for row in active) or len(entry) != 1
                    or not entry[0].get("terminated_at")):
                raise ValueError("deletion not verified; escalate immediately")
            elapsed = time.monotonic() - started_monotonic
            cost = Decimal(str(entry[0]["total_cost"]))
            budget_ok = within_budget(elapsed, cost, config)
            save(state / "done.json", {"state": "deleted", "pod": entry[0],
                                       "elapsed_seconds": elapsed,
                                       "within_budget": budget_ok})
            if not budget_ok:
                raise ValueError("deleted but time/cost ceiling breached")

    def cutoff() -> None:
        try:
            shutdown("hard-cutoff")
        except Exception as error:
            cutoff_errors.append(error)
            print("CUTOFF FAILED: " + repr(error), file=sys.stderr, flush=True)
        finally:
            os.write(descriptor, b"cutoff\n")

    timer = threading.Timer(config["cutoff_seconds"], cutoff)
    timer.daemon = False
    handlers = {number: signal.signal(
        number, lambda *_: os.write(descriptor, b"host-signal\n"))
                for number in (signal.SIGINT, signal.SIGTERM)}
    timer.start()  # Before create: includes provisioning and setup time.
    rental_failure: Exception | None = None
    try:
        create = ["create", "--id", config["offer_id"], "--cloud-id",
                  config["cloud_id"], "--name", name,
                  "--vcpus", str(config["vcpus"]),
                  "--memory", str(config["memory_gb"]),
                  "--disk-size", str(config["disk_gb"]),
                  "--image", config["image"], "--yes", "--plain"]
        save(state / "create-command.json", command(config, *create))
        try:
            result = call(
                config, state, "create", create, 90,
                not_after=(started_monotonic
                           + min(180, config["setup_seconds"])),
                cancel_descriptor=descriptor)
            result.check_returncode()
            pods = pages(config, state, "list", "identify")
            matches = [pod for pod in pods if pod["name"] == name]
            if len(matches) != 1:
                raise ValueError("ambiguous pod identity; never rent again")
            pod_id = matches[0]["id"]
            with lock:
                match lease:
                    case Unidentified():
                        lease = Identified(pod_id)
                    case NotCreated() | Identified() | Attempted():
                        raise RuntimeError("cutoff already owns this rental")
                    case _:
                        assert_never(lease)
            save(state / "session.json", {"pod_id": pod_id, "name": name,
                                         "started_epoch": started})
            rendered = (state / "create.stdout").read_text().splitlines()
            request = {"cloudId": config["cloud_id"],
                       "vcpus": str(config["vcpus"]),
                       "memory": str(config["memory_gb"]),
                       "diskSize": str(config["disk_gb"]),
                       "image": config["image"],
                       "provider": config["provider"],
                       "gpuCount": "0", "team": "None"}
            summary = [line.strip() for line in rendered]
            if any(key + ": " + value not in summary
                   for key, value in request.items()):
                raise ValueError("CLI request differs from pinned offer")
            status = status_read(config, state, pod_id, "initial-status")
            save(state / "unavailable-initial-fields.json", [
                key for key in ("offer_id", "cloud_id", "is_spot", "vcpus",
                            "memory_gb", "disk_gb")
                if key not in status])
            if mismatches(status, config):
                raise ValueError("reported rental differs from approved pins")
            print("RENTED; state=" + str(state), flush=True)
        except CreateRefused as error:
            with lock:
                lease = NotCreated()
            save(state / "done.json", {"state": "not-created",
                                       "reason": str(error)})
            raise
        except Exception as error:
            rental_failure = error
            save(state / "rental-error.json", {"error": repr(error)})
            match lease:
                case NotCreated():
                    pass
                case Identified() | Attempted():
                    os.write(descriptor, b"rental-refusal\n")
                case Unidentified():
                    os.write(descriptor, b"rental-uncertain\n")
                case _:
                    assert_never(lease)
            print("RENTAL REFUSED/UNCERTAIN; do not create again", flush=True)
        # One request or the one-shot cutoff; no polling.
        select.select([descriptor], [], [])
        reason = os.read(descriptor, 1024).decode()
        # Early reconciliation failure cannot suppress proven deletion.
        shutdown(reason)
        timer.cancel()
        timer.join()
        if cutoff_errors:
            raise cutoff_errors[0]
        if rental_failure is not None:
            raise RuntimeError("rental refused; deletion verified") \
                from rental_failure
    finally:
        match lease:
            case NotCreated() | Attempted():
                timer.cancel()
                timer.join()
                os.close(descriptor)
            case Unidentified() | Identified():
                pass  # Retain the non-daemon cutoff until an attempt is made.
            case _:
                assert_never(lease)
        for number, handler in handlers.items():
            signal.signal(number, handler)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("rent", "stop"))
    parser.add_argument("--state", type=Path, required=True)
    parser.add_argument("--config", type=Path)
    args = parser.parse_args()
    if not args.state.is_absolute():
        parser.error("state must be an absolute private host directory")
    if args.mode == "rent":
        if args.config is None:
            parser.error("approved config required")
        rent(args.config, args.state)
    elif not (args.state / "done.json").exists():
        stop_request(args.state)


if __name__ == "__main__":
    main()
