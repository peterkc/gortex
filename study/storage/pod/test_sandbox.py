"""Sandbox controller scenarios. All remote effects use local fakes."""
from copy import deepcopy
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import threading
import unittest
from unittest import mock

import sandbox as driver
from test_sandbox_policy import NOW, details, pins


def result(code=0, stdout=""):
    return subprocess.CompletedProcess(["synthetic-prime"], code), stdout


class OwnerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.state = self.root / "state"
        self.destination = self.root / "results"
        self.state.mkdir(mode=0o700)
        self.destination.mkdir(mode=0o700)
        self.config = {**pins(), "destination": str(self.destination),
                       "cli_executable": "/synthetic/prime",
                       "upload_seconds": 90,
                       "support_sha256": {name: "e" * 64 for name in
                                          ("cgroup_setup.py", "artifacts.py",
                                           "preflight.py")}}
        self.owner = driver.Owner(self.config, self.state)
        self.addCleanup(self.owner.close)
        self.owner.name, self.owner.label = "owned-name", "owned-label"
        self.commands = []

    def owned(self):
        self.owner.lease = driver.Identified("sandbox_123", "team-approved")

    def invoke(self, arguments, *args, **kwargs):
        self.commands.append(arguments)
        if arguments[0] == "delete":
            return result(0, "Successfully deleted sandbox sandbox_123\n")
        return result(0, json.dumps(details(status="TERMINATED")))

    def test_wrong_team_is_refused_for_use_but_owned_cleanup_deletes(self):
        # Wrong billing refuses use, not deletion of a proved-owned ID.
        self.owner.lease = driver.Unidentified("sandbox_123")
        self.owner.get = mock.Mock(return_value=details(team_id="wrong-team"))
        doc = self.owner.reconcile()
        self.assertIn("team_id", driver.policy.mismatches(doc, self.config))
        self.assertTrue(self.owner.confirmed)
        self.owner.invoke = self.invoke
        self.assertTrue(self.owner.cleanup("billing-mismatch"))
        self.assertEqual([c for c in self.commands if c[0] == "delete"],
                         [["delete", "sandbox_123", "--yes"]])

    def test_concurrent_cutoff_signal_and_exception_delete_once(self):
        self.owned()
        self.owner.invoke = self.invoke
        threads = [threading.Thread(target=self.owner.cleanup, args=(reason,))
                   for reason in ("cutoff", "signal", "exception")]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        self.assertTrue(self.owner.deleted.is_set())
        self.assertEqual(sum(c[0] == "delete" for c in self.commands), 1)

    def test_delete_ack_loss_and_logging_failure_never_replay_delete(self):
        self.owned()
        delete_timeouts = []

        def invoke(arguments, seconds=30, *args, **kwargs):
            self.commands.append(arguments)
            if arguments[0] == "delete":
                delete_timeouts.append(seconds)
                raise subprocess.TimeoutExpired(arguments, seconds)
            return result(0, json.dumps(details(status="TERMINATED")))
        self.owner.invoke = invoke
        with mock.patch.object(
                driver, "save", side_effect=OSError("disk full")), \
                mock.patch.object(self.owner, "record_error"):
            self.assertFalse(self.owner.cleanup("cutoff"))
        self.assertTrue(self.owner.cleanup("verify-only"))
        self.assertEqual(sum(c[0] == "delete" for c in self.commands), 1)
        self.assertEqual(delete_timeouts, [120])

    def test_unknown_or_mismatched_identity_never_deletes_or_claims_done(self):
        self.owner.lease = driver.Unidentified()
        self.owner.scoped_list = mock.Mock(return_value=[])
        self.owner.invoke = mock.Mock()
        self.assertFalse(self.owner.cleanup("unknown-create"))
        self.owner.invoke.assert_not_called()
        self.assertFalse(self.owner.deleted.is_set())
        self.owner.lease = driver.Unidentified("sandbox_123")
        self.owner.get = mock.Mock(return_value=details(name="unrelated"))
        self.assertFalse(self.owner.cleanup("wrong-name"))
        self.owner.invoke.assert_not_called()

    def test_unknown_create_reconciles_owned_identity_then_deletes(self):
        self.owner.lease = driver.Unidentified()
        self.owner.scoped_list = mock.Mock(return_value=[details()])
        self.owner.get = mock.Mock(return_value=details())
        self.owner.invoke = self.invoke
        self.assertTrue(self.owner.cleanup("lost-create-ack"))
        self.assertEqual([c[0] for c in self.commands], ["delete", "get"])
        self.assertFalse(any(c[0] == "create" for c in self.commands))

    def test_create_is_once_and_malformed_ack_never_erases_confirmed_id(self):
        self.owner.timer = mock.Mock(return_value=mock.Mock())
        self.owner.get = mock.Mock(return_value=details())
        self.owner.invoke = mock.Mock(return_value=result(
            0, "Successfully created sandbox sandbox_123\n"))
        self.owner.create()
        with self.assertRaises(ValueError):
            self.owner.create()
        self.assertEqual(self.owner.invoke.call_count, 1)
        self.owner.adopt_created_id("no trustworthy identity")
        self.assertEqual(self.owner.sandbox_id, "sandbox_123")
        with self.assertRaises(ValueError):
            self.owner.adopt_created_id(
                "Successfully created sandbox sandbox_456\n")
        self.assertEqual(self.owner.sandbox_id, "sandbox_123")

    def test_nonterminal_and_404_deletion_proofs(self):
        self.owned()
        for status in ("ERROR", "PAUSED", "RUNNING"):
            self.owner.invoke = mock.Mock(return_value=result(
                0, json.dumps(details(status=status))))
            self.assertFalse(self.owner.stopped())
        for stdout in ("not found", "Unauthorized: no access",
                       "Error: HTTP 500: down"):
            self.owner.invoke = mock.Mock(return_value=result(1, stdout))
            self.assertFalse(self.owner.stopped())
        self.owner.invoke = mock.Mock(return_value=result(
            1, "Error: HTTP 404: missing"))
        self.owner.scoped_list = mock.Mock(return_value=[details()])
        self.assertFalse(self.owner.stopped())
        self.owner.scoped_list.return_value = []
        self.assertTrue(self.owner.stopped())
        self.owner.scoped_list.assert_called_with("team-approved")
        self.owner.lease = driver.Identified("sandbox_123", None)
        self.assertFalse(self.owner.stopped())

    def test_first_exec_deadline_is_original_and_cutoff_does_not_extend(self):
        self.owner.ready_timer = mock.Mock()
        self.owner.timer = mock.Mock(return_value=mock.Mock())
        original = self.owner.monotonic
        with mock.patch.object(driver.time, "time", return_value=1000), \
                mock.patch.object(driver.time, "monotonic",
                                  return_value=original + 10):
            deadline = self.owner.first_exec()
        with mock.patch.object(driver.time, "monotonic",
                               return_value=original + 200):
            self.assertEqual(self.owner.first_exec(), deadline)
        self.owner.timer.assert_called_once_with(300, "root-check")
        self.owner.ready_timer.cancel.assert_called_once()
        self.assertEqual(self.owner.cutoff, original + 10680)
        with mock.patch.object(driver.time, "monotonic",
                               return_value=self.owner.cutoff):
            with self.assertRaises(TimeoutError):
                self.owner.ensure_work(self.owner.cutoff + 999)

    def test_late_contact_or_cancellation_cannot_start_root_window(self):
        self.owner.ready_timer = mock.Mock()
        self.owner.timer = mock.Mock()
        with mock.patch.object(driver.time, "monotonic",
                               return_value=self.owner.monotonic + 900):
            with self.assertRaises(TimeoutError):
                self.owner.first_exec()
        self.assertIsNone(self.owner.first_contact)
        self.owner.cancelled.set()
        with self.assertRaises(TimeoutError):
            self.owner.remote("must not run", 30, self.owner.cutoff)
        self.owner.timer.assert_not_called()

    def test_root_refusal_prevents_payload_and_launch(self):
        self.owner.reconcile = mock.Mock(
            return_value=details(status="RUNNING"))
        self.owner.remote = mock.Mock()
        self.owner.ready_timer = mock.Mock()
        self.owner.timer = mock.Mock(return_value=mock.Mock())
        self.owner.upload = mock.Mock()
        self.owner.guest_json = mock.Mock(return_value={"euid": 1})
        with mock.patch.object(driver.host_run, "stage_python",
                               return_value=self.root / "python.tar.gz"):
            with self.assertRaisesRegex(ValueError, "actual root"):
                self.owner.prepare()
        self.assertEqual([c.args[1] for c in self.owner.upload.call_args_list],
                         ["cgroup_setup.py", "python.tar.gz"])
        self.assertFalse((self.state / "launch-requested.json").exists())

    def test_changed_upload_and_reused_download_refuse(self):
        source = self.root / "input"
        source.write_bytes(b"changed")
        self.owner.invoke = mock.Mock()
        with mock.patch.object(driver, "checked_checkout"):
            with self.assertRaisesRegex(ValueError, "approved hash"):
                self.owner.upload(source, "env.sh", self.owner.cutoff,
                                  "a" * 64)
        with self.assertRaises(ValueError):
            self.owner.download("/remote", source, 30, self.owner.cutoff)
        self.owner.invoke.assert_not_called()

    def test_guest_json_uses_downloaded_file_not_console_wrapped_json(self):
        self.owner.remote = mock.Mock()
        def download(remote, local, *args):
            local.write_text('{"actual": true}')
        self.owner.download = download
        self.assertEqual(
            self.owner.guest_json("trusted source", self.owner.cutoff),
                         {"actual": True})
        script = self.owner.remote.call_args.args[0]
        self.assertIn(" > receipt-", script)
        self.assertIn("-B -c", script)

    def bundle(self, scope="full-evidence"):
        tree = self.root / ("tree-" + scope)
        evidence = tree / "evidence"
        evidence.mkdir(parents=True)
        body = b"public fixture"
        (evidence / "answer.txt").write_bytes(body)
        (evidence / "file-manifest.json").write_text(json.dumps({
            "scope": scope, "files": {
                "answer.txt": hashlib.sha256(body).hexdigest()}}))
        archive = self.root / (scope + ".tar")
        with tarfile.open(archive, "w") as stream:
            for path in sorted(evidence.iterdir()):
                stream.add(path, arcname="evidence/" + path.name)
        return archive

    def test_copy_checks_size_hash_scope_then_publishes_verified_archive(self):
        archive = self.bundle()
        self.owner.remote = mock.Mock()
        def download(remote, local, *args):
            if remote.endswith(".sha256"):
                local.write_text(driver.artifacts.digest(archive)
                                 + "  evidence.tar\n")
            else:
                local.write_bytes(archive.read_bytes())
        self.owner.download = download
        with mock.patch.object(driver, "checked_checkout"):
            self.owner.copy("evidence.tar", "full-evidence", self.owner.cutoff,
                            archive.stat().st_size)
        self.assertTrue((self.destination / "evidence.tar").is_file())
        answer = self.destination / "verified-evidence/evidence/answer.txt"
        self.assertTrue(answer.is_file())
        with mock.patch.object(driver, "checked_checkout"):
            with self.assertRaises(ValueError):
                self.owner.copy("evidence.tar", "full-evidence",
                                self.owner.cutoff,
                                archive.stat().st_size + 1)

    def test_copy_download_uses_remaining_window_not_fixed_300(self):
        self.owner.remote = mock.Mock()
        started = self.owner.monotonic
        cases = (("evidence.tar", "full-evidence", 420, 1000, 410),
                 ("receipts.tar", "diagnostics-only", 60, 1000, 50),
                 ("partial.tar", "partial-evidence", 420, 200, 190))
        for name, scope, window, cutoff, remaining in cases:
            with self.subTest(scope=scope):
                archive = self.bundle(scope)
                self.owner.cutoff = started + cutoff
                downloads = []

                def download(remote, local, seconds, deadline):
                    downloads.append((seconds, deadline))
                    if remote.endswith(".sha256"):
                        local.write_text(driver.artifacts.digest(archive)
                                         + "  " + name + "\n")
                    else:
                        local.write_bytes(archive.read_bytes())

                self.owner.download = download
                with mock.patch.object(driver, "checked_checkout"), \
                        mock.patch.object(driver.time, "monotonic",
                                          return_value=started + 10):
                    self.owner.copy(name, scope, started + window,
                                    archive.stat().st_size)
                self.assertEqual(downloads, [(remaining, started + window),
                                             (30, started + window)])

    def test_copy_rejects_hash_scope_and_destination_alias(self):
        archive = self.bundle("partial-evidence")
        self.owner.remote = mock.Mock()
        def download(remote, local, *args):
            if remote.endswith(".sha256"):
                local.write_text("bad hash")
            else:
                local.write_bytes(archive.read_bytes())
        self.owner.download = download
        with mock.patch.object(driver, "checked_checkout"):
            with self.assertRaisesRegex(ValueError, "hash"):
                self.owner.copy("evidence.tar", "partial-evidence",
                                self.owner.cutoff)
        self.assertFalse((self.destination / "evidence.tar").exists())
        def valid_download(remote, local, *args):
            if remote.endswith(".sha256"):
                local.write_text(driver.artifacts.digest(archive)
                                 + "  evidence.tar\n")
            else:
                local.write_bytes(archive.read_bytes())
        self.owner.download = valid_download
        with mock.patch.object(driver, "checked_checkout"):
            with self.assertRaisesRegex(ValueError, "scope"):
                self.owner.copy("evidence.tar", "full-evidence",
                                self.owner.cutoff)
        self.assertFalse((self.destination / "verified-evidence").exists())
        alias = self.root / "alias"
        alias.symlink_to(self.destination, target_is_directory=True)
        self.owner.config["destination"] = str(alias)
        with self.assertRaisesRegex(ValueError, "identity"):
            self.owner.copy("evidence.tar", "partial-evidence",
                            self.owner.cutoff)




    def test_remote_timeout_is_clamped_to_original_deadline(self):
        self.owner.lease = driver.Unidentified("sandbox_123")
        self.owner.invoke = mock.Mock(return_value=result())
        original = self.owner.monotonic
        with mock.patch.object(driver.time, "monotonic",
                               return_value=original + 290):
            self.owner.remote("read only", 30, original + 300)
        arguments = self.owner.invoke.call_args.args[0]
        self.assertEqual(arguments[arguments.index("--timeout") + 1], "10")
        with mock.patch.object(driver.time, "monotonic",
                               return_value=original + 300):
            with self.assertRaises(TimeoutError):
                self.owner.remote("must not run", 30, original + 300)
        self.assertEqual(self.owner.invoke.call_count, 1)

    def test_lost_launch_ack_preserves_intent_and_observes(self):
        self.owner.reconcile = mock.Mock(
            return_value=details(status="RUNNING"))
        self.owner.ready_timer = mock.Mock()
        self.owner.timer = mock.Mock(return_value=mock.Mock())
        self.owner.upload = mock.Mock()
        self.owner.remote = mock.Mock(side_effect=[None, None,
            subprocess.TimeoutExpired(["launch"], 30)])
        facts = {"root": "/sys/fs/cgroup", "writable": True,
                 "memory_controller": True, "euid": 0, "machine": "x86_64",
                 "os_id": "ubuntu", "os_version": "22.04",
                 "python_implementation": "CPython",
                 "python_version": "3.12.16",
                 "glibc_version": "2.35"}
        self.owner.guest_json = mock.Mock(return_value=facts)
        with mock.patch.object(driver.host_run, "stage_python",
                               return_value=self.root / "python.tar.gz"), \
                mock.patch.object(driver, "checked_checkout"):
            self.owner.prepare()
            intent = (self.state / "launch-requested.json").read_bytes()
            with self.assertRaisesRegex(ValueError, "observe only"):
                self.owner.prepare()
        self.assertEqual(self.owner.remote.call_count, 3)
        self.assertEqual(
            (self.state / "launch-requested.json").read_bytes(), intent)
        self.assertFalse(json.loads(intent)["relaunch_allowed"])
        completed = {"exit": 0, "scope": "full-evidence",
                     "bundle_bytes": 10240}
        self.owner.guest_json.return_value = {
            "completion_hex": json.dumps(completed).encode().hex()}
        self.owner.copy = mock.Mock(
            side_effect=[OSError("diagnostic failed"), None])
        self.assertEqual(self.owner.observe_and_copy(), 0)
        self.assertEqual([c.args[0] for c in self.owner.copy.call_args_list],
                         ["receipts.tar", "evidence.tar"])
        first, second = self.owner.copy.call_args_list
        self.assertGreater(second.args[2] - first.args[2], 300)
        self.assertEqual(self.owner.remote.call_count, 3)



    def test_image_receipts_count_logical_images_not_artifact_rows(self):
        rows = [{"id": identifier, "imageName": "ubuntu", "imageTag": "pinned",
                 "ownerType": "team", "teamId": "team-approved",
                 "artifactType": kind} for identifier, kind in
                (("container_123", "container"), ("vm_123", "vm"))]
        document = {"data": rows, "totalCount": 1, "offset": 0, "limit": 250}
        self.owner.invoke = mock.Mock(
            return_value=result(0, json.dumps(document)))
        self.assertEqual(self.owner.image_receipt("before"), rows)
        self.assertEqual(self.owner.invoke.call_args.kwargs["namespace"],
                         "images")
        self.assertTrue((self.destination / "images-before.json").is_file())
        self.owner.invoke.return_value = result(0, json.dumps({**document,
            "data": rows + [rows[0]]}))
        with self.assertRaisesRegex(ValueError, "duplicate"):
            self.owner.image_receipt("bad")

    def test_new_team_image_is_reported_for_manual_review_never_deleted(self):
        old = {"id": "old_123", "teamId": "team-approved"}
        new = {"id": "new_123", "teamId": "team-approved"}
        unrelated = {"id": "unrelated_123", "teamId": "other-team"}
        self.owner.invoke = mock.Mock()
        self.owner.image_followup([old], [old, new, unrelated])
        receipt = json.loads(
            (self.destination / "image-cleanup-review.json").read_text())
        self.assertEqual(receipt["new_team_owned_rows"], [new])
        self.assertEqual(receipt["new_rows"], [new, unrelated])
        self.assertFalse(receipt["images_auto_deleted"])
        self.owner.invoke.assert_not_called()



    def test_pre_spawn_create_refusal_retains_no_paid_effect(self):
        self.owner.timer = mock.Mock(return_value=mock.Mock())
        self.owner.invoke = mock.Mock(
            side_effect=driver.CreateRefused("cancelled"))
        with self.assertRaises(driver.CreateRefused):
            self.owner.create()
        self.assertIsInstance(self.owner.lease, driver.NotCreated)
        self.assertTrue(self.owner.cleanup("cancelled-before-spawn"))
        self.assertFalse(self.owner.delete_attempted)
        self.assertEqual(self.owner.invoke.call_count, 1)

    def test_invoke_carries_deadline_and_cancel_pipe(self):
        def call(config, state, label, arguments, seconds, **kwargs):
            (state / (label + ".stdout")).write_text("")
            self.assertEqual(kwargs["not_after"], self.owner.cutoff)
            self.assertEqual(kwargs["cancel_descriptor"],
                             self.owner.cancel_read)
            return subprocess.CompletedProcess(arguments, 0)
        with mock.patch.object(driver, "call", side_effect=call):
            self.owner.invoke(["run", "sandbox_123"],
                              deadline=self.owner.cutoff)



    def test_warning_journal_failure_cannot_veto_valid_bulk_copy(self):
        completed = {"exit": 0, "scope": "full-evidence",
                     "bundle_bytes": 10240}
        (self.state / "launch-requested.json").write_text(json.dumps({
            "requested_epoch": self.owner.started, "relaunch_allowed": False}))
        self.owner.guest_json = mock.Mock(return_value={
            "completion_hex": json.dumps(completed).encode().hex()})
        self.owner.copy = mock.Mock()
        original = driver.save
        def save(path, value):
            if path.name == "download-forecast-exceeded.json":
                raise OSError("warning receipt unavailable")
            original(path, value)
        with mock.patch.object(driver, "save", side_effect=save), \
                mock.patch.object(driver.admission, "upload_seconds",
                                  return_value=99999):
            self.assertEqual(self.owner.observe_and_copy(), 0)
        self.assertEqual([c.args[0] for c in self.owner.copy.call_args_list],
                         ["receipts.tar", "evidence.tar"])



    def test_reconcile_cannot_erase_an_inflight_delete_intent(self):
        self.owned()
        def get(sandbox_id):
            self.owner.lease = driver.DeleteRequested(
                sandbox_id, "team-approved")
            return details(status="RUNNING")
        self.owner.get = get
        self.owner.reconcile()
        self.assertIsInstance(self.owner.lease, driver.DeleteRequested)
        self.owner.invoke = self.invoke
        self.assertTrue(self.owner.cleanup("verify-existing-delete"))
        self.assertFalse(any(c[0] == "delete" for c in self.commands))


class AdmissionTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.config = {**pins(), "destination": str(self.root / "results")}
        clock = mock.patch.object(driver.time, "time", return_value=NOW)
        clock.start()
        self.addCleanup(clock.stop)
        for field, filename in (("payload_path", "inputs.tar"),
                                ("session_path", "session.sh"),
                                ("environment_path", "env.sh")):
            path = self.root / filename
            path.write_bytes((driver.HERE / "accuracy_session.sh").read_bytes()
                             if field == "session_path" else b"public fixture")
            self.config[field] = str(path)
            self.config[driver.admission.PATHS[field]] = (
                driver.artifacts.digest(path))
        (self.root / "inputs.tar.sha256").write_text(
            self.config["payload_sha256"] + "  inputs.tar\n")
        self.checkout = mock.patch.object(driver, "checked_checkout")
        self.checkout.start()
        self.addCleanup(self.checkout.stop)
        self.payload = mock.patch.object(driver.admission, "payload_check",
                                         return_value=34_505_639)
        self.payload_mock = self.payload.start()
        self.addCleanup(self.payload.stop)
        self.which = mock.patch.object(driver.shutil, "which",
                                       return_value="/synthetic/prime")
        self.which.start()
        self.addCleanup(self.which.stop)
        def output(arguments, **kwargs):
            if arguments[0] == "git":
                return "\n".join("study/storage/pod/" + name for name in
                                 ("artifacts.py", "preflight.py",
                                  "cgroup_setup.py"))
            self.assertEqual(arguments, ["/synthetic/prime", "--version"])
            return "Prime CLI, version 0.17.0\n"
        self.output = mock.patch.object(driver.subprocess, "check_output",
                                        side_effect=output)
        self.output.start()
        self.addCleanup(self.output.stop)

    def test_local_admission_has_no_ssh_or_account_calls(self):
        before = deepcopy(self.config)
        checked = driver.admit(self.config)
        self.assertGreater(checked["upload_seconds"], 0)
        self.assertNotIn("ssh_key_path", checked)
        self.assertEqual(self.config, before)
        self.payload_mock.assert_called_once()

    def test_changed_transport_and_sidecar_refuse_locally(self):
        path = Path(self.config["environment_path"])
        path.write_bytes(b"changed")
        with self.assertRaisesRegex(ValueError, "transport hash"):
            driver.admit(self.config)
        self.payload_mock.assert_not_called()
        self.config["environment_sha256"] = driver.artifacts.digest(path)
        (self.root / "inputs.tar.sha256").write_text("wrong")
        with self.assertRaisesRegex(ValueError, "external hash"):
            driver.admit(self.config)
        self.payload_mock.assert_not_called()

    def test_alias_reused_or_nested_destination_and_dirty_source_refuse(self):
        alias = self.root / "aliased.sh"
        alias.symlink_to(self.config["environment_path"])
        with self.assertRaisesRegex(ValueError, "canonical"):
            driver.admit({**self.config, "environment_path": str(alias)})
        Path(self.config["destination"]).mkdir()
        with self.assertRaisesRegex(ValueError, "fresh"):
            driver.admit(self.config)
        with self.assertRaises(ValueError):
            driver.fresh_directory(str(driver.ROOT / "forbidden-output"))
        with mock.patch.object(driver, "checked_checkout",
                               side_effect=ValueError("dirty")):
            with self.assertRaisesRegex(ValueError, "dirty"):
                driver.admit(self.config)


class SchemaTests(unittest.TestCase):
    def test_list_requires_complete_unique_consistent_pages(self):
        page = {"sandboxes": [details()], "total": 1, "page": 1,
                "per_page": 100, "has_next": False}
        seen = set()
        parsed = driver.listed_ids(page, 1, seen, None)
        self.assertEqual(parsed[1:3], (1, False))
        self.assertEqual(parsed[3], frozenset({"sandbox_123"}))
        self.assertEqual(seen, set())
        for change in ({"total": 2}, {"has_next": True}, {"page": 2},
                       {"sandboxes": [details(), details()], "total": 2},
                       {"per_page": 50}, {"total": True}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                driver.listed_ids({**page, **change}, 1, set(), None)
        with self.assertRaises(ValueError):
            driver.listed_ids(page, 1, {"sandbox_123"}, 1)
        with self.assertRaises(ValueError):
            driver.listed_ids(page, 1, set(), 2)

    def test_completion_accepts_scopes_and_refuses_bad_bounds(self):
        for code, scope in ((0, "full-evidence"), (1, "partial-evidence"),
                            (78, "diagnostics-only"), (1, "none")):
            value = {"exit": code, "scope": scope, "bundle_bytes": 10240}
            self.assertEqual(driver.completion(value), value)
        for value in ({"exit": False, "scope": "full-evidence"},
                      {"exit": 0, "scope": "partial-evidence"},
                      {"exit": 1, "scope": "full-evidence"},
                      {"exit": 1, "scope": "partial-evidence",
                       "bundle_bytes": -1},
                      {"exit": 1, "scope": "partial-evidence",
                       "bundle_bytes": 9 * 1024**3}):
            with self.subTest(value=value), self.assertRaises(ValueError):
                driver.completion(value)

    def test_profile_inputs_preserve_absolute_workload_deadlines(self):
        value = pins()
        before = deepcopy(value)
        inputs = driver.session_inputs(value, 1000)
        self.assertEqual(inputs["provider"], "prime-sandbox")
        self.assertEqual(inputs["venue"], "sandbox")
        self.assertEqual(inputs["vcpus"], 8)
        self.assertEqual(inputs["cutoff_epoch"], 11680)
        self.assertEqual(inputs["analysis_seconds"], 9600)
        self.assertEqual(value, before)

    def test_guest_admission_preserves_pod_and_sandbox_caps(self):
        source = (driver.HERE / "accuracy_session.sh").read_text()
        start = source.index(
            "import json\nimport os\nfrom decimal import Decimal\n")
        program = source[start:source.index("\nPY", start)]
        base = driver.session_inputs(pins(), 1000)
        for changes, accepted in (({}, True),
                                  ({"budget": "3.01"}, False),
                                  ({"hourly_ceiling": "0.61"}, False),
                                  ({"venue": "other"}, False),
                                  ({"provider": "nebius"}, False),
                                  ({"venue": "pod", "provider": "nebius",
                                    "budget": "2",
                                    "hourly_ceiling": "0.50"}, True),
                                  ({"venue": "pod", "provider": "nebius",
                                    "budget": "2.01",
                                    "hourly_ceiling": "0.50"}, False)):
            value = {**base, **changes}
            with self.subTest(changes=changes), \
                    tempfile.TemporaryDirectory() as directory:
                Path(directory, "session-inputs.json").write_text(
                    json.dumps(value))
                completed = subprocess.run(
                    [sys.executable, "-I", "-c", program],
                                           cwd=directory, capture_output=True,
                    env={**os.environ, "HARNESS_COMMIT": "a" * 40})
                self.assertEqual(completed.returncode, 0 if accepted else 78,
                                 completed.stderr.decode())
        # The historical no-venue input still takes the pod profile.
        base.pop("venue")
        base.update(provider="nebius", budget="2.00", hourly_ceiling="0.50")
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory, "session-inputs.json")
            for ceiling, expected in (("0.50", 0), ("0.51", 78)):
                path.write_text(json.dumps(
                    {**base, "hourly_ceiling": ceiling}))
                completed = subprocess.run(
                    [sys.executable, "-I", "-c", program],
                                           cwd=directory, capture_output=True,
                    env={**os.environ, "HARNESS_COMMIT": "a" * 40})
                self.assertEqual(completed.returncode, expected,
                                 completed.stderr.decode())


if __name__ == "__main__":
    unittest.main()
