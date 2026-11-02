"""C2 lifecycle regressions. Fakes replace every account operation."""
from contextlib import ExitStack
from copy import deepcopy
from datetime import datetime, timezone
from io import StringIO
import errno
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

import artifacts
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
        self.ssh_receipt = None
        self.ssh_receipt_after = 0
        self.root_receipt = None
        self.root_receipt_after = 0
        self.calls = []
        self.reconciliations = []
        self.reconcile_errors = []
        self.polls = []
        self.handlers = {}
        self.old_handlers = {number: object() for number in SIGNALS}
        self.restored = []
        self.delivered = []
        self.terminate_times = []
        self.verifications = []
        self.verify_error = None
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
            if self.reconcile_errors:
                raise self.reconcile_errors.pop(0)
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
        contact_path = self.state / "ssh-contact.json"
        if (self.ssh_receipt is not None and not contact_path.exists()
                and self.clock.elapsed >= self.ssh_receipt_after):
            contact_path.write_text(self.ssh_receipt)
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
        if self.verify_error is not None:
            raise self.verify_error

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

    def test_reconciliation_receipt_survives_broken_stderr(self):
        # Given an unknown-identity read error and broken stderr after
        # HUP, when identity appears at 15 s, then delete once.
        for diagnostic_error in (BrokenPipeError(errno.EPIPE, "closed pipe"),
                                 OSError(errno.EIO, "terminal lost")):
            with self.subTest(error=repr(diagnostic_error)), \
                    tempfile.TemporaryDirectory() as temporary:
                rental = Rental(self, Path(temporary).resolve())
                rental.identity_after = 15
                rental.stop_after = 0
                primary = subprocess.TimeoutExpired(["create"], 90)
                rental.create_error = primary
                read_error = subprocess.CalledProcessError(1, ["list"])
                rental.reconcile_errors = [read_error]

                def broken_stderr(_):
                    # Save the original failure before output.
                    receipts = list(rental.state.glob("owner-error-*.json"))
                    self.assertEqual(len(receipts), 1)
                    receipt = json.loads(receipts[0].read_text())
                    self.assertEqual(receipt, {
                        "error": repr(read_error),
                        "reason": "host-signal\n" * len(SIGNALS),
                        "observed_epoch": rental.clock.epoch,
                        "monotonic": 1000.0})
                    raise diagnostic_error

                stderr = SimpleNamespace(write=broken_stderr)
                with patch.object(lifecycle.sys, "stderr", stderr):
                    with self.assertRaises(subprocess.TimeoutExpired) as got:
                        rental.run()
                self.assertIs(got.exception, primary)
                self.assertEqual(rental.reconciliations, [0, 15])
                self.assertEqual(rental.terminate_times, [15])
                self.assertEqual(rental.calls,
                                 ["availability", "create", "terminate"])
                self.assertEqual(rental.restored, list(SIGNALS))
                self.assertTrue(rental.timer.cancelled)
                self.assertTrue(rental.timer.joined)
                self.assertIn("Cleanup observation: " + repr(read_error),
                              primary.__notes__)
                self.assertIn("Owner stderr failed: " + repr(diagnostic_error),
                              primary.__notes__)

    def test_journal_failure_keeps_owner_and_exception_notes(self):
        # Given failed journaling and stderr, when a read recovers,
        # then retain both original notes and finish the one cleanup.
        with tempfile.TemporaryDirectory() as temporary:
            rental = Rental(self, Path(temporary).resolve())
            rental.identity_after = 15
            primary = subprocess.TimeoutExpired(["create"], 90)
            primary.add_note("original create note")
            rental.create_error = primary
            read_error = subprocess.CalledProcessError(1, ["list"])
            read_error.add_note("original read note")
            rental.reconcile_errors = [read_error]
            journal_error = OSError(errno.EIO, "state journal unavailable")
            stderr_error = BrokenPipeError(errno.EPIPE, "closed pipe")
            original_save = lifecycle.save
            failed_journals = []

            def save(path, value):
                if path.name.startswith("owner-error-"):
                    failed_journals.append(value)
                    raise journal_error
                original_save(path, value)

            with patch.object(lifecycle, "save", side_effect=save), \
                    patch.object(lifecycle.sys, "stderr") as stderr:
                stderr.write.side_effect = stderr_error
                with self.assertRaises(subprocess.TimeoutExpired) as got:
                    rental.run()
            self.assertIs(got.exception, primary)
            self.assertEqual(rental.reconciliations, [0, 15])
            self.assertEqual(rental.terminate_times, [15])
            self.assertEqual(rental.restored, list(SIGNALS))
            self.assertTrue(rental.timer.joined)
            self.assertEqual(len(failed_journals), 1)
            self.assertEqual(failed_journals[0]["error"], repr(read_error))
            self.assertEqual(list(rental.state.glob("owner-error-*.json")), [])
            self.assertIn("original create note", primary.__notes__)
            self.assertIn("original read note", primary.__notes__)
            self.assertIn("Cleanup observation: " + repr(read_error),
                          primary.__notes__)
            for note in ("Owner error journal failed: " + repr(journal_error),
                         "Owner stderr failed: " + repr(stderr_error)):
                self.assertIn(note, read_error.__notes__)
                self.assertIn(note, primary.__notes__)

    def test_cutoff_error_survives_broken_stderr(self):
        # Given cutoff proof failure and broken stderr, when owner and
        # timer race, then retain the error without a second mutation.
        with tempfile.TemporaryDirectory() as temporary:
            rental = Rental(self, Path(temporary).resolve())
            rental.stop_after = 15
            rental.race_cutoff = True
            primary = ValueError("deletion proof unavailable")
            rental.verify_error = primary
            stderr_error = OSError(errno.EIO, "terminal lost")
            with patch.object(lifecycle.sys, "stderr") as stderr:
                stderr.write.side_effect = stderr_error
                with self.assertRaises(ValueError) as got:
                    rental.run()
            self.assertIs(got.exception, primary)
            self.assertEqual(rental.terminate_times, [15])
            self.assertEqual(rental.restored, list(SIGNALS))
            self.assertTrue(rental.timer.joined)
            receipts = [json.loads(path.read_text()) for path in
                        rental.state.glob("owner-error-*.json")]
            self.assertEqual(receipts, [{
                "error": repr(primary), "reason": "hard-cutoff",
                "observed_epoch": rental.clock.epoch + 15,
                "monotonic": 1015.0}])
            self.assertIn("Owner stderr failed: " + repr(stderr_error),
                          primary.__notes__)

    def test_broken_rented_stdout_requests_cleanup(self):
        # Given validated create and broken RENTED stdout, when print
        # fails, then delete and restore before raising the same error.
        with tempfile.TemporaryDirectory() as temporary:
            rental = Rental(self, Path(temporary).resolve())
            primary = BrokenPipeError(errno.EPIPE, "rental output closed")
            with patch.object(lifecycle.sys, "stdout") as stdout:
                stdout.write.side_effect = primary
                with self.assertRaises(BrokenPipeError) as got:
                    rental.run()
            self.assertIs(got.exception, primary)
            self.assertTrue((rental.state / "validated.json").exists())
            self.assertEqual(json.loads(
                (rental.state / "rental-error.json").read_text()),
                {"error": repr(primary)})
            self.assertEqual(rental.terminate_times, [0])
            self.assertEqual(rental.reconciliations, [])
            self.assertEqual(rental.calls,
                             ["availability", "create", "terminate"])
            self.assertEqual(rental.restored, list(SIGNALS))
            self.assertTrue(rental.timer.cancelled)
            self.assertTrue(rental.timer.joined)

    def test_unidentified_failure_keeps_owner_past_cutoff(self):
        # Given permanent read/identity failure, when cutoff passes,
        # then retain every error without releasing ownership.
        for failed_reads in (False, True):
            with self.subTest(failed_reads=failed_reads), \
                    tempfile.TemporaryDirectory() as temporary:
                rental = Rental(self, Path(temporary).resolve())
                rental.identity_after = float("inf")
                rental.create_error = subprocess.TimeoutExpired(["create"], 90)
                rental.abort_after = rental.config["cutoff_seconds"] + 15
                failures = [subprocess.CalledProcessError(index, ["list"])
                            for index in range(1, 7)] if failed_reads else []
                rental.reconcile_errors = list(failures)
                try:
                    with patch.object(lifecycle.sys, "stderr",
                                      StringIO()) as log:
                        with self.assertRaises(OwnerObserved):
                            rental.run()
                    self.assertIn("DELETION UNPROVED", log.getvalue())
                    receipts = [json.loads(path.read_text()) for path in
                                rental.state.glob("owner-error-*.json")]
                    receipts.sort(key=lambda row: row["monotonic"])
                    self.assertEqual(len(receipts),
                                     len(rental.reconciliations))
                    if failed_reads:
                        self.assertEqual([row["error"] for row in receipts],
                                         [repr(error) for error in failures])
                    else:
                        self.assertIn("owner retained; escalate",
                                      log.getvalue())
                    self.assertEqual(
                        [row["observed_epoch"] for row in receipts],
                        [rental.clock.epoch + elapsed
                         for elapsed in rental.reconciliations])
                    self.assertEqual([row["monotonic"] for row in receipts],
                                     [1000 + elapsed
                                      for elapsed in rental.reconciliations])
                    self.assertEqual({row["reason"] for row in receipts},
                                     {"owner-refusal-or-cutoff"})
                    self.assertEqual(rental.reconcile_errors, [])
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
    def test_evidence_bound_uses_artifact_constants(self):
        # Given archive bounds, when evidence meets their sum,
        # then accept it and refuse one byte above the sum.
        bounds = ((artifacts.MAX_PAYLOAD, artifacts.TAR_OVERHEAD),
                  (17, 5), (31, 11))
        for payload, overhead in bounds:
            with self.subTest(payload=payload, overhead=overhead), \
                    patch.object(artifacts, "MAX_PAYLOAD", payload), \
                    patch.object(artifacts, "TAR_OVERHEAD", overhead):
                for extra, expected in ((0, dict), (1, lifecycle.Invalid)):
                    with self.subTest(extra=extra):
                        config = {**execution_fixture(100),
                                  "expected_evidence_bytes":
                                  payload + overhead + extra}
                        before = deepcopy(config)
                        checked = lifecycle.execution_pins(config, 100)
                        self.assertIsInstance(checked, expected)
                        self.assertEqual(config, before)
                        if extra == 0:
                            self.assertEqual(checked, config)

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

    def first_contact(self, rental, seconds):
        rental.ssh_receipt = json.dumps({
            "connected_epoch": rental.clock.epoch + seconds,
            "connected_monotonic": 1000.0 + seconds})
        rental.ssh_receipt_after = seconds

    def test_missing_host_contact_stops_by_readiness_deadline(self):
        # Given no SSH contact, when readiness expires at 900 s,
        # then delete once without granting an idle study allowance.
        for seconds in (299, 300):
            with self.subTest(seconds=seconds), \
                    tempfile.TemporaryDirectory() as temporary:
                rental = self.fallback(Path(temporary).resolve(), seconds)
                rental.run()
                self.assertEqual(rental.terminate_times, [900])
                self.assertEqual(rental.calls,
                                 ["availability", "create", "terminate"])
                self.assertEqual(rental.restored, list(SIGNALS))
                self.assertTrue(rental.timer.started)
                self.assertTrue(rental.timer.cancelled)
                owner = json.loads((rental.state / "owner.json").read_text())
                self.assertEqual(owner["started_epoch"], rental.clock.epoch)
                self.assertEqual(owner["started_monotonic"], 1000.0)
                self.assertEqual(owner["cutoff_epoch"], rental.clock.epoch
                                 + rental.config["cutoff_seconds"])

    def test_first_contact_starts_full_probe_window(self):
        # Given first contact at 0/320/899/900 s, when proof is absent,
        # then grant the full probe window from those original clocks.
        for connected, expected in ((0, 300), (320, 620),
                                    (899, 1199), (900, 1200)):
            with self.subTest(connected=connected), \
                    tempfile.TemporaryDirectory() as temporary:
                rental = self.fallback(Path(temporary).resolve())
                self.first_contact(rental, connected)
                rental.run()
                self.assertEqual(rental.terminate_times, [expected])
                self.assertEqual(rental.calls,
                                 ["availability", "create", "terminate"])
                self.assertEqual(rental.restored, list(SIGNALS))
                owner = json.loads((rental.state / "owner.json").read_text())
                self.assertEqual(lifecycle.first_ssh_contact(
                    rental.state, rental.config, owner),
                    (rental.clock.epoch + connected, 1000.0 + connected))
                raw = (rental.state / "ssh-contact.json").read_text()
                self.assertEqual(raw, rental.ssh_receipt)

    def test_root_receipt_at_contact_deadline_retains_original_cutoff(self):
        # Given root proof arriving at the contact-based deadline,
        # when select returns, then retain only the original cutoff.
        for connected, probe in ((0, 299), (0, 300), (320, 300),
                                  (899, 300), (900, 300)):
            with self.subTest(connected=connected, probe=probe), \
                    tempfile.TemporaryDirectory() as temporary:
                rental = self.fallback(Path(temporary).resolve(), probe)
                self.first_contact(rental, connected)
                rental.root_receipt = json.dumps({
                    "root": "/sys/fs/cgroup", "writable": True})
                rental.root_receipt_after = connected + probe
                rental.run()
                self.assertEqual(rental.terminate_times,
                                 [rental.config["cutoff_seconds"]])
                self.assertIn(connected + probe,
                              [elapsed for elapsed, _ in rental.polls])
                self.assertEqual(rental.restored, list(SIGNALS))

    def test_each_contact_clock_bounds_owner_cleanup(self):
        # Given differing contact clocks, when either deadline expires,
        # then the other clock cannot extend ownership.
        for epoch_offset, monotonic_offset in ((320, 400), (400, 320)):
            with self.subTest(epoch_offset=epoch_offset), \
                    tempfile.TemporaryDirectory() as temporary:
                rental = self.fallback(Path(temporary).resolve())
                rental.ssh_receipt = json.dumps({
                    "connected_epoch": rental.clock.epoch + epoch_offset,
                    "connected_monotonic": 1000.0 + monotonic_offset})
                rental.ssh_receipt_after = 400
                rental.run()
                self.assertEqual(rental.terminate_times, [620])

    def test_contact_deadlines_cannot_extend_original_cutoffs(self):
        # Given a probe exceeding the hard cutoff in mocked admission,
        # when contact is valid, then both clocks keep the hard bound.
        with tempfile.TemporaryDirectory() as temporary:
            rental = self.fallback(Path(temporary).resolve(), 20000)
            self.first_contact(rental, 320)
            rental.run()
            self.assertEqual(rental.terminate_times,
                             [rental.config["cutoff_seconds"]])

    def test_invalid_contacts_keep_readiness_deadline(self):
        # Given malformed, aliased, oversized or out-of-range contact,
        # when readiness expires, then no unproved clock extends it.
        clock = Clock()
        valid = {"connected_epoch": clock.epoch + 320,
                 "connected_monotonic": 1320.0}
        invalid = ["{invalid", "[]", "{}", json.dumps({
            "epoch": valid["connected_epoch"],
            "monotonic": valid["connected_monotonic"]}),
            json.dumps({**valid, "extra": 0}),
            json.dumps(valid) + " " * 1024,
            '{"connected_epoch":0,"connected_epoch":'
            + str(valid["connected_epoch"]) + ',"connected_monotonic":1320}',
            '{"connected_epoch":' + str(valid["connected_epoch"])
            + ',"connected_monotonic":0,"connected_monotonic":1320}']
        for name, start in (("connected_epoch", clock.epoch),
                            ("connected_monotonic", 1000.0)):
            for value in (True, False, None, "320", [], {}, float("nan"),
                          float("inf"), -float("inf"), 10 ** 400,
                          start - 1, start + 901):
                invalid.append(json.dumps({**valid, name: value}))
        for raw in invalid:
            with self.subTest(raw=raw[:80]), \
                    tempfile.TemporaryDirectory() as temporary:
                rental = self.fallback(Path(temporary).resolve())
                rental.ssh_receipt = raw
                rental.run()
                self.assertEqual(rental.terminate_times, [900])
                self.assertEqual(rental.restored, list(SIGNALS))
                owner = json.loads((rental.state / "owner.json").read_text())
                self.assertIsNone(lifecycle.first_ssh_contact(
                    rental.state, rental.config, owner))

    def test_contact_byte_bound_accepts_edge_and_rejects_missing_io(self):
        # Given a capped receipt, when its size is exactly 1 KiB,
        # then accept it; one extra byte or missing I/O stays unproved.
        with tempfile.TemporaryDirectory() as temporary:
            state = Path(temporary)
            owner = {"started_epoch": 10, "started_monotonic": 20}
            config = {"ready_seconds": 900}
            receipt = state / "ssh-contact.json"
            self.assertIsNone(lifecycle.first_ssh_contact(
                state, config, owner))
            raw = json.dumps({"connected_epoch": 910,
                              "connected_monotonic": 920})
            receipt.write_text(raw + " " * (1024 - len(raw)))
            self.assertEqual(lifecycle.first_ssh_contact(state, config, owner),
                             (910.0, 920.0))
            receipt.write_text(raw + " " * (1025 - len(raw)))
            self.assertIsNone(lifecycle.first_ssh_contact(
                state, config, owner))
            receipt.unlink()
            receipt.mkdir()
            self.assertIsNone(lifecycle.first_ssh_contact(
                state, config, owner))

    def test_invalid_root_receipts_keep_contact_probe_deadline(self):
        # Given valid contact and malformed or structurally wrong root
        # proof, when the probe expires, then do not extend the rental.
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
                self.first_contact(rental, 320)
                rental.root_receipt = raw
                rental.run()
                self.assertEqual(rental.terminate_times, [620])
                self.assertEqual(rental.restored, list(SIGNALS))

    def test_connect_pending_expires_at_readiness_in_both_modes(self):
        # Given a pending pod in either root mode, when 900 s passes,
        # then connect requests cleanup at the readiness deadline.
        for mode in ("first-in-pod-check", "prior-evidence"):
            with self.subTest(mode=mode), \
                    tempfile.TemporaryDirectory() as temporary:
                state = Path(temporary)
                clock = Clock()
                config = execution_fixture(clock.time())
                config["cgroup_root"] = {"mode": mode, "probe_seconds": 300}
                (state / "config.json").write_text(json.dumps(config))
                (state / "session.json").write_text(json.dumps({
                    "pod_id": "pod", "started_epoch": clock.epoch}))
                (state / "validated.json").write_text("{}")
                status = {"id": "pod", "provider": "nebius",
                          "image": "image", "gpu": "CPU_NODE x1",
                          "cost_per_hour": "0.264", "status": "PENDING"}
                with patch.object(lifecycle.time, "time", clock.time), \
                        patch.object(lifecycle.threading, "Event",
                                     clock.event), \
                        patch.object(lifecycle, "status_read",
                                     return_value=status) as read, \
                        patch.object(lifecycle, "request_cleanup") as stop:
                    with self.assertRaisesRegex(ValueError, "readiness"):
                        lifecycle.connect(state)
                self.assertEqual(clock.elapsed, 900)
                self.assertEqual(sum(clock.waits), 900)
                self.assertEqual(read.call_count, 60)
                stop.assert_called_once()

    def test_connect_active_after_320_accepts_valid_ssh_in_both_modes(self):
        # Given slow boot and valid ACTIVE SSH metadata at 320 s,
        # when connect reads it, then preserve the 900 s ready window.
        for mode in ("first-in-pod-check", "prior-evidence"):
            with self.subTest(mode=mode), \
                    tempfile.TemporaryDirectory() as temporary:
                state = Path(temporary)
                clock = Clock()
                clock.elapsed = 320
                config = execution_fixture(clock.time())
                config["cgroup_root"] = {"mode": mode, "probe_seconds": 300}
                (state / "config.json").write_text(json.dumps(config))
                (state / "session.json").write_text(json.dumps({
                    "pod_id": "pod", "started_epoch": clock.epoch}))
                (state / "validated.json").write_text("{}")
                status = {"id": "pod", "provider": "nebius",
                          "image": "image", "gpu": "CPU_NODE x1",
                          "cost_per_hour": "0.264", "status": "ACTIVE",
                          "ssh": "root@127.0.0.1 -p 22"}
                with patch.object(lifecycle.time, "time", clock.time), \
                        patch.object(lifecycle, "status_read",
                                     return_value=status) as read, \
                        patch.object(lifecycle, "request_cleanup") as stop:
                    lifecycle.connect(state)
                self.assertEqual(clock.elapsed, 320)
                self.assertEqual(clock.waits, [])
                read.assert_called_once()
                stop.assert_not_called()
                self.assertEqual(json.loads(
                    (state / "connection.json").read_text()),
                    {"target": "root@127.0.0.1", "port": "22"})
                inputs = json.loads(
                    (state / "session-inputs.json").read_text())
                self.assertEqual(inputs["started_epoch"], clock.epoch)
                self.assertEqual(inputs["cutoff_epoch"], clock.epoch + 10680)


if __name__ == "__main__":
    unittest.main()
