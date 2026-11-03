"""One owned Prime Sandbox for the unchanged D14 workload, without SSH.

The owner stays alive until deletion is proved. Never rerun a create or launch
whose acknowledgement is uncertain. CLI output is not a guest JSON transport.
"""
import argparse
from collections.abc import Mapping, Sequence, Set
from dataclasses import dataclass
from decimal import Decimal
import json
import math
import os
from pathlib import Path
import re
import shlex
import shutil
import signal
import subprocess
import threading
import time
import uuid
from typing import assert_never

import admission
import artifacts
import host_run
import sandbox_policy as policy
from lifecycle import CreateRefused, call, root_facts_confirmed, save


HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
REMOTE = host_run.REMOTE
IMAGE_INDEX_DIGEST = (
    "sha256:076e7bf82319619f28dd0aa9c02ab34b055ad37100395b4f49a2ffba04fb5e11"
)
STOPPED = {"TERMINATED", "TIMEOUT"}


def checked_checkout(config: Mapping) -> None:
    head = subprocess.check_output(
        ["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    dirty = subprocess.check_output(
        ["git", "status", "--porcelain", "--untracked-files=all"],
        cwd=ROOT, text=True)
    if head != config["harness_commit"] or dirty:
        raise ValueError("helper checkout must remain clean and reviewed")


def fresh_directory(name: str) -> Path:
    path = Path(name)
    if (str(path) != name or not path.is_absolute()
            or path.resolve() != path or path.exists()
            or not path.parent.is_dir() or not os.access(path.parent, os.W_OK)
            or path == ROOT or ROOT in path.parents):
        raise ValueError("state/results need a fresh canonical external path")
    return path


def admit(value: Mapping) -> dict:
    """Local-only admission; no account, quote, SSH or sandbox operation."""
    config = policy.validate_config(value, time.time())
    checked_checkout(config)
    for field, pin in admission.PATHS.items():
        path = Path(config[field])
        if (str(path) != config[field] or not path.is_absolute()
                or path.resolve() != path or not path.is_file()):
            raise ValueError("transport input is not canonical/regular")
        if artifacts.digest(path) != config[pin]:
            raise ValueError("transport hash differs from approval: " + field)
    if artifacts.digest(HERE / "accuracy_session.sh") != config[
            "session_sha256"]:
        raise ValueError("session must be the exact reviewed workload script")
    payload = Path(config["payload_path"])
    if (payload.name != "inputs.tar"
            or payload.with_suffix(".tar.sha256").read_text()
            != config["payload_sha256"] + "  inputs.tar\n"):
        raise ValueError("input archive name/external hash receipt differs")
    roots = ["study/storage/" + name for name in
             ("harness", "onnx", "neon-eval", "pod-cost", "pod")]
    names = subprocess.check_output(
        ["git", "ls-files", "--", *roots], cwd=ROOT, text=True).splitlines()
    if not names:
        raise ValueError("reviewed harness export is empty")
    expected = {}
    for name in names:
        source = ROOT / name
        if source.resolve() != source or not source.is_file():
            raise ValueError("reviewed source is aliased or nonregular")
        expected[name] = artifacts.digest(source)
    python_bytes = admission.payload_check(payload, expected)
    size = sum(Path(config[field]).stat().st_size for field in admission.PATHS)
    size += python_bytes + len(json.dumps(config).encode()) + 4096
    size += sum((HERE / name).stat().st_size for name in
                ("artifacts.py", "preflight.py", "cgroup_setup.py"))
    seconds = admission.upload_seconds(size, config["minimum_upload_mbps"])
    if (config["ready_seconds"] + config["root_probe_seconds"] + seconds
            + config["bootstrap_seconds"] > config["setup_seconds"]):
        raise ValueError("readiness/root/upload/build exceed original setup")
    admission.check_download(config, config["expected_evidence_bytes"],
                             config["copy_seconds"])
    fresh_directory(config["destination"])
    executable = shutil.which("prime")
    if executable is None:
        raise ValueError("Prime CLI is not installed")
    version = subprocess.check_output([executable, "--version"], text=True)
    if re.search(r"(?<![0-9.])0\.17\.0(?![0-9.])", version) is None:
        raise ValueError("this reviewed contract requires Prime CLI 0.17.0")
    return {**config, "cli_executable": str(Path(executable).resolve()),
            "upload_seconds": seconds,
            "support_sha256": {name: expected["study/storage/pod/" + name]
                               for name in ("artifacts.py", "preflight.py",
                                            "cgroup_setup.py")}}


def session_inputs(config: Mapping, started: float) -> dict:
    keys = ("analysis_seconds", "approval_reference", "harness_commit",
            "max_seconds", "setup_seconds", "budget", "hourly_ceiling",
            "memory_gb", "minimum_free_bytes", "minimum_memory_bytes")
    return {**{key: config[key] for key in keys}, "venue": "sandbox",
            "provider": "prime-sandbox", "vcpus": config["cpu_cores"],
            "started_epoch": started,
            "cutoff_epoch": started + config["cutoff_seconds"]}


def listed_ids(document: object, page: int, seen: Set[str],
               expected_total: int | None
               ) -> tuple[list[dict], int, bool, frozenset[str]]:
    """Check the native paginated schema before drawing absence conclusions."""
    if not isinstance(document, dict):
        raise ValueError("sandbox list is not an object")
    seen = set(seen)
    rows = document.get("sandboxes")
    total, actual_page = document.get("total"), document.get("page")
    per_page, more = document.get("per_page"), document.get("has_next")
    if (not isinstance(rows, list) or type(total) is not int or total < 0
            or type(actual_page) is not int or actual_page != page
            or type(per_page) is not int or per_page != 100
            or type(more) is not bool or len(rows) > per_page
            or expected_total is not None and total != expected_total):
        raise ValueError("incomplete or drifting sandbox list pagination")
    for row in rows:
        sandbox_id = row.get("id") if isinstance(row, dict) else None
        if (not isinstance(sandbox_id, str)
                or re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{7,127}",
                                sandbox_id) is None
                or sandbox_id in seen):
            raise ValueError("invalid or duplicate sandbox list ID")
        seen.add(sandbox_id)
    if (len(seen) > total or more != (len(seen) < total)
            or more and not rows):
        raise ValueError("sandbox list does not cover its declared total")
    return rows, total, more, frozenset(seen)


def http_not_found(result: subprocess.CompletedProcess, stdout: str) -> bool:
    # Native core.client APIError HTTP status frame, not free-text guessing.
    return (result.returncode == 1
            and re.match(r"\AError: HTTP 404: [^\n]+", stdout.strip())
            is not None)


def completion(value: object) -> dict:
    if (not isinstance(value, dict) or type(value.get("exit")) is not int
            or not 0 <= value["exit"] <= 255
            or value.get("scope") not in ("full-evidence", "partial-evidence",
                                          "diagnostics-only", "none")
            or (value["scope"] == "full-evidence") != (value["exit"] == 0)):
        raise ValueError("invalid completion marker")
    if value["scope"] in ("full-evidence", "partial-evidence"):
        size = value.get("bundle_bytes")
        if (type(size) is not int
                or not 1 <= size <= artifacts.MAX_PAYLOAD
                + artifacts.TAR_OVERHEAD):
            raise ValueError("invalid completed archive size")
    return value


@dataclass(frozen=True)
class NotCreated:
    pass


@dataclass(frozen=True)
class Unidentified:
    candidate: str | None = None


@dataclass(frozen=True)
class Identified:
    sandbox_id: str
    team_id: str | None


@dataclass(frozen=True)
class DeleteRequested:
    sandbox_id: str
    team_id: str | None


Lease = NotCreated | Unidentified | Identified | DeleteRequested


class Owner:
    """One create, one launch and one delete, with retained reconciliation."""

    def __init__(self, config: Mapping, state: Path):
        self.config, self.state = config, state
        nonce = uuid.uuid4().hex
        self.name, self.label = "d14-" + nonce, "d14-owned-" + nonce
        self.started, self.monotonic = time.time(), time.monotonic()
        self.cutoff = self.monotonic + config["cutoff_seconds"]
        self.cancelled = threading.Event()
        self.cancel_read, self.cancel_write = os.pipe()
        os.set_blocking(self.cancel_write, False)
        self.deleted = threading.Event()
        self.lock = threading.RLock()
        self.lease: Lease = NotCreated()
        self.first_contact = None
        self.identity_journal_error = None
        self.timers = []
        destination = Path(config["destination"])
        facts = destination.stat()
        self.destination_identity = (facts.st_dev, facts.st_ino)

    @property
    def sandbox_id(self) -> str | None:
        match self.lease:
            case NotCreated():
                return None
            case Unidentified(candidate):
                return candidate
            case Identified(sandbox_id, _) | DeleteRequested(sandbox_id, _):
                return sandbox_id
            case _ as unhandled:
                assert_never(unhandled)

    @property
    def create_attempted(self) -> bool:
        return not isinstance(self.lease, NotCreated)

    @property
    def confirmed(self) -> bool:
        return isinstance(self.lease, (Identified, DeleteRequested))

    @property
    def delete_attempted(self) -> bool:
        return isinstance(self.lease, DeleteRequested)

    @property
    def cleanup_team(self) -> str | None:
        if isinstance(self.lease, (Identified, DeleteRequested)):
            return self.lease.team_id
        return None

    def close(self) -> None:
        os.close(self.cancel_read)
        os.close(self.cancel_write)

    def record_error(self, label: str, error: BaseException) -> None:
        try:
            save(self.state / (label + "-" + uuid.uuid4().hex + ".json"),
                 {"error": repr(error), "epoch": time.time()})
        except OSError as journal_error:
            # Failure to journal must not veto the independently owned delete.
            print("Sandbox journal failed: " + repr(journal_error), flush=True)

    def invoke(self, arguments: list[str], seconds: float = 30,
               deadline: float | None = None,
               label: str = "read", namespace: str = "sandbox"
               ) -> tuple[subprocess.CompletedProcess, str]:
        if deadline is not None:
            seconds = min(seconds, deadline - time.monotonic())
        if seconds <= 0:
            raise TimeoutError("original sandbox allowance expired")
        receipt = (label if label == "terminate"
                   else label + "-" + uuid.uuid4().hex)
        boundary = ({"not_after": deadline,
                     "cancel_descriptor": self.cancel_read}
                    if deadline is not None else {})
        try:
            result = call(self.config, self.state, receipt,
                          arguments, seconds,
                          namespace=namespace, **boundary)
        except CreateRefused as error:
            if arguments[0] == "create":
                raise
            raise TimeoutError(
                "sandbox operation cancelled/expired") from error
        return result, (self.state / (receipt + ".stdout")).read_text()

    def image_receipt(self, phase: str) -> list[dict]:
        """Read only; PRIME_TEAM_ID pins the context-scoped image list."""
        rows, ids, logical, expected, page = [], set(), set(), None, 1
        while True:
            result, stdout = self.invoke([
                "list", "--output", "json", "--page", str(page),
                "--num", "250"],
                label="images-" + phase, namespace="images")
            if result.returncode:
                raise RuntimeError("read-only image receipt failed")
            document = json.loads(stdout)
            data, total = document.get("data"), document.get("totalCount")
            offset = (page - 1) * 250
            if (not isinstance(data, list) or document.get("offset") != offset
                    or document.get("limit") != 250
                    or total is not None
                    and (type(total) is not int or total < 0)
                    or expected is not None and total != expected):
                raise ValueError("image receipt pagination changed/malformed")
            expected = total
            page_logical = set()
            for row in data:
                if (not isinstance(row, dict)
                        or any(not isinstance(row.get(field), str)
                               or not row[field] for field in
                               ("id", "imageName", "imageTag", "ownerType"))
                        or row["id"] in ids):
                    raise ValueError("malformed/duplicate image receipt rows")
                ids.add(row["id"])
                key = (row.get("teamId") or row["ownerType"],
                       row["imageName"], row["imageTag"])
                page_logical.add(key)
            logical.update(page_logical)
            rows.extend(data)
            more = (offset + 250 < total if total is not None
                    else len(page_logical) >= 250)
            if more and not data:
                raise ValueError("image receipt pagination is incomplete")
            if not more:
                if total is not None and len(logical) != total:
                    raise ValueError("image receipt logical count differs")
                break
            page += 1
        receipt = {"team_id": self.config["team_id"], "phase": phase,
                   "epoch": time.time(), "images": rows}
        save(self.state / ("images-" + phase + ".json"), receipt)
        save(Path(self.config["destination"]) / ("images-" + phase + ".json"),
             receipt)
        return rows

    def image_followup(self, before: Sequence[Mapping],
                       after: Sequence[Mapping]) -> None:
        previous = {row["id"] for row in before}
        added = [row for row in after if row["id"] not in previous]
        team_owned = [row for row in added
                      if row.get("teamId") == self.config["team_id"]]
        receipt = {"new_rows": added, "new_team_owned_rows": team_owned,
                   "action": "manual-ownership-and-cleanup-review-only",
                   "images_auto_deleted": False,
                   "attribution": "new since baseline; origin unproved"}
        save(self.state / "image-cleanup-review.json", receipt)
        save(Path(self.config["destination"]) / "image-cleanup-review.json",
             receipt)
        if added:
            print("New image artifacts observed; review private receipts "
                  "for manual cleanup. No image was deleted.", flush=True)

    def get(self, sandbox_id: str) -> dict:
        result, stdout = self.invoke(["get", sandbox_id, "--output", "json"])
        if result.returncode:
            raise RuntimeError("sandbox get failed; see private read receipts")
        document = json.loads(stdout)
        if not isinstance(document, dict) or document.get("id") != sandbox_id:
            raise ValueError("sandbox get returned the wrong ID/schema")
        return document

    def scoped_list(self, team: str) -> list[dict]:
        rows, seen, expected, page = [], set(), None, 1
        while True:
            result, stdout = self.invoke([
                "list", "--team-id", team, "--label", self.label, "--all",
                "--output", "json", "--page", str(page), "--num", "100"])
            if result.returncode:
                raise RuntimeError("scoped sandbox list failed")
            current, expected, more, seen = listed_ids(
                json.loads(stdout), page, seen, expected)
            if any(not policy.owned(row, row["id"], self.name, self.label)
                   for row in current):
                raise ValueError("scoped list returned an unrelated identity")
            rows.extend(current)
            if not more:
                return rows
            page += 1

    def reconcile(self) -> dict | None:
        if self.sandbox_id is None:
            rows = self.scoped_list(self.config["team_id"])
            if len(rows) != 1:
                if rows:
                    raise ValueError("unique nonce matched multiple sandboxes")
                return None
            with self.lock:
                if self.sandbox_id is None:
                    self.lease = Unidentified(rows[0]["id"])
                elif self.sandbox_id != rows[0]["id"]:
                    raise ValueError("reconciliation identity changed")
        document = self.get(self.sandbox_id)
        if not policy.owned(document, self.sandbox_id, self.name, self.label):
            raise ValueError("sandbox ID/name/label ownership is unproved")
        with self.lock:
            if not isinstance(self.lease, DeleteRequested):
                team = document.get("team_id")
                if not isinstance(team, str) or not team:
                    team = None
                self.lease = Identified(self.sandbox_id, team)
        try:
            save(self.state / "identity.json", document)
        except OSError as error:
            # Keep identity in memory so logging cannot veto deletion.
            self.identity_journal_error = error
        return document

    def stopped(self) -> bool:
        result, stdout = self.invoke(
            ["get", self.sandbox_id, "--output", "json"])
        if result.returncode == 0:
            document = json.loads(stdout)
            proved = (policy.owned(document, self.sandbox_id, self.name,
                                   self.label)
                      and document.get("status") in STOPPED)
        elif http_not_found(result, stdout) and isinstance(
                self.cleanup_team, str) and self.cleanup_team:
            proved = not self.scoped_list(self.cleanup_team)
        else:
            proved = False
        if proved:
            save(self.state / "deletion-proof.json", {
                "id": self.sandbox_id, "epoch": time.time(),
                "method": "terminal-get" if result.returncode == 0
                else "http404-and-complete-owned-team-label-list"})
        return proved

    def cleanup(self, reason: str) -> bool:
        cancel_error = None
        if not self.cancelled.is_set():
            self.cancelled.set()
            try:
                os.write(self.cancel_write, b"x")
            except OSError as error:
                # Event checks still stop use; never let this veto deletion.
                cancel_error = error
        with self.lock:
            if self.deleted.is_set():
                return True
            try:
                if not self.create_attempted:
                    for timer in self.timers:
                        timer.cancel()
                    self.deleted.set()
                    return True
                if not self.confirmed and self.reconcile() is None:
                    return False
                if not self.delete_attempted:
                    journal_error = None
                    try:
                        save(self.state / "delete-requested.json", {
                            "id": self.sandbox_id, "reason": reason,
                            "epoch": time.time(), "replay_allowed": False})
                    except OSError as error:
                        journal_error = error
                    self.lease = DeleteRequested(self.sandbox_id,
                                                 self.cleanup_team)
                    try:
                        self.invoke(["delete", self.sandbox_id, "--yes"],
                                    seconds=120, label="terminate")
                    except Exception as error:
                        self.record_error("delete-ack", error)
                    if journal_error is not None:
                        self.record_error("delete-journal", journal_error)
                    if self.identity_journal_error is not None:
                        self.record_error("identity-journal",
                                          self.identity_journal_error)
                if cancel_error is not None:
                    self.record_error("cancel-pipe", cancel_error)
                if self.stopped():
                    self.deleted.set()
                    for timer in self.timers:
                        timer.cancel()
                    save(self.state / "done.json", {"id": self.sandbox_id,
                         "epoch": time.time(), "deleted": True})
                    return True
            except Exception as error:
                self.record_error("cleanup", error)
            return False

    def timer(self, seconds: float, reason: str) -> threading.Timer:
        timer = threading.Timer(max(0, seconds), self.cleanup, (reason,))
        timer.daemon = True
        self.timers.append(timer)
        timer.start()
        return timer

    def ensure_work(self, deadline: float) -> None:
        if self.cancelled.is_set() or time.monotonic() >= min(
                deadline, self.cutoff):
            raise TimeoutError("sandbox stopped or original window expired")

    def create(self) -> None:
        with self.lock:
            if self.create_attempted or self.cancelled.is_set():
                raise ValueError("create already attempted or cancelled")
        self.started, self.monotonic = time.time(), time.monotonic()
        self.cutoff = self.monotonic + self.config["cutoff_seconds"]
        save(self.state / "owner.json", {
            "name": self.name, "label": self.label,
            "started_epoch": self.started,
            "cutoff_epoch": self.started + self.config["cutoff_seconds"],
            "server_timeout_minutes": self.config["timeout_minutes"],
            "image_index_digest": IMAGE_INDEX_DIGEST})
        self.timer(self.config["cutoff_seconds"], "hard-cutoff")
        self.ready_timer = self.timer(self.config["ready_seconds"],
                                      "readiness")
        arguments = policy.create_arguments(self.config, self.name, self.label)
        with self.lock:
            self.ensure_work(self.cutoff)
            if self.create_attempted:
                raise ValueError("create may be issued only once")
            save(self.state / "create-requested.json", {
                "arguments": arguments, "epoch": time.time(),
                "replay_allowed": False})
            self.lease = Unidentified()
        # Do not hold the cleanup lock during network I/O.
        try:
            result, stdout = self.invoke(arguments, 120,
                                         min(self.cutoff, self.monotonic
                                             + self.config["ready_seconds"]),
                                         "create")
        except CreateRefused:
            # This boundary guarantees that no subprocess was started.
            with self.lock:
                self.lease = NotCreated()
            raise
        except Exception:
            # The CLI may have printed an ID before timing out. Reconcile only.
            outputs = list(self.state.glob("create-*.stdout"))
            stdout = outputs[0].read_text() if len(outputs) == 1 else ""
            self.adopt_created_id(stdout)
            raise
        self.adopt_created_id(stdout)
        with self.lock:
            document = self.reconcile()
        if document is None:
            raise RuntimeError("create identity unproved; owner retained")
        if result.returncode or policy.mismatches(document, self.config):
            raise ValueError("create failed or sandbox use pins differ")
        save(self.state / "validated.json", document)

    def adopt_created_id(self, stdout: str) -> None:
        candidate = policy.created_id(stdout)
        with self.lock:
            if candidate is not None:
                if self.sandbox_id not in (None, candidate):
                    raise ValueError("create/list identity conflict")
                if not self.confirmed:
                    self.lease = Unidentified(candidate)

    def remote(self, script: str, seconds: float, deadline: float) -> None:
        self.ensure_work(deadline)
        remaining = min(seconds, deadline - time.monotonic(),
                        self.cutoff - time.monotonic())
        result, _ = self.invoke([
            "run", self.sandbox_id, "--user", "root", "--timeout",
            str(max(1, math.floor(remaining))), "--", "bash", "-c", script],
            remaining, deadline, "exec")
        if result.returncode:
            raise subprocess.CalledProcessError(result.returncode, result.args)

    def download(self, remote: str, local: Path, seconds: float,
                 deadline: float) -> None:
        self.ensure_work(deadline)
        if local.exists() or local.is_symlink():
            raise ValueError("download must use a fresh path, never prompt")
        result, _ = self.invoke(
            ["download", self.sandbox_id, remote, str(local)], seconds,
            min(deadline, self.cutoff), "download")
        if result.returncode or not local.is_file() or local.is_symlink():
            raise RuntimeError("sandbox download did not complete")

    def upload(self, source: Path, name: str, deadline: float,
               expected: str | None = None) -> None:
        self.ensure_work(deadline)
        checked_checkout(self.config)
        pin = artifacts.digest(source)
        if expected is not None and pin != expected:
            raise ValueError("upload source differs from its approved hash")
        result, _ = self.invoke(["upload", self.sandbox_id, str(source),
                                REMOTE + name], self.config["upload_seconds"],
                               min(deadline, self.cutoff), "upload")
        if result.returncode:
            raise RuntimeError("sandbox upload failed")
        self.remote("cd " + shlex.quote(REMOTE) + "; printf '%s  %s\\n' "
                    + shlex.quote(pin) + " " + shlex.quote(name)
                    + " | sha256sum --check -", 30, deadline)

    def guest_json(self, source: str, deadline: float,
                   arguments: str = "", *, refusal: bool = False) -> dict:
        name = "receipt-" + uuid.uuid4().hex + ".json"
        self.remote("set -eu; cd " + shlex.quote(REMOTE) + "; "
                    + host_run.PYTHON + " -B -c " + shlex.quote(source)
                    + " " + arguments + " > " + name
                    + (" || exit 78" if refusal else "")
                    + '; test "$(stat -c %s ' + name + ')" -le 1048576',
                    30, deadline)
        path = self.state / name
        self.download(REMOTE + name, path, 30, deadline)
        if path.stat().st_size > 1024 ** 2:
            raise ValueError("guest JSON receipt exceeds bound")
        value = json.loads(path.read_text())
        if not isinstance(value, dict):
            raise ValueError("guest receipt is not a JSON object")
        return value

    def first_exec(self) -> float:
        """Persist one contact and never refresh its root-check deadline."""
        if self.first_contact is None:
            self.ensure_work(self.monotonic + self.config["ready_seconds"])
            self.first_contact = (time.time(), time.monotonic())
            save(self.state / "first-exec.json", {
                "epoch": self.first_contact[0],
                "monotonic": self.first_contact[1]})
            self.ready_timer.cancel()
            self.root_timer = self.timer(self.config["root_probe_seconds"],
                                         "root-check")
        return self.first_contact[1] + self.config["root_probe_seconds"]

    def prepare(self) -> None:
        if (self.state / "launch-requested.json").exists():
            raise ValueError("launch already requested; observe only")
        config = self.config
        ready = self.monotonic + config["ready_seconds"]
        while True:
            self.ensure_work(ready)
            document = self.reconcile()
            if document is not None:
                if policy.mismatches(document, config):
                    raise ValueError("sandbox pins changed before readiness")
                if document.get("status") in STOPPED | {"ERROR", "PAUSED"}:
                    raise ValueError("sandbox is not usable")
                if document.get("status") == "RUNNING":
                    try:
                        self.remote("umask 077; mkdir -p " + REMOTE, 30, ready)
                    except (RuntimeError, subprocess.SubprocessError):
                        self.ensure_work(ready)
                    else:
                        break
            threading.Event().wait(min(5, max(0, ready - time.monotonic())))
        root_deadline = self.first_exec()
        python_archive = host_run.stage_python(Path(config["payload_path"]),
                                               self.state)
        self.upload(HERE / "cgroup_setup.py", "cgroup_setup.py", root_deadline,
                    config["support_sha256"]["cgroup_setup.py"])
        self.upload(python_archive, "python.tar.gz", root_deadline,
                    admission.TOOLS["python.tar.gz"])
        # Idempotent bootstrap may finish after an acknowledgement is lost.
        # Every attempt keeps the original first-contact deadline.
        while True:
            try:
                self.remote(host_run.PYTHON_BOOTSTRAP_COMMAND,
                            host_run.PYTHON_BOOTSTRAP_SECONDS, root_deadline)
                facts = self.guest_json(host_run.ROOT_PROBE_SOURCE,
                                        root_deadline, refusal=True)
                break
            except (subprocess.SubprocessError, RuntimeError) as error:
                if (isinstance(error, subprocess.CalledProcessError)
                        and error.returncode == 78):
                    raise
                self.record_error("bootstrap-root-transport", error)
                self.ensure_work(root_deadline)
                threading.Event().wait(min(
                    5, max(0, root_deadline - time.monotonic())))
        if not root_facts_confirmed(facts):
            save(self.state / "root-probe-refusal.json", facts)
            raise ValueError("actual root/runtime/OS/cgroup facts refused")
        self.ensure_work(root_deadline)
        self.root_timer.cancel()
        save(self.state / "root-capability.json", facts)
        setup = self.monotonic + config["setup_seconds"]
        upload_deadline = min(time.monotonic() + config["upload_seconds"],
                              setup - config["bootstrap_seconds"])
        save(self.state / "session-inputs.json", session_inputs(config,
                                                               self.started))
        sources = [(Path(config["environment_path"]), "env.sh"),
                   (Path(config["session_path"]), "session.sh"),
                   (HERE / "artifacts.py", "artifacts.py"),
                   (HERE / "preflight.py", "preflight.py"),
                   (self.state / "session-inputs.json", "session-inputs.json"),
                   (Path(config["payload_path"]), "inputs.tar"),
                   (Path(config["payload_path"] + ".sha256"),
                    "inputs.tar.sha256")]
        checked_checkout(config)
        pins = {"env.sh": config["environment_sha256"],
                "session.sh": config["session_sha256"],
                "inputs.tar": config["payload_sha256"],
                **config["support_sha256"]}
        for path, name in sources:
            self.upload(path, name, upload_deadline, pins.get(name))
        self.ensure_work(setup)
        # Local intent and guest mkdir are both one-shot, including lost ACKs.
        with (self.state / "launch-requested.json").open("x") as stream:
            json.dump({"requested_epoch": time.time(),
                       "relaunch_allowed": False}, stream)
        script = ("set -eu; cd " + REMOTE
                  + "; mkdir launched; . ./env.sh; "
                  + 'export PATH="/srv/storage-pod/python/bin:$PATH"; '
                  + "nohup setsid bash ./session.sh "
                  + ">session.log 2>&1 </dev/null &")
        try:
            self.remote(script, 30, setup)
        except (RuntimeError, subprocess.SubprocessError) as error:
            self.record_error("launch-ack-uncertain", error)

    def copy(self, name: str, scope: str, deadline: float,
             expected_bytes: int | None = None) -> None:
        destination = Path(self.config["destination"])
        facts = destination.stat()
        if (destination.resolve() != destination
                or (facts.st_dev, facts.st_ino) != self.destination_identity
                or facts.st_mode & 0o077):
            raise ValueError("approved private destination identity changed")
        # The native SDK buffers a whole file. Bound it before transfer too.
        self.remote("set -eu; cd " + REMOTE + "; test -f " + name
                    + "; test ! -L " + name + "; test "
                    + '"$(stat -c %s ' + name + ')" -le '
                    + str(artifacts.MAX_PAYLOAD + artifacts.TAR_OVERHEAD)
                    + "; test -f " + name + ".sha256; test ! -L " + name
                    + '.sha256; test "$(stat -c %s ' + name
                    + '.sha256)" -le 256', 30, deadline)
        partial = destination / (".download-" + uuid.uuid4().hex + ".tar")
        sidecar = partial.with_suffix(".sha256")
        self.download(REMOTE + name, partial,
                      min(deadline, self.cutoff) - time.monotonic(), deadline)
        self.download(REMOTE + name + ".sha256", sidecar, 30, deadline)
        checked_checkout(self.config)
        if (expected_bytes is not None
                and partial.stat().st_size != expected_bytes):
            raise ValueError("copied archive size differs from marker")
        if sidecar.stat().st_size > 256 or sidecar.read_text() != (
                artifacts.digest(partial) + "  " + name + "\n"):
            raise ValueError("copied archive external hash differs")
        verified = destination / ("verified-" + name.removesuffix(".tar"))
        extracting = destination / (".verify-" + uuid.uuid4().hex)
        artifacts.extract(partial, extracting)
        manifest = extracting / "evidence/file-manifest.json"
        artifacts.verify(manifest)
        if json.loads(manifest.read_text()).get("scope") != scope:
            raise ValueError("copied manifest scope differs from completion")
        if (verified.exists() or destination.joinpath(name).exists()
                or destination.joinpath(name + ".sha256").exists()):
            raise ValueError("verified publication cannot overwrite evidence")
        extracting.rename(verified)
        partial.rename(destination / name)
        sidecar.rename(destination / (name + ".sha256"))
        save(self.state / (name + "-verified.json"), {
            "scope": scope, "harness_commit": self.config["harness_commit"],
            "approval_reference": self.config["approval_reference"]})

    def salvage(self) -> None:
        if not (self.state / "launch-requested.json").exists():
            return
        deadline = min(self.cutoff, time.monotonic() + 60)
        try:
            self.copy("receipts.tar", "diagnostics-only", deadline)
        except Exception as error:
            self.record_error("fallback-copy", error)
            try:
                name = "log-tail-" + uuid.uuid4().hex + ".partial.txt"
                self.remote("cd " + REMOTE
                            + "; tail -c 1048576 session.log > " + name,
                            15, deadline)
                self.download(REMOTE + name,
                              Path(self.config["destination"]) / name,
                              15, deadline)
            except Exception as tail_error:
                self.record_error("fallback-tail", tail_error)

    def observe_and_copy(self) -> int:
        config = self.config
        deadline = min(self.cutoff - config["copy_seconds"] - 90,
                       self.monotonic + config["analysis_seconds"] + 360)
        requested = json.loads(
            (self.state / "launch-requested.json").read_text())
        completed = None
        while time.monotonic() < deadline:
            try:
                observed = self.guest_json(host_run.PROBE_SOURCE, deadline,
                                            shlex.quote(REMOTE))
            except (RuntimeError, subprocess.SubprocessError) as error:
                self.record_error("observation", error)
            else:
                marker = observed.get("completion_hex")
                if marker is not None:
                    completed = completion(json.loads(bytes.fromhex(marker)))
                    save(self.state / "completion.json", completed)
                    break
                status = observed.get("process")
                if (status not in {"missing", "dead", "running", "invalid",
                                   "unproved"}
                        or type(observed.get("launched")) is not bool):
                    raise ValueError("invalid session liveness receipt")
                if status == "dead" or (
                        status == "missing" or not observed["launched"]) and (
                        time.time() >= requested["requested_epoch"] + 30):
                    raise RuntimeError("session dead or never started")
            self.ensure_work(deadline)
            threading.Event().wait(min(15, deadline - time.monotonic()))
        if completed is None:
            raise TimeoutError("completion unproved before copy reserve")
        diagnostic_deadline = min(self.cutoff, time.monotonic() + 60)
        diagnostic_error = None
        try:
            self.copy("receipts.tar", "diagnostics-only", diagnostic_deadline)
        except Exception as error:
            diagnostic_error = error
        copy_deadline = min(self.cutoff,
                            time.monotonic() + config["copy_seconds"])
        if completed["scope"] in ("full-evidence", "partial-evidence"):
            available = max(0, int(copy_deadline - time.monotonic()))
            required = admission.upload_seconds(
                completed["bundle_bytes"], config["minimum_download_mbps"])
            if required > available:
                try:
                    save(self.state / "download-forecast-exceeded.json", {
                        "bytes": completed["bundle_bytes"],
                        "available": available,
                        "action": "attempt-copy-with-original-deadline"})
                except OSError as error:
                    if diagnostic_error is None:
                        diagnostic_error = error
                    else:
                        diagnostic_error.add_note(
                            "Forecast warning journal failed: " + repr(error))
            self.copy("evidence.tar", completed["scope"], copy_deadline,
                      completed["bundle_bytes"])
        if diagnostic_error is not None:
            self.record_error("diagnostics", diagnostic_error)
        save(self.state / "copy-result.json", completed)
        return completed["exit"] or (0 if completed["scope"] == "full-evidence"
                                     else 1)


def run(config: Mapping, state: Path) -> int:
    os.umask(0o077)
    state = fresh_directory(str(state))
    destination = fresh_directory(config["destination"])
    if (state == destination or state in destination.parents
            or destination in state.parents):
        raise ValueError("state and result directories must be separate")
    state.mkdir(mode=0o700)
    destination.mkdir(mode=0o700)
    save(state / "config.json", dict(config))
    # This is the driver's own process environment, not persisted CLI config.
    # Native images list has no --team-id option; Config honors this override.
    os.environ["PRIME_TEAM_ID"] = config["team_id"]
    owner = Owner(config, state)
    before_images = None
    previous = {}
    numbers = (signal.SIGINT, signal.SIGTERM, signal.SIGHUP, signal.SIGQUIT)
    for number in numbers:
        previous[number] = signal.signal(
            number, lambda signum, frame: threading.Thread(
                target=owner.cleanup, args=("signal-" + str(signum),),
                daemon=True).start())
    try:
        before_images = owner.image_receipt("before")
        owner.create()
        owner.prepare()
        return owner.observe_and_copy()
    except BaseException as error:
        owner.record_error("execution", error)
        owner.salvage()
        raise
    finally:
        # A failed acknowledgement is not deletion. Retain the same owner;
        # only read-only reconciliation/verification repeats, never effects.
        while not owner.cleanup("controller-exit"):
            print("Deletion unproved; retaining owner. See private state: "
                  + str(state), flush=True)
            threading.Event().wait(10)
        try:
            after_images = owner.image_receipt("after")
            if before_images is not None:
                owner.image_followup(before_images, after_images)
        except Exception as error:
            owner.record_error("post-delete-image-receipt", error)
            raise RuntimeError(
                "Sandbox cleanup complete; required image receipt incomplete. "
                "Manual image review required.") from error
        finally:
            for number, handler in previous.items():
                signal.signal(number, handler)
            owner.close()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("check", "run"))
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--state", type=Path)
    args = parser.parse_args()
    config = admit(json.loads(args.config.read_text()))
    if args.action == "check":
        print(json.dumps({"hourly": str(policy.hourly(config)),
                          "four_hour_resource_bound":
                              str(policy.hourly(config) * 4),
                          "image_allowance": config["image_allowance"],
                          "modeled_total": str(policy.hourly(config) * 4
                              + Decimal(str(config["image_allowance"]))),
                          "scope": "resources plus bounded image allowance; "
                                   "not a provider-proved all-fee cap",
                          "upload_seconds": config["upload_seconds"]}))
        return 0
    if args.state is None:
        parser.error("run requires a fresh --state")
    return run(config, args.state)


if __name__ == "__main__":
    raise SystemExit(main())
