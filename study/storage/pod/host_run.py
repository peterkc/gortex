"""Launch once, observe, copy and request owned deletion."""
import argparse
import json
import os
import signal
import shlex
import uuid
import subprocess
import threading
import time
from collections.abc import Sequence
from pathlib import Path

import artifacts
from admission import check_download, root_probe_seconds, validate
from lifecycle import connect, request_cleanup, save

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
    value = {"completion_hex": None,
             "launched": (root / "launched").is_dir(), "process": status}
print(json.dumps(value))
'''
ROOT_PROBE_SOURCE = '''import json
import os
import cgroup_setup
root = cgroup_setup.unified_root()
if not os.access(root, os.W_OK):
    raise ValueError("true cgroup root is not writable")
print(json.dumps({"root": str(root), "writable": True}))
'''
PROBE_COMMAND = ("python3.12 -B -c " + shlex.quote(PROBE_SOURCE)
                 + " " + shlex.quote(REMOTE))
ROOT_PROBE_COMMAND = ("cd " + REMOTE + "; python3.12 -B -c "
                      + shlex.quote(ROOT_PROBE_SOURCE) + " || exit 78")


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


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ("state", "destination", "ssh-key", "session",
                 "environment", "payload"):
        parser.add_argument("--" + flag, type=Path, required=True)
    parser.add_argument("--observe-only", action="store_true")
    args = parser.parse_args()
    state = args.state.resolve()
    config = json.loads((state / "config.json").read_text())
    primary: BaseException | None = None
    ssh, scp, connection = [], [], {}
    cutoff = json.loads((state / "owner.json").read_text())["cutoff_epoch"]

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
            root_deadline = min(ready, owner["started_epoch"]
                                + root_probe_seconds(config))
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
            upload([*scp, str(HERE / "cgroup_setup.py"),
                    connection["target"] + ":" + REMOTE + "cgroup_setup.py"],
                   min(time.time() + config["upload_seconds"], root_deadline))
            try:
                root_receipt = json.loads(
                    run([*ssh, ROOT_PROBE_COMMAND], 30, root_deadline))
                if (root_receipt != {"root": "/sys/fs/cgroup",
                                     "writable": True}
                        or root_receipt["writable"] is not True):
                    raise ValueError("true-root precheck receipt differs")
                save(state / "root-capability.json", root_receipt)
            except subprocess.CalledProcessError as error:
                if error.returncode != 78:
                    raise
                primary = error
                save(state / "root-probe-refusal.json", {"exit": 78,
                     "epoch": time.time(), "error": repr(error),
                     "stdout_hex": (error.stdout or b"").hex(),
                     "stderr_hex": (error.stderr or b"").hex()})
                return 78
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
                if (status not in ("missing", "dead", "running")
                        or type(observation["launched"]) is not bool):
                    raise ValueError("invalid session liveness receipt")
                if (status == "dead"
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
            check_download(config, size,
                           int(min(config["copy_seconds"],
                                   cutoff - time.time())))
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
        try:
            diagnostics()
        except Exception as copy_error:
            error.add_note("Diagnostics copy failed: " + repr(copy_error))
        raise
    finally:
        request_cleanup(state, primary)


if __name__ == "__main__":
    raise SystemExit(main())
