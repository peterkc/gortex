#!/usr/bin/env python3
"""Frozen v9 fresh-store attempts on Darwin; stdlib only."""

import argparse
from collections.abc import Mapping, Sequence
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import signal
import sqlite3
import subprocess
import sys
import time
import uuid


SPEC = importlib.util.spec_from_file_location(
    "storage_native_driver", Path(__file__).parents[1] / "pod/run.py")
native = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = native
SPEC.loader.exec_module(native)
BUILD = "1ef632d25f179d63a8f0d5039ddc09ef2a25a63b"
VERSION = "v0.64.11-dev-storage+1ef632d2"
SHA = "b7f8061fc441d5070b2b07e39e2b0d8103b97bdc7b242639bd3dcf46d942c4c2"
D6_SHA = "038316fd85301a6bb2cd3a6e809e92c04683db0984be413a8c177a87bfe49b9a"
K9 = {"apimachinery", "apiextensions-apiserver", "code-generator", "kubectl",
      "component-base", "kube-aggregator", "metrics", "cli-runtime",
      "controller-manager"}


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
    try:
        return native.persistent_path(path, parent)
    except native.FatalSession as error:
        raise Unsafe(str(error)) from error


def environment(directory: Path, *, mode: str) -> dict[str, str]:
    env = {key: str(directory / child) for key, child in (
        ("HOME", "home"), ("XDG_CONFIG_HOME", "config"),
        ("XDG_DATA_HOME", "data"), ("XDG_CACHE_HOME", "cache"),
        ("XDG_STATE_HOME", "state"), ("TMPDIR", "tmp"),
        ("TMP", "tmp"), ("TEMP", "tmp"))}
    env.update(PATH="/usr/bin:/bin", LANG="C.UTF-8", LC_ALL="C",
               GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_NOSYSTEM="1",
               GIT_TERMINAL_PROMPT="0", GIT_NO_LAZY_FETCH="1",
               GOMAXPROCS="10")
    for key, child in (("SOCKET", "d.sock"), ("PIDFILE", "d.pid"),
                       ("LOGFILE", "daemon.log"), ("STATEFILE", "d.json")):
        env["GORTEX_DAEMON_" + key] = str(directory / child)
    return env


def frozen(config: Mapping, env: Mapping, job: str) -> dict:
    scratch, spec = Path(config["scratch_root"]), config["v9"]
    paths = [inside(Path(spec[key]), scratch) for key in
             ("receipt", "binary", "linkage_file")]
    if any(not p.is_file() for p in paths):
        raise Blocked("v9 staging/receipt/linkage record missing")
    receipt, binary, linkage = paths
    manifest = native.read_json(Path(config["manifest_file"]))
    required = {str(p) for p in paths}
    if (not required <= set(manifest["jobs"].get("*", [])) |
            set(manifest["jobs"].get(job, [])) or any(
            manifest["files"].get(str(p)) != digest(p) for p in paths)
            or digest(receipt) != D6_SHA or digest(binary) != SHA):
        raise Blocked("unbound or mismatched v9 staged identity")

    def probe(argv):
        try:
            return native.native_output(argv, env)
        except native.FatalSession as error:
            raise Blocked("v9 staging probe unverified") from error

    record = native.read_json(linkage)
    if not isinstance(record, dict) or not {"receipt_sha256", "binary_sha256",
            "architecture", "linked_libraries"} <= record.keys():
        raise Blocked("incomplete v9 architecture/linkage record")
    if (record["receipt_sha256"] != D6_SHA
            or record["binary_sha256"] != SHA
            or record["architecture"] != "arm64"
            or not record["linked_libraries"]
            or probe(["/usr/bin/lipo", "-archs", str(binary)]).strip()
            != "arm64"
            or [l.strip() for l in probe(
                ["/usr/bin/otool", "-L", str(binary)]).splitlines()[1:]]
            != record["linked_libraries"]
            or probe([str(binary), "version"]).splitlines()[:1]
            != ["gortex " + VERSION]):
        raise Blocked("v9 recorded architecture/linkage/version mismatch")
    return {"receipt_sha256": D6_SHA, "binary_sha256": SHA,
            "linkage_sha256": digest(linkage)}


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
        version: {key: {"historical": value, "v9": counts[key]}
                  for key, value in previous.items() if counts[key] != value}
        for version, previous in spec["history"].items()}
    return counts


class Attempt:
    """Own one foreground daemon and all short-lived control clients."""

    def __init__(self, directory: Path, deadline: float, *, mode: str,
                 root: Path, config: Mapping, swapouts: int):
        self.directory, self.deadline = directory, deadline
        self.root, self.config, self.birth = root, config, None
        self.binary = inside(Path(config["v9"]["binary"]),
                             Path(config["scratch_root"]))
        inside(directory, root)
        self.env = environment(directory, mode=mode)
        (root / "v9").mkdir(exist_ok=True)
        self.env["GORTEX_DAEMON_SOCKET"] = str(root / "v9" /
                                           (directory.name + ".sock"))
        self.daemon = None
        self.samples = []
        self.serial = 0
        self.last_sample, self.swapouts = 0.0, swapouts
        for key in ("HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
                    "XDG_CACHE_HOME", "XDG_STATE_HOME", "TMPDIR"):
            Path(self.env[key]).mkdir(parents=True)
        write(directory / "cpu.json", {
            "cpu_scope": "M1 Max 8P+2E; no affinity claim",
            "GOMAXPROCS": self.env["GOMAXPROCS"],
        })

    def observe(self):
        now = time.time()
        native.window_limit(native.read_json(Path(self.config["window_file"])),
                            digest(Path(self.config["manifest_file"])), now)
        if now >= self.deadline:
            raise WorkFailed("absolute work deadline; incomplete output")
        if self.daemon is not None and self.daemon.poll() is not None:
            raise WorkFailed("owned daemon exited unexpectedly")
        if not self.samples or now - self.last_sample >= 5:
            sample = native.native_resources(self.env)
            self.samples.append({**sample, "time": now,
                                 "pid": None, "rss_bytes": 0})
            write(self.directory / "resources.json", self.samples)
            if len(self.samples) > 1 and (
                    sample["swapouts"] < self.samples[-2]["swapouts"]):
                raise native.FatalSession("native swapout counter reset")
            native.resource_limit(sample, 0, self.swapouts, admission=False)
            if self.daemon is not None:
                usage = native.darwin_usage(self.daemon.pid)
                if usage is None or usage.proc_start_abstime != self.birth:
                    raise Unsafe("private daemon birth changed or unverified")
                self.samples[-1].update(pid=self.daemon.pid,
                                        rss_bytes=usage.resident_size)
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
        return self.command([str(self.binary), *args])

    def start(self, phase: str, identity: Mapping):
        sock = Path(self.env["GORTEX_DAEMON_SOCKET"])
        pidfile = Path(self.env["GORTEX_DAEMON_PIDFILE"])
        if sock.exists() or pidfile.exists() or len(str(sock).encode()) >= 104:
            raise Unsafe("existing socket/PID or unsafe socket length")
        request = {**identity, "phase": phase, "nonce": uuid.uuid4().hex,
                   "set": self.directory.name, "version": VERSION,
                   "resolver_semantics": 6,
                   "paths": self.env, "binary": str(self.binary),
                   "build": BUILD,
                   "binary_sha256": SHA, "deadline": self.deadline}
        base = self.root / "control/v9-go" / (
            f"{self.directory.parent.name}-{phase}-{request['nonce']}")
        request_path = base.with_suffix(".request.json")
        answer_path = base.with_suffix(".go.json")
        if request_path.exists() or answer_path.exists():
            raise Unsafe("existing approval request; no replay")
        write(request_path, request)
        while not answer_path.exists():
            if time.time() >= self.deadline:
                raise Blocked("no lead go within block deadline")
            time.sleep(0.2)
        answer = json.loads(answer_path.read_text())
        if not go_matches(request, answer):
            raise Blocked("approval mismatch")
        write(self.directory / (phase + "-go.json"),
              {"request": request, "answer": answer})
        self.observe()
        native.daemon_off(native.read_json(Path(
            self.config["identities_file"])), self.env)
        log = self.directory / "daemon.log"
        with log.open("a") as output:
            self.daemon = subprocess.Popen(
                [str(self.binary), "daemon", "start", "--backend", "sqlite",
                 "--backend-path", str(self.directory / "store.sqlite"),
                 "--embeddings=false", "--no-progress"], env=self.env,
                cwd=self.directory,
                stdout=output, stderr=subprocess.STDOUT)
        usage = native.darwin_usage(self.daemon.pid)
        if usage is None:
            raise Unsafe("missing private daemon birth")
        self.birth = usage.proc_start_abstime
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
        texts = native.native_output(
            ["/usr/sbin/lsof", "-nP", "-d", "txt", "-Fn"], self.env,
            absent_ok=True)
        if any(Path(line[1:].removesuffix(" (deleted)")).resolve()
               == self.binary for line in texts.splitlines()
               if line.startswith("n/")):
            raise Unsafe("private binary process survived shutdown")
        sockets = native.native_output(
            ["/usr/sbin/lsof", "-nP", "-U", "-Fn"], self.env, absent_ok=True)
        if self.env["GORTEX_DAEMON_SOCKET"] in sockets:
            raise Unsafe("private socket holder survived shutdown")
        envs = native.native_output(
            ["/bin/ps", "-Eww", "-U", str(os.getuid()), "-o", "command="], {})
        if f"GORTEX_DAEMON_SOCKET={self.env['GORTEX_DAEMON_SOCKET']}" in envs:
            raise Unsafe("private marker process survived shutdown")
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
        inputs = Path(attempt.config["inputs"]) / "batch-b"
        source = inside(inputs / set_id / record["label"], inputs)
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
            "reason_code": getattr(error, "reason_code", type(error).__name__),
            "reason": str(error),
            "clean_handoff": not isinstance(error, (
                Unsafe, native.FatalSession))}


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
    for field in ("source-pin", "config-pin", "input-pin", "receipt",
                  "root", "native-config"):
        parser.add_argument("--" + field, required=True)
    args = parser.parse_args()
    identity = {k: getattr(args, k) for k in
                ("source_pin", "config_pin", "input_pin")}
    config = json.loads(Path(args.native_config).read_text())
    root = inside(Path(args.root), Path(config["scratch_root"]))
    receipt = inside(Path(args.receipt), root)
    directory = receipt.parent / "attempt"
    # Existing attempts are never overwritten, including a partial attempt.
    if not claim_attempt(directory, receipt):
        return 1
    attempt = None
    outcome = {"state": "completed", "exit_code": 0, "reason_code": "",
               "reason": "", "source_build": BUILD,
               "peak_rss_method": "Darwin daemon RSS every 5s; lower bound"}
    try:
        write(directory / "attempt.json", identity)
        for sig in (signal.SIGTERM, signal.SIGINT):
            signal.signal(sig, interrupted)
        now = time.time()
        go = native.read_json(Path(config["window_file"]))
        remaining = native.window_limit(
            go, digest(Path(config["manifest_file"])), now)
        deadline = now + min(remaining, native.CASE_MINUTES[
            "smoke" if args.mode == "smoke" else "baseline"] * 60) - 60
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
        identity["v9"] = frozen(config, environment(directory, mode=args.mode),
                                receipt.parent.name)
        if args.mode == "baseline_a7":
            smoke_path = inside(Path(config["v9"]["smoke_receipt"]), root)
            if not smoke_path.is_file():
                raise Blocked("v9 smoke receipt missing")
            smoke = json.loads(smoke_path.read_text())
            if (smoke["state"] != "completed" or not smoke["clean_handoff"]
                    or any(smoke.get(k) != v for k, v in identity.items())
                    or not smoke["files"] or any(
                        digest(inside(smoke_path.parent / p,
                                      smoke_path.parent)) != sha
                        for p, sha in smoke["files"].items())):
                raise Blocked("v9 smoke incomplete")
        specs["K"] = {**specs["K"], "history": {},
                      "repositories": [r for r in specs["K"]["repositories"]
                                       if r["label"] in K9],
                      **{key: [pair for pair in specs["K"][key]
                               if set(pair) <= K9]
                         for key in ("pairs", "runtime_pairs")}}
        selected = ("PR" if args.mode == "smoke" else
                    config["v9"]["admitted_sets"])
        if selected not in ("P", "R", "K") and args.mode != "smoke":
            raise Blocked("one fit-admitted baseline set per job required")
        outcome["selected"] = {s: "P6" if s == "P" else "R5" if s == "R"
                               else "K9" for s in selected}
        outcome["omitted"] = [s for s in "PRK" if s not in selected]
        outcome["completed_sets"] = []
        for set_id in selected:
            sample = native.native_resources(environment(directory,
                                                          mode=args.mode))
            write(directory / f"{set_id}-admission.json", sample)
            native.resource_limit(sample, 0, sample["swapouts"],
                                  admission=True)
            subdir = directory / set_id
            subdir.mkdir()
            attempt = Attempt(subdir, min(deadline, time.time() + 45 * 60),
                              mode=args.mode, root=root, config=config,
                              swapouts=sample["swapouts"])
            write(subdir / "environment.json", attempt.env)
            run_set(attempt, specs[set_id], set_id, identity)
            outcome["completed_sets"].append(set_id)
    except Exception as error:
        outcome.update(failure(error))
    finally:
        code = finish(receipt.parent, directory, receipt, identity,
                      attempt, outcome)
    return code


if __name__ == "__main__":
    raise SystemExit(main())
