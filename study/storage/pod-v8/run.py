#!/usr/bin/env python3
"""Frozen v8 fresh-store attempts. Linux Python 3.12; stdlib only."""

import argparse
from collections.abc import Mapping, Sequence
from datetime import datetime
import hashlib
import json
import os
from pathlib import Path
import signal
import sqlite3
import subprocess
import time
import uuid


BUILD = "21baab3b1781e06b3cf74851cad2444c843aaf4b"
VERSION = "v0.64.10-dev-storage+21baab3b"
BINARY = Path("/work/inputs/v8/gortex-21baab3b-linux-amd64")
SHA = "e8c29b5dfcfcae46db8b40d05e2ea00fd6543e418c36529dfccdf6d76c3b5c5d"


class Rejected(RuntimeError):
    """A clean measurement rejection, never an implicit retry."""


class Blocked(Rejected):
    """No effects admitted; the dependent attempt remains blocked."""


class Unsafe(RuntimeError):
    """Isolation or unverified cleanup failure: route directly to A9."""


class WorkFailed(RuntimeError):
    """Block-local interruption; cleanup verification decides handoff."""


def digest(path: Path) -> str:
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def write(path: Path, value: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, indent=2) + "\n")
    temporary.replace(path)


def inside(path: Path, parent: Path) -> Path:
    if not path.is_absolute() or not path.resolve().is_relative_to(parent):
        raise Unsafe(f"path escapes {parent}: {path}")
    if any(part.is_symlink() for part in (path, *path.parents)):
        raise Unsafe(f"symlinked path: {path}")
    return path


def environment(directory: Path) -> dict[str, str]:
    env = {key: str(directory / child) for key, child in (
        ("HOME", "home"), ("XDG_CONFIG_HOME", "config"),
        ("XDG_DATA_HOME", "data"), ("XDG_CACHE_HOME", "cache"),
        ("XDG_STATE_HOME", "state"), ("TMPDIR", "tmp"))}
    env.update(PATH="/usr/bin:/bin", LANG="C.UTF-8", LC_ALL="C",
               GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_NOSYSTEM="1",
               GIT_TERMINAL_PROMPT="0", GIT_NO_LAZY_FETCH="1",
               GOMAXPROCS="30")
    for key, child in (("SOCKET", "d.sock"), ("PIDFILE", "d.pid"),
                       ("LOGFILE", "daemon.log"), ("STATEFILE", "d.json")):
        env["GORTEX_DAEMON_" + key] = str(directory / child)
    return env


def go_matches(request: Mapping, answer: Mapping) -> bool:
    return (answer.get("lead_go") is True
            and answer.get("pi_gortex_notified") is True
            and all(answer.get(k) == v for k, v in request.items()))


def ready(status: Mapping, pid: int, sock: str,
          prefixes: Sequence[str]) -> bool:
    if (status.get("pid") != pid or status.get("socket_path") != sock
            or status.get("version") != VERSION):
        raise Unsafe("wrong daemon identity/reachability")
    actual = [r["prefix"] for r in status.get("tracked_repos", [])]
    return (sorted(actual) == sorted(prefixes)
            and status.get("ready") is True
            and status.get("enrichment_complete") is True
            and status.get("enrichment", {}).get("running") is False
            and status.get("build_lane", {}).get("active") is False
            and status.get("build_lane", {}).get("background_queued") == 0
            and status.get("build_lane", {}).get("interactive_queued") == 0)


def measure(db: sqlite3.Connection, spec: Mapping, set_id: str) -> dict:
    def rows(sql, args=()):
        return [list(row) for row in db.execute(sql, args)]
    counts = {table: rows(f"SELECT count(*) FROM {table}")[0][0]
              for table in ("nodes", "edges", "files", "file_index_failures")}
    counts["edges_by_kind"] = dict(rows(
        "SELECT kind,count(*) FROM edges GROUP BY kind"))
    counts["co_change"] = counts["edges_by_kind"].get("co_change", 0)
    counts["nodes_by_repo"] = dict(rows(
        "SELECT repo_prefix,count(*) FROM nodes GROUP BY repo_prefix"))
    cross = (" FROM edges e JOIN nodes a ON a.id=e.from_id AND "
             "a.view_gen=e.view_gen JOIN nodes b ON b.id=e.to_id AND "
             "b.view_gen=e.view_gen WHERE a.repo_prefix!='' AND "
             "b.repo_prefix!='' AND a.repo_prefix!=b.repo_prefix")
    counts["cross"] = rows("SELECT e.kind,e.confidence,e.cross_repo,count(*)"
                           + cross + " GROUP BY 1,2,3 ORDER BY 1,2,3")
    counts["owned_cross_total"] = sum(row[3] for row in counts["cross"])
    pairs = rows("SELECT a.repo_prefix,b.repo_prefix,e.kind,count(*)" + cross
                 + " GROUP BY 1,2,3 ORDER BY 1,2,3")
    by_pair = {(a, b, kind): n for a, b, kind, n in pairs}
    counts["coverage"] = [{"source": a, "target": b,
        "runtime": [a, b] in spec["runtime_pairs"],
        **{k: by_pair.get((set_id + "-" + a, set_id + "-" + b, k), 0)
           for k in ("imports", "calls")}} for a, b in spec["pairs"]]
    active = rows("SELECT a.generation_id,g.state,g.node_count FROM "
                  "analysis_active_generation a JOIN analysis_generations g "
                  "USING(generation_id) WHERE a.view_gen=0 AND g.view_gen=0 "
                  "AND a.slot=1")
    counts["active"] = active
    exact = False
    if len(active) == 1:
        gen, state, total = active[0]
        missing = rows("SELECT id FROM nodes WHERE view_gen=0 EXCEPT "
                       "SELECT node_id FROM analysis_nodes "
                       "WHERE generation_id=? LIMIT 1", (gen,))
        extra = rows("SELECT node_id FROM analysis_nodes WHERE "
                     "generation_id=? EXCEPT SELECT id FROM nodes "
                     "WHERE view_gen=0 LIMIT 1", (gen,))
        exact = (state == 1 and total == counts["nodes"] and not missing
                 and not extra and all(set_id + "-" + r["label"]
                 in counts["nodes_by_repo"] for r in spec["repositories"]))
    counts["active_covers_all_current_nodes"] = exact
    counts["differences"] = {
        version: {key: {"historical": value, "v8": counts[key]}
                  for key, value in previous.items() if counts[key] != value}
        for version, previous in spec["history"].items()}
    return counts


class Attempt:
    """Own one foreground daemon and all short-lived control clients."""

    def __init__(self, directory: Path, deadline: float):
        self.directory, self.deadline = directory, deadline
        self.env = environment(directory)
        self.daemon = None
        self.samples = []
        self.serial = 0
        self.last_sample = 0.0
        for key in ("HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
                    "XDG_CACHE_HOME", "XDG_STATE_HOME", "TMPDIR"):
            Path(self.env[key]).mkdir(parents=True)

    def observe(self):
        now = time.time()
        if now >= self.deadline:
            raise WorkFailed("absolute work deadline; incomplete output")
        if self.daemon is not None and self.daemon.poll() is not None:
            raise WorkFailed("owned daemon exited unexpectedly")
        if self.daemon is not None and now - self.last_sample >= 5:
            status = Path(f"/proc/{self.daemon.pid}/status").read_text()
            rss = int(next(l.split()[1] for l in status.splitlines()
                           if l.startswith("VmRSS:")))
            self.samples.append({"time": now, "pid": self.daemon.pid,
                                 "rss_kib": rss,
                                 "load": os.getloadavg()})
            write(self.directory / "rss.json", self.samples)
            self.last_sample = now

    def command(self, argv: Sequence[str]) -> str:
        self.observe()
        self.serial += 1
        path = self.directory / f"command-{self.serial}.log"
        start = time.time()
        with path.open("w") as output:
            child = subprocess.Popen(argv, env=self.env, cwd=self.directory,
                                     stdout=output, stderr=subprocess.STDOUT)
            try:
                while child.poll() is None:
                    self.observe()
                    time.sleep(0.2)
            finally:
                if child.poll() is None:
                    child.kill()
                child.wait(timeout=5)
        write(path.with_suffix(".json"), {"argv": list(argv),
              "exit_code": child.returncode, "seconds": time.time() - start})
        if child.returncode:
            raise Rejected(f"command exit {child.returncode}: {path}")
        return path.read_text()

    def gx(self, *args):
        return self.command([str(BINARY), *args])

    def start(self, phase: str, identity: Mapping):
        sock = Path(self.env["GORTEX_DAEMON_SOCKET"])
        pidfile = Path(self.env["GORTEX_DAEMON_PIDFILE"])
        if sock.exists() or pidfile.exists() or len(str(sock).encode()) >= 104:
            raise Unsafe("existing socket/PID or unsafe socket length")
        request = {**identity, "phase": phase, "nonce": uuid.uuid4().hex,
                   "set": self.directory.name, "version": VERSION,
                   "resolver_semantics": 6,
                   "paths": self.env, "binary": str(BINARY), "build": BUILD,
                   "binary_sha256": SHA, "deadline": self.deadline}
        base = Path("/work/control/v8-go") / (
            f"{self.directory.parent.name}-{self.directory.name}-{phase}")
        request_path = base.with_suffix(".request.json")
        answer_path = base.with_suffix(".go.json")
        if request_path.exists() or answer_path.exists():
            raise Unsafe("existing approval request; no replay")
        write(request_path, request)
        while not answer_path.exists():
            if time.time() >= self.deadline:
                raise Blocked("no lead go within block deadline")
            time.sleep(0.2)
        if not go_matches(request, json.loads(answer_path.read_text())):
            raise Blocked("approval mismatch")
        self.observe()
        log = self.directory / "daemon.log"
        with log.open("a") as output:
            self.daemon = subprocess.Popen(
                [str(BINARY), "daemon", "start", "--backend", "sqlite",
                 "--backend-path", str(self.directory / "store.sqlite"),
                 "--embeddings=false", "--no-progress"], env=self.env,
                cwd=self.directory,
                stdout=output, stderr=subprocess.STDOUT)
        while not sock.exists():
            self.observe()
            time.sleep(0.2)
        status = json.loads(self.gx("daemon", "status", "--format", "json"))
        ready(status, self.daemon.pid, str(sock), [])

    def stop(self):
        exceeded = False
        # Signal only our actual Popen child; never trust a foreign PID file.
        if self.daemon is not None:
            if self.daemon.poll() is None:
                self.daemon.terminate()
            try:
                self.daemon.wait(timeout=30)
            except subprocess.TimeoutExpired:
                self.daemon.kill()
                self.daemon.wait(timeout=2)
                exceeded = True
            self.daemon = None
        for proc in Path("/proc").iterdir():
            try:
                if (not proc.name.isdigit() or int(proc.name) == os.getpid()
                        or proc.stat().st_uid != os.getuid()):
                    continue
                env = (proc / "environ").read_bytes().split(b"\0")
            except FileNotFoundError:
                continue
            except PermissionError:
                # Only a non-dumpable process hides environ from its own
                # uid. Our daemon tree runs no setuid binary, so it is not
                # one of ours (seen with a CI runner helper process).
                continue
            marker = ("GORTEX_DAEMON_SOCKET="
                      + self.env["GORTEX_DAEMON_SOCKET"]).encode()
            if marker in env:
                raise Unsafe(f"owned process survived: {proc.name}")
        if (Path(self.env["GORTEX_DAEMON_SOCKET"]).exists()
                or Path(self.env["GORTEX_DAEMON_PIDFILE"]).exists()):
            raise Unsafe("owned socket/PID survived shutdown")
        if exceeded:
            raise Rejected("daemon exceeded graceful shutdown bound; reaped")

    def wait_ready(self, prefixes: Sequence[str]) -> dict:
        while True:
            status = json.loads(self.gx(
                "daemon", "status", "--format", "json"))
            if ready(status, self.daemon.pid,
                     self.env["GORTEX_DAEMON_SOCKET"], prefixes):
                return status
            time.sleep(1)

    def snapshot(self, label: str, spec: Mapping, set_id: str) -> dict:
        path = self.directory / (label + ".sqlite")
        source = self.directory / "store.sqlite"
        with sqlite3.connect(f"file:{source}?mode=ro", uri=True) as live:
            with sqlite3.connect(path) as copy:
                live.backup(copy, pages=1000,
                            progress=lambda *_: self.observe())
        with sqlite3.connect(f"file:{path}?mode=ro", uri=True) as copy:
            copy.set_progress_handler(lambda: self.observe(), 10000)
            result = measure(copy, spec, set_id)
        write(self.directory / (label + ".json"), result)
        return result


def run_set(attempt: Attempt, spec: Mapping, set_id: str,
            identity: Mapping) -> None:
    repos = []
    for record in spec["repositories"]:
        source = inside(Path("/work/inputs/batch-b") / set_id
                        / record["label"], Path("/work/inputs/batch-b"))
        target = attempt.directory / "repos" / record["label"]
        attempt.command(["git", "clone", "--no-hardlinks", str(source),
                         str(target)])
        for ref in ("HEAD", record["tag"] + "^{commit}"):
            actual = attempt.command(["git", "-C", str(target),
                                      "rev-parse", ref]).strip()
            if actual != record["commit"]:
                raise Rejected("inventory commit/tag mismatch")
        if attempt.command(["git", "-C", str(target), "rev-parse",
                            "--is-shallow-repository"]).strip() != "false":
            raise Rejected("shallow source history")
        if attempt.command(["git", "-C", str(target), "status",
                            "--porcelain"]).strip():
            raise Rejected("dirty source clone")
        (target / ".gortex.yaml").write_text(f"workspace: study-{set_id}\n")
        with (target / ".git/info/exclude").open("a") as exclude:
            exclude.write("\n/.gortex.yaml\n/.gortex/\n")
        repos.append(target)
    attempt.start("start", identity)
    for target in repos:
        attempt.gx("track", str(target), "--name", set_id + "-" + target.name,
                   "--wait", "--wait-timeout", "45m", "--no-progress")
    prefixes = [set_id + "-" + r.name for r in repos]
    attempt.wait_ready(prefixes)
    attempt.snapshot("before-restart", spec, set_id)
    attempt.stop()
    attempt.start("restart", identity)
    attempt.wait_ready(prefixes)
    after = attempt.snapshot("after-warmup", spec, set_id)
    if not after["active_covers_all_current_nodes"]:
        log = attempt.directory / "daemon.log"
        start = log.stat().st_size
        attempt.gx("call", "get_communities", "--index", str(repos[0]),
                   "--arg", "id=community-0", "--format", "json")
        while '"msg":"mcp: analysis pass complete"' not in (
                log.read_text()[start:]):
            attempt.observe()
            time.sleep(1)
        attempt.wait_ready(prefixes)
        after = attempt.snapshot("after-analysis", spec, set_id)
    attempt.wait_ready(prefixes)
    attempt.stop()
    after = attempt.snapshot("final", spec, set_id)
    logs = [json.loads(line) for line in
            (attempt.directory / "daemon.log").read_text().splitlines()
            if line.startswith("{")]
    warmups = [r for r in logs if r.get("msg") == "daemon: warmup summary"]
    errors = [r for r in logs if r.get("level") in ("error", "fatal")
              or any(s in r.get("msg", "") for s in
                     ("warmup incomplete", "warmup resolve failed",
                      "database is closed"))]
    write(attempt.directory / "native-times.json", {
        "load_final": os.getloadavg(), "warmups": warmups,
        "analysis": [r for r in logs
        if r.get("msg") == "mcp: analysis pass complete"], "errors": errors,
        "backend": {p.name: p.stat().st_size for p in
        attempt.directory.glob("store.sqlite*") if p.is_file()}})
    bridges = [r for r in after["coverage"] if r["runtime"] or set_id == "R"]
    if (errors or not warmups or after["file_index_failures"]
            or not after["active_covers_all_current_nodes"]
            or any(not r["imports"] or not r["calls"] for r in bridges)):
        raise Rejected("completion/bridge checks failed; inspect evidence")


def interrupted(signum, frame):
    raise WorkFailed(f"interrupted by signal {signum}; no replay")


def finish(root: Path, directory: Path, receipt: Path, identity: Mapping,
           attempt: Attempt | None, outcome: Mapping) -> int:
    result = {"clean_handoff": True, **identity, **outcome}
    if attempt is not None:
        try:
            attempt.stop()
        except Rejected as error:
            result.update(state="checks_failed", exit_code=1,
                          reason_code="time", reason=str(error))
        except Exception as error:
            result.update(state="partial", exit_code=1, clean_handoff=False,
                          reason_code="cleanup", reason=str(error))
    files = {}
    try:
        summary = receipt.with_name("outcome.json")
        write(summary, result)
        files[str(summary.relative_to(root))] = digest(summary)
        for current, dirs, names in os.walk(directory):
            dirs[:] = [d for d in dirs if d not in
                       {"repos", "home", "config", "cache", "data", "state",
                        "tmp"}]
            for name in names:
                path = Path(current) / name
                if path.is_file():
                    files[str(path.relative_to(root))] = digest(path)
    except Exception as error:
        result.update(state="partial", exit_code=1,
                      reason_code="output", reason=str(error))
    result["files"] = files
    write(receipt, result)
    return result["exit_code"]


def failure(error: Exception) -> dict:
    state, code = "partial", 1
    if isinstance(error, Blocked):
        state, code = "dependency_blocked", 0
    elif isinstance(error, Rejected):
        state = "checks_failed"
    return {"state": state, "exit_code": code,
            "reason_code": type(error).__name__, "reason": str(error),
            "clean_handoff": not isinstance(error, Unsafe)}


def claim_attempt(directory: Path, receipt: Path) -> bool:
    if receipt.exists() or receipt.is_symlink():
        return False
    try:
        directory.mkdir(exist_ok=False)
    except FileExistsError:
        return False
    return True


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("smoke", "baseline_a7"))
    for field in ("source-pin", "config-pin", "input-pin", "receipt"):
        parser.add_argument("--" + field, required=True)
    args = parser.parse_args()
    identity = {k: getattr(args, k) for k in
                ("source_pin", "config_pin", "input_pin")}
    directory = Path("/work/smoke" if args.mode == "smoke"
                     else "/work/baseline")
    receipt = inside(Path(args.receipt), Path("/work"))
    inside(directory, Path("/work"))
    # Existing attempts are never overwritten, including a partial attempt.
    if not claim_attempt(directory, receipt):
        return 1
    attempt = None
    outcome = {"state": "completed", "exit_code": 0, "reason_code": "",
               "reason": "", "source_build": BUILD,
               "peak_rss_method": "daemon /proc VmRSS every 5s; lower bound"}
    try:
        write(directory / "attempt.json", identity)
        for sig in (signal.SIGTERM, signal.SIGINT):
            signal.signal(sig, interrupted)
        allocation_path = Path("/work/control/allocation.json")
        allocation = json.loads(allocation_path.read_text())
        zero = datetime.fromisoformat(allocation["created_at"]).timestamp()
        minutes, latest, end = ((25, 45, 70) if args.mode == "smoke"
                                else (40, 650, 690))
        now = time.time()
        if now > zero + latest * 60:
            raise Blocked("latest-start deadline passed")
        deadline = min(now + minutes * 60, zero + end * 60) - 60
        spec_path = Path(__file__).with_name("inputs.json")
        if digest(spec_path) != args.input_pin:
            raise Rejected("input manifest hash mismatch")
        if digest(Path(__file__)) != args.config_pin:
            raise Rejected("runner configuration hash mismatch")
        specs = json.loads(spec_path.read_text())
        if set(specs) != set("KPR"):
            raise Rejected("expected complete K/P/R inventory")
        for set_id, count in (("K", 12), ("P", 6), ("R", 5)):
            labels = [r["label"] for r in specs[set_id]["repositories"]]
            if len(labels) != count or len(set(labels)) != count:
                raise Rejected("missing/duplicate inventory member")
            if any(not label or "/" in label or label in (".", "..")
                   for label in labels):
                raise Rejected("unsafe inventory member")
        if not BINARY.is_file():
            raise Rejected("frozen Linux binary missing")
        if digest(inside(BINARY, Path("/work/inputs"))) != SHA:
            raise Rejected("frozen binary SHA-256 mismatch")
        if args.mode == "baseline_a7":
            smoke_path = Path("/work/results/smoke/v8-smoke/receipt.json")
            if not smoke_path.is_file():
                raise Blocked("v8 smoke receipt missing")
            smoke = json.loads(smoke_path.read_text())
            if (smoke["state"] != "completed" or not smoke["clean_handoff"]
                    or any(smoke.get(k) != v for k, v in identity.items())):
                raise Blocked("v8 smoke incomplete")
        for set_id in ("PR" if args.mode == "smoke" else "KPR"):
            subdir = directory / set_id
            subdir.mkdir()
            attempt = Attempt(subdir, min(deadline, time.time() + 45 * 60))
            if VERSION not in attempt.gx("version"):
                raise Rejected("frozen binary version mismatch")
            write(subdir / "environment.json", attempt.env)
            run_set(attempt, specs[set_id], set_id, identity)
    except Exception as error:
        outcome.update(failure(error))
    finally:
        code = finish(Path("/work"), directory, receipt, identity,
                      attempt, outcome)
    return code


if __name__ == "__main__":
    raise SystemExit(main())
