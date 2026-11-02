"""C2 lifecycle regressions. Fakes replace every account operation."""
from contextlib import ExitStack
from copy import deepcopy
from datetime import datetime, timezone
from io import StringIO
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import lifecycle
from test_lifecycle_support import Clock, offer_document, pins


SIGNALS = (signal.SIGINT, signal.SIGTERM, signal.SIGHUP, signal.SIGQUIT)


def execution_fixture(now):
    # Semantic admission is mocked in this account-free lifecycle fixture.
    return {**pins(), "quote_checked_epoch": now,
            "cgroup_root": {"mode": "prior-evidence",
                            "source_reference": "synthetic; admission mocked"}}


def availability(config, **changes):
    return offer_document(config, **changes)


def probe_seconds_fixture(config):
    return config["cgroup_root"].get("probe_seconds", config["ready_seconds"])


class OwnerObserved(BaseException):
    """End a permanently unresolved fake after observing the cutoff."""


class Rental:
    """Own fake state, signal handlers and an optional cutoff thread."""

    def __init__(self, test, root):
        self.test = test
        self.clock = Clock()
        self.state = root / "state"
        self.path = root / "execution.json"
        self.config = {**execution_fixture(self.clock.time()),
                       "destination": str(root / "copy")}
        self.path.write_text(json.dumps(self.config))
        self.identity_after = 0
        self.stop_after = None
        self.abort_after = None
        self.create_error = None
        self.race_cutoff = False
        self.root_receipt = None
        self.root_receipt_after = 0
        self.calls = []
        self.reconciliations = []
        self.polls = []
        self.handlers = {}
        self.old_handlers = {number: object() for number in SIGNALS}
        self.restored = []
        self.delivered = []
        self.terminate_times = []
        self.verifications = []
        self.status_reads = 0
        self.descriptor = None
        self.worker = None
        self.terminating = threading.Event()
        self.owner_requested = threading.Event()
        self.original_read = os.read
        self.timer = SimpleNamespace(
            started=False, cancelled=False, joined=False)

    def make_timer(self, seconds, callback):
        self.test.assertEqual(seconds, self.config["cutoff_seconds"])
        self.timer.callback = callback
        self.timer.start = self.start_timer
        self.timer.cancel = self.cancel_timer
        self.timer.join = self.join_timer
        return self.timer

    def start_timer(self):
        self.timer.started = True

    def cancel_timer(self):
        self.timer.cancelled = True

    def join_timer(self):
        if self.worker is not None:
            self.worker.join(5)
            self.test.assertFalse(self.worker.is_alive())
        self.timer.joined = True

    def install_signal(self, number, handler):
        if handler is self.old_handlers[number]:
            self.test.assertEqual(len(self.terminate_times), 1)
            self.restored.append(number)
        else:
            self.handlers[number] = handler
        return self.old_handlers[number]

    def call(self, config, state, label, arguments, timeout=30, **kwargs):
        self.calls.append(label)
        if label == "availability":
            (state / "availability.stdout").write_text(
                json.dumps(availability(config)))
        elif label == "create":
            self.test.assertTrue(self.timer.started)
            self.test.assertEqual(tuple(self.handlers), SIGNALS)
            identity = json.loads(
                (state / "destination-identity.json").read_text())
            destination = Path(config["destination"])
            current = destination.stat()
            self.test.assertEqual(identity, {
                "path": str(destination), "device": current.st_dev,
                "inode": current.st_ino})
            self.test.assertEqual(current.st_mode & 0o777, 0o700)
            if self.create_error is not None:
                raise self.create_error
            (state / "create.stdout").write_text(
                "cloudId: cpu-d3_8vcpu-32gb\nvcpus: 8\n"
                "memory: 32\ndiskSize: 100\nimage: image\n"
                "provider: nebius\ngpuCount: 1\nteam: None\n")
        elif label == "terminate":
            self.test.assertEqual(arguments, ["terminate", "pod", "--yes"])
            self.test.assertEqual(self.restored, [])
            self.terminate_times.append(self.clock.elapsed)
            if self.race_cutoff:
                self.terminating.set()
                self.test.assertTrue(self.owner_requested.wait(5))
        else:
            self.test.fail("unexpected account operation: " + label)
        return subprocess.CompletedProcess(arguments, 0)

    def pages(self, config, state, mode, label, deadline=None):
        self.test.assertEqual(mode, "list")
        self.test.assertIn(label, ("identify", "reconcile"))
        if label == "reconcile":
            self.reconciliations.append(self.clock.elapsed)
            self.test.assertEqual(self.restored, [])
            if self.clock.elapsed == 0 and self.identity_after > 0:
                raise subprocess.CalledProcessError(1, ["list"])
        if self.clock.elapsed < self.identity_after:
            return []
        owner = json.loads((state / "owner.json").read_text())
        return [{"id": "pod", "name": owner["name"]}]

    def read(self, descriptor, size):
        self.owner_requested.set()
        return self.original_read(descriptor, size)

    def select(self, descriptors, unused_write, unused_error, timeout):
        self.descriptor = descriptors[0]
        self.test.assertEqual(self.restored, [])
        self.test.assertEqual(tuple(self.handlers), SIGNALS)
        self.test.assertTrue((self.state / "stop.fifo").exists())
        self.clock.elapsed += timeout
        if (self.root_receipt is not None
                and self.clock.elapsed >= self.root_receipt_after):
            (self.state / "root-capability.json").write_text(self.root_receipt)
        if self.abort_after is not None and self.clock.elapsed == 45:
            self.clock.elapsed = self.config["cutoff_seconds"] - 15
        self.polls.append((self.clock.elapsed, timeout))
        if (self.abort_after is not None
                and self.clock.elapsed > self.abort_after):
            raise OwnerObserved()
        if (self.stop_after is not None
                and self.clock.elapsed >= self.stop_after):
            for number in SIGNALS:
                self.handlers[number](number, None)
                self.delivered.append(number)
            if self.race_cutoff and self.worker is None:
                self.worker = threading.Thread(target=self.timer.callback)
                self.worker.start()
                self.test.assertTrue(self.terminating.wait(5))
            return descriptors, [], []
        return [], [], []

    def verify(self, config, state, pod_id, started, deadline):
        self.test.assertEqual(pod_id, "pod")
        self.test.assertEqual(len(self.terminate_times), 1)
        self.verifications.append(deadline)

    def status_read(self, config, state, pod_id, label):
        self.status_reads += 1
        self.test.assertEqual((pod_id, label), ("pod", "initial-status"))
        return {"id": "pod", "provider": "nebius", "image": "image",
                "cost_per_hour": "0.264", "gpu": "CPU_NODE x1"}

    def run(self):
        replacements = [
            (lifecycle.threading, "Timer", self.make_timer),
            (lifecycle.signal, "signal", self.install_signal),
            (lifecycle.shutil, "which", lambda _: "/prime"),
            (lifecycle.subprocess, "check_output",
             lambda *_, **kw: "Prime CLI version: 0.17.0\n"),
            (lifecycle.time, "time", self.clock.time),
            (lifecycle.time, "monotonic", self.clock.monotonic),
            (lifecycle, "call", self.call),
            (lifecycle, "pages", self.pages),
            (lifecycle, "status_read", self.status_read),
            (lifecycle, "verify_deletion", self.verify),
            (lifecycle.select, "select", self.select),
            (lifecycle.os, "read", self.read),
        ]
        with ExitStack() as stack:
            stack.enter_context(patch.dict(
                sys.modules, {"admission": SimpleNamespace(
                    validate=lambda _: 60,
                    root_probe_seconds=probe_seconds_fixture)}))
            for owner, name, replacement in replacements:
                stack.enter_context(patch.object(owner, name, replacement))
            try:
                lifecycle.rent(self.path, self.state)
            finally:
                if self.worker is not None:
                    self.owner_requested.set()
                    self.worker.join(5)
                    self.test.assertFalse(self.worker.is_alive())


class OwnerTests(unittest.TestCase):
    def test_unidentified_reconciles_without_fifo_readiness(self):
        # Given a failed first read, when identity appears at 15/30 s,
        # then reconciliation deletes once without a FIFO wake.
        for visible in (15, 30):
            with self.subTest(visible=visible), \
                    tempfile.TemporaryDirectory() as temporary:
                rental = Rental(self, Path(temporary).resolve())
                rental.identity_after = visible
                primary = subprocess.TimeoutExpired(["create"], 90)
                rental.create_error = primary
                with self.assertRaises(subprocess.TimeoutExpired) as raised:
                    rental.run()
                self.assertIs(raised.exception, primary)
                self.assertEqual(rental.reconciliations,
                                 list(range(0, visible + 1, 15)))
                self.assertEqual(rental.terminate_times, [visible])
                self.assertEqual(rental.status_reads, 0)
                self.assertEqual(rental.calls,
                                 ["availability", "create", "terminate"])
                self.assertEqual(rental.restored, list(SIGNALS))
                self.assertTrue(rental.timer.cancelled)
                self.assertTrue(rental.timer.joined)

    def test_unidentified_failure_keeps_owner_past_cutoff(self):
        # Given permanent identity failure, when cutoff passes,
        # then reads recur without releasing ownership or handlers.
        with tempfile.TemporaryDirectory() as temporary:
            rental = Rental(self, Path(temporary).resolve())
            rental.identity_after = float("inf")
            rental.create_error = subprocess.TimeoutExpired(["create"], 90)
            rental.abort_after = rental.config["cutoff_seconds"] + 15
            try:
                with patch.object(lifecycle.sys, "stderr", StringIO()) as log:
                    with self.assertRaises(OwnerObserved):
                        rental.run()
                self.assertIn("owner retained; escalate", log.getvalue())
                self.assertEqual(rental.restored, [])
                self.assertEqual(rental.calls, ["availability", "create"])
                self.assertEqual(rental.terminate_times, [])
                self.assertEqual(rental.reconciliations[-1],
                                 rental.abort_after)
                self.assertEqual(rental.reconciliations[:3], [0, 15, 30])
                self.assertFalse(rental.timer.cancelled)
                self.assertTrue((rental.state / "stop.fifo").exists())
            finally:
                if rental.descriptor is not None:
                    os.close(rental.descriptor)

    def test_identified_owner_waits_for_fifo_or_cutoff(self):
        # Given a known rental and no readiness, when time passes,
        # then no reconciliation runs before FIFO or cutoff.
        for stop_after in (30, None):
            with self.subTest(stop_after=stop_after), \
                    tempfile.TemporaryDirectory() as temporary:
                rental = Rental(self, Path(temporary).resolve())
                rental.stop_after = stop_after
                rental.run()
                expected = stop_after or rental.config["cutoff_seconds"]
                self.assertEqual(rental.terminate_times, [expected])
                self.assertEqual(rental.reconciliations, [])
                self.assertEqual(rental.status_reads, 1)
                self.assertEqual(rental.polls[0], (15, 15))
                self.assertEqual(rental.calls,
                                 ["availability", "create", "terminate"])
                self.assertEqual(rental.restored, list(SIGNALS))

    def test_four_signals_and_concurrent_cutoff_share_one_attempt(self):
        # Given four queued terminal signals and a concurrent cutoff,
        # when both request cleanup, one effect precedes all restores.
        with tempfile.TemporaryDirectory() as temporary:
            rental = Rental(self, Path(temporary).resolve())
            rental.stop_after = 15
            rental.race_cutoff = True
            rental.run()
            self.assertEqual(lifecycle.OWNER_SIGNALS, SIGNALS)
            self.assertEqual(rental.delivered, list(SIGNALS))
            self.assertEqual(rental.terminate_times, [15])
            self.assertEqual(len(rental.verifications), 1)
            self.assertEqual(rental.restored, list(SIGNALS))
            self.assertTrue(rental.timer.joined)

    def test_cancel_exception_survives_successful_cleanup(self):
        # Given create cancellation, when cleanup identifies the pod,
        # then the same cancellation propagates after the one attempt.
        with tempfile.TemporaryDirectory() as temporary:
            rental = Rental(self, Path(temporary).resolve())
            primary = KeyboardInterrupt("cancel create")
            rental.create_error = primary
            with self.assertRaises(KeyboardInterrupt) as raised:
                rental.run()
            self.assertIs(raised.exception, primary)
            self.assertEqual(rental.terminate_times, [0])
            self.assertEqual(rental.restored, list(SIGNALS))


class OfferTests(unittest.TestCase):
    def test_exact_selected_row_accepts_unmatched_different_id(self):
        # Given one matching row, when another ID does not match,
        # then accept the selected row without changing the listing.
        config = execution_fixture(0)
        document = availability(config)
        selected = document["gpu_resources"][0]
        document["gpu_resources"].append({
            **selected, "id": "other", "location": "US"})
        document["total_count"] = 2
        before = deepcopy(document)
        self.assertIs(lifecycle.select_offer(document, config), selected)
        self.assertEqual(document, before)

    def test_multiple_matching_different_ids_are_refused(self):
        # Given two matching offers, when only one ID was approved,
        # then ambiguity is refused rather than silently selecting it.
        config = execution_fixture(0)
        document = availability(config)
        document["gpu_resources"].append({
            **document["gpu_resources"][0], "id": "other"})
        document["total_count"] = 2
        with self.assertRaisesRegex(ValueError, "ambiguous"):
            lifecycle.select_offer(document, config)

    def test_matching_row_requires_operator_selected_id(self):
        # Given one matching row with another ID, when selection runs,
        # then the operator's exact approved ID remains mandatory.
        config = execution_fixture(0)
        with self.assertRaisesRegex(ValueError, "absent"):
            lifecycle.select_offer(availability(config, id="other"), config)


class ExecutionPinTests(unittest.TestCase):
    def test_download_evidence_and_root_pins_are_mandatory(self):
        # Given a valid structural config, when a new pin is absent,
        # then reject it before rent can perform account operations.
        config = execution_fixture(100)
        before = deepcopy(config)
        self.assertIsInstance(lifecycle.execution_pins(config, 100), dict)
        self.assertEqual(config, before)
        fields = ("minimum_download_mbps", "download_reference",
                  "expected_evidence_bytes", "evidence_size_reference",
                  "cgroup_root")
        for name in fields:
            with self.subTest(missing=name):
                missing = {key: value for key, value in config.items()
                           if key != name}
                self.assertIsInstance(lifecycle.execution_pins(missing, 100),
                                      lifecycle.Invalid)

    def test_download_evidence_and_reference_types_are_rejected(self):
        # Given pins with wrong types, references or bounds,
        # then no default or bool-as-number bypass is accepted.
        config = execution_fixture(100)
        invalid = [
            ("minimum_download_mbps", value)
            for value in (True, "100", 0, -1, float("nan"), float("inf"))
        ] + [
            ("expected_evidence_bytes", value)
            for value in (True, "1", 1.5, 0, -1,
                          8 * 1024 ** 3 + 16 * 1024 ** 2 + 1)
        ] + [
            (name, value)
            for name in ("download_reference", "evidence_size_reference")
            for value in ("", " ", None, 7)
        ] + [("cgroup_root", value) for value in ([], "probe", True)]
        for name, value in invalid:
            with self.subTest(name=name, value=value):
                self.assertIsInstance(lifecycle.execution_pins(
                    {**config, name: value}, 100), lifecycle.Invalid)
        maximum = {**config, "expected_evidence_bytes":
                   8 * 1024 ** 3 + 16 * 1024 ** 2}
        self.assertIsInstance(lifecycle.execution_pins(maximum, 100), dict)


class DeletionProofTests(unittest.TestCase):
    def proof_case(self, root, failures, *, wrong_proof=None):
        clock = Clock()
        config = execution_fixture(clock.time())
        state = root
        reads = []
        pending = list(failures)
        deadline = clock.monotonic() + 12
        if wrong_proof == "rate":
            config["quoted_total_hourly"] = "0.51"
        started = clock.epoch - (10800 if wrong_proof == "elapsed" else 10)
        ended = datetime.fromtimestamp(
            clock.epoch, timezone.utc).strftime("%Y-%m-%d %H:%M:%S UTC")
        if wrong_proof == "bad-time":
            ended = "not a UTC timestamp"

        def call(config, state, label, arguments, timeout=30, **kwargs):
            mode = arguments[0]
            self.assertIn(mode, ("list", "history"))
            reads.append((mode, clock.elapsed))
            failure = pending.pop(0) if pending else None
            if failure == "cli":
                return subprocess.CompletedProcess(arguments, 1)
            if failure == "timeout":
                raise subprocess.TimeoutExpired(arguments, timeout)
            if failure == "read-deadline":
                raise TimeoutError("paginated read did not finish")
            if failure == "json":
                raw = "{invalid-json"
            else:
                rows = ([] if mode == "list" else
                        [{"id": "pod", "terminated_at": ended,
                          "total_cost": 12961.0}])
                raw = json.dumps({"pods" if mode == "list" else "history":
                                  rows, "offset": 0, "total_count": len(rows)})
            (state / (label + ".stdout")).write_text(raw)
            return subprocess.CompletedProcess(arguments, 0)

        with patch.object(lifecycle, "call", side_effect=call), \
                patch.object(lifecycle.time, "monotonic", clock.monotonic), \
                patch.object(lifecycle.time, "time", clock.time), \
                patch.object(lifecycle.threading, "Event", clock.event):
            if failures == ["cli"] * 3:
                with self.assertRaises(TimeoutError):
                    lifecycle.verify_deletion(config, state, "pod", started,
                                              deadline)
            elif wrong_proof is not None:
                with self.assertRaisesRegex(ValueError, "time/rate"):
                    lifecycle.verify_deletion(config, state, "pod", started,
                                              deadline)
            else:
                lifecycle.verify_deletion(config, state, "pod", started,
                                          deadline)
        receipts = [json.loads(path.read_text()) for path in
                    sorted(state.glob("deletion-read-error-*.json"))]
        return clock, reads, sorted(receipts, key=lambda row: row["attempt"])

    def test_transient_cli_and_json_reads_recover_with_receipts(self):
        # Given CLI and malformed-JSON failures, when reads recover,
        # then proof succeeds within the same reserve and errors remain.
        with tempfile.TemporaryDirectory() as temporary:
            state = Path(temporary)
            clock, reads, receipts = self.proof_case(
                state, ["cli", None, "json"])
            self.assertEqual(reads, [("list", 0), ("list", 5),
                                     ("history", 5), ("list", 10),
                                     ("history", 10)])
            self.assertEqual(clock.waits, [5, 5])
            self.assertEqual([row["attempt"] for row in receipts], [1, 2])
            self.assertEqual([row["observed_epoch"] for row in receipts],
                             [clock.epoch, clock.epoch + 5])
            self.assertIn("CalledProcessError", receipts[0]["error"])
            self.assertIn("JSONDecodeError", receipts[1]["error"])
            done = json.loads((state / "done.json").read_text())
            self.assertIs(done["within_time_and_approved_rate"], True)
            self.assertEqual(done["pod"]["total_cost"], 12961.0)
            self.assertIn("not-verified", done["billed_cost"])

    def test_permanent_read_failures_expire_original_deadline(self):
        # Given only failed reads, when the original deadline expires,
        # then proof refuses without renewing the reserve or mutation.
        with tempfile.TemporaryDirectory() as temporary:
            state = Path(temporary)
            clock, reads, receipts = self.proof_case(state, ["cli"] * 3)
            self.assertEqual(clock.elapsed, 12)
            self.assertEqual(clock.waits, [5, 5, 2])
            self.assertEqual(reads, [("list", 0), ("list", 5), ("list", 10)])
            self.assertEqual([row["attempt"] for row in receipts], [1, 2, 3])
            self.assertFalse((state / "done.json").exists())

    def test_read_timeout_classes_retry_without_effect_replay(self):
        # Given a read timeout, when the next read succeeds,
        # then only list/history repeat and the receipt is retained.
        for failure in ("timeout", "read-deadline"):
            with self.subTest(failure=failure), \
                    tempfile.TemporaryDirectory() as temporary:
                clock, reads, receipts = self.proof_case(
                    Path(temporary), [failure])
                self.assertEqual(clock.waits, [5])
                self.assertEqual(reads, [("list", 0), ("list", 5),
                                         ("history", 5)])
                self.assertEqual(len(receipts), 1)

    def test_final_rate_or_time_proof_failure_is_not_retried(self):
        # Given deletion observations outside the approved proof,
        # when rate/time is checked, refuse immediately without retry.
        for wrong in ("rate", "elapsed", "bad-time"):
            with self.subTest(wrong=wrong), \
                    tempfile.TemporaryDirectory() as temporary:
                state = Path(temporary)
                clock, reads, receipts = self.proof_case(
                    state, [], wrong_proof=wrong)
                self.assertEqual(reads, [("list", 0), ("history", 0)])
                self.assertEqual(clock.waits, [])
                self.assertEqual(receipts, [])
                done = json.loads((state / "done.json").read_text())
                self.assertIs(done["within_time_and_approved_rate"], False)


class RootDeadlineTests(unittest.TestCase):
    def fallback(self, root, seconds=300):
        rental = Rental(self, root)
        rental.config["cgroup_root"] = {
            "mode": "first-in-pod-check", "probe_seconds": seconds,
            "approval_reference": "approved first root check"}
        rental.path.write_text(json.dumps(rental.config))
        return rental

    def test_missing_host_root_check_stops_by_probe_deadline(self):
        # Given no host helper, when the first-check allowance expires,
        # then the owner attempts deletion by that original deadline.
        for seconds in (299, 300):
            with self.subTest(seconds=seconds), \
                    tempfile.TemporaryDirectory() as temporary:
                rental = self.fallback(Path(temporary).resolve(), seconds)
                rental.run()
                self.assertEqual(rental.terminate_times, [seconds])
                self.assertEqual(rental.calls,
                                 ["availability", "create", "terminate"])
                self.assertEqual(rental.restored, list(SIGNALS))
                self.assertTrue(rental.timer.started)
                self.assertTrue(rental.timer.cancelled)

    def test_valid_root_receipt_retains_normal_cutoff(self):
        # Given root proof arriving at the early deadline,
        # when the owner wakes, then the original hard cutoff applies.
        with tempfile.TemporaryDirectory() as temporary:
            rental = self.fallback(Path(temporary).resolve(), 299)
            rental.root_receipt = json.dumps({
                "root": "/sys/fs/cgroup", "writable": True})
            rental.root_receipt_after = 299
            rental.run()
            self.assertEqual(rental.terminate_times,
                             [rental.config["cutoff_seconds"]])
            self.assertIn((299, 14), rental.polls)
            self.assertEqual(rental.restored, list(SIGNALS))

    def test_invalid_root_receipts_keep_early_cleanup_deadline(self):
        # Given malformed or structurally wrong root proof,
        # when the allowance expires, then do not extend the rental.
        invalid = (
            "{invalid", "[]", "{}",
            '{"root":"/sys/fs/cgroup","writable":1}',
            '{"root":"/sys/fs/cgroup","writable":false}',
            '{"root":"/another/root","writable":true}',
            '{"root":"/sys/fs/cgroup","writable":true,"extra":0}',
            '{"root":"/sys/fs/cgroup","writable":true}' + " " * 1024,
        )
        for raw in invalid:
            with self.subTest(raw=raw[:80]), \
                    tempfile.TemporaryDirectory() as temporary:
                rental = self.fallback(Path(temporary).resolve())
                rental.root_receipt = raw
                rental.run()
                self.assertEqual(rental.terminate_times, [300])
                self.assertEqual(rental.restored, list(SIGNALS))

    def test_connect_readiness_uses_probe_or_existing_ready_allowance(self):
        # Given a pending pod, when its readiness allowance expires,
        # then connect requests cleanup at the original deadline.
        for mode, expected in (("first-in-pod-check", 299),
                               ("prior-evidence", 900)):
            with self.subTest(mode=mode), \
                    tempfile.TemporaryDirectory() as temporary:
                state = Path(temporary)
                clock = Clock()
                config = execution_fixture(clock.time())
                config["cgroup_root"] = {"mode": mode}
                if mode == "first-in-pod-check":
                    config["cgroup_root"]["probe_seconds"] = expected
                (state / "config.json").write_text(json.dumps(config))
                (state / "session.json").write_text(json.dumps({
                    "pod_id": "pod", "started_epoch": clock.epoch}))
                (state / "validated.json").write_text("{}")
                status = {"id": "pod", "provider": "nebius",
                          "image": "image", "gpu": "CPU_NODE x1",
                          "cost_per_hour": "0.264", "status": "PENDING"}
                admission = SimpleNamespace(
                    root_probe_seconds=probe_seconds_fixture)
                with patch.dict(sys.modules, {"admission": admission}), \
                        patch.object(lifecycle.time, "time", clock.time), \
                        patch.object(lifecycle.threading, "Event",
                                     clock.event), \
                        patch.object(lifecycle, "status_read",
                                     return_value=status) as read, \
                        patch.object(lifecycle, "request_cleanup") as stop:
                    with self.assertRaisesRegex(ValueError, "readiness"):
                        lifecycle.connect(state)
                self.assertEqual(clock.elapsed, expected)
                self.assertEqual(sum(clock.waits), expected)
                self.assertEqual(read.call_count,
                                 20 if expected == 299 else 60)
                stop.assert_called_once()


if __name__ == "__main__":
    unittest.main()
