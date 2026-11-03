"""Cloud-free lifecycle, shape, archive and language identity fixtures."""
from copy import deepcopy
from datetime import datetime, timezone
from decimal import Decimal
from hashlib import sha256
from io import BytesIO
import errno
import json
import signal
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
from types import SimpleNamespace
import unittest
from pathlib import Path
from unittest.mock import patch

import numpy as np

import admission
import artifacts
import accuracy
import groups
import host_run
import lifecycle
import preflight


def pins() -> dict:
    return {
        "cli_context": "approved", "provider": "nebius", "offer_id": "840aa0",
        "cloud_id": "cpu-d3_8vcpu-32gb", "image": "ubuntu_22_cuda_12",
        "gpu_label": "CPU_NODE x1", "location": "US", "gpu_count": 1,
        "disk_gb": 100, "vcpus": 8, "memory_gb": 32,
        "quoted_total_hourly": "0.274", "quote_checked_epoch": time.time(),
        "disk_hourly_per_gb": "0.0001",
        "disk_price_reference": "synthetic USD per GB-hour unit assumption",
        "harness_commit": "a" * 40, "quote_reference": "public offer receipt",
        "max_seconds": 10800, "cutoff_seconds": 10680, "setup_seconds": 1800,
        "ready_seconds": 900, "analysis_seconds": 9600,
        "copy_seconds": 420, "bootstrap_seconds": 300,
        "minimum_upload_mbps": 100,
        "minimum_download_mbps": 100,
        "download_reference": "synthetic 100 Mbps download measurement",
        "expected_evidence_bytes": 768 * 1024 ** 2,
        "evidence_size_reference": "synthetic normal-output envelope",
        "cgroup_root": {"mode": "first-in-pod-check",
                        "approval_reference": "synthetic root-probe approval",
                        "probe_seconds": 300},
        "upload_reference": "reviewed 100 Mbps upload measurement",
        "approval_reference": "reviewed D14 execution approval",
        "token_json_max_bytes": 256 * 1024 ** 2,
        "token_size_reference": "CI token preparation byte-size receipt",
        "budget": "2", "hourly_ceiling": "0.50", "contingency_seconds": 3600,
        "minimum_free_bytes": 32 * 1024 ** 3,
        "minimum_memory_bytes": 28 * 1024 ** 3,
        "payload_sha256": "b" * 64, "session_sha256": "c" * 64,
        "environment_sha256": "d" * 64,
        "payload_path": "/approved/inputs.tar",
        "session_path": "/approved/session.sh",
        "environment_path": "/approved/env.sh",
        "ssh_key_path": "/approved/key", "destination": "/approved/copy",
    }


FIXTURE_TOOLS = {
    "go.tar.gz": b"synthetic pinned Go archive; never extracted",
    "uv.tar.gz": b"synthetic pinned uv archive; never extracted",
    "python.tar.gz": b"synthetic pinned Python archive; never executed",
}
FIXTURE_SOURCES = {
    "study/storage/pod/lifecycle.py": b"reviewed lifecycle fixture source",
    "study/storage/pod/preflight.py": b"reviewed preflight fixture source",
}


def fixture_tool_pins():
    return {name: sha256(raw).hexdigest()
            for name, raw in FIXTURE_TOOLS.items()}


def fixture_source_pins():
    return {name: sha256(raw).hexdigest()
            for name, raw in FIXTURE_SOURCES.items()}


def fixture_source_digest(path):
    expected = {admission.HERE.parents[2] / name: pin
                for name, pin in fixture_source_pins().items()}
    if path in expected:
        return expected[path]
    return artifacts.digest(path)


def fixture_git_output(arguments, **kwargs):
    if arguments == ["git", "rev-parse", "HEAD"]:
        return "a" * 40
    if arguments[:2] == ["git", "status"]:
        return ""
    expected = ["git", "ls-files", "--", "study/storage/harness",
                "study/storage/onnx", "study/storage/neon-eval",
                "study/storage/pod-cost", "study/storage/pod"]
    if (arguments != expected or
            kwargs.get("cwd") != admission.HERE.parents[2]):
        raise AssertionError("unexpected reviewed-source enumeration")
    return "\n".join(FIXTURE_SOURCES) + "\n"


def invalid_pin_values(value):
    return (
        ("quoted_total_hourly", "0.51"), ("quoted_total_hourly", "NaN"),
        ("quoted_total_hourly", "0"), ("hourly_ceiling", "0.51"),
        ("budget", "2.01"), ("budget", "0"),
        ("quote_checked_epoch", 0), ("quote_checked_epoch", float("inf")),
        ("quote_checked_epoch", value["quote_checked_epoch"] + 901),
        ("provider", "other"), ("vcpus", True), ("vcpus", 7),
        ("memory_gb", 31), ("gpu_count", 0), ("gpu_count", 2),
        ("disk_gb", 0), ("disk_gb", 101), ("image", "other"),
        ("disk_hourly_per_gb", None), ("disk_hourly_per_gb", True),
        ("disk_hourly_per_gb", 0.0001), ("disk_hourly_per_gb", "-0.0001"),
        ("disk_hourly_per_gb", "NaN"), ("disk_hourly_per_gb", "Infinity"),
        ("disk_hourly_per_gb", "-Infinity"), ("disk_hourly_per_gb", "bad"),
        ("disk_price_reference", " "), ("disk_price_reference", False),
        ("max_seconds", 10801),
        ("cutoff_seconds", 10681), ("cutoff_seconds", 10700),
        ("setup_seconds", 1801), ("ready_seconds", 1800),
        ("analysis_seconds", 9601), ("analysis_seconds", 1800),
        ("copy_seconds", 421), ("bootstrap_seconds", 0),
        ("contingency_seconds", 3601), ("minimum_upload_mbps", 0),
        ("minimum_upload_mbps", float("nan")),
        ("minimum_memory_bytes", -1),
        ("minimum_memory_bytes", 28 * 1024 ** 3 - 1),
        ("minimum_free_bytes", 32 * 1024 ** 3 - 1),
        ("payload_sha256", "missing"), ("session_sha256", "C" * 64),
        ("environment_sha256", "d" * 63), ("harness_commit", "z" * 40),
        ("upload_reference", " "), ("approval_reference", ""),
        ("token_size_reference", None),
        ("token_json_max_bytes", 256 * 1024 ** 2 + 1),
        ("payload_path", ""), ("session_path", None),
        ("environment_path", " "), ("ssh_key_path", ""),
        ("destination", ""),
    )


def offer_document(config, **changes):
    # Sanitized public CPU row: display price is rounded; price_value is not.
    row = {
        "id": config["offer_id"], "cloud_id": config["cloud_id"],
        "provider": "nebius", "location": config["location"],
        "gpu_type": "CPU NODE",
        "gpu_count": 1, "vcpus": "8", "memory_gb": "32", "disk_gb": "100+",
        "stock_status": "Available", "is_spot": None,
        "price_per_hour": "$0.26", "price_value": 0.264,
        **changes,
    }
    return {"gpu_resources": [row], "total_count": 1,
            "filters": {"group_similar": False}}


def active_status(**changes):
    return {"id": "pod", "provider": "nebius",
            "image": "ubuntu_22_cuda_12", "cost_per_hour": "0.274",
            "gpu": "CPU_NODE x1",
            "status": "ACTIVE", "ssh": "root@127.0.0.1 -p 22", **changes}


def root_facts(**changes):
    return {
        "root": "/sys/fs/cgroup", "writable": True,
        "memory_controller": True, "euid": 0, "machine": "x86_64",
        "os_id": "ubuntu", "os_version": "22.04",
        "python_implementation": "CPython", "python_version": "3.12.16",
        "glibc_version": "2.35", **changes,
    }


class Clock:
    """Advance waits without sleeping or changing the process clock."""

    def __init__(self):
        self.epoch = 1_793_643_060.0
        self.elapsed = 0.0
        self.waits = []

    def time(self):
        return self.epoch + self.elapsed

    def monotonic(self):
        return 1000.0 + self.elapsed

    def wait(self, seconds):
        self.waits.append(seconds)
        self.elapsed += seconds

    def event(self):
        return SimpleNamespace(wait=self.wait)


def write_tar(bundle, members):
    with tarfile.open(bundle, "w") as archive:
        for name, raw in members.items():
            member = tarfile.TarInfo("evidence/" + name)
            member.size = len(raw)
            archive.addfile(member, BytesIO(raw))


def json_bytes(value):
    return (json.dumps(value, sort_keys=True) + "\n").encode()


def input_fixture(root):
    """Tiny byte-bound proof fixture; never a model or an inference run."""
    members = {
        **FIXTURE_TOOLS,
        **{"harness/" + name: raw for name, raw in FIXTURE_SOURCES.items()},
        "corpus/document.jsonl": b'{"id":"fixture"}\n',
        "tokenizer/config.json": b'{}\n',
        "tokenizer/tokenizer_config.json": b'{}\n',
        "model/fixture.onnx": b"not an executable model",
        "runtime/libonnxruntime.so.1.31.0": b"not an executable library",
    }
    canonical = {"files": {"document.jsonl": sha256(
        members["corpus/document.jsonl"]).hexdigest()}}
    members["corpus/freeze-canonical.json"] = json_bytes(canonical)
    freeze = sha256(members["corpus/freeze-canonical.json"]).hexdigest()
    members["corpus/freeze.json"] = json_bytes(
        {**canonical, "freeze_id": freeze})
    inputs = {"data/cpu-cost/" + name: sha256(raw).hexdigest()
              for name, raw in members.items()
              if name.startswith(("tokenizer/", "model/"))}
    members["harness/study/storage/pod-cost/cpu-cost-v1.json"] = json_bytes({
        "model_revision": accuracy.REVISION, "inputs": inputs})
    members["model-limit.json"] = json_bytes({
        "schema": "model-limit/v1", "model_revision": accuracy.REVISION,
        "config_sha256": inputs["data/cpu-cost/tokenizer/config.json"],
        "tokenizer_config_sha256": inputs[
            "data/cpu-cost/tokenizer/tokenizer_config.json"],
        "maximum_tokens": 8192, "includes_special_tokens": True,
        "source_reference": "reviewed fixture model limit"})
    members["runtime/receipt.json"] = json_bytes({
        "schema": "linux-ort/v1", "version": "1.31.0",
        "platform": "linux-x86_64",
        "member_sha256": sha256(
            members["runtime/libonnxruntime.so.1.31.0"]).hexdigest(),
        "member_path": "onnxruntime/capi/libonnxruntime.so.1.31.0",
        "download_sha256": "d" * 64,
        "download_url": "https://example.invalid/trusted-wheel",
        "verification": "reviewed fixture release pin and wheel RECORD"})
    members["file-manifest.json"] = json_bytes({
        "files": {name: sha256(raw).hexdigest()
                  for name, raw in members.items()}})
    payload = root / "inputs.tar"
    write_tar(payload, members)
    session = root / "session.sh"
    environment, key = root / "env.sh", root / "key"
    for path in (session, environment, key):
        path.write_text("fixture; never executed\n")
    key.chmod(0o600)
    config = {**pins(), "payload_path": str(payload),
              "session_path": str(session),
              "environment_path": str(environment),
              "ssh_key_path": str(key), "destination": str(root / "copy")}
    for name, pin in admission.PATHS.items():
        config[pin] = artifacts.digest(Path(config[name]))
    payload.with_suffix(".tar.sha256").write_text(
        config["payload_sha256"] + "  inputs.tar\n")
    return config, freeze, members


class SupportTests(unittest.TestCase):
    def test_pins_and_rejections_do_not_mutate_input(self):
        # Given approved pins, return a copy and refuse bad D14 limits.
        value = pins()
        snapshot = deepcopy(value)
        parsed = lifecycle.execution_pins(value, value["quote_checked_epoch"])
        self.assertNotIsInstance(parsed, lifecycle.Invalid)
        self.assertEqual(parsed, value)
        self.assertIsNot(parsed, value)
        self.assertEqual(value, snapshot)
        for key, wrong in invalid_pin_values(value):
            with self.subTest(key=key, wrong=wrong):
                candidate = {**value, key: wrong}
                before = deepcopy(candidate)
                self.assertIsInstance(lifecycle.execution_pins(
                    candidate, value["quote_checked_epoch"]),
                    lifecycle.Invalid)
                self.assertEqual(candidate, before)
        for key in value:
            with self.subTest(missing=key):
                candidate = {k: v for k, v in value.items() if k != key}
                self.assertIsInstance(lifecycle.execution_pins(
                    candidate, value["quote_checked_epoch"]),
                    lifecycle.Invalid)
        self.assertIsInstance(lifecycle.execution_pins({}, time.time()),
                              lifecycle.Invalid)

    def test_bad_configuration_refuses_before_create(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            path = root / "execution.json"
            value = pins()
            cases = [{**value, key: wrong}
                     for key, wrong in invalid_pin_values(value)]
            cases += [{k: v for k, v in value.items() if k != missing}
                      for missing in value]
            for index, candidate in enumerate(cases):
                with self.subTest(case=index):
                    path.write_text(json.dumps(candidate))
                    with patch.object(lifecycle, "call") as call, \
                            patch.object(admission, "validate") as validate, \
                            patch.object(lifecycle.shutil, "which",
                                         side_effect=AssertionError(
                                             "bad pins reached CLI lookup")):
                        with self.assertRaises(ValueError):
                            lifecycle.rent(path, root / "state")
                    validate.assert_not_called()
                    call.assert_not_called()

    def test_shape_refuses_each_insufficient_resource(self):
        config = pins()
        facts = {"cpus": 16, "effective_cpus": 8,
                 "available_memory_bytes": 32 * 1024 ** 3,
                 "free_disk_bytes": 40 * 1024 ** 3,
                 "cgroup_controllers": "cpu memory"}
        self.assertFalse(preflight.refused(facts, config))
        for key, wrong in (("cpus", 7), ("effective_cpus", 4),
                           ("available_memory_bytes", 1),
                           ("free_disk_bytes", 1),
                           ("cgroup_controllers", "cpu")):
            with self.subTest(key=key):
                self.assertTrue(preflight.refused(
                    {**facts, key: wrong}, config))
        self.assertIn("vcpus", lifecycle.mismatches({"vcpus": 16}, config))
        self.assertEqual(lifecycle.mismatches({"vcpus": 8}, config), [])

    def test_create_refuses_before_spawn_after_cancel_or_expiry(self):
        for expired in (True, False):
            with self.subTest(expired=expired), \
                    tempfile.TemporaryDirectory() as temporary:
                state = Path(temporary).resolve()
                ready = ([], [], []) if expired else ([123], [], [])
                with patch.object(lifecycle.select, "select",
                                  return_value=ready), \
                        patch.object(lifecycle.subprocess, "run") as run:
                    with self.assertRaises(lifecycle.CreateRefused):
                        lifecycle.call(
                            {}, state, "create", ["create"], 90,
                            not_after=(time.monotonic()
                                       + (-1 if expired else 60)),
                            cancel_descriptor=123)
                    run.assert_not_called()

    def test_complete_pages_reject_wrong_counts_and_duplicates(self):
        good = [{"offset": 0, "total_count": 2, "pods": [{"id": "one"}]},
                {"offset": 1, "total_count": 2, "pods": [{"id": "two"}]}]
        cases = [good,
                 [{"offset": 0, "total_count": 1, "pods": []}],
                 [{"offset": 0, "total_count": 1,
                   "pods": [{"id": "one"}, {"id": "two"}]}],
                 [{"offset": 0, "total_count": 2,
                   "pods": [{"id": "one"}, {"id": "one"}]}],
                 [good[0], {"offset": 1, "total_count": 3,
                            "pods": [{"id": "two"}]}]]
        for index, documents in enumerate(cases):
            with self.subTest(case=index), \
                    tempfile.TemporaryDirectory() as tmp:
                state = Path(tmp).resolve()
                iterator = iter(documents)

                def call(config, state, label, arguments, timeout):
                    (state / (label + ".stdout")).write_text(
                        json.dumps(next(iterator)))
                    return subprocess.CompletedProcess(arguments, 0)

                with patch.object(lifecycle, "call", side_effect=call):
                    if index == 0:
                        self.assertEqual(len(lifecycle.pages(
                            {}, state, "list", "fixture")), 2)
                    else:
                        with self.assertRaises(ValueError):
                            lifecycle.pages({}, state, "list", "fixture")

    def test_limit_and_linux_runtime_proofs_are_bound(self):
        value = {"inputs": {
            "data/cpu-cost/tokenizer/config.json": "a" * 64,
            "data/cpu-cost/tokenizer/tokenizer_config.json": "b" * 64}}
        limit = {"schema": "model-limit/v1",
                 "model_revision": accuracy.REVISION,
                 "config_sha256": "a" * 64,
                 "tokenizer_config_sha256": "b" * 64,
                 "maximum_tokens": 8192, "includes_special_tokens": True,
                 "source_reference": "reviewed public model configuration"}
        runtime = {"schema": "linux-ort/v1", "version": "1.31.0",
                   "platform": "linux-x86_64", "member_sha256": "c" * 64,
                   "member_path": "onnxruntime/capi/libonnxruntime.so.1.31.0",
                   "download_sha256": "d" * 64,
                   "download_url": "https://example.invalid/trusted-wheel",
                   "verification": "trusted release pin and wheel RECORD"}
        accuracy.verify_proofs(value, limit, runtime, "c" * 64)
        for key, wrong in (("maximum_tokens", 2048),
                           ("includes_special_tokens", False),
                           ("config_sha256", "wrong"),
                           ("source_reference", "")):
            with self.assertRaises(ValueError):
                accuracy.verify_proofs(value, {**limit, key: wrong},
                                       runtime, "c" * 64)
        for key, wrong in (("version", "1.30.0"),
                           ("platform", "macos-arm64"),
                           ("member_sha256", "wrong"),
                           ("download_sha256", "untrusted")):
            with self.assertRaises(ValueError):
                accuracy.verify_proofs(value, limit, {**runtime, key: wrong},
                                       "c" * 64)

    def test_capacity_anchor_keeps_real_inherited_limits(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            cgroup = root / "cgroup"
            phase = cgroup / "owned/bootstrap"
            phase.mkdir(parents=True)
            meminfo = root / "meminfo"
            membership = root / "membership"
            meminfo.write_text("MemTotal: 33554432 kB\n")
            membership.write_text("0::/owned/bootstrap\n")
            (cgroup / "cgroup.controllers").write_text("cpu memory")
            (phase / "cpu.max").write_text("800000 100000")
            for path, limit in ((cgroup, "max"),
                                (cgroup / "owned", str(26 * 1024 ** 3)),
                                (phase, str(4 * 1024 ** 3))):
                (path / "memory.max").write_text(limit)
            mapping = {"/sys/fs/cgroup": cgroup, "/proc/meminfo": meminfo,
                       "/proc/self/cgroup": membership}

            def mapped(value):
                return mapping.get(value, Path(value))

            with patch.object(preflight, "Path", side_effect=mapped), \
                    patch.object(preflight.os, "sched_getaffinity",
                                 return_value=set(range(16)), create=True), \
                    patch.object(preflight.shutil, "disk_usage",
                                 return_value=SimpleNamespace(
                                     free=40 * 1024 ** 3)):
                current = preflight.observe(pins(), root)
                self.assertTrue(preflight.refused(current, pins()))
                capacity = preflight.observe(pins(), root, cgroup)
                self.assertEqual(capacity["cpus"], 16)
                self.assertEqual(capacity["effective_cpus"], 8)
                self.assertFalse(preflight.refused(capacity, pins()))
                (phase / "cpu.max").write_text("400000 100000")
                limited = preflight.observe(pins(), root, cgroup)
                self.assertEqual(limited["effective_cpus"], 4)
                self.assertTrue(preflight.refused(limited, pins()))
                (phase / "cpu.max").write_text("800000 100000")
                (cgroup / "memory.max").write_text(str(20 * 1024 ** 3))
                self.assertTrue(preflight.refused(
                    preflight.observe(pins(), root, cgroup), pins()))
                with self.assertRaises(ValueError):
                    preflight.observe(pins(), root, cgroup / "unrelated")
                with self.assertRaises(ValueError):
                    preflight.observe(pins(), root, cgroup / "owned/..")

    def test_timer_manual_and_uncertain_effects_never_replay(self):
        for create_timeout, delete_timeout, early_error in (
                (False, False, False), (True, False, False),
                (False, True, False), (False, False, True),
                (True, False, True)):
            with self.subTest(create=create_timeout, delete=delete_timeout,
                              early_error=early_error):
                self.lifecycle_case(create_timeout, delete_timeout,
                                    early_error)

    def lifecycle_case(self, create_timeout, delete_timeout, early_error,
                       refusal=None):
        # Given an uncertain create, retain FIFO/signals until one deletion.
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary).resolve()
            state = directory / "state"
            config_path = directory / "execution.json"
            clock = Clock()
            config = {**pins(), "quote_checked_epoch": clock.time(),
                      "destination": str(directory / "destination")}
            config_path.write_text(json.dumps(config))
            counts = {"create": 0, "terminate": 0}
            timers, restored, reconciliations = [], [], []
            handlers = {}
            deleted = False
            create_error = subprocess.TimeoutExpired(["create"], 90)

            class Timer:
                def __init__(self, seconds, callback):
                    self.callback = callback
                    self.started = False
                    self.cancelled = False
                    self.worker = None
                    timers.append(self)

                def start(self):
                    self.started = True

                def cancel(self):
                    self.cancelled = True

                def join(self):
                    if self.worker is not None:
                        self.worker.join()

            def signal_handler(number, handler):
                if callable(handler):
                    handlers[number] = handler
                    return "old-" + str(number)
                self.assertEqual(counts["terminate"], 1,
                                 "signals restored while lease still live")
                restored.append(number)

            def call(config, state, label, arguments, timeout=30, **kwargs):
                nonlocal deleted
                if label == "availability":
                    self.assertEqual(timers, [])
                    self.assertEqual(kwargs["namespace"], "availability")
                    self.assertEqual(arguments, [
                        "list", "--gpu-type", "CPU_NODE", "--no-group-similar",
                        "--output", "json"])
                    (state / "availability.stdout").write_text(
                        json.dumps(offer_document(config)))
                    return subprocess.CompletedProcess(arguments, 0)
                self.assertTrue(timers[0].started)
                self.assertEqual(set(handlers),
                                 set(lifecycle.OWNER_SIGNALS))
                counts[label] += 1
                if label == "create":
                    destination = Path(config["destination"])
                    self.assertTrue(destination.is_dir())
                    self.assertEqual(destination.stat().st_mode & 0o777, 0o700)
                    self.assertNotIn("--cloud-id", arguments)
                    self.assertNotIn("--env", arguments)
                    self.assertEqual(arguments[arguments.index("--id") + 1],
                                     config["offer_id"])
                    for option, key in (("--vcpus", "vcpus"),
                                        ("--memory", "memory_gb"),
                                        ("--disk-size", "disk_gb"),
                                        ("--image", "image")):
                        self.assertEqual(
                            arguments[arguments.index(option) + 1],
                            str(config[key]))
                    self.assertEqual(arguments[-1], "--yes")
                    if early_error and not create_timeout:
                        timers[0].callback()
                        self.assertEqual(restored, [])
                        self.assertTrue((state / "stop.fifo").exists())
                    if create_timeout:
                        raise create_error
                    summary = (
                        "cloudId: cpu-d3_8vcpu-32gb\nvcpus: 8\n"
                        "memory: 32\ndiskSize: 100\n"
                        "image: ubuntu_22_cuda_12\n"
                        "provider: nebius\ngpuCount: 1\nteam: None\n")
                    if refusal == "summary":
                        summary = summary.replace("gpuCount: 1", "gpuCount: 0")
                    (state / "create.stdout").write_text(summary)
                if label == "terminate":
                    deleted = True
                    clock.elapsed = max(clock.elapsed, 10)
                    self.assertEqual(arguments, ["terminate", "pod", "--yes"])
                    if delete_timeout:
                        raise subprocess.TimeoutExpired(arguments, timeout)
                return subprocess.CompletedProcess(arguments, 0)

            def pages(config, state, mode, label, deadline=None):
                if label == "reconcile":
                    reconciliations.append(label)
                    if early_error and len(reconciliations) == 1:
                        return []
                if mode == "history":
                    ended = datetime.fromtimestamp(
                        clock.epoch + 10, timezone.utc).strftime(
                            "%Y-%m-%d %H:%M:%S UTC")
                    return [{"id": "pod", "terminated_at": ended,
                             "total_cost": 12961.0}]
                if deleted:
                    return []
                return [{"id": "pod", "name": json.loads(
                    (state / "owner.json").read_text())["name"]}]

            def select(descriptors, *unused):
                self.assertEqual(restored, [])
                self.assertTrue((state / "stop.fifo").exists())
                if not early_error and timers[0].worker is None:
                    timers[0].worker = threading.Thread(
                        target=timers[0].callback)
                    timers[0].worker.start()
                # Exercise the installed fake signal handler and manual FIFO.
                for number in lifecycle.OWNER_SIGNALS:
                    handlers[number]()
                lifecycle.stop_request(state)
                return (descriptors, [], [])

            status = active_status()
            if refusal not in (None, "summary"):
                status[refusal] = "wrong"
                if refusal == "cost_per_hour":
                    status[refusal] = "0.50"
            with patch.object(admission, "validate", return_value=60), \
                    patch.object(lifecycle.threading, "Timer", Timer), \
                    patch.object(lifecycle.signal, "signal", signal_handler), \
                    patch.object(lifecycle.shutil, "which",
                                 return_value="/prime"), \
                    patch.object(lifecycle.subprocess, "check_output",
                                 return_value="Prime CLI version: 0.17.0\n"), \
                    patch.object(lifecycle.time, "time", clock.time), \
                    patch.object(lifecycle.time, "monotonic",
                                 clock.monotonic), \
                    patch.object(lifecycle, "call", side_effect=call), \
                    patch.object(lifecycle, "pages", side_effect=pages), \
                    patch.object(lifecycle, "status_read",
                                 return_value=status), \
                    patch.object(lifecycle.select, "select",
                                 side_effect=select):
                if create_timeout:
                    with self.assertRaises(
                            subprocess.TimeoutExpired) as raised:
                        lifecycle.rent(config_path, state)
                    self.assertIs(raised.exception, create_error)
                elif refusal:
                    with self.assertRaisesRegex(ValueError, "approved"):
                        lifecycle.rent(config_path, state)
                elif early_error:
                    with self.assertRaisesRegex(
                            ValueError, "identity unproved"):
                        lifecycle.rent(config_path, state)
                else:
                    lifecycle.rent(config_path, state)
            self.assertEqual(counts, {"create": 1, "terminate": 1})
            if refusal or create_timeout:
                self.assertFalse((state / "validated.json").exists())
            else:
                self.assertTrue((state / "validated.json").exists())
            if create_timeout and early_error:
                self.assertGreaterEqual(len(reconciliations), 2)
            self.assertCountEqual(restored, lifecycle.OWNER_SIGNALS)
            self.assertTrue(timers[0].cancelled)
            done = json.loads((state / "done.json").read_text())
            self.assertEqual(done["state"], "deleted")
            self.assertTrue(done["within_time_and_approved_rate"])
            argv = json.loads((state / "create-command.json").read_text())
            self.assertEqual(argv[:5],
                             ["/prime", "--context", "approved", "--plain",
                              "pods"])
            self.assertIn("--yes", argv)
            lifecycle.stop_request(state)  # Already-deleted stop is a no-op.

    def test_summary_and_status_mismatch_terminate_without_validation(self):
        for refusal in ("summary", "cost_per_hour", "provider", "image",
                        "gpu"):
            with self.subTest(refusal=refusal):
                self.lifecycle_case(False, False, False, refusal)

    def test_select_offer_requires_one_exact_ungrouped_on_demand_row(self):
        config = pins()
        for spot in (None, False):
            document = offer_document(config, is_spot=spot)
            before = deepcopy(document)
            selected = lifecycle.select_offer(document, config)
            self.assertEqual(selected["id"], config["offer_id"])
            self.assertEqual(document, before)
        wrong_rows = (
            {"is_spot": True}, {"is_spot": 0}, {"is_spot": "false"},
            {"id": "other"}, {"cloud_id": "other"}, {"provider": "other"},
            {"location": "FI"}, {"stock_status": "Unavailable"},
            {"gpu_type": "CPU NODE (Spot)"}, {"gpu_count": 0},
            {"vcpus": "16"}, {"vcpus": 8}, {"memory_gb": "16"},
            {"disk_gb": "64+"}, {"disk_gb": "200+"},
            {"price_value": 0.26}, {"price_value": 0.265},
        )
        documents = [offer_document(config, **wrong) for wrong in wrong_rows]
        absent_spot = offer_document(config)
        del absent_spot["gpu_resources"][0]["is_spot"]
        documents.append(absent_spot)
        empty = offer_document(config)
        empty.update(gpu_resources=[], total_count=0)
        documents.append(empty)
        duplicate = offer_document(config)
        duplicate["gpu_resources"] *= 2
        duplicate["total_count"] = 2
        documents.append(duplicate)
        multiple = offer_document(config)
        multiple["gpu_resources"].append({
            **multiple["gpu_resources"][0], "id": "other"})
        multiple["total_count"] = 2
        documents.append(multiple)
        grouped = offer_document(config)
        grouped["filters"]["group_similar"] = True
        documents.append(grouped)
        incomplete = offer_document(config)
        incomplete["total_count"] = 2
        documents.append(incomplete)
        for index, document in enumerate(documents):
            with self.subTest(case=index):
                before = deepcopy(document)
                with self.assertRaises(ValueError):
                    lifecycle.select_offer(document, config)
                self.assertEqual(document, before)

    def test_empty_availability_refuses_after_one_read_without_timer(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            config = root / "execution.json"
            config.write_text(json.dumps({
                **pins(), "destination": str(root / "destination")}))

            def call(config, state, label, arguments, timeout=30, **kwargs):
                self.assertEqual(label, "availability")
                self.assertEqual(kwargs["namespace"], "availability")
                (state / "availability.stdout").write_text(json.dumps({
                    "gpu_resources": [], "total_count": 0,
                    "filters": {"group_similar": False}}))
                return subprocess.CompletedProcess(arguments, 0)

            with patch.object(admission, "validate", return_value=60), \
                    patch.object(lifecycle.shutil, "which",
                                 return_value="/prime"), \
                    patch.object(lifecycle.subprocess, "check_output",
                                 return_value="Prime CLI version: 0.17.0\n"), \
                    patch.object(lifecycle, "call",
                                 side_effect=call) as read, \
                    patch.object(lifecycle.threading, "Timer") as timer:
                with self.assertRaisesRegex(ValueError, "no retry"):
                    lifecycle.rent(config, root / "state")
            self.assertEqual(read.call_count, 1)
            timer.assert_not_called()
            self.assertFalse((root / "destination").exists())

    def test_call_uses_context_plain_namespace_and_yes(self):
        with tempfile.TemporaryDirectory() as temporary:
            state = Path(temporary).resolve()
            config = {**pins(), "cli_executable": "/prime"}
            with patch.object(lifecycle.subprocess, "run") as run:
                run.return_value = subprocess.CompletedProcess([], 0, "", "")
                lifecycle.call(config, state, "create",
                               ["create", "--id", "cpu", "--yes"])
                self.assertEqual(run.call_args.args[0], [
                    "/prime", "--context", "approved", "--plain", "pods",
                    "create", "--id", "cpu", "--yes"])
                lifecycle.call(config, state, "availability", ["list"],
                               namespace="availability")
                self.assertEqual(run.call_args.args[0], [
                    "/prime", "--context", "approved", "--plain",
                    "availability",
                    "list"])
                lifecycle.call(config, state, "terminate",
                               ["terminate", "pod", "--yes"])
                self.assertEqual(run.call_args.args[0][-4:],
                                 ["pods", "terminate", "pod", "--yes"])
                self.assertTrue(run.call_args.kwargs["start_new_session"])

    def test_deletion_waits_for_history_without_trusting_raw_total(self):
        self.deletion_case("boundary", None)
        for cost in (None, 12961.0):
            with self.subTest(cost=cost):
                self.deletion_case("delayed", cost)

    def test_deletion_requires_absence_history_reserve_and_rate_time(self):
        for mode in ("active", "missing-history", "missing-time", "reserve",
                     "elapsed", "rate", "bad-time"):
            with self.subTest(mode=mode):
                self.deletion_case(mode, None)

    def deletion_case(self, mode, cost):
        with tempfile.TemporaryDirectory() as temporary:
            state = Path(temporary).resolve()
            clock = Clock()
            config = {**pins(), "cli_executable": "/prime"}
            if mode == "rate":
                config["quoted_total_hourly"] = "0.51"
            reads = []
            histories = 0
            elapsed = {"elapsed": 10800, "boundary": 10799}.get(mode, 10)
            started = clock.epoch - elapsed
            deadline = clock.monotonic() + 12
            ended = datetime.fromtimestamp(
                clock.epoch, timezone.utc).strftime(
                    "%Y-%m-%d %H:%M:%S UTC")
            entry = {"id": "pod", "terminated_at": ended}
            if mode == "bad-time":
                entry["terminated_at"] = "not a UTC timestamp"
            if mode == "missing-time":
                del entry["terminated_at"]
            if cost is not None:
                entry["total_cost"] = cost

            def call(config, state, label, arguments, timeout=30, **kwargs):
                nonlocal histories
                mode_arg = arguments[0]
                reads.append(arguments)
                self.assertIn(mode_arg, ("list", "history"))
                if mode_arg == "list":
                    rows = [{"id": "pod"}] if mode == "active" else []
                    key = "pods"
                else:
                    histories += 1
                    absent = (mode == "missing-history" or
                              mode == "delayed" and histories == 1)
                    rows = [] if absent else [entry]
                    key = "history"
                (state / (label + ".stdout")).write_text(json.dumps({
                    key: rows, "offset": 0, "total_count": len(rows)}))
                if mode == "reserve":
                    clock.elapsed += 20
                return subprocess.CompletedProcess(arguments, 0)

            with patch.object(lifecycle, "call", side_effect=call), \
                    patch.object(lifecycle.time, "monotonic",
                                 clock.monotonic), \
                    patch.object(lifecycle.threading, "Event", clock.event):
                if mode in ("active", "missing-history", "missing-time",
                            "reserve"):
                    with self.assertRaises(TimeoutError):
                        lifecycle.verify_deletion(config, state, "pod",
                                                  started, deadline)
                    self.assertFalse((state / "done.json").exists())
                elif mode in ("elapsed", "rate", "bad-time"):
                    with self.assertRaisesRegex(ValueError, "time/rate"):
                        lifecycle.verify_deletion(config, state, "pod",
                                                  started, deadline)
                    done = json.loads((state / "done.json").read_text())
                    self.assertEqual(done["state"], "deleted")
                    self.assertFalse(done["within_time_and_approved_rate"])
                else:
                    lifecycle.verify_deletion(config, state, "pod",
                                              started, deadline)
                    done = json.loads((state / "done.json").read_text())
                    self.assertEqual(done["state"], "deleted")
                    expected = 10800 if mode == "boundary" else 11
                    self.assertEqual(done["elapsed_seconds"], expected)
                    self.assertTrue(done["within_time_and_approved_rate"])
                    estimate = Decimal(expected) / 3600 * Decimal("0.284")
                    self.assertEqual(
                        Decimal(done["conservative_hourly_bound"]),
                        Decimal("0.284"))
                    self.assertEqual(done["quoted_expected_total_hourly"],
                                     "0.274")
                    self.assertEqual(done["disk_price_reference"],
                                     config["disk_price_reference"])
                    self.assertEqual(Decimal(done["rate_time_estimate"]),
                                     estimate)
                    self.assertIn("not-verified", done["billed_cost"])
                    self.assertEqual(histories, 1 if mode == "boundary" else 2)
                    self.assertEqual(clock.waits,
                                     [] if mode == "boundary" else [5])
            self.assertTrue(reads)
            self.assertFalse(any(argument in ("create", "terminate")
                                 for read in reads for argument in read))

    def test_verify_command_only_reads_existing_lease(self):
        with tempfile.TemporaryDirectory() as temporary:
            state = Path(temporary).resolve()
            (state / "config.json").write_text(json.dumps(pins()))
            (state / "session.json").write_text(json.dumps({
                "pod_id": "pod", "started_epoch": 1_793_643_060}))
            with patch.object(sys, "argv", [
                    "lifecycle.py", "verify", "--state", str(state)]), \
                    patch.object(lifecycle, "verify_deletion") as verify, \
                    patch.object(lifecycle, "rent") as rent, \
                    patch.object(lifecycle, "stop_request") as stop, \
                    patch.object(lifecycle, "call") as call:
                lifecycle.main()
            verify.assert_called_once()
            self.assertEqual(verify.call_args.args[2:4],
                             ("pod", 1_793_643_060))
            rent.assert_not_called()
            stop.assert_not_called()
            call.assert_not_called()

    def test_connect_waits_four_minutes_but_refuses_readiness_expiry(self):
        self.connect_case("ready")
        self.connect_case("expired")

    def test_connect_requires_validation_and_exact_root_ip_port(self):
        for mode in ("unvalidated", "refused", "deleted"):
            with self.subTest(mode=mode):
                self.connect_case(mode)
        for ssh in ("ssh root@127.0.0.1 -p 22", "user@127.0.0.1 -p 22",
                    "root@example.invalid -p 22", "root@127.0.0.1 -P 22",
                    "root@127.0.0.1 -p 0", "root@127.0.0.1 -p 65536",
                    "root@127.0.0.1 -p +22", "root@127.0.0.1 -p 22 extra"):
            with self.subTest(ssh=ssh):
                self.connect_case("bad-ssh", ssh)

    def connect_case(self, mode, ssh="root@127.0.0.1 -p 22"):
        with tempfile.TemporaryDirectory() as temporary:
            state = Path(temporary).resolve()
            clock = Clock()
            config = {**pins(), "quote_checked_epoch": clock.time()}
            (state / "config.json").write_text(json.dumps(config))
            (state / "session.json").write_text(json.dumps({
                "pod_id": "pod", "started_epoch": clock.epoch}))
            if mode != "unvalidated":
                (state / "validated.json").write_text("{}")
            if mode == "refused":
                (state / "rental-error.json").write_text("{}")
            if mode == "deleted":
                (state / "done.json").write_text('{"state":"deleted"}')
            observed = []

            def status_read(*arguments):
                observed.append(clock.elapsed)
                status = ("INSTALLING" if mode == "expired" or
                          mode == "ready" and clock.elapsed < 240
                          else "ACTIVE")
                return active_status(status=status, ssh=ssh)

            with patch.object(lifecycle.time, "time", clock.time), \
                    patch.object(lifecycle.threading, "Event", clock.event), \
                    patch.object(lifecycle, "status_read",
                                 side_effect=status_read), \
                    patch.object(lifecycle, "stop_request") as stop:
                if mode == "ready":
                    lifecycle.connect(state)
                    self.assertEqual(clock.elapsed, 240)
                    connection = json.loads(
                        (state / "connection.json").read_text())
                    self.assertEqual(connection,
                                     {"target": "root@127.0.0.1",
                                      "port": "22"})
                    self.assertEqual(json.loads(
                        (state / "session-inputs.json").read_text())[
                            "analysis_seconds"], 9600)
                    stop.assert_not_called()
                else:
                    with self.assertRaises(ValueError):
                        lifecycle.connect(state)
                    stop.assert_called_once_with(state)
                    self.assertFalse((state / "connection.json").exists())
            if mode == "expired":
                self.assertEqual(clock.elapsed, 900)
            if mode in ("unvalidated", "refused", "deleted"):
                self.assertEqual(observed, [])

    def test_cleanup_ipc_failure_keeps_primary_exception(self):
        state = Path("/approved/state")
        failure = OSError(errno.ENXIO, "no FIFO reader")
        primary = RuntimeError("original operation failed")
        with patch.object(lifecycle, "stop_request", side_effect=failure):
            lifecycle.request_cleanup(state, primary)
            self.assertEqual(str(primary), "original operation failed")
            self.assertTrue(any("ENXIO" in note or "No such device" in note
                                or "no FIFO reader" in note
                                for note in primary.__notes__))
            with self.assertRaises(OSError) as raised:
                lifecycle.request_cleanup(state)
            self.assertIs(raised.exception, failure)

    def test_archive_manifest_extra_changed_and_unsafe_members(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            source = root / "source"
            source.mkdir()
            (source / "receipt.txt").write_text("retained")
            bundle = root / "evidence.tar"
            with patch.object(sys, "argv", ["artifacts.py", "--source",
                              str(source), "--bundle", str(bundle)]):
                artifacts.main()
            destination = root / "verified"
            artifacts.extract(bundle, destination)
            manifest = destination / "evidence/file-manifest.json"
            artifacts.verify(manifest)
            extra = manifest.parent / "extra"
            extra.write_text("not manifested")
            with self.assertRaises(ValueError):
                artifacts.verify(manifest)
            extra.unlink()
            (manifest.parent / "receipt.txt").write_text("changed")
            with self.assertRaises(ValueError):
                artifacts.verify(manifest)
            bad = root / "bad.tar"
            with tarfile.open(bad, "w") as archive:
                archive.addfile(tarfile.TarInfo("evidence/../outside"))
            with self.assertRaises(ValueError):
                artifacts.extract(bad, root / "rejected")

    def test_archive_bounds_links_duplicates_and_nested_paths(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            file = root / "two-bytes"
            file.write_bytes(b"ok")
            for constant, limit in (("MAX_FILE", 1), ("MAX_PAYLOAD", 1),
                                    ("MAX_MEMBERS", 0)):
                with patch.object(artifacts, constant, limit):
                    with self.assertRaises(ValueError):
                        artifacts.validate_payload([file])
            source = root / "linked"
            source.mkdir()
            (source / "alias").symlink_to(file)
            with patch.object(sys, "argv", ["artifacts.py", "--source",
                              str(source), "--bundle",
                              str(root / "link.tar")]):
                with self.assertRaises(SystemExit):
                    artifacts.main()
            for kind in ("duplicate", "link", "absolute"):
                bundle = root / (kind + ".tar")
                with tarfile.open(bundle, "w") as archive:
                    member = tarfile.TarInfo("evidence/a")
                    if kind == "link":
                        member.type = tarfile.SYMTYPE
                        member.linkname = "outside"
                    if kind == "absolute":
                        member.name = "/evidence/a"
                    archive.addfile(member)
                    if kind == "duplicate":
                        archive.addfile(member)
                with self.assertRaises(ValueError):
                    artifacts.extract(bundle, root / kind)
            nested = root / "nested.tar"
            with tarfile.open(nested, "w") as archive:
                archive.addfile(tarfile.TarInfo("evidence/nested/receipt"))
            artifacts.extract(nested, root / "safe")
            self.assertTrue((root / "safe/evidence/nested/receipt").is_file())
            with self.assertRaises(ValueError):
                groups.owned(root, str(root / "subdirectory/../receipt"))

    def test_language_groups_keep_docs_and_interleaved_query_order(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            out = root / "cut"
            out.mkdir()
            languages = ["go", "python"] * 29 + ["go"] * 21
            lines = [json.dumps({"id": str(i), "language": language}) + "\n"
                     for i, language in enumerate(languages)]
            (root / "queries.jsonl").write_text("".join(lines))
            vectors = np.arange(81 * 768, dtype=np.float32).reshape(81, 768)
            np.save(root / "vectors.npy", vectors, allow_pickle=False)
            (root / "documents.jsonl").write_text("{}\n{}\n")
            config = {"documents": {"path": "documents.jsonl", "rows": 2},
                      "queries": {"path": "queries.jsonl"},
                      "vectors": {"path": "vectors.npy"}}
            for entry in config.values():
                entry["sha256"] = artifacts.digest(root / entry["path"])
            (out / "config.json").write_text(json.dumps(config))
            command = ["uv", "run", "--config", str(out / "config.json"),
                       "--receipt", "old", "--config-pin", "old",
                       "--input-pin", "old"]
            (out / "cut.json").write_text(json.dumps({
                "score_command": command,
                "config_pin": artifacts.digest(out / "config.json")}))
            with patch.object(sys, "argv", ["groups.py", "--root", str(root),
                              "--cut", str(out / "cut.json")]):
                groups.main()
                with self.assertRaises(FileExistsError):
                    groups.main()
            query_path = root / "queries.jsonl"
            original_queries = query_path.read_bytes()
            query_path.write_bytes(original_queries + b"\n")
            with patch.object(sys, "argv", ["groups.py", "--root", str(root),
                              "--cut", str(out / "cut.json")]):
                with self.assertRaises(ValueError):
                    groups.main()
            query_path.write_bytes(original_queries)
            for language in ("go", "python"):
                indices = [i for i, entry in enumerate(languages)
                           if entry == language]
                grouped = np.load(out / language / "vectors.npy",
                                  allow_pickle=False)
                np.testing.assert_array_equal(grouped[:2], vectors[:2])
                np.testing.assert_array_equal(grouped[2:],
                    vectors[[i + 2 for i in indices]])
                self.assertEqual(
                    (out / language / "queries.jsonl").read_text(),
                                 "".join(lines[i] for i in indices))


    def test_language_grouping_refuses_wrong_denominator(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            (root / "documents.jsonl").write_text("{}\n")
            (root / "queries.jsonl").write_text(
                json.dumps({"id": "only", "language": "go"}) + "\n")
            np.save(root / "vectors.npy", np.zeros((2, 768)),
                    allow_pickle=False)
            config = {key: {"path": name, "rows": 1,
                            "sha256": artifacts.digest(root / name)}
                      for key, name in (("documents", "documents.jsonl"),
                                        ("queries", "queries.jsonl"),
                                        ("vectors", "vectors.npy"))}
            (root / "config.json").write_text(json.dumps(config))
            (root / "cut.json").write_text(json.dumps({
                "config_pin": artifacts.digest(root / "config.json"),
                "score_command": ["uv", "--config",
                                  str(root / "config.json")]}))
            with patch.object(sys, "argv", ["groups.py", "--root", str(root),
                              "--cut", str(root / "cut.json")]):
                with self.assertRaises(ValueError):
                    groups.main()
            self.assertFalse((root / "groups.json").exists())
            self.assertFalse((root / "go").exists())


class AdmissionTests(unittest.TestCase):
    def test_valid_local_inputs_are_checked_without_mutation(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            config, freeze, _ = input_fixture(root)
            before = deepcopy(config)
            with patch.object(admission, "FREEZE", freeze), \
                    patch.object(admission, "TOOLS", fixture_tool_pins()), \
                    patch.object(admission, "digest", fixture_source_digest), \
                    patch.object(admission.subprocess, "check_output",
                                 side_effect=fixture_git_output):
                seconds = admission.validate(config)
            self.assertGreaterEqual(seconds, 30)
            self.assertLessEqual(seconds + 900 + 300 + 300, 1800)
            self.assertEqual(config, before)
            self.assertFalse(Path(config["destination"]).exists())
            destination = Path(config["destination"])
            destination.mkdir(mode=0o700)
            with patch.object(admission, "FREEZE", freeze), \
                    patch.object(admission, "TOOLS", fixture_tool_pins()), \
                    patch.object(admission, "digest", fixture_source_digest), \
                    patch.object(admission.subprocess, "check_output",
                                 side_effect=fixture_git_output):
                self.assertEqual(admission.validate(
                    config, destination_ready=True), seconds)
                (destination / "unapproved").write_text("existing")
                with self.assertRaisesRegex(ValueError, "empty"):
                    admission.validate(config, destination_ready=True)

    def test_first_root_window_is_reserved_before_create(self):
        # Given admitted inputs, only the fallback adds a root setup phase.
        for mode, root_seconds in (("first-in-pod-check", 300),
                                   ("prior-evidence", 0)):
            with self.subTest(mode=mode), \
                    tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary).resolve()
                config, freeze, _ = input_fixture(directory)
                if mode == "prior-evidence":
                    proof = directory / "root-evidence.txt"
                    proof.write_bytes(b"synthetic; not provider evidence\n")
                    config["cgroup_root"] = {
                        **{name: config[name] for name in
                           ("provider", "cloud_id", "location", "image")},
                        "mode": mode, "source_reference": "synthetic fixture",
                        "evidence_path": str(proof),
                        "evidence_sha256": artifacts.digest(proof)}
                with patch.object(admission, "FREEZE", freeze), \
                        patch.object(admission, "TOOLS",
                                     fixture_tool_pins()), \
                        patch.object(admission, "digest",
                                     fixture_source_digest), \
                        patch.object(admission.subprocess, "check_output",
                                     side_effect=fixture_git_output), \
                        patch.object(lifecycle, "call") as create:
                    seconds = admission.validate(config)
                    config["setup_seconds"] = (config["ready_seconds"]
                        + root_seconds + config["bootstrap_seconds"] + seconds)
                    self.assertEqual(admission.validate(config), seconds)
                    config["setup_seconds"] -= 1
                    path = directory / "execution.json"
                    path.write_text(json.dumps(config))
                    with self.assertRaisesRegex(ValueError, "approved setup"):
                        lifecycle.rent(path, directory / "state")
                    create.assert_not_called()
                self.assertFalse((directory / "state").exists())

    def test_transport_payload_key_destination_refuse_before_create(self):
        cases = ("payload-hash", "session-hash", "environment-hash",
                 "key-mode", "key-alias", "payload-alias", "relative-path",
                 "noncanonical-text",
                 "destination-exists", "destination-parent",
                 "destination-source",
                 "sidecar", "payload-name", "upload-feasibility", "dirty-head",
                 "wrong-head", "token-cap", "token-small")
        for mode in cases:
            with self.subTest(mode=mode), \
                    tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary).resolve()
                config, freeze, _ = input_fixture(root)
                payload = Path(config["payload_path"])
                head, dirty = "a" * 40, ""
                if mode.endswith("-hash"):
                    config[mode.replace("-hash", "_sha256")] = "0" * 64
                elif mode == "key-mode":
                    Path(config["ssh_key_path"]).chmod(0o644)
                elif mode in ("key-alias", "payload-alias"):
                    name = ("ssh_key_path" if mode == "key-alias"
                            else "payload_path")
                    alias = root / "alias"
                    alias.symlink_to(config[name])
                    config[name] = str(alias)
                elif mode == "noncanonical-text":
                    path = Path(config["environment_path"])
                    config["environment_path"] = (
                        str(path.parent) + "/./" + path.name)
                elif mode == "relative-path":
                    config["environment_path"] = "env.sh"
                elif mode == "destination-exists":
                    Path(config["destination"]).mkdir(mode=0o700)
                elif mode == "destination-parent":
                    config["destination"] = str(root / "missing/copy")
                elif mode == "destination-source":
                    config["destination"] = str(admission.HERE / "copy")
                elif mode == "sidecar":
                    payload.with_suffix(".tar.sha256").write_text("wrong\n")
                elif mode == "payload-name":
                    other = root / "other.tar"
                    payload.rename(other)
                    config["payload_path"] = str(other)
                elif mode == "upload-feasibility":
                    config["minimum_upload_mbps"] = 0.000001
                elif mode == "dirty-head":
                    dirty = " M fixture.py\n"
                elif mode == "wrong-head":
                    head = "e" * 40
                elif mode == "token-cap":
                    config["token_json_max_bytes"] += 1
                elif mode == "token-small":
                    config["token_json_max_bytes"] = (
                        admission.TOKEN_JSON_BOUND - 1)

                def git_output(arguments, **kwargs):
                    if arguments == ["git", "rev-parse", "HEAD"]:
                        return head
                    if arguments[:2] == ["git", "status"]:
                        return dirty
                    return fixture_git_output(arguments, **kwargs)

                path = root / "execution.json"
                path.write_text(json.dumps(config))
                with patch.object(admission, "FREEZE", freeze), \
                        patch.object(admission, "TOOLS",
                                     fixture_tool_pins()), \
                        patch.object(admission, "digest",
                                     fixture_source_digest), \
                        patch.object(admission.subprocess, "check_output",
                                     side_effect=git_output), \
                        patch.object(lifecycle, "call") as create:
                    with self.assertRaises(ValueError):
                        lifecycle.rent(path, root / "state")
                create.assert_not_called()
                self.assertFalse((root / "state").exists())

    def test_missing_or_changed_tools_sources_refuse_before_create(self):
        for mode in ("missing-go", "changed-go", "missing-uv", "changed-uv",
                     "missing-python", "changed-python",
                     "missing-source", "changed-source"):
            with self.subTest(mode=mode):
                self.export_case(mode)

    def test_reviewed_source_set_must_be_nonempty_and_canonical(self):
        for mode in ("empty-source-set", "aliased-source",
                     "nonregular-source"):
            with self.subTest(mode=mode):
                self.export_case(mode)

    def export_case(self, mode):
        # An approved manifest cannot replace independent tool/source pins.
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            config, freeze, members = input_fixture(root)
            if (mode.endswith(("-go", "-uv", "-python", "-source"))
                    and mode not in ("aliased-source", "nonregular-source")):
                name = {"go": "go.tar.gz", "uv": "uv.tar.gz",
                        "python": "python.tar.gz",
                        "source": "harness/study/storage/pod/preflight.py"}[
                            mode.rsplit("-", 1)[1]]
                if mode.startswith("missing-"):
                    del members[name]
                else:
                    members[name] = b"changed but self-consistently manifested"
                members["file-manifest.json"] = json_bytes({
                    "files": {name: sha256(raw).hexdigest()
                              for name, raw in members.items()
                              if name != "file-manifest.json"}})
                payload = Path(config["payload_path"])
                write_tar(payload, members)
                config["payload_sha256"] = artifacts.digest(payload)
                payload.with_suffix(".tar.sha256").write_text(
                    config["payload_sha256"] + "  inputs.tar\n")

            def git_output(arguments, **kwargs):
                if arguments[:2] == ["git", "ls-files"]:
                    if mode == "empty-source-set":
                        return ""
                    if mode == "aliased-source":
                        return "study/storage/pod/../pod/lifecycle.py\n"
                    if mode == "nonregular-source":
                        return "study/storage/pod/not-a-reviewed-file.py\n"
                return fixture_git_output(arguments, **kwargs)

            expected = "pinned Go/uv/Python archive"
            if mode in ("missing-source", "changed-source"):
                expected = "complete reviewed harness export"
            if mode == "empty-source-set":
                expected = "file set is empty"
            if mode in ("aliased-source", "nonregular-source"):
                expected = "nonregular/aliased path"
            path = root / "execution.json"
            path.write_text(json.dumps(config))
            with patch.object(admission, "FREEZE", freeze), \
                    patch.object(admission, "TOOLS", fixture_tool_pins()), \
                    patch.object(admission, "digest", fixture_source_digest), \
                    patch.object(admission.subprocess, "check_output",
                                 side_effect=git_output), \
                    patch.object(lifecycle, "call") as create:
                with self.assertRaisesRegex(ValueError, expected):
                    lifecycle.rent(path, root / "state")
            create.assert_not_called()
            self.assertFalse((root / "state").exists())

    def test_python_staging_is_private_idempotent_and_unexecuted(self):
        # Given admitted archive bytes, when staged twice, then reuse
        # only
        # the pinned bytes and keep both the interpreter and tar
        # unexecuted.
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary).resolve()
            config, _, _ = input_fixture(directory)
            state = directory / "state"
            state.mkdir(mode=0o700)
            with patch.object(admission, "TOOLS", fixture_tool_pins()), \
                    patch.object(host_run.subprocess, "Popen") as execute:
                first = host_run.stage_python(Path(config["payload_path"]),
                                              state)
                identity = first.stat().st_ino
                second = host_run.stage_python(Path(config["payload_path"]),
                                               state)
            execute.assert_not_called()
            self.assertEqual(first, second)
            self.assertEqual(second.stat().st_ino, identity)
            self.assertEqual(second.stat().st_mode & 0o777, 0o600)
            self.assertEqual(second.read_bytes(),
                             FIXTURE_TOOLS["python.tar.gz"])
            self.assertEqual(list(state.glob("*.tmp")), [])
            self.assertEqual(json.loads(
                (state / "python-bootstrap-input.json").read_text()), {
                    "member": "evidence/python.tar.gz",
                    "sha256": fixture_tool_pins()["python.tar.gz"],
                    "bytes": len(FIXTURE_TOOLS["python.tar.gz"])})

    def test_python_staging_rejects_changed_missing_or_aliased_bytes(self):
        # Given a staged path or changed input, when its identity or
        # hash
        # differs, then refuse without connecting or replacing that
        # path.
        for mode in ("changed-member", "missing-member", "changed-stage",
                     "stage-alias"):
            with self.subTest(mode=mode), \
                    tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary).resolve()
                config, _, members = input_fixture(directory)
                bundle = Path(config["payload_path"])
                state = directory / "state"
                state.mkdir(mode=0o700)
                target = state / "python.tar.gz"
                if mode == "changed-member":
                    members["python.tar.gz"] = b"changed archive bytes"
                    write_tar(bundle, members)
                elif mode == "missing-member":
                    del members["python.tar.gz"]
                    write_tar(bundle, members)
                elif mode == "changed-stage":
                    target.write_bytes(b"changed stage")
                else:
                    outside = directory / "outside"
                    outside.write_bytes(FIXTURE_TOOLS["python.tar.gz"])
                    target.symlink_to(outside)
                with patch.object(admission, "TOOLS", fixture_tool_pins()), \
                        patch.object(host_run, "connect") as connect:
                    with self.assertRaises((ValueError, KeyError)):
                        host_run.stage_python(bundle, state)
                connect.assert_not_called()
                self.assertFalse(
                    (state / "python-bootstrap-input.json").exists())
                self.assertEqual(list(state.glob("*.tmp")), [])
                if mode == "changed-stage":
                    self.assertEqual(target.read_bytes(), b"changed stage")
                elif mode == "stage-alias":
                    self.assertTrue(target.is_symlink())
                    self.assertEqual(outside.read_bytes(),
                                     FIXTURE_TOOLS["python.tar.gz"])
                else:
                    self.assertFalse(target.exists())

    def test_upload_forecast_counts_python_both_in_payload_and_preupload(self):
        # Given a pinned member inside inputs.tar, when feasibility is
        # checked, then count its extra pre-root copy as well as the
        # payload.
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary).resolve()
            config, freeze, _ = input_fixture(directory)
            sizes = []
            original = admission.upload_seconds

            def forecast(size, rate):
                sizes.append(size)
                return original(size, rate)

            with patch.object(admission, "FREEZE", freeze), \
                    patch.object(admission, "TOOLS", fixture_tool_pins()), \
                    patch.object(admission, "digest", fixture_source_digest), \
                    patch.object(admission.subprocess, "check_output",
                                 side_effect=fixture_git_output), \
                    patch.object(admission, "upload_seconds",
                                 side_effect=forecast):
                admission.validate(config)
            expected = sum(Path(config[name]).stat().st_size
                           for name in admission.PATHS)
            support = ("artifacts.py", "preflight.py", "cgroup_setup.py")
            expected += sum((admission.HERE / name).stat().st_size
                            for name in support)
            expected += len(json.dumps(config).encode("utf-8")) + 4096
            expected += len(FIXTURE_TOOLS["python.tar.gz"])
            self.assertEqual(sizes[-1], expected)

    def test_token_json_bound_accepts_exact_source_supported_cap(self):
        self.assertEqual(admission.TOKEN_JSON_BOUND, 193_052_672)
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            config, freeze, _ = input_fixture(root)
            config["token_json_max_bytes"] = 193_052_672
            with patch.object(admission, "FREEZE", freeze), \
                    patch.object(admission, "TOOLS", fixture_tool_pins()), \
                    patch.object(admission, "digest", fixture_source_digest), \
                    patch.object(admission.subprocess, "check_output",
                                 side_effect=fixture_git_output):
                self.assertGreaterEqual(admission.validate(config), 30)

    def test_upload_seconds_includes_margin_and_rounds_at_byte_boundary(self):
        self.assertEqual(admission.upload_seconds(0, 100), 30)
        self.assertEqual(admission.upload_seconds(9_999_999, 100), 31)
        self.assertEqual(admission.upload_seconds(10_000_000, 100), 31)
        self.assertEqual(admission.upload_seconds(10_000_001, 100), 32)
        for size, rate in ((-1, 100), (1, 0), (1, -1),
                           (1, float("nan")), (1, float("inf"))):
            with self.subTest(size=size, rate=rate):
                with self.assertRaises(ValueError):
                    admission.upload_seconds(size, rate)

    def test_payload_parser_refuses_unsafe_or_missing_bound_proofs(self):
        cases = ("traversal", "link", "duplicate", "missing-limit",
                 "missing-runtime", "wrong-freeze", "wrong-model",
                 "wrong-limit", "wrong-runtime", "changed-member",
                 "unmanifested")
        for mode in cases:
            with self.subTest(mode=mode), \
                    tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary).resolve()
                config, freeze, members = input_fixture(root)
                payload = Path(config["payload_path"])
                if mode in ("traversal", "link", "duplicate"):
                    with tarfile.open(payload, "w") as archive:
                        member = tarfile.TarInfo("evidence/a")
                        if mode == "traversal":
                            member.name = "evidence/../outside"
                        if mode == "link":
                            member.type = tarfile.SYMTYPE
                            member.linkname = "outside"
                        archive.addfile(member)
                        if mode == "duplicate":
                            archive.addfile(member)
                else:
                    if mode == "missing-limit":
                        del members["model-limit.json"]
                    elif mode == "missing-runtime":
                        del members["runtime/receipt.json"]
                    elif mode == "wrong-freeze":
                        members["corpus/freeze.json"] = json_bytes({
                            "files": {}, "freeze_id": "0" * 64})
                    elif mode == "wrong-model":
                        name = ("harness/study/storage/pod-cost/"
                                "cpu-cost-v1.json")
                        value = json.loads(members[name])
                        value["model_revision"] = "wrong"
                        members[name] = json_bytes(value)
                    elif mode == "wrong-limit":
                        value = json.loads(members["model-limit.json"])
                        value["maximum_tokens"] = 2048
                        members["model-limit.json"] = json_bytes(value)
                    elif mode == "wrong-runtime":
                        value = json.loads(members["runtime/receipt.json"])
                        value["platform"] = "macos-arm64"
                        members["runtime/receipt.json"] = json_bytes(value)
                    elif mode == "changed-member":
                        members["model/fixture.onnx"] = b"different bytes"
                    if mode != "unmanifested":
                        members["file-manifest.json"] = json_bytes({
                            "files": {name: sha256(raw).hexdigest()
                                      for name, raw in members.items()
                                      if name != "file-manifest.json"}})
                    else:
                        members["extra"] = b"not in the manifest"
                    write_tar(payload, members)
                with patch.object(admission, "FREEZE", freeze), \
                        patch.object(admission, "TOOLS", fixture_tool_pins()):
                    with self.assertRaises((ValueError, KeyError)):
                        admission.payload_check(payload, fixture_source_pins())


class HostTransferTests(unittest.TestCase):
    OBSERVATION_PREFLIGHT_REFUSALS = (
        "observe-no-launch", "observe-replaced-root", "observe-wrong-argument",
        "observe-done", "observe-error", "observe-bad-config",
        "observe-changed-input",
    )
    INVALID_MARKER_MODES = (
        "marker-exit", "marker-scope", "marker-json", "marker-overflow",
        "marker-negative", "marker-inconsistent",
    )

    def test_success_loss_and_copy_timeout_have_one_cleanup_owner(self):
        for mode in ("success", "lost", "copy-timeout", "changed-input"):
            with self.subTest(mode=mode):
                self.host_case(mode)

    def test_ambiguous_launch_probes_without_relaunch(self):
        for mode in ("launch-255", "launch-timeout", "probe-recovery"):
            with self.subTest(mode=mode):
                self.host_case(mode)

    def test_shape_refusal_and_partial_evidence_copy_before_cleanup(self):
        for mode in ("shape-refusal", "partial", "pack-refusal"):
            with self.subTest(mode=mode):
                self.host_case(mode)

    def test_copied_archive_hash_scope_manifest_and_head_are_verified(self):
        for mode in ("bad-hash", "bad-scope", "bad-manifest", "changed-head",
                     "dirty-verifier", "receipt-loss"):
            with self.subTest(mode=mode):
                self.host_case(mode)

    def test_invalid_completion_marker_never_claims_a_verified_result(self):
        for mode in self.INVALID_MARKER_MODES:
            with self.subTest(mode=mode):
                self.host_case(mode)

    def test_duplicate_launch_or_argument_mismatch_never_uses_ssh(self):
        for mode in ("duplicate", "wrong-argument"):
            with self.subTest(mode=mode):
                self.host_case(mode)

    def test_readiness_and_upload_recovery_keep_original_deadline(self):
        for mode in ("ready-recovery", "upload-recovery", "upload-failure"):
            with self.subTest(mode=mode):
                self.host_case(mode)

    def test_copy_only_recovery_preserves_original_files_and_clocks(self):
        for mode in (("observe", "observe-late", "observe-never-started",
                      "observe-corrected")
                     + self.OBSERVATION_PREFLIGHT_REFUSALS):
            with self.subTest(mode=mode):
                self.host_case(mode)

    def test_session_startup_grace_and_dead_or_missing_pid(self):
        for mode in ("startup-recovery", "dead-session", "never-started",
                     "stale-pid"):
            with self.subTest(mode=mode):
                self.host_case(mode)

    def test_binary_tail_and_diagnostic_journal_failure_allow_bulk_copy(self):
        for mode in ("binary-tail", "diagnostic-journal", "tail-decode-error"):
            with self.subTest(mode=mode):
                self.host_case(mode)

    def test_root_precheck_refuses_before_bulk_upload_or_launch(self):
        self.host_case("root-refusal")
        self.host_case("root-refusal-stop-error")

    def test_actual_size_and_remaining_download_window_are_enforced(self):
        for mode in ("bad-size", "archive-bound", "observe-download-window",
                     "observe-download-timeout", "observe-warning-journal"):
            with self.subTest(mode=mode):
                self.host_case(mode)

    def test_bootstrap_failure_never_launches_or_refreshes_contact(self):
        # Given no system Python, when remote hash/unpack fails or
        # expires,
        # then only two precheck uploads occur and cleanup is requested
        # once.
        for mode in ("bootstrap-failure", "bootstrap-timeout"):
            with self.subTest(mode=mode):
                self.host_case(mode)

    def test_actual_root_runtime_facts_precede_bulk_in_both_modes(self):
        # Given either prior proof or first-check approval, when actual
        # facts
        # differ, then refuse with 78 before bulk upload or analysis
        # launch.
        wrong = [
            {**root_facts(), key: value}
            for key, value in (
                ("euid", 1000), ("euid", False), ("euid", 0.0),
                ("machine", "aarch64"), ("os_id", "debian"),
                ("os_version", "24.04"), ("python_version", "3.12.15"),
                ("python_implementation", "PyPy"),
                ("glibc_version", "2.27"), ("glibc_version", "not-a-version"),
                ("writable", False), ("memory_controller", False),
                ("root", "/delegated/cgroup"))
        ]
        wrong += [{key: value for key, value in root_facts().items()
                   if key != missing} for missing in root_facts()]
        for mode in ("first-in-pod-check", "prior-evidence"):
            with self.subTest(mode=mode, valid=True):
                self.host_case("success", root_mode=mode)
            for facts in wrong:
                with self.subTest(mode=mode, facts=facts):
                    self.host_case("root-facts-refusal", root_mode=mode,
                                   facts=facts)

    def test_commands_and_early_session_paths_use_standalone_python(self):
        # Given an image without system Python 3.12, when constructing
        # every
        # early command, then the staged interpreter owns startup and
        # cleanup.
        absolute = "/srv/storage-pod/python/bin/python3.12"
        self.assertTrue(host_run.PROBE_COMMAND.startswith(absolute + " -B"))
        self.assertIn("&& " + absolute + " -B", host_run.ROOT_PROBE_COMMAND)
        bootstrap = host_run.PYTHON_BOOTSTRAP_COMMAND
        self.assertLess(bootstrap.index("sha256sum --check"),
                        bootstrap.index("tar -xzf"))
        self.assertIn(admission.TOOLS["python.tar.gz"], bootstrap)
        self.assertNotIn("python3.12 -", bootstrap)
        script = (host_run.HERE / "accuracy_session.sh").read_text()
        exported = 'export PATH="/srv/storage-pod/python/bin:$PATH"'
        self.assertLess(script.index(exported), script.index("python3.12 -B"))
        self.assertEqual(script.count("--python " + absolute), 2)
        self.assertNotIn("/usr/bin/python3.12", script)
        self.assertNotIn("python3.12-venv", script)
        self.assertIn('test "$VERSION_ID" = 22.04', script)
        self.assertIn("apt-get install -y ca-certificates build-essential",
                      script)
        setup = script[script.index("setup() {"):]
        self.assertLess(setup.index(exported), setup.index("python3.12 -B"))
        self.assertIn("gnu_get_libc_version", host_run.ROOT_PROBE_SOURCE)
        self.assertIn("platform.freedesktop_os_release()",
                      host_run.ROOT_PROBE_SOURCE)

    def test_readonly_root_probe_recovers_without_restaging_or_relaunch(self):
        for mode in ("root-probe-recovery", "root-probe-timeout",
                     "root-probe-expired"):
            with self.subTest(mode=mode):
                self.host_case(mode)

    def test_slow_boot_gets_root_window_but_contact_never_resets(self):
        self.host_case("slow-boot")
        self.host_case("root-probe-existing-contact")

    def test_uncertain_liveness_retries_readonly_until_completion(self):
        for mode in ("unproved-recovery", "invalid-pid-recovery"):
            with self.subTest(mode=mode):
                self.host_case(mode)

    def host_case(self, mode, *, root_mode="first-in-pod-check", facts=None):
        # Given approved bytes, launch once, verify copies, then stop.
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            state = root / "lease"
            state.mkdir()
            config, freeze, _ = input_fixture(root)
            if root_mode == "prior-evidence":
                proof = root / "prior-root.txt"
                proof.write_bytes(b"synthetic prior proof, not live evidence")
                config["cgroup_root"] = {
                    **{key: config[key] for key in
                       ("provider", "cloud_id", "location", "image")},
                    "mode": root_mode, "source_reference": "fixture",
                    "evidence_path": str(proof),
                    "evidence_sha256": artifacts.digest(proof)}
            destination = Path(config["destination"])
            destination.mkdir(mode=0o700)
            clock = Clock()
            config.update(quote_checked_epoch=clock.time(), upload_seconds=60)
            (state / "config.json").write_text(json.dumps(config))
            (state / "validated.json").write_text('{"pod_id":"pod"}')
            owner = {"started_epoch": clock.epoch,
                     "started_monotonic": clock.monotonic(),
                     "cutoff_epoch": clock.epoch + config["cutoff_seconds"]}
            (state / "owner.json").write_text(json.dumps(owner))
            identity = destination.stat()
            (state / "destination-identity.json").write_text(json.dumps({
                "path": str(destination), "device": identity.st_dev,
                "inode": identity.st_ino}))
            original_destination = destination
            copy_destination = destination
            observing = mode.startswith("observe")
            if observing:
                (destination / "old-partial.tar").write_bytes(b"preserve me")
                (state / "connection.json").write_text(json.dumps({
                    "target": "root@127.0.0.1", "port": "22"}))
                if mode != "observe-no-launch":
                    (state / "launch-requested.json").write_text(json.dumps({
                        "requested_epoch": clock.epoch,
                        "relaunch_allowed": False}))
                if mode == "observe-late":
                    clock.elapsed = config["analysis_seconds"] + 400
                if mode == "observe-never-started":
                    clock.elapsed = 60
                if mode in {"observe-download-window",
                            "observe-download-timeout",
                            "observe-warning-journal"}:
                    clock.elapsed = config["cutoff_seconds"] - 20
                if mode in {"observe-done", "observe-error"}:
                    marker = ("done.json" if mode == "observe-done"
                              else "rental-error.json")
                    (state / marker).write_text("{}")
                    (state / "config.json").unlink()
                if mode == "observe-replaced-root":
                    destination.rename(root / "replaced-original")
                    destination.mkdir(mode=0o700)
            if mode == "observe-bad-config":
                (state / "config.json").write_text("{invalid-json")
            if mode in {"changed-input", "observe-changed-input"}:
                Path(config["payload_path"]).write_text("changed")
            if mode == "duplicate":
                (state / "launch-requested.json").write_text(
                    '{"relaunch_allowed":false}')
            events, transports, copies = [], [], []
            launches, probes, mkdirs, root_probes = 0, 0, 0, 0
            bootstraps = 0
            upload_attempts = []
            completed = {"exit": 0, "scope": "full-evidence"}
            if mode == "shape-refusal":
                completed = {"exit": 78, "scope": "partial-evidence"}
            if mode == "pack-refusal":
                completed = {"exit": 1, "scope": "diagnostics-only"}
            if mode == "partial":
                completed = {"exit": 1, "scope": "partial-evidence"}
            if mode == "marker-exit":
                completed = {"exit": "0", "scope": "full-evidence"}
            if mode == "marker-overflow":
                completed = {"exit": 256, "scope": "full-evidence"}
            if mode == "marker-negative":
                completed = {"exit": -1, "scope": "partial-evidence"}
            if mode == "marker-inconsistent":
                completed = {"exit": 1, "scope": "full-evidence"}
            if mode == "marker-scope":
                completed = {"exit": 0, "scope": "accepted-anything"}

            completed["bundle_bytes"] = (None if mode == "pack-refusal"
                                         else 10_240)
            if mode == "bad-size":
                completed["bundle_bytes"] = 1
            if mode == "archive-bound":
                completed["bundle_bytes"] = (artifacts.MAX_PAYLOAD
                                             + artifacts.TAR_OVERHEAD + 1)

            def connect(state):
                self.assertFalse(observing)
                self.assertEqual(transports, [])
                events.append("connect")
                if mode == "slow-boot":
                    clock.elapsed = 320
                if mode == "root-probe-existing-contact":
                    (state / "ssh-contact.json").write_text(json.dumps({
                        "connected_epoch": clock.epoch,
                        "connected_monotonic": clock.monotonic()}))
                    clock.elapsed = 200
                (state / "connection.json").write_text(json.dumps({
                    "target": "root@127.0.0.1", "port": "22"}))
                (state / "session-inputs.json").write_text("{}")

            def git_output(arguments, **kwargs):
                self.assertEqual(arguments[:1], ["git"])
                verifying_evidence = copies and copies[-1] == "evidence.tar"
                if arguments[1] == "rev-parse":
                    return "e" * 40 if (mode == "changed-head" and
                                         verifying_evidence) else "a" * 40
                if arguments[1] == "ls-files":
                    return fixture_git_output(arguments, **kwargs)
                self.assertEqual(arguments[1], "status")
                return " M verifier.py\n" if (mode == "dirty-verifier" and
                                              verifying_evidence) else ""

            completed_2048 = {
                "results/cap2048/assembled/score.json": json_bytes({
                    "schema": "fixture-completed-score",
                    "source_pin": "a" * 40, "config_pin": "b" * 64,
                    "input_pin": "c" * 64}),
                "results/cap2048/assembled/score.metrics.json": json_bytes({
                    "r10": 0.4, "mrr10": 0.2}),
                "results/cap2048/assembled/config.json": json_bytes({
                    "schema": "fixture-bound-config",
                    "input_pin": "c" * 64}),
            }

            def download(name, target):
                scope = ("diagnostics-only" if name == "receipts.tar"
                         else completed["scope"])
                if mode == "bad-scope" and name == "evidence.tar":
                    scope = "diagnostics-only"
                raw = b"retained tiny fixture receipt\n"
                members = {"receipt.txt": raw, **completed_2048}
                members["file-manifest.json"] = json_bytes({
                    "schema": "pod-file-manifest/v1", "scope": scope,
                    "files": {name: sha256(value).hexdigest()
                              for name, value in members.items()}})
                if mode == "bad-manifest" and name == "evidence.tar":
                    members["receipt.txt"] = b"changed after manifest creation"
                bundle = target / name
                write_tar(bundle, members)
                digest = artifacts.digest(bundle)
                if mode == "bad-hash" and name == "evidence.tar":
                    digest = "0" * 64
                (target / (name + ".sha256")).write_text(
                    digest + "  " + name + "\n")

            download("evidence.tar", root)
            if mode not in {"bad-size", "archive-bound", "pack-refusal"}:
                completed["bundle_bytes"] = (
                    root / "evidence.tar").stat().st_size

            def transport(arguments, deadline):
                nonlocal launches, probes, mkdirs, root_probes, bootstraps
                nonlocal copy_destination
                arguments = list(arguments)
                transports.append((arguments, deadline))
                self.assertLessEqual(deadline, owner["cutoff_epoch"])
                self.assertGreater(deadline, clock.time())
                self.assertIn(arguments[0], ("ssh", "scp"))
                if arguments[0] == "scp":
                    if Path(arguments[-1]).is_absolute():
                        copy_destination = Path(arguments[-1])
                        self.assertTrue(copy_destination.is_relative_to(
                            original_destination))
                        name = Path(arguments[-3].split(":", 1)[1]).name
                        self.assertEqual(arguments[-2],
                                         arguments[-3] + ".sha256")
                        copies.append(name)
                        events.append("copy-" + name)
                        if (mode in {"copy-timeout",
                                     "observe-download-timeout"}
                                and name == "evidence.tar"):
                            clock.elapsed += deadline - clock.time()
                            raise subprocess.TimeoutExpired(arguments, 1)
                        if (mode in {"receipt-loss", "binary-tail",
                                     "diagnostic-journal", "tail-decode-error"}
                                and name == "receipts.tar"):
                            raise subprocess.CalledProcessError(255, arguments)
                        download(name, copy_destination)
                        return b""
                    upload_attempts.append((arguments, deadline))
                    events.append("upload")
                    if mode in ("upload-recovery", "upload-failure"):
                        same = [entry for entry in upload_attempts
                                if entry[0] == arguments]
                        if mode == "upload-failure" or len(same) == 1:
                            clock.elapsed += 10
                            raise subprocess.CalledProcessError(255, arguments)
                    return b""
                remote = arguments[-1]
                if "mkdir -p" in remote:
                    mkdirs += 1
                    events.append("mkdir")
                    self.assertLessEqual(deadline,
                                         clock.epoch + config["ready_seconds"])
                    if mode == "ready-recovery" and mkdirs == 1:
                        raise subprocess.CalledProcessError(255, arguments)
                    return b""
                if remote == host_run.PYTHON_BOOTSTRAP_COMMAND:
                    bootstraps += 1
                    events.append("python-bootstrap")
                    self.assertEqual(len(upload_attempts),
                                     4 if mode == "upload-recovery" else 2)
                    if mode == "bootstrap-failure":
                        raise subprocess.CalledProcessError(1, arguments)
                    if mode == "bootstrap-timeout":
                        clock.elapsed += deadline - clock.time()
                        raise subprocess.TimeoutExpired(arguments, 300)
                    return b""
                if remote == host_run.ROOT_PROBE_COMMAND:
                    root_probes += 1
                    events.append("root-probe")
                    self.assertEqual(len(upload_attempts), 4 if mode ==
                                     "upload-recovery" else 2)
                    self.assertEqual(bootstraps, 1)
                    if root_mode == "first-in-pod-check":
                        contact = json.loads(
                            (state / "ssh-contact.json").read_text())
                        self.assertLessEqual(
                            deadline, contact["connected_epoch"] + 300)
                    else:
                        self.assertLessEqual(deadline, clock.epoch + 900)
                    if mode in {"root-refusal", "root-refusal-stop-error"}:
                        raise subprocess.CalledProcessError(
                            78, arguments, output=b"",
                            stderr=b"\x80true root unavailable\n")
                    if mode in {"root-probe-expired",
                                "root-probe-existing-contact"} or (
                            mode in {"root-probe-recovery",
                                     "root-probe-timeout"}
                            and root_probes == 1):
                        clock.elapsed += deadline - clock.time()
                        if mode == "root-probe-timeout":
                            raise subprocess.TimeoutExpired(arguments, 30)
                        raise subprocess.CalledProcessError(255, arguments)
                    return json_bytes(root_facts() if facts is None else facts)
                if "nohup" in remote:
                    launches += 1
                    events.append("launch")
                    self.assertTrue((state / "launch-requested.json").exists())
                    self.assertEqual(remote.count("nohup"), 1)
                    self.assertIn("mkdir launched", remote)
                    self.assertLessEqual(deadline, clock.time() + 30)
                    if mode in {"launch-255", "never-started"}:
                        raise subprocess.CalledProcessError(255, arguments)
                    if mode == "launch-timeout":
                        clock.elapsed += 30
                        raise subprocess.TimeoutExpired(arguments, 30)
                    return b""
                if remote == host_run.PROBE_COMMAND:
                    probes += 1
                    events.append("probe")
                    if (mode == "lost" or
                            mode == "probe-recovery" and probes == 1):
                        clock.elapsed += deadline - clock.time()
                        raise subprocess.CalledProcessError(255, arguments)
                    if (mode in {"unproved-recovery", "invalid-pid-recovery"}
                            and probes == 1):
                        return json_bytes({"completion_hex": None,
                            "launched": True, "process": "unproved"
                            if mode == "unproved-recovery" else "invalid"})
                    if (mode in {"dead-session", "never-started", "stale-pid",
                                 "observe-never-started"}
                            or mode == "startup-recovery" and probes < 3):
                        return json_bytes({"completion_hex": None,
                            "launched": mode not in {"never-started",
                                "stale-pid", "observe-never-started"},
                            "process": "dead" if mode == "dead-session"
                            else "running" if mode == "stale-pid"
                            else "missing" if probes == 1
                            or mode in {"never-started",
                                        "observe-never-started"}
                            else "running"})
                    marker = (b"not JSON" if mode == "marker-json"
                              else json_bytes(completed))
                    return json_bytes({"completion_hex": marker.hex()})
                if remote.startswith("tail -c "):
                    events.append("tail")
                    if mode == "receipt-loss":
                        raise subprocess.CalledProcessError(255, arguments)
                    if mode == "tail-decode-error":
                        raise UnicodeDecodeError("utf-8", b"\x80", 0, 1,
                                                 "fixture decode boundary")
                    return b"\x80partial log tail\xe2\x82\xac\n"
                self.fail("unexpected transport command: " + repr(arguments))

            def journal(path, value):
                if (mode == "observe-warning-journal"
                        and path.name == "download-forecast-exceeded.json"):
                    raise OSError("fixture forecast journal unavailable")
                if (mode == "diagnostic-journal"
                        and path.name.startswith("fallback-copy-error-")):
                    raise OSError("fixture diagnostic journal unavailable")
                lifecycle.save(path, value)

            def cleanup(state, primary=None):
                events.append("cleanup")
                self.assertEqual(json.loads(
                    (state / "owner.json").read_text()),
                    owner, "transport failure cleared cutoff owner")
                self.assertFalse((state / "done.json").exists())
                if mode == "root-refusal-stop-error":
                    self.assertIsNone(primary)
                    raise OSError(errno.ENXIO, "fixture owner absent")
                if (primary is None and mode not in
                        {"root-refusal", "root-facts-refusal"}
                        and completed["scope"] != "diagnostics-only"):
                    self.assertTrue(
                        (state / "evidence.tar-verified.json").exists())

            argv = ["host_run.py", "--state", str(state), "--destination",
                    str(destination), "--ssh-key", config["ssh_key_path"],
                    "--session", config["session_path"], "--environment",
                    config["environment_path"], "--payload",
                    config["payload_path"]]
            if mode in {"wrong-argument", "observe-wrong-argument"}:
                argv[-1] = str(root / "different-inputs.tar")
            if observing:
                argv.append("--observe-only")
            failure_modes = {"lost", "copy-timeout", "changed-input",
                             "bad-hash", "bad-scope", "bad-manifest",
                             "changed-head",
                             "dirty-verifier", "duplicate", "wrong-argument",
                             "upload-failure", "bootstrap-failure",
                             "bootstrap-timeout", "dead-session",
                             "never-started",
                             "stale-pid", "observe-never-started",
                             "bad-size", "archive-bound",
                             "observe-download-timeout", "root-probe-expired",
                             "root-probe-existing-contact",
                             "root-refusal-stop-error"} | set(
                                 self.INVALID_MARKER_MODES) | set(
                                 self.OBSERVATION_PREFLIGHT_REFUSALS)
            preprobe_refusal = mode in self.OBSERVATION_PREFLIGHT_REFUSALS
            with patch.object(sys, "argv", argv), \
                    patch.object(admission, "FREEZE", freeze), \
                    patch.object(admission, "TOOLS", fixture_tool_pins()), \
                    patch.object(admission, "digest", fixture_source_digest), \
                    patch.object(host_run, "connect", side_effect=connect), \
                    patch.object(host_run, "save", side_effect=journal), \
                    patch.object(host_run, "request_cleanup",
                                 side_effect=cleanup) as stop, \
                    patch.object(host_run.subprocess, "check_output",
                                 side_effect=git_output), \
                    patch.object(host_run, "transport",
                                 side_effect=transport), \
                    patch.object(host_run.time, "time", clock.time), \
                    patch.object(host_run.time, "monotonic",
                                 clock.monotonic), \
                    patch.object(host_run.threading, "Event", clock.event):
                if mode == "observe-corrected":
                    payload_index = argv.index("--payload") + 1
                    approved_payload = argv[payload_index]
                    argv[payload_index] = str(root / "typo-inputs.tar")
                    with self.assertRaisesRegex(
                            ValueError, "arguments differ"):
                        host_run.main()
                    stop.assert_not_called()
                    self.assertEqual(transports, [])
                    self.assertEqual(len(list(state.glob(
                        "observe-refusal-*.json"))), 1)
                    argv[payload_index] = approved_payload
                if mode in failure_modes:
                    with self.assertRaises((ValueError, TimeoutError,
                                            RuntimeError, OSError,
                                            subprocess.SubprocessError)):
                        host_run.main()
                    if preprobe_refusal:
                        stop.assert_not_called()
                    elif mode == "root-refusal-stop-error":
                        self.assertIsNone(stop.call_args.args[1])
                    else:
                        self.assertIsNotNone(stop.call_args.args[1])
                    self.assertFalse((state / "copy-result.json").exists())
                elif mode in {"root-refusal", "root-facts-refusal"}:
                    self.assertEqual(host_run.main(), 78)
                    self.assertIsNone(stop.call_args.args[1])
                    self.assertFalse((state / "copy-result.json").exists())
                else:
                    self.assertEqual(host_run.main(), completed["exit"])
                    self.assertEqual(json.loads(
                        (state / "copy-result.json").read_text()), completed)
                if preprobe_refusal:
                    stop.assert_not_called()
                else:
                    stop.assert_called_once()
            if preprobe_refusal:
                self.assertEqual(events, [])
                self.assertEqual(transports, [])
                self.assertEqual(launches, 0)
                refusals = list(state.glob("observe-refusal-*.json"))
                self.assertEqual(len(refusals), 1)
                refusal = json.loads(refusals[0].read_text())
                self.assertIs(refusal["cleanup_requested"], False)
                self.assertEqual(refusal["phase"], "before-first-probe")
                if mode in {"observe-done", "observe-error"}:
                    self.assertIn("rental refused or already deleted",
                                  refusal["error"])
                self.assertEqual(json.loads(
                    (state / "owner.json").read_text()), owner)
                retained = (root / "replaced-original"
                            if mode == "observe-replaced-root"
                            else original_destination)
                self.assertEqual((retained / "old-partial.tar").read_bytes(),
                                 b"preserve me")
                return
            self.assertEqual(events[-1], "cleanup")
            no_launch = (observing or mode in {"changed-input", "duplicate",
                         "wrong-argument", "upload-failure", "root-refusal",
                         "bootstrap-failure", "bootstrap-timeout",
                         "root-facts-refusal",
                         "root-refusal-stop-error", "root-probe-expired",
                         "root-probe-existing-contact"})
            self.assertEqual(launches, 0 if no_launch else 1)
            if observing:
                retained = (root / "replaced-original"
                            if mode == "observe-replaced-root"
                            else original_destination)
                self.assertEqual((retained / "old-partial.tar").read_bytes(),
                                 b"preserve me")
                self.assertEqual(upload_attempts, [])
                self.assertEqual(bootstraps, 0)
                self.assertFalse((state / "python.tar.gz").exists())
                self.assertFalse(
                    (state / "python-bootstrap-input.json").exists())
                self.assertEqual(mkdirs, 0)
                self.assertEqual(events[0], "probe")
                self.assertNotEqual(copy_destination, original_destination)
                self.assertEqual(copy_destination.parent, original_destination)
                self.assertEqual(copy_destination.stat().st_mode & 0o777,
                                 0o700)
                self.assertTrue((state / (copy_destination.name
                                         + ".json")).is_file())
                destination = copy_destination
            elif mode in {"changed-input", "duplicate", "wrong-argument"}:
                self.assertEqual(transports, [])
                return
            else:
                self.assertEqual(events[0], "connect")
                self.assertEqual(events[1], "mkdir")
                expected_uploads = (2 if mode == "upload-failure" else
                                    10 if mode == "upload-recovery" else
                                    2 if mode in {"root-refusal",
                                        "root-facts-refusal",
                                        "bootstrap-failure",
                                        "bootstrap-timeout",
                                        "root-refusal-stop-error",
                                        "root-probe-expired",
                                        "root-probe-existing-contact"} else 5)
                self.assertEqual(len(upload_attempts), expected_uploads)
                if mode in {"upload-recovery", "upload-failure"}:
                    for first, second in zip(upload_attempts[::2],
                                             upload_attempts[1::2],
                                             strict=True):
                        self.assertEqual(first, second)
                    errors = list(state.glob("upload-error-*.json"))
                    self.assertEqual(len(errors),
                                     2 if mode == "upload-failure" else 5)
                if mode in {"upload-failure", "root-refusal",
                            "root-facts-refusal", "bootstrap-failure",
                            "bootstrap-timeout",
                            "root-refusal-stop-error", "root-probe-expired",
                            "root-probe-existing-contact"}:
                    self.assertFalse(
                        (state / "launch-requested.json").exists())
                    if mode in {"root-refusal", "root-refusal-stop-error"}:
                        self.assertEqual(root_probes, 1)
                        self.assertEqual(copies, [])
                        refusal = json.loads((state /
                            "root-probe-refusal.json").read_text())
                        self.assertEqual(refusal["exit"], 78)
                        self.assertEqual(bytes.fromhex(refusal["stderr_hex"]),
                                         b"\x80true root unavailable\n")
                        self.assertLessEqual(clock.elapsed, 300)
                    if mode == "root-facts-refusal":
                        self.assertEqual(root_probes, 1)
                        self.assertEqual(copies, [])
                        refusal = json.loads((state /
                            "root-probe-refusal.json").read_text())
                        self.assertEqual(refusal["exit"], 78)
                        self.assertEqual(
                            json.dumps(refusal["facts"], sort_keys=True),
                            json.dumps(facts, sort_keys=True))
                    if mode in {"bootstrap-failure", "bootstrap-timeout"}:
                        self.assertEqual(bootstraps, 1)
                        self.assertEqual(root_probes, 0)
                        self.assertFalse(
                            (state / "root-capability.json").exists())
                        contact = json.loads(
                            (state / "ssh-contact.json").read_text())
                        self.assertEqual(contact["connected_epoch"],
                                         clock.epoch)
                        if mode == "bootstrap-timeout":
                            self.assertEqual(clock.elapsed, 300)
                    if mode in {"root-probe-expired",
                                "root-probe-existing-contact"}:
                        self.assertEqual(clock.elapsed, 300)
                        errors = list(state.glob("root-probe-error-*.json"))
                        self.assertEqual(len(errors), root_probes)
                        self.assertGreater(root_probes, 1)
                    return
                staged = (upload_attempts[::2] if mode == "upload-recovery"
                          else upload_attempts)
                target = "root@127.0.0.1:" + host_run.REMOTE
                self.assertEqual(staged[0][0][-2:], [
                    str(host_run.HERE / "cgroup_setup.py"),
                    target + "cgroup_setup.py"])
                self.assertEqual(staged[1][0][-2:],
                    [str(state / "python.tar.gz"), target + "python.tar.gz"])
                self.assertEqual(staged[2][0][-2:],
                    [config["environment_path"], target + "env.sh"])
                self.assertEqual(staged[3][0][-2:],
                    [config["session_path"], target + "session.sh"])
                self.assertEqual(staged[4][0][-6:], [
                    str(host_run.HERE / "artifacts.py"),
                    str(host_run.HERE / "preflight.py"),
                    str(state / "session-inputs.json"), config["payload_path"],
                    config["payload_path"] + ".sha256", target])
                upload_start = (clock.epoch
                                + (5 if mode == "ready-recovery" else 0)
                                + (320 if mode == "slow-boot" else 0)
                                + (35 if mode in {"root-probe-recovery",
                                    "root-probe-timeout"} else 0))
                extra = 20 if mode == "upload-recovery" else 0
                probe_delay = (35 if mode in {"root-probe-recovery",
                                            "root-probe-timeout"} else 0)
                self.assertEqual([deadline for _, deadline in staged],
                    [upload_start - probe_delay + config["upload_seconds"]] * 2
                    + [upload_start + extra + config["upload_seconds"]] * 3)
                if probe_delay:
                    self.assertEqual(root_probes, 2)
                    self.assertEqual(len(list(state.glob(
                        "root-probe-error-*.json"))), 1)
                self.assertLess(events.index("python-bootstrap"),
                                events.index("root-probe"))
                self.assertLess(events.index("root-probe"),
                                events.index("launch"))
                self.assertEqual(json.loads((state /
                    "root-capability.json").read_text()),
                    root_facts())
                if mode == "slow-boot":
                    contact = json.loads((state /
                        "ssh-contact.json").read_text())
                    self.assertEqual(contact, {
                        "connected_epoch": clock.epoch + 320,
                        "connected_monotonic": 1320.0})
                    self.assertLess(clock.elapsed, config["ready_seconds"])
                    self.assertEqual(root_probes, 1)
            launch = json.loads((state / "launch-requested.json").read_text())
            self.assertIs(launch["relaunch_allowed"], False)
            self.assertGreaterEqual(probes, 1)
            if not observing:
                self.assertLess(events.index("launch"), events.index("probe"))
            if mode in {"dead-session", "never-started", "stale-pid",
                        "observe-never-started"}:
                if observing:
                    self.assertEqual(clock.elapsed, 60)
                    self.assertEqual(probes, 1)
                else:
                    self.assertLessEqual(clock.elapsed, 30)
                self.assertEqual(copies, ["receipts.tar"])
                return
            if mode == "startup-recovery":
                self.assertEqual(probes, 3)
                self.assertEqual(clock.elapsed, 30)
            if mode in {"launch-255", "launch-timeout"}:
                self.assertTrue((state / "launch-ack-uncertain.json").exists())
            if mode in {"lost", "probe-recovery"}:
                errors = [json.loads(path.read_text())
                          for path in state.glob("probe-error-*.json")]
                self.assertEqual(len(errors), probes if mode == "lost" else 1)
                self.assertEqual({error["attempt"] for error in errors},
                                 set(range(len(errors))))
                self.assertGreater(probes, 1)
            if (mode in {"lost", "archive-bound"}
                    or mode in self.INVALID_MARKER_MODES):
                self.assertNotIn("evidence.tar", copies)
            elif mode == "pack-refusal":
                self.assertEqual(copies, ["receipts.tar"])
                self.assertFalse(
                    (state / "evidence.tar-verified.json").exists())
            else:
                self.assertEqual(copies, ["receipts.tar", "evidence.tar"])
            if mode in self.INVALID_MARKER_MODES:
                self.assertEqual(copies, ["receipts.tar"])
                for receipt in ("completion.json", "copy-result.json",
                                "evidence.tar-verified.json"):
                    self.assertFalse((state / receipt).exists())
                retained = destination / "verified-receipts/evidence"
                manifest = json.loads(
                    (retained / "file-manifest.json").read_text())
                self.assertEqual(manifest["scope"], "diagnostics-only")
                for name, expected in completed_2048.items():
                    self.assertEqual((retained / name).read_bytes(), expected)
                    self.assertEqual(manifest["files"][name],
                                     sha256(expected).hexdigest())
            if mode in {"partial", "shape-refusal"}:
                verified = json.loads(
                    (state / "evidence.tar-verified.json").read_text())
                self.assertEqual(verified["scope"], "partial-evidence")
                manifest = (destination /
                            "verified-evidence/evidence/file-manifest.json")
                self.assertNotIn("accepted-pair.json",
                                 json.loads(manifest.read_text())["files"])
            if mode in {"bad-hash", "bad-scope", "bad-manifest",
                        "changed-head", "dirty-verifier", "copy-timeout",
                        "bad-size", "archive-bound",
                        "observe-download-timeout"}:
                self.assertFalse(
                    (state / "evidence.tar-verified.json").exists())
                self.assertTrue(
                    (state / "receipts.tar-verified.json").exists())
            diagnostic_failures = {"receipt-loss", "binary-tail",
                                   "diagnostic-journal", "tail-decode-error"}
            if mode not in diagnostic_failures:
                self.assertTrue(
                    (state / "receipts.tar-verified.json").exists())
            if mode in diagnostic_failures:
                self.assertTrue(
                    (state / "evidence.tar-verified.json").exists())
            if mode in {"receipt-loss", "tail-decode-error"}:
                errors = list(state.glob("log-tail-error-*.json"))
                self.assertEqual(len(errors), 1)
            if mode == "binary-tail":
                self.assertEqual((destination /
                    "session-log-tail.partial.txt").read_bytes(),
                    b"\x80partial log tail\xe2\x82\xac\n")
            if mode in {"diagnostic-journal", "observe-warning-journal"}:
                self.assertEqual(len(list(state.glob(
                    "diagnostics-error-*.json"))), 1)
            if mode in {"observe-download-window", "observe-download-timeout"}:
                warning = json.loads((state /
                    "download-forecast-exceeded.json").read_text())
                self.assertGreater(warning["required_seconds"],
                                   warning["available_seconds"])
                self.assertEqual(warning["available_seconds"], 20)
                self.assertEqual(warning["cutoff_epoch"],
                                 owner["cutoff_epoch"])
                self.assertEqual(copies, ["receipts.tar", "evidence.tar"])
                bulk = [entry for entry in transports
                        if entry[0][0] == "scp" and len(entry[0]) >= 3
                        and entry[0][-3].endswith("/evidence.tar")]
                self.assertEqual(len(bulk), 1)
                self.assertEqual(bulk[0][1], owner["cutoff_epoch"])
            if mode in {"unproved-recovery", "invalid-pid-recovery"}:
                self.assertEqual(probes, 2)
                self.assertEqual(clock.elapsed, 15)
                self.assertEqual(len(list(state.glob(
                    "probe-liveness-error-*.json"))), 1)

    def test_observation_producer_checks_pid_and_prioritizes_completion(self):
        # Given files and process outcomes, the real read-only producer emits
        # a closed receipt; final marker bytes win over a dead/invalid PID.
        cases = ("missing", "running", "dead", "invalid", "zero", "unicode",
                 "too-long", "overflow", "permission", "complete", "race")
        for mode in cases:
            with self.subTest(mode=mode), \
                    tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                (directory / "launched").mkdir()
                if mode != "missing":
                    pid = {"invalid": "bad", "zero": "0", "unicode": "３",
                           "too-long": "9" * 11,
                           "overflow": "9" * 10}.get(mode, "123")
                    (directory / "session.pid").write_text(pid)
                raw = b'\x80not a valid completion marker'
                if mode == "race":
                    raw = json_bytes({"exit": 0, "scope": "full-evidence",
                                      "bundle_bytes": 10_240})
                if mode == "complete":
                    (directory / "complete.json").write_bytes(raw)
                output = []

                def finished_during_signal(pid, signum):
                    (directory / "complete.json").write_bytes(raw)
                    raise ProcessLookupError()

                error = (finished_during_signal if mode == "race" else
                         ProcessLookupError() if mode == "dead" else
                         PermissionError() if mode == "permission" else
                         OverflowError() if mode == "overflow" else None)
                with patch.object(sys, "argv", ["-c", temporary]), \
                        patch.object(host_run.os, "kill", side_effect=error) \
                        as exists, \
                        patch("builtins.print", side_effect=output.append):
                    exec(compile(host_run.PROBE_SOURCE, "pod-probe", "exec"),
                         {})
                receipt = json.loads(output[0])
                if mode in {"complete", "race"}:
                    self.assertEqual(bytes.fromhex(receipt["completion_hex"]),
                                     raw)
                    if mode == "race":
                        exists.assert_called_once_with(123, 0)
                    else:
                        exists.assert_not_called()
                else:
                    expected = ("invalid" if mode in {
                        "invalid", "zero", "unicode", "too-long"} else
                        "unproved" if mode in {"permission", "overflow"}
                        else mode)
                    self.assertEqual(receipt, {"completion_hex": None,
                                     "launched": True, "process": expected})
                    if mode in {"running", "dead", "permission", "overflow"}:
                        exists.assert_called_once_with(
                            9_999_999_999 if mode == "overflow" else 123, 0)
                    else:
                        exists.assert_not_called()

    def test_transport_timeout_kills_and_reaps_process_group(self):
        child = SimpleNamespace(pid=101, returncode=None)
        failure = subprocess.TimeoutExpired(["scp", "fixture"], 1)
        with patch.object(host_run.time, "time", return_value=100), \
                patch.object(host_run.subprocess, "Popen",
                             return_value=child) as popen, \
                patch.object(host_run.os, "killpg") as kill:
            child.communicate = unittest.mock.Mock(side_effect=failure)
            child.poll = unittest.mock.Mock(return_value=None)
            child.wait = unittest.mock.Mock(return_value=-9)
            with self.assertRaises(subprocess.TimeoutExpired) as raised:
                host_run.transport(["scp", "fixture"], 101)
            self.assertIs(raised.exception, failure)
            kill.assert_called_once_with(101, signal.SIGKILL)
            child.wait.assert_called_once_with()
            child.communicate.assert_called_once_with(timeout=1)
            self.assertTrue(popen.call_args.kwargs["start_new_session"])

    def test_transport_returns_small_stdout_and_rejects_expired_window(self):
        child = SimpleNamespace(returncode=0)
        child.communicate = unittest.mock.Mock(
            return_value=(b"\x80small stdout", b""))
        with patch.object(host_run.time, "time", return_value=100), \
                patch.object(host_run.subprocess, "Popen",
                             return_value=child) as popen:
            self.assertEqual(host_run.transport(["ssh", "fixture"], 101),
                             b"\x80small stdout")
            child.returncode = 255
            with self.assertRaises(subprocess.CalledProcessError) as raised:
                host_run.transport(["ssh", "fixture"], 101)
            self.assertEqual(raised.exception.output, b"\x80small stdout")
            self.assertNotIn("text", popen.call_args.kwargs)
            self.assertEqual(popen.call_count, 2)
            with self.assertRaises(TimeoutError):
                host_run.transport(["ssh", "fixture"], 100)
            self.assertEqual(popen.call_count, 2)


if __name__ == "__main__":
    unittest.main()
