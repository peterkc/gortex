"""Run one pod session and bounded copy under its host owner."""
import argparse
import json
import os
import signal
import subprocess
import time
from collections.abc import Sequence
from pathlib import Path
from typing import Literal, assert_never

from artifacts import digest
from lifecycle import connect, stop_request


HERE = Path(__file__).resolve().parent


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state", type=Path, required=True)
    parser.add_argument("--destination", type=Path, required=True)
    parser.add_argument("--ssh-key", type=Path, required=True)
    parser.add_argument("--session", type=Path, required=True)
    parser.add_argument("--environment", type=Path, required=True)
    parser.add_argument("--payload", type=Path, required=True)
    args = parser.parse_args()
    state = args.state.resolve()
    config = json.loads((state / "config.json").read_text())
    try:
        head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=HERE,
                                       text=True).strip()
        if head != config["harness_commit"]:
            raise ValueError("helper checkout is not the reviewed harness")
        dirty = subprocess.check_output(
            ["git", "status", "--porcelain", "--untracked-files=all"],
            cwd=HERE, text=True)
        if dirty:
            raise ValueError("helper checkout is not clean")
        for path, key in ((args.payload, "payload_sha256"),
                          (args.session, "session_sha256"),
                          (args.environment, "environment_sha256")):
            if digest(path) != config[key]:
                raise ValueError("transport input differs from approved hash")
        if args.payload.name != "inputs.tar":
            raise ValueError("payload must be the approved inputs.tar")
        if args.ssh_key.stat().st_mode & 0o077:
            raise ValueError("SSH key must be private")
        args.destination.mkdir(mode=0o700)
    except BaseException:
        stop_request(state)
        raise
    connect(state)  # Refusal requests immediate termination.
    connection = json.loads((state / "connection.json").read_text())
    cutoff = json.loads((state / "owner.json").read_text())["cutoff_epoch"]
    ssh = ["ssh", "-T", "-o", "BatchMode=yes",
           "-o", "IdentitiesOnly=yes", "-o",
           "StrictHostKeyChecking=accept-new", "-o",
           "UserKnownHostsFile=" + str(state / "known_hosts"),
           "-i", str(args.ssh_key),
           "-p", connection["port"], connection["target"]]
    scp = ["scp", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o",
           "StrictHostKeyChecking=accept-new", "-o",
           "UserKnownHostsFile=" + str(state / "known_hosts"),
           "-i", str(args.ssh_key),
           "-P", connection["port"]]
    transport: Literal["intact", "lost"] = "intact"

    def transfer(arguments: Sequence[str], seconds: int) -> None:
        remaining = min(seconds, int(cutoff - time.time()))
        if remaining <= 0:
            raise TimeoutError("copy window has expired")
        child = subprocess.Popen(
            arguments,
            stdin=subprocess.DEVNULL, start_new_session=True)
        try:
            code = child.wait(timeout=remaining)
        except BaseException:
            if child.poll() is None:
                try:
                    os.killpg(child.pid, signal.SIGKILL)
                except ProcessLookupError as error:
                    if child.poll() is None:
                        raise RuntimeError(
                            "copy group state is unknown") from error
            child.wait()
            raise
        if code:
            raise subprocess.CalledProcessError(code, child.args)

    def copy(name: str, seconds: int) -> None:
        remote = connection["target"] + ":/srv/storage-pod/" + name
        transfer([*scp, remote, remote + ".sha256", str(args.destination)],
                 seconds)

    try:
        subprocess.run([*ssh, "umask 077; mkdir /srv/storage-pod"],
                       stdin=subprocess.DEVNULL, timeout=30, check=True)
        transfer([*scp, str(args.environment),
                  connection["target"] + ":/srv/storage-pod/env.sh"], 30)
        transfer([*scp, str(args.session),
                  connection["target"] + ":/srv/storage-pod/session.sh"], 30)
        for source in (HERE / "artifacts.py", HERE / "preflight.py",
                       state / "session-inputs.json", args.payload,
                       args.payload.with_suffix(args.payload.suffix
                                                + ".sha256")):
            transfer([*scp, str(source), connection["target"]
                      + ":/srv/storage-pod/" + source.name], 420)
        remote = ("set -eu; cd /srv/storage-pod; . ./env.sh; "
                  "nohup setsid bash ./session.sh >session.log 2>&1 "
                  "</dev/null & child=$!; echo $child >session.pid; "
                  "wait $child")
        try:
            result = subprocess.run([*ssh, remote], stdin=subprocess.DEVNULL,
                                    timeout=max(1, int(cutoff - time.time())))
        except subprocess.TimeoutExpired:
            transport = "lost"
            raise
        if result.returncode == 255:
            transport = "lost"
            raise RuntimeError("SSH lost; remote survives; cutoff stays armed")
        if result.returncode == 78:
            return 78  # finally requests immediate shape-refusal termination.
        if result.returncode == 0:
            try:
                copy("evidence.tar", 420)
            except (subprocess.SubprocessError, TimeoutError) as error:
                (state / "bulk-copy-error.txt").write_text(repr(error))
            else:
                (state / "copy-result.txt").write_text(
                    "full; verify locally\n")
                return 0
        try:
            copy("receipts.tar", 60)
        except (subprocess.SubprocessError, TimeoutError) as error:
            (state / "fallback-copy-error.txt").write_text(repr(error))
            # Early bootstrap may fail before the packer is available.
            with (args.destination / "session-log-tail.partial.txt").open(
                    "xb") as output:
                subprocess.run([
                    *ssh, "tail -c 1048576 /srv/storage-pod/session.log"],
                    stdin=subprocess.DEVNULL, stdout=output,
                    timeout=max(1, min(15, int(cutoff - time.time()))),
                    check=True)
        else:
            (state / "copy-result.txt").write_text(
                "diagnostics only; no accepted result\n")
        return 1
    finally:
        match transport:
            case "intact":
                stop_request(state)
            case "lost":
                pass  # Detached work remains covered by the independent owner.
            case _:
                assert_never(transport)


if __name__ == "__main__":
    raise SystemExit(main())
