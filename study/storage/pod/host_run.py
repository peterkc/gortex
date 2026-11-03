"""Launch once, observe, copy and request owned deletion."""
import argparse
import json
import os
import signal
import shlex
import shutil
import tarfile
import tempfile
import uuid
import subprocess
import threading
import time
from collections.abc import Sequence
from pathlib import Path

import admission
import artifacts
from admission import root_probe_seconds, upload_seconds, validate
from lifecycle import (connect, first_ssh_contact, request_cleanup,
                       root_facts_confirmed, save)

HERE = Path(__file__).resolve().parent
REMOTE = "/srv/storage-pod/"

PROBE_SOURCE = '''import json
import os
import sys
from pathlib import Path
root = Path(sys.argv[1])
marker = root / "complete.json"
if marker.is_file():
    value = {"completion_hex": marker.read_bytes().hex()}
else:
    status = "missing"
    try:
        text = (root / "session.pid").read_text().strip()
        if (not text.isascii() or not text.isdecimal()
                or len(text) > 10 or int(text) < 1):
            status = "invalid"
        else:
            try:
                os.kill(int(text), 0)
            except ProcessLookupError:
                status = "dead"
            else:
                status = "running"
    except FileNotFoundError:
        pass
    except (OSError, OverflowError):
        status = "unproved"
    if status == "dead" and marker.is_file():
        value = {"completion_hex": marker.read_bytes().hex()}
    else:
        value = {"completion_hex": None,
                 "launched": (root / "launched").is_dir(), "process": status}
print(json.dumps(value))
'''
ROOT_PROBE_SOURCE = '''import ctypes
import json
import os
import platform
import cgroup_setup
root = cgroup_setup.unified_root()
if not os.access(root, os.W_OK):
    raise ValueError("true cgroup root is not writable")
libc = ctypes.CDLL(None)
libc.gnu_get_libc_version.restype = ctypes.c_char_p
release = platform.freedesktop_os_release()
print(json.dumps({
    "root": str(root), "writable": True, "memory_controller": True,
    "euid": os.geteuid(), "machine": platform.machine(),
    "os_id": release.get("ID"), "os_version": release.get("VERSION_ID"),
    "python_implementation": platform.python_implementation(),
    "python_version": platform.python_version(),
    "glibc_version": libc.gnu_get_libc_version().decode("ascii"),
}))
'''
PYTHON = REMOTE + "python/bin/python3.12"
PYTHON_BOOTSTRAP_SECONDS = 30
PYTHON_BOOTSTRAP_COMMAND = (
    "set -eu; cd " + shlex.quote(REMOTE) + "; pin="
    + shlex.quote(admission.TOOLS["python.tar.gz"]) + "\n"
    + r'''
printf '%s  python.tar.gz\n' "$pin" | sha256sum --check -
completed() {
    test -d python && test ! -L python &&
    test -f python/.bootstrap-sha256 &&
    test "$(cat python/.bootstrap-sha256)" = "$pin" &&
    test -x python/bin/python3.12
}
if completed; then exit 0; fi
if test -e python || test -L python; then
    echo "refusing an incomplete or different Python prefix" >&2
    exit 78
fi
temporary=$(mktemp -d .python-bootstrap.XXXXXXXXXX)
trap 'rm -rf -- "$temporary"' EXIT
tar -xzf python.tar.gz -C "$temporary"
test -x "$temporary/python/bin/python3.12"
printf '%s\n' "$pin" > "$temporary/python/.bootstrap-sha256"
if mv -T -- "$temporary/python" python; then exit 0; fi
# An earlier attempt may finish after its SSH acknowledgement was lost.
completed
''')
PROBE_COMMAND = (PYTHON + " -B -c " + shlex.quote(PROBE_SOURCE)
                 + " " + shlex.quote(REMOTE))
ROOT_PROBE_COMMAND = ("cd " + REMOTE + " && " + PYTHON + " -B -c "
                      + shlex.quote(ROOT_PROBE_SOURCE) + " || exit 78")


def stage_python(bundle: Path, state: Path) -> Path:
    """Stage the pinned member from admitted inputs without executing it."""
    target = state / "python.tar.gz"
    pin = admission.TOOLS["python.tar.gz"]
    with tempfile.NamedTemporaryFile(dir=state, prefix="python-",
                                     suffix=".tmp", delete=False) as stream:
        temporary = Path(stream.name)
        try:
            with tarfile.open(bundle, "r:") as archive:
                member = archive.getmember("evidence/python.tar.gz")
                if (not member.isfile()
                        or not 0 <= member.size <= artifacts.MAX_FILE):
                    raise ValueError("invalid bootstrap Python archive member")
                with archive.extractfile(member) as source:
                    shutil.copyfileobj(source, stream)
            stream.flush()
            if artifacts.digest(temporary) != pin:
                raise ValueError("bootstrap Python archive SHA-256 differs")
            if target.exists() or target.is_symlink():
                if (target.resolve() != target or not target.is_file()
                        or target.stat().st_mode & 0o077
                        or artifacts.digest(target) != pin):
                    raise ValueError("staged Python archive changed/aliased")
            else:
                temporary.replace(target)
            save(state / "python-bootstrap-input.json", {
                "member": "evidence/python.tar.gz", "sha256": pin,
                "bytes": member.size})
        finally:
            temporary.unlink(missing_ok=True)
    return target


def publish_first_ssh_contact(state: Path, connected_epoch: float,
                              connected_monotonic: float) -> None:
    """Publish complete first-contact clocks without replacing a winner."""
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(
                mode="w", dir=state, prefix="ssh-contact-", suffix=".tmp",
                delete=False) as stream:
            temporary = Path(stream.name)
            json.dump({"connected_epoch": connected_epoch,
                       "connected_monotonic": connected_monotonic}, stream)
        try:
            os.link(temporary, state / "ssh-contact.json")
        except FileExistsError:
            pass  # The first contact must never be refreshed.
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def transport(arguments: Sequence[str], deadline: float) -> bytes:
    remaining = deadline - time.time()
    if remaining <= 0:
        raise TimeoutError("transport window expired")
    child = subprocess.Popen(arguments, stdin=subprocess.DEVNULL,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                             start_new_session=True)
    try:
        stdout, stderr = child.communicate(timeout=remaining)
    except BaseException:
        if child.poll() is None:
            try:
                os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError:
                if child.poll() is None:
                    raise RuntimeError("transport process group state unknown")
        child.wait()
        raise
    if child.returncode:
        raise subprocess.CalledProcessError(child.returncode, arguments,
                                            output=stdout, stderr=stderr)
    return stdout


def observation_refused(state: Path, error: BaseException) -> None:
    """Record a local rescue refusal without acquiring cleanup authority."""
    try:
        save(state / ("observe-refusal-" + uuid.uuid4().hex + ".json"),
             {"error": repr(error), "epoch": time.time(),
              "phase": "before-first-probe", "cleanup_requested": False})
    except OSError as journal_error:
        error.add_note("Observation refusal journal failed: "
                       + repr(journal_error))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ("state", "destination", "ssh-key", "session",
                 "environment", "payload"):
        parser.add_argument("--" + flag, type=Path, required=True)
    parser.add_argument("--observe-only", action="store_true")
    args = parser.parse_args()
    state = args.state.resolve()
    try:
        if args.observe_only and any((state / name).exists() for name in
                                     ("done.json", "rental-error.json")):
            raise ValueError("rental refused or already deleted")
        config = json.loads((state / "config.json").read_text())
        cutoff = json.loads((state / "owner.json").read_text())["cutoff_epoch"]
    except BaseException as error:
        if args.observe_only:
            observation_refused(state, error)
        raise
    primary: BaseException | None = None
    cleanup_owned = not args.observe_only
    ssh, scp, connection = [], [], {}

    def run(arguments: Sequence[str], seconds: float,
            deadline: float = cutoff) -> bytes:
        return transport(arguments,
                         min(time.time() + seconds, deadline, cutoff))

    def record_error(label: str, error: Exception, **details: object) -> None:
        try:
            save(state / (label + "-error-" + uuid.uuid4().hex + ".json"),
                 {"error": repr(error), "epoch": time.time(),
                  "notes": getattr(error, "__notes__", []), **details})
        except Exception as journal_error:
            journal_error.add_note("Recording " + label + ": " + repr(error))
            raise

    def upload(arguments: Sequence[str], deadline: float) -> None:
        # Before launch, re-transfer overwrites the same approved bytes.
        # This never restarts analysis or refreshes the setup allowance.
        for attempt in range(2):
            try:
                run(arguments, config["upload_seconds"], deadline)
                return
            except (subprocess.SubprocessError, TimeoutError) as error:
                record_error("upload", error, attempt=attempt,
                             source=str(arguments[-2]))
                if attempt == 1 or time.time() >= deadline:
                    raise

    def copy(name: str, seconds: int, expected: str,
             expected_bytes: int | None = None) -> None:
        remote = connection["target"] + ":" + REMOTE + name
        run([*scp, remote, remote + ".sha256", str(args.destination)], seconds)
        head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=HERE,
                                       text=True).strip()
        dirty = subprocess.check_output(["git", "status", "--porcelain"],
                                        cwd=HERE,
                                        text=True)
        if head != config["harness_commit"] or dirty:
            raise ValueError("verifier checkout changed during paid execution")
        bundle = args.destination / name
        if (expected_bytes is not None
                and bundle.stat().st_size != expected_bytes):
            raise ValueError("copied archive size differs from marker")
        if (args.destination / (name + ".sha256")).read_text() != (
                artifacts.digest(bundle) + "  " + name + "\n"):
            raise ValueError("copied archive hash mismatch")
        destination = (args.destination
                       / ("verified-" + name.removesuffix(".tar")))
        artifacts.extract(bundle, destination)
        manifest = destination / "evidence/file-manifest.json"
        artifacts.verify(manifest)
        if json.loads(manifest.read_text())["scope"] != expected:
            raise ValueError(
                "copied archive scope differs from completion marker")
        save(state / (name + "-verified.json"), {"scope": expected,
             "harness_commit": head,
             "approval_reference": config["approval_reference"]})

    diagnostics_copied = False

    def diagnostics() -> None:
        nonlocal diagnostics_copied
        if not connection or diagnostics_copied:
            return
        try:
            copy("receipts.tar", 60, "diagnostics-only")
            diagnostics_copied = True
        except Exception as error:
            record_error("fallback-copy", error)
            try:
                tail = run([*ssh, "tail -c 1048576 " + REMOTE
                            + "session.log"], 15)
                tail_path = args.destination / "session-log-tail.partial.txt"
                tail_path.write_bytes(tail)
            except Exception as tail_error:
                record_error("log-tail", tail_error)

    try:
        for flag, key in (("destination", "destination"),
                          ("ssh_key", "ssh_key_path"),
                          ("session", "session_path"),
                          ("environment", "environment_path"),
                          ("payload", "payload_path")):
            canonical = getattr(args, flag).resolve()
            if str(canonical) != config[key]:
                raise ValueError(
                    "host arguments differ from pre-rental approval")
            setattr(args, flag, canonical)
        owner = json.loads((state / "owner.json").read_text())
        ready = owner["started_epoch"] + config["ready_seconds"]
        setup = owner["started_epoch"] + config["setup_seconds"]
        launch_path = state / "launch-requested.json"
        if args.observe_only:
            launch_receipt = json.loads(launch_path.read_text())
            requested = launch_receipt["requested_epoch"]
            if (launch_receipt["relaunch_allowed"] is not False
                    or type(requested) not in (int, float)
                    or not owner["started_epoch"] <= requested < cutoff
                    or not (state / "validated.json").is_file()):
                raise ValueError("original validated launch receipt required")
            identity = json.loads(
                (state / "destination-identity.json").read_text())
            original = args.destination
            observed = original.stat()
            if (identity != {"path": str(original),
                             "device": observed.st_dev,
                             "inode": observed.st_ino}
                    or not original.is_dir()
                    or observed.st_mode & 0o077):
                raise ValueError("approved destination identity changed")
            args.destination = original / ("recovery-" + uuid.uuid4().hex)
            validate({**config, "destination": str(args.destination)})
            args.destination.mkdir(mode=0o700)
            save(state / (args.destination.name + ".json"),
                 {"destination": str(args.destination),
                  "cutoff_epoch": cutoff, "epoch": time.time()})
        else:
            validate(config, destination_ready=True)
            if launch_path.exists():
                raise ValueError("one launch already requested; use "
                                 "--observe-only, never relaunch")
            python_archive = stage_python(args.payload, state)
            connect(state)
        connection = json.loads((state / "connection.json").read_text())
        options = ["-F", "/dev/null", "-o", "BatchMode=yes",
                   "-o", "IdentitiesOnly=yes",
                   "-o", "ServerAliveInterval=30",
                   "-o", "ServerAliveCountMax=6",
                   "-o", "StrictHostKeyChecking=accept-new", "-o",
                   "UserKnownHostsFile=" + str(state / "known_hosts"),
                   "-i", str(args.ssh_key)]
        ssh = ["ssh", "-T", *options, "-p", connection["port"],
               connection["target"]]
        scp = ["scp", *options, "-P", connection["port"]]
        if not args.observe_only:
            root_deadline = ready
            while True:
                try:
                    run([*ssh, "umask 077; mkdir -p " + REMOTE],
                        30, root_deadline)
                    break
                except (subprocess.CalledProcessError,
                        subprocess.TimeoutExpired) as error:
                    if (isinstance(error, subprocess.CalledProcessError)
                            and error.returncode != 255):
                        raise
                    if time.time() >= root_deadline:
                        raise TimeoutError("sshd readiness expired") from error
                    threading.Event().wait(min(5, root_deadline - time.time()))
            if config["cgroup_root"]["mode"] == "first-in-pod-check":
                publish_first_ssh_contact(state, time.time(), time.monotonic())
                contact = first_ssh_contact(state, config, owner)
                if contact is None:
                    raise ValueError("original first SSH contact is unproved")
                root_deadline = contact[0] + root_probe_seconds(config)
            preupload_deadline = min(
                time.time() + config["upload_seconds"], root_deadline)
            upload([*scp, str(HERE / "cgroup_setup.py"),
                    connection["target"] + ":" + REMOTE + "cgroup_setup.py"],
                   preupload_deadline)
            upload([*scp, str(python_archive),
                    connection["target"] + ":" + REMOTE + "python.tar.gz"],
                   preupload_deadline)
            bootstrap_attempt = 0
            while True:
                started = time.time()
                if started >= min(root_deadline, cutoff):
                    error = TimeoutError("Python bootstrap readiness expired")
                    record_error("python-bootstrap-window", error,
                                 sha256=admission.TOOLS["python.tar.gz"],
                                 root_deadline_epoch=root_deadline)
                    raise error
                attempt_deadline = min(started + PYTHON_BOOTSTRAP_SECONDS,
                                       root_deadline, cutoff)
                details = {
                    "attempt": bootstrap_attempt,
                    "sha256": admission.TOOLS["python.tar.gz"],
                    "started_epoch": started,
                    "attempt_deadline_epoch": attempt_deadline,
                    "root_deadline_epoch": root_deadline,
                }
                try:
                    run([*ssh, PYTHON_BOOTSTRAP_COMMAND],
                        PYTHON_BOOTSTRAP_SECONDS, attempt_deadline)
                except Exception as error:
                    remaining = root_deadline - time.time()
                    record_error("python-bootstrap", error, **details,
                                 finished_epoch=time.time(),
                                 window_exhausted=remaining <= 0)
                    retryable = (
                        isinstance(error, subprocess.TimeoutExpired)
                        or isinstance(error, subprocess.CalledProcessError)
                        and error.returncode == 255)
                    if not retryable:
                        raise
                    if remaining <= 0:
                        raise TimeoutError(
                            "Python bootstrap readiness expired") from error
                    threading.Event().wait(min(5, remaining))
                    bootstrap_attempt += 1
                else:
                    save(state / ("python-bootstrap-" + uuid.uuid4().hex
                                  + ".json"),
                         {**details, "finished_epoch": time.time(), "exit": 0})
                    break
            root_attempt = 0
            while True:
                try:
                    root_receipt = json.loads(
                        run([*ssh, ROOT_PROBE_COMMAND], 30, root_deadline))
                    if not root_facts_confirmed(root_receipt):
                        save(state / "root-probe-refusal.json", {
                            "exit": 78, "epoch": time.time(),
                            "facts": root_receipt,
                            "error": "actual host/runtime/root facts refused"})
                        return 78
                    save(state / "root-capability.json", root_receipt)
                    break
                except (subprocess.CalledProcessError,
                        subprocess.TimeoutExpired) as error:
                    if (isinstance(error, subprocess.CalledProcessError)
                            and error.returncode == 78):
                        save(state / "root-probe-refusal.json", {"exit": 78,
                             "epoch": time.time(), "error": repr(error),
                             "stdout_hex": (error.stdout or b"").hex(),
                             "stderr_hex": (error.stderr or b"").hex()})
                        return 78
                    if (isinstance(error, subprocess.CalledProcessError)
                            and error.returncode != 255):
                        raise
                    record_error("root-probe", error, attempt=root_attempt)
                    root_attempt += 1
                    remaining = root_deadline - time.time()
                    if remaining <= 0:
                        raise TimeoutError(
                            "root probe readiness expired") from error
                    threading.Event().wait(min(5, remaining))
            upload_deadline = min(time.time() + config["upload_seconds"],
                                  setup - config["bootstrap_seconds"])
            upload([*scp, str(args.environment),
                    connection["target"] + ":" + REMOTE + "env.sh"],
                   upload_deadline)
            upload([*scp, str(args.session),
                    connection["target"] + ":" + REMOTE + "session.sh"],
                   upload_deadline)
            sources = [HERE / name for name in
                       ("artifacts.py", "preflight.py")]
            sources += [state / "session-inputs.json", args.payload,
                        args.payload.with_suffix(".tar.sha256")]
            upload([*scp, *[str(source) for source in sources],
                    connection["target"] + ":" + REMOTE],
                   upload_deadline)
            # Keep the exclusive receipt after an ambiguous SSH response.
            with (state / "launch-requested.json").open("x") as stream:
                json.dump({"requested_epoch": time.time(),
                           "relaunch_allowed": False}, stream)
            remote = ("set -eu; cd " + REMOTE
                      + "; mkdir launched; . ./env.sh; "
                      'export PATH="/srv/storage-pod/python/bin:$PATH"; '
                      "nohup setsid bash ./session.sh "
                      ">session.log 2>&1 </dev/null &")
            try:
                run([*ssh, remote], 30, setup)
            except (subprocess.TimeoutExpired,
                    subprocess.CalledProcessError) as error:
                if (isinstance(error, subprocess.CalledProcessError)
                        and error.returncode != 255):
                    raise
                save(state / "launch-ack-uncertain.json",
                     {"error": repr(error)})
        completion_deadline = min(cutoff - config["copy_seconds"] - 90,
                                  owner["started_epoch"]
                                  + config["analysis_seconds"] + 360)
        attempt = 0
        requested = json.loads(launch_path.read_text())["requested_epoch"]
        while (time.time() < completion_deadline
               or args.observe_only and attempt == 0 and time.time() < cutoff):
            probe_deadline = (cutoff if args.observe_only and attempt == 0
                              else completion_deadline)
            try:
                cleanup_owned = True
                raw = run([*ssh, PROBE_COMMAND], 30, probe_deadline)
            except (subprocess.SubprocessError, TimeoutError) as error:
                record_error("probe", error, attempt=attempt)
            else:
                observation = json.loads(raw)
                marker = observation["completion_hex"]
                if marker is not None:
                    completed = json.loads(bytes.fromhex(marker))
                    if (type(completed["exit"]) is not int
                            or not 0 <= completed["exit"] <= 255
                            or completed["scope"] not in
                            ("full-evidence", "partial-evidence",
                             "diagnostics-only", "none")
                            or (completed["scope"] == "full-evidence")
                            != (completed["exit"] == 0)):
                        raise ValueError("invalid completion marker")
                    save(state / "completion.json", completed)
                    break
                status = observation["process"]
                if (status not in ("missing", "dead", "running",
                                   "invalid", "unproved")
                        or type(observation["launched"]) is not bool):
                    raise ValueError("invalid session liveness receipt")
                if status in ("invalid", "unproved"):
                    record_error("probe-liveness", RuntimeError(
                        "session liveness " + status), attempt=attempt)
                elif (status == "dead"
                        or (status == "missing" or not observation["launched"])
                        and time.time() >= requested + 30):
                    raise RuntimeError(
                        "analysis session dead or never started")
            attempt += 1
            remaining = completion_deadline - time.time()
            if remaining > 0:
                threading.Event().wait(min(15, remaining))
        else:
            raise TimeoutError("completion unproved before copy reserve")
        # Diagnostic retrieval must not veto an independently valid bulk copy.
        diagnostic_error = None
        try:
            diagnostics()
        except Exception as error:
            # Defer journal failures too, so they cannot veto bulk retrieval.
            diagnostic_error = error
        if completed["scope"] in ("full-evidence", "partial-evidence"):
            size = completed["bundle_bytes"]
            if (type(size) is not int
                    or not 1 <= size <= artifacts.MAX_PAYLOAD
                    + artifacts.TAR_OVERHEAD):
                raise ValueError("invalid completed archive byte bound")
            available = int(min(config["copy_seconds"], cutoff - time.time()))
            required = upload_seconds(size, config["minimum_download_mbps"])
            if required > available:
                try:
                    save(state / "download-forecast-exceeded.json", {
                        "bundle_bytes": size, "required_seconds": required,
                        "available_seconds": available,
                        "minimum_download_mbps":
                            config["minimum_download_mbps"],
                        "cutoff_epoch": cutoff, "epoch": time.time(),
                        "action": "attempt-copy-with-original-deadline"})
                except OSError as error:
                    if diagnostic_error is None:
                        diagnostic_error = error
                    else:
                        diagnostic_error.add_note(
                            "Forecast warning journal failed: " + repr(error))
            copy("evidence.tar", config["copy_seconds"], completed["scope"],
                 size)
        if diagnostic_error is not None:
            try:
                record_error("diagnostics", diagnostic_error)
            except Exception as journal_error:
                journal_error.add_note("Diagnostics failed: "
                                       + repr(diagnostic_error))
                raise
        save(state / "copy-result.json", completed)
        return completed["exit"] or (
            0 if completed["scope"] == "full-evidence" else 1)
    except BaseException as error:
        primary = error
        if args.observe_only and not cleanup_owned:
            observation_refused(state, error)
        else:
            try:
                diagnostics()
            except Exception as copy_error:
                error.add_note("Diagnostics copy failed: " + repr(copy_error))
        raise
    finally:
        if cleanup_owned:
            request_cleanup(state, primary)


if __name__ == "__main__":
    raise SystemExit(main())
