"""Cloud-free lifecycle, shape, archive and language identity fixtures."""
import json
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

import artifacts
import accuracy
import groups
import host_run
import lifecycle
import preflight


def pins() -> dict:
    return {
        "cli_context": "approved", "provider": "nebius", "offer_id": "cpu",
        "cloud_id": "cloud", "image": "image", "gpu_label": "CPU",
        "disk_gb": 64, "vcpus": 8, "memory_gb": 32,
        "quoted_total_hourly": "0.25", "quote_checked_epoch": time.time(),
        "harness_commit": "a" * 40, "quote_reference": "receipt",
        "max_seconds": 10800, "cutoff_seconds": 10680, "setup_seconds": 1800,
        "budget": "2", "hourly_ceiling": "0.50", "contingency_seconds": 3600,
        "minimum_free_bytes": 32 * 1024 ** 3,
        "minimum_memory_bytes": 28 * 1024 ** 3,
        "payload_sha256": "b" * 64, "session_sha256": "c" * 64,
        "environment_sha256": "d" * 64,
    }


class SupportTests(unittest.TestCase):
    def test_pins_and_rejections_do_not_mutate_input(self):
        value = pins()
        snapshot = dict(value)
        self.assertNotIsInstance(lifecycle.execution_pins(
            value, value["quote_checked_epoch"]), lifecycle.Invalid)
        self.assertEqual(value, snapshot)
        for key, wrong in (("quoted_total_hourly", "0.51"),
                           ("hourly_ceiling", "0.51"),
                           ("quote_checked_epoch", 0), ("vcpus", True),
                           ("minimum_memory_bytes", -1),
                           ("payload_sha256", "missing")):
            with self.subTest(key=key):
                self.assertIsInstance(lifecycle.execution_pins(
                    {**value, key: wrong}, time.time()), lifecycle.Invalid)
        self.assertIsInstance(lifecycle.execution_pins({}, time.time()),
                              lifecycle.Invalid)

    def test_shape_refuses_each_insufficient_resource(self):
        config = pins()
        facts = {"cpus": 8, "available_memory_bytes": 32 * 1024 ** 3,
                 "free_disk_bytes": 40 * 1024 ** 3,
                 "cgroup_controllers": "cpu memory"}
        self.assertFalse(preflight.refused(facts, config))
        for key, wrong in (("cpus", 7), ("available_memory_bytes", 1),
                           ("free_disk_bytes", 1),
                           ("cgroup_controllers", "cpu")):
            self.assertTrue(preflight.refused({**facts, key: wrong}, config))
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
                                 return_value=set(range(8)), create=True), \
                    patch.object(preflight.shutil, "disk_usage",
                                 return_value=SimpleNamespace(
                                     free=40 * 1024 ** 3)):
                current = preflight.observe(pins(), root)
                self.assertTrue(preflight.refused(current, pins()))
                capacity = preflight.observe(pins(), root, cgroup)
                self.assertFalse(preflight.refused(capacity, pins()))
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
                (False, True, False), (False, False, True)):
            with self.subTest(create=create_timeout, delete=delete_timeout,
                              early_error=early_error):
                self.lifecycle_case(create_timeout, delete_timeout,
                                    early_error)

    def lifecycle_case(self, create_timeout, delete_timeout, early_error):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary).resolve()
            state = directory / "state"
            config = directory / "execution.json"
            config.write_text(json.dumps(pins()))
            counts = {"create": 0, "terminate": 0}
            deleted = False
            failed_early = False
            timers = []

            class Timer:
                def __init__(self, seconds, callback):
                    self.callback = callback
                    self.started = False
                    self.worker = None
                    timers.append(self)

                def start(self):
                    self.started = True

                def cancel(self):
                    pass

                def join(self):
                    if self.worker is not None:
                        self.worker.join()

            def call(config, state, label, arguments, timeout, **kwargs):
                nonlocal deleted
                self.assertTrue(timers[0].started)
                counts[label] += 1
                if label == "create":
                    if create_timeout:
                        raise subprocess.TimeoutExpired(arguments, timeout)
                    (state / "create.stdout").write_text(
                        "cloudId: cloud\nvcpus: 8\nmemory: 32\ndiskSize: 64\n"
                        "image: image\nprovider: nebius\n"
                        "gpuCount: 0\nteam: None\n")
                    if early_error:
                        timers[0].callback()
                if label == "terminate":
                    deleted = True
                    if delete_timeout:
                        raise subprocess.TimeoutExpired(arguments, timeout)
                return subprocess.CompletedProcess(arguments, 0)

            def pages(config, state, mode, label):
                nonlocal failed_early
                if label == "reconcile" and early_error and not failed_early:
                    failed_early = True
                    return []
                if mode == "history":
                    return [{"id": "pod", "terminated_at": "observed",
                             "total_cost": "0.01"}]
                if deleted:
                    return []
                return [{"id": "pod", "name": json.loads(
                    (state / "owner.json").read_text())["name"]}]

            def select(*unused):
                lifecycle.stop_request(state)
                timer = timers[0]
                if early_error:
                    return ([], [], [])
                timer.worker = threading.Thread(target=timer.callback)
                timer.worker.start()
                return ([], [], [])

            status = {"id": "pod", "provider": "nebius", "image": "image",
                      "cost_per_hour": "0.25", "gpu": "CPU"}
            with patch.object(lifecycle.threading, "Timer", Timer), \
                    patch.object(lifecycle.shutil, "which",
                                 return_value="/prime"), \
                    patch.object(lifecycle.subprocess, "check_output",
                                 return_value="Prime CLI version: 0.17.0\n"), \
                    patch.object(lifecycle, "call", side_effect=call), \
                    patch.object(lifecycle, "pages", side_effect=pages), \
                    patch.object(lifecycle, "status_read",
                                 return_value=status), \
                    patch.object(lifecycle.select, "select",
                                 side_effect=select):
                if create_timeout:
                    with self.assertRaises(RuntimeError):
                        lifecycle.rent(config, state)
                elif early_error:
                    with self.assertRaises(ValueError):
                        lifecycle.rent(config, state)
                else:
                    lifecycle.rent(config, state)
            self.assertEqual(counts, {"create": 1, "terminate": 1})
            self.assertEqual(json.loads((state / "done.json").read_text())[
                "state"], "deleted")
            lifecycle.stop_request(state)  # Already-done stop changes nothing.

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


class HostTransferTests(unittest.TestCase):
    def test_success_loss_and_copy_timeout_have_one_cleanup_owner(self):
        for mode in ("success", "lost", "copy-timeout", "changed-input"):
            with self.subTest(mode=mode):
                self.host_case(mode)

    def host_case(self, mode):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            state = root / "lease"
            state.mkdir()
            session, environment = root / "session.sh", root / "env.sh"
            payload, key = root / "inputs.tar", root / "key"
            for path in (session, environment, payload, key):
                path.write_text("fixture")
            key.chmod(0o600)
            config = {**pins(), "session_sha256": artifacts.digest(session),
                      "environment_sha256": artifacts.digest(environment),
                      "payload_sha256": artifacts.digest(payload)}
            (state / "config.json").write_text(json.dumps(config))
            (state / "connection.json").write_text(json.dumps({
                "target": "root@127.0.0.1", "port": "22"}))
            (state / "owner.json").write_text(json.dumps({
                "cutoff_epoch": time.time() + 600}))
            if mode == "changed-input":
                payload.write_text("changed")
            children = []

            class Child:
                def __init__(self, command, **kwargs):
                    self.args = command
                    self.pid = 101 + len(children)
                    self.returncode = None
                    self.timed_out = False
                    self.full = any(
                        arg.endswith(":/srv/storage-pod/evidence.tar")
                        for arg in command)
                    children.append(self)
                    if kwargs.get("start_new_session") is not True:
                        raise AssertionError("SCP must own a process group")

                def wait(self, timeout=None):
                    if (mode == "copy-timeout" and self.full
                            and not self.timed_out):
                        self.timed_out = True
                        raise subprocess.TimeoutExpired(self.args, timeout)
                    self.returncode = 0
                    return 0

                def poll(self):
                    return self.returncode

            argv = ["host_run.py", "--state", str(state), "--destination",
                    str(root / "copy"), "--ssh-key", str(key), "--session",
                    str(session), "--environment", str(environment),
                    "--payload", str(payload)]
            responses = [subprocess.CompletedProcess([], 0),
                         subprocess.CompletedProcess([], 255 if mode == "lost"
                                                     else 0)]
            with patch.object(sys, "argv", argv), \
                    patch.object(host_run, "connect"), \
                    patch.object(host_run, "stop_request") as stop, \
                    patch.object(host_run.subprocess, "check_output",
                                 side_effect=["a" * 40, ""]), \
                    patch.object(host_run.subprocess, "Popen", Child), \
                    patch.object(host_run.subprocess, "run",
                                 side_effect=responses), \
                    patch.object(host_run.os, "killpg") as kill:
                if mode in ("lost", "changed-input"):
                    with self.assertRaises((RuntimeError, ValueError)):
                        host_run.main()
                else:
                    self.assertEqual(host_run.main(),
                                     0 if mode == "success" else 1)
                self.assertEqual(stop.call_count, 0 if mode == "lost" else 1)
                self.assertEqual(kill.call_count,
                                 1 if mode == "copy-timeout" else 0)
            self.assertEqual(sum(child.full for child in children),
                             0 if mode in ("lost", "changed-input") else 1)


if __name__ == "__main__":
    unittest.main()
