"""Launch once, observe, copy and request owned deletion."""
import argparse
import json
import os
import signal
import subprocess
import threading
import time
from collections.abc import Sequence
from pathlib import Path

import artifacts
from admission import validate
from lifecycle import connect, request_cleanup, save

HERE = Path(__file__).resolve().parent
REMOTE = "/srv/storage-pod/"


def transport(arguments: Sequence[str], deadline: float) -> str:
    remaining = deadline - time.time()
    if remaining <= 0:
        raise TimeoutError("transport window expired")
    child = subprocess.Popen(arguments, stdin=subprocess.DEVNULL,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                             text=True, start_new_session=True)
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
    args = parser.parse_args()
    state = args.state.resolve()
    config = json.loads((state / "config.json").read_text())
    primary: BaseException | None = None
    ssh, scp, connection = [], [], {}
    cutoff = json.loads((state / "owner.json").read_text())["cutoff_epoch"]

    def run(arguments: Sequence[str], seconds: float,
            deadline: float = cutoff) -> str:
        return transport(arguments,
                         min(time.time() + seconds, deadline, cutoff))

    def upload(arguments: Sequence[str], deadline: float) -> None:
        # Before launch, re-transfer overwrites the same approved bytes.
        # This never restarts analysis or refreshes the setup allowance.
        for attempt in range(2):
            try:
                run(arguments, config["upload_seconds"], deadline)
                return
            except (subprocess.SubprocessError, TimeoutError) as error:
                save(state / "upload-error.json",
                     {"attempt": attempt, "error": repr(error)})
                if attempt == 1 or time.time() >= deadline:
                    raise

    def copy(name: str, seconds: int, expected: str) -> None:
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
        except (OSError, ValueError, subprocess.SubprocessError,
                TimeoutError) as error:
            save(state / "fallback-copy-error.json", {"error": repr(error)})
            try:
                tail = run([*ssh, "tail -c 1048576 " + REMOTE
                            + "session.log"], 15)
                (args.destination / "session-log-tail.partial.txt").write_text(
                    tail)
            except (subprocess.SubprocessError, TimeoutError) as tail_error:
                save(state / "log-tail-error.json",
                     {"error": repr(tail_error)})

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
        validate(config, destination_ready=True)
        if (state / "launch-requested.json").exists():
            raise ValueError(
                "one launch already requested; never relaunch this attempt")
        connect(state)
        connection = json.loads((state / "connection.json").read_text())
        owner = json.loads((state / "owner.json").read_text())
        ready = owner["started_epoch"] + config["ready_seconds"]
        setup = owner["started_epoch"] + config["setup_seconds"]
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
        while True:
            try:
                run([*ssh, "umask 077; mkdir -p " + REMOTE], 30, ready)
                break
            except (subprocess.CalledProcessError,
                    subprocess.TimeoutExpired) as error:
                if (isinstance(error, subprocess.CalledProcessError)
                        and error.returncode != 255):
                    raise
                if time.time() >= ready:
                    raise TimeoutError("sshd readiness expired") from error
                threading.Event().wait(min(5, ready - time.time()))
        upload_deadline = min(time.time() + config["upload_seconds"],
                              setup - config["bootstrap_seconds"])
        upload([*scp, str(args.environment),
                connection["target"] + ":" + REMOTE + "env.sh"],
               upload_deadline)
        upload([*scp, str(args.session),
                connection["target"] + ":" + REMOTE + "session.sh"],
               upload_deadline)
        sources = [HERE / name for name in
                   ("artifacts.py", "preflight.py", "cgroup_setup.py")]
        sources += [state / "session-inputs.json", args.payload,
                    args.payload.with_suffix(".tar.sha256")]
        upload([*scp, *[str(source) for source in sources],
                connection["target"] + ":" + REMOTE],
               upload_deadline)
        # Keep the exclusive receipt after an ambiguous SSH response.
        with (state / "launch-requested.json").open("x") as stream:
            json.dump({"requested_epoch": time.time(),
                       "relaunch_allowed": False}, stream)
        remote = ("set -eu; cd " + REMOTE + "; mkdir launched; . ./env.sh; "
                  "nohup setsid bash ./session.sh "
                  ">session.log 2>&1 </dev/null & "
                  "echo $! >session.pid")
        try:
            run([*ssh, remote], 30, setup)
        except (subprocess.TimeoutExpired,
                subprocess.CalledProcessError) as error:
            if (isinstance(error, subprocess.CalledProcessError)
                    and error.returncode != 255):
                raise
            save(state / "launch-ack-uncertain.json", {"error": repr(error)})
        completion_deadline = min(cutoff - config["copy_seconds"] - 90,
                                  owner["started_epoch"]
                                  + config["analysis_seconds"] + 360)
        attempt = 0
        while time.time() < completion_deadline:
            try:
                raw = run([*ssh, "test ! -f " + REMOTE
                           + "complete.json || cat "
                           + REMOTE + "complete.json"],
                          30, completion_deadline)
            except (subprocess.SubprocessError, TimeoutError) as error:
                save(state / "probe-error.json",
                     {"attempt": attempt, "error": repr(error)})
            else:
                if raw.strip():
                    completed = json.loads(raw)
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
            attempt += 1
            remaining = completion_deadline - time.time()
            if remaining > 0:
                threading.Event().wait(min(15, remaining))
        else:
            raise TimeoutError("completion unproved before copy reserve")
        diagnostics()  # Includes shape refusal 78 and first-cut-only output.
        if completed["scope"] in ("full-evidence", "partial-evidence"):
            copy("evidence.tar", config["copy_seconds"], completed["scope"])
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
