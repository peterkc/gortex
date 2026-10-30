"""Run vector experiments. The default only prints a plan."""

import argparse
from dataclasses import dataclass
import hashlib
import importlib.util
import fcntl
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import time
from typing import Literal, assert_never, cast

ROOT = Path(__file__).resolve().parent
SCRATCH = Path.home() / ".prime/research/gortex/storage-architecture/tmp"
WORK = SCRATCH / "vector/hybrid-followup"
REPOS = ("beads", "pi_agent_rust", "prime-agent")
Kind = Literal["accuracy", "preparation", "timing"]
Mode = Literal["accuracy", "timing", "latency"]


@dataclass(frozen=True, slots=True)
class Job:
    name: str
    command: tuple[str, ...]
    artifact: Path | None
    kind: Kind


def dataset_dirs() -> tuple[Path, ...]:
    return tuple(WORK / "real-10k-export" / repo for repo in REPOS) + (
        WORK / "real-100k-export/beads",
    )


def accuracy_jobs(ann: Path) -> list[Job]:
    jobs = []
    for directory in dataset_dirs():
        label = directory.parent.name + "-" + directory.name
        base = (str(ann), "-dataset", str(directory))
        jobs.append(Job(label + "-truth", base + ("-phase", "truth"),
                        directory / "truth.json", "accuracy"))
        jobs.append(Job(label + "-int8-build",
                        base + ("-phase", "int8-build"),
                        directory / "int8.bin", "accuracy"))
        jobs.append(Job(label + "-int8-query",
                        base + ("-phase", "query", "-method", "int8"),
                        directory / "result-int8-M16-ef512-k128-rep1.json",
                        "accuracy"))
        for m in (16, 32):
            for repeat in range(1, 6):
                for method in ("f32", "i8"):
                    if (method == "f32"
                            and "real-10k-export" in directory.parts
                            and directory.name == "beads"
                            and m == 16 and repeat != 1):
                        continue
                    config = ("-m", str(m), "-repeat", str(repeat),
                              "-method", method)
                    build_dir = directory / f"native-M{m}-{method}-rep{repeat}"
                    if (method == "i8"
                            or "real-100k-export" in directory.parts
                            or (m == 16 and repeat != 1)):
                        jobs.append(Job(
                            f"{label}-{m}-{method}-{repeat}-build",
                            base + ("-phase", "build") + config,
                            build_dir / "build.json", "accuracy",
                        ))
                    for ef in (512, 1024):
                        candidate_counts = ((10,) if method == "f32"
                                            else (50, 100, 200))
                        for k in candidate_counts:
                            flags = config + ("-ef", str(ef))
                            if method == "i8":
                                flags += ("-k", str(k))
                            name = (f"{label}-{m}-{method}-{repeat}"
                                    f"-e{ef}-k{k}")
                            result = directory / (
                                f"result-{method}-M{m}-ef{ef}-k{k}"
                                f"-rep{repeat}.json"
                            )
                            jobs.append(Job(name, base + ("-phase", "query")
                                            + flags, result, "accuracy"))
    return jobs


def timing_jobs(timing: Path) -> list[Job]:
    jobs = []
    directory = WORK / "timing-data"
    for dimensions in (384, 1024):
        for n in (10000, 100000, 1000000):
            base = (str(timing), "-n", str(n), "-dimensions", str(dimensions),
                    "-data-dir", str(directory))
            label = f"n{n}-d{dimensions}"
            jobs.append(Job(label + "-build", base + ("-phase", "build"),
                            directory / f"int8-n{n}-d{dimensions}.bin",
                            "preparation"))
            for repeat in range(1, 4):
                for workers in (1, 4, 8):
                    name = f"{label}-w{workers}-rep{repeat}"
                    flags = ("-phase", "query", "-workers", str(workers),
                             "-repeat", str(repeat), "-queries", "256")
                    jobs.append(Job(name, base + flags,
                                    directory / f"{name}.json", "timing"))
    return jobs


def latency_jobs(ann: Path, generated: bool = False) -> list[Job]:
    jobs = []
    directories = ([WORK / f"generated-n{n}" for n in (10000, 100000)]
                   if generated else dataset_dirs())
    for directory in directories:
        label = directory.parent.name + "-" + directory.name
        if generated:
            prep = (str(ann), "-dataset", str(directory))
            n = directory.name.removeprefix("generated-n")
            for suffix, flags, artifact in (
                ("generate", ("-phase", "generate", "-n", n),
                 "input-receipt.json"),
                ("int8", ("-phase", "int8-build"), "int8.bin"),
                ("native", ("-phase", "build", "-m", "32"),
                 "native-M32-f32-rep1/build.json"),
            ):
                jobs.append(Job(label + "-" + suffix, prep + flags,
                                directory / artifact, "preparation"))
        base = (str(ann), "-dataset", str(directory), "-phase", "latency")
        methods = [("exact", 16, 512, 10, 1)]
        methods += [("int8", 16, 512, 128, workers) for workers in (1, 4, 8)]
        methods += [(method, m, ef, k, 1)
                    for method in ("f32", "i8") for m in (16, 32)
                    for ef in (512, 1024)
                    for k in ((10,) if method == "f32" else (50, 100, 200))]
        if generated:
            methods = [recipe for recipe in methods
                       if recipe[0] in ("exact", "int8")
                       or recipe == ("f32", 32, 1024, 10, 1)]
        for method, m, ef, k, workers in methods:
            if generated and method == "exact":
                continue
            flags = ("-method", method, "-m", str(m), "-ef", str(ef),
                     "-workers", str(workers))
            if method == "i8":
                flags += ("-k", str(k))
            for repeat in range(1, 4):
                name = (f"{label}-{method}-m{m}-e{ef}-k{k}-w{workers}"
                        f"-block{repeat}")
                if generated:
                    jobs.append(Job(name + "-reference", base + (
                        "-method", "exact", "-workers", "1"), None, "timing"))
                jobs.append(Job(name, base + flags, None, "timing"))
    return jobs


def file_digest(path: Path) -> str:
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def source_fingerprint() -> str:
    paths = runtime_paths() + [Path(__file__).resolve()]
    digest = hashlib.sha256()
    for path in sorted(paths):
        digest.update(str(path.relative_to(ROOT)).encode())
        digest.update(path.read_bytes())
    return digest.hexdigest()


def parse_mac_memory(output: str) -> dict:
    lines = output.splitlines()
    levels = [int(line.split(":", 1)[1]) for line in lines
              if line.startswith("kern.memorystatus_level:")]
    header = next(i for i, line in enumerate(lines)
                  if line.split() and line.split()[-1] == "swapouts")
    rows = [line.split() for line in lines[header + 1:]
            if line.strip() and not line.startswith("kern.")
            and len(line.split()) == len(lines[header].split())]
    if len(levels) != 2 or len(rows) != 2:
        raise RuntimeError("unrecognized Mac memory measurement")
    return {"levels": levels, "swapouts_delta_30s": rows[-1][-1],
            "passes": min(levels) >= 30 and rows[-1][-1] == "0"}


def mac_memory_gate(path: Path) -> dict:
    outputs = []
    for command in (["sysctl", "kern.memorystatus_level"],
                    ["vm_stat", "-c", "2", "30"],
                    ["sysctl", "kern.memorystatus_level"]):
        outputs.append(subprocess.check_output(command, text=True))
    output = "".join(outputs)
    reading = {**parse_mac_memory(output), "raw": output}
    path.write_text(json.dumps(reading, indent=2) + "\n")
    if not reading["passes"]:
        raise RuntimeError("Mac memory rule failed; accuracy stopped")
    return reading


def mac_memory_retry(directory: Path, name: str) -> dict:
    # Initial window plus at most six rechecks, five minutes apart.
    window_id = time.time_ns()
    for attempt in range(7):
        path = directory / (name + f"-memory-{window_id}-a{attempt}.json")
        try:
            return mac_memory_gate(path)
        except RuntimeError as error:
            if str(error) != "Mac memory rule failed; accuracy stopped":
                raise
            if attempt == 6:
                raise
            # Observe the full five-minute interval instead of polling.
            output = subprocess.check_output(
                ["vm_stat", "-c", "2", "300"], text=True)
            (directory / (name + f"-wait-{window_id}-a{attempt}.log"
                          )).write_text(output)
    raise AssertionError("memory retry loop did not terminate")


def compatible_prior(prior: dict, configuration: dict) -> bool:
    adoption = json.loads((WORK / "orchestration-adoption.json").read_text())
    accepted = adoption.get("old_signatures", [adoption["old_signature"]])
    if prior["source_fingerprint"] not in accepted:
        return False
    previous = {k: v for k, v in prior["configuration"].items()
                if k != "source"}
    current = {k: v for k, v in configuration.items() if k != "source"}
    if previous != current:
        return False
    expected = adoption["runtime_artifacts"]
    actual = {str(path.relative_to(ROOT)): file_digest(path)
              for path in runtime_paths()}
    return actual == expected


def runtime_paths() -> list[Path]:
    paths = [ROOT / "go.mod", ROOT / "go.sum"]
    for directory in (ROOT / "cmd", ROOT / "internal"):
        for base, dirs, files in os.walk(directory):
            dirs[:] = [name for name in dirs if name not in (".git", "cache")]
            paths += [Path(base) / name for name in files
                      if name.endswith(".go")
                      and not name.endswith("_test.go")]
    return paths


def build(binary: Path, mode: Mode, native: Path) -> None:
    if shutil.which("go") is None:
        raise RuntimeError("Go1.27.1 is required; no toolchain is installed")
    environment = dict(os.environ)
    environment["GOMAXPROCS"] = "2"
    environment["GOTOOLCHAIN"] = "local"
    environment["GOWORK"] = "off"
    for key, suffix in (("GOCACHE", "cache/go-build"),
                        ("GOMODCACHE", "cache/go-mod"),
                        ("TMPDIR", "cache/tmp")):
        path = SCRATCH / suffix
        path.mkdir(parents=True, exist_ok=True)
        environment[key] = str(path)
    if mode != "timing":
        if not (native / "usearch.h").is_file():
            raise RuntimeError("stage the pinned native header; see README")
        libraries = ("libusearch_c.a", "libusearch_c.so")
        if sys.platform == "darwin":
            libraries = ("libusearch_c.a", "libusearch_c.dylib")
        if not any((native / name).is_file() for name in libraries):
            raise RuntimeError("stage the native library for this CPU/OS")
        environment["CGO_ENABLED"] = "1"
        environment["CGO_CFLAGS"] = "-I" + str(native)
        runtime_flags = "-lstdc++ -lm -pthread"
        if sys.platform == "darwin":
            runtime_flags = ("-lc++ -framework CoreFoundation "
                             "-framework Security")
        environment["CGO_LDFLAGS"] = (
            f"-L{native} -Wl,-rpath,{native} " + runtime_flags
        )
    else:
        environment["CGO_ENABLED"] = "0"
    binary.parent.mkdir(parents=True, exist_ok=True)
    package = "./cmd/timing" if mode == "timing" else "./cmd/ann"
    subprocess.run(["go", "build", "-p=1", "-o", str(binary), package],
                   cwd=ROOT, env=environment, check=True)


def generated_input_sha(job: Job) -> str | None:
    if "-n" in job.command:
        return None
    folder = Path(job.command[job.command.index("-dataset") + 1])
    receipt = folder / "input-receipt.json"
    fingerprints = json.loads(receipt.read_text())
    for name in ("document.f32", "query.f32", "records.json",
                 "query-records.json"):
        if file_digest(folder / name) != fingerprints[name]["sha256"]:
            raise RuntimeError("generated input hash drift")
    return file_digest(receipt)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("accuracy", "timing", "latency"))
    parser.add_argument("--execute", action="store_true")
    parser.add_argument("--generated", action="store_true")
    parser.add_argument("--native-dir", type=Path,
                        default=SCRATCH / "native/usearch")
    parser.add_argument("--run-id", default="run-1")
    parser.add_argument("--limit", type=int, default=0)
    parser.add_argument("--jobs-file", type=Path)
    parser.add_argument("--output-dir", type=Path)
    args = parser.parse_args()
    mode = cast(Mode, args.mode)
    if args.generated and mode != "latency":
        parser.error("--generated is a latency-only input mode")
    if (not args.run_id or len(args.run_id) > 100
            or any(char not in "abcdefghijklmnopqrstuvwxyz"
                   "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_."
                   for char in args.run_id)):
        parser.error("run-id must contain only letters, digits, - _ .")
    if args.limit < 0:
        parser.error("limit must be nonnegative")
    architecture = "arm64" if platform.machine() in ("arm64", "aarch64") else (
        platform.machine()
    )
    if sys.platform == "linux" and architecture in ("x86_64", "amd64"):
        architecture = "amd64"
    binary = WORK / "bin" / (sys.platform + "-" + architecture) / (
        "timing" if mode == "timing" else "ann"
    )
    match mode:
        case "accuracy":
            jobs = accuracy_jobs(binary)
        case "timing":
            jobs = timing_jobs(binary)
        case "latency":
            jobs = latency_jobs(binary, args.generated)
        case _:
            assert_never(mode)
    if args.jobs_file is not None:
        names = json.loads(args.jobs_file.read_text())
        by_name = {job.name: job for job in jobs}
        if (not isinstance(names, list)
                or not all(isinstance(name, str) for name in names)
                or len(set(names)) != len(names)
                or any(name not in by_name for name in names)):
            parser.error("jobs-file must list unique exact planned job names")
        jobs = [by_name[name] for name in names]
    telemetry = None
    if args.execute and mode != "accuracy":
        if (sys.platform != "darwin" or architecture != "arm64"
                or (mode == "latency" and not args.generated)
                or not jobs or any("1000000" in job.command for job in jobs)):
            parser.error("selected generated darwin-arm64 recipes required")
        if mode == "latency" and not (
                (args.native_dir / "usearch.h").is_file() and any(
                    (args.native_dir / name).is_file()
                    for name in ("libusearch_c.a", "libusearch_c.dylib"))):
            parser.error("missing pinned native header/library")
        spec = importlib.util.spec_from_file_location(
            "storage_telemetry", ROOT.parents[1] / "pod/run.py")
        telemetry = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = telemetry
        spec.loader.exec_module(telemetry)
    directory = args.output_dir or WORK / "runs" / mode / args.run_id
    if args.output_dir and mode == "timing":
        jobs = [Job(j.name, j.command + ("-output", str(
            directory / (j.name + ".result.json"))),
            directory / (j.name + ".result.json"), j.kind)
            if j.kind == "timing" else j for j in jobs]
    directory.mkdir(parents=True, exist_ok=True)
    plan = [{"name": job.name, "command": job.command, "kind": job.kind,
             "artifact": str(job.artifact) if job.artifact else None}
            for job in jobs]
    plan_name = ("jobs.json" if args.jobs_file is None
                 else "jobs-" + args.jobs_file.stem + ".json")
    (directory / plan_name).write_text(json.dumps(plan, indent=2) + "\n")
    if not args.execute:
        print(json.dumps({"execute": False, "jobs": len(jobs),
                          "timing_blocks": sum(job.kind == "timing"
                                               for job in jobs),
                          "plan": str(directory.relative_to(WORK))
                          + "/jobs.json"}))
        return
    temporary = SCRATCH / "cache/tmp"
    temporary.mkdir(parents=True, exist_ok=True)
    os.environ["TMPDIR"] = str(temporary)
    with (WORK / "execution.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        build(binary, mode, args.native_dir.resolve())
        configuration = {"source": source_fingerprint(),
                         "go_version": subprocess.check_output(
                             ["go", "version"], text=True).strip(),
                         "cpu_count": os.cpu_count(),
                         "runtime_environment": {
                         key: os.environ.get(key) for key in
                         ("GOMAXPROCS", "GOGC", "GOMEMLIMIT", "GODEBUG",
                          "GOFLAGS")}}
        if mode != "timing":
            native_files = {}
            for name in ("usearch.h", "libusearch_c.a", "libusearch_c.so",
                         "libusearch_c.dylib"):
                path = args.native_dir.resolve() / name
                if path.is_file():
                    native_files[name] = file_digest(path)
            configuration["native_files"] = native_files
            configuration["input_receipts"] = {} if args.generated else {
                str(path.relative_to(WORK)): file_digest(path)
                for path in (directory / "input-receipt.json"
                             for directory in dataset_dirs())
            }
        if args.generated:
            configuration["generated_seed"] = 20261023
        source = hashlib.sha256(json.dumps(configuration, sort_keys=True)
                                .encode()).hexdigest()
        completed = 0
        for job in jobs:
            input_sha = generated_input_sha(job) if args.generated else None
            receipt_path = directory / (job.name + ".json")
            if receipt_path.exists():
                if job.kind == "timing":
                    raise RuntimeError("whole groups own timing reuse")
                prior = json.loads(receipt_path.read_text())
                if (args.generated and prior.get("input_receipt_sha256")
                        != input_sha):
                    raise RuntimeError("generated receipt drift")
                if (prior["source_fingerprint"] != source
                        and not compatible_prior(prior, configuration)):
                    raise RuntimeError("runner source drift; inspect evidence")
                if job.artifact is not None:
                    if (not job.artifact.exists()
                            or file_digest(job.artifact)
                            != prior["artifact_sha256"]):
                        raise RuntimeError("recorded output drift")
                if prior["exit_code"] != 0:
                    raise RuntimeError("failed job retained; inspect log")
                if job.kind == "timing" and not all(key in prior for key in
                        ("resource_before", "resource_after")):
                    raise RuntimeError("missing retained timing telemetry")
                continue
            if job.artifact is not None and job.artifact.exists():
                raise RuntimeError("unrecorded output exists; inspect first")
            if args.generated and (directory / (job.name + ".log")).exists():
                raise RuntimeError("incomplete generated job; inspect first")
            memory = None
            if mode == "accuracy" and sys.platform == "darwin":
                memory = mac_memory_retry(directory, job.name)
            before = (telemetry.native_resources(os.environ) if telemetry
                      else {"load1": os.getloadavg()[0]})
            start = time.perf_counter()
            with (directory / (job.name + ".log")).open("w") as log:
                result = subprocess.run(job.command, cwd=ROOT, stdout=log,
                                        stderr=subprocess.STDOUT, check=False)
            after = (telemetry.native_resources(os.environ) if telemetry
                     else {"load1": os.getloadavg()[0]})
            measured = None
            if result.returncode == 0 and job.kind == "timing":
                result_path = job.artifact or (directory / (job.name + ".log"))
                measured = json.loads(result_path.read_text())
            receipt = {"command": job.command, "exit_code": result.returncode,
                       "seconds": time.perf_counter() - start,
                       "load1_before": before["load1"],
                       "load1_after": after["load1"],
                       "resource_before": before, "resource_after": after,
                       "valid_timing_comparison": False,
                       "source_fingerprint": source,
                       "configuration": configuration,
                       "platform": sys.platform,
                       "architecture": architecture, "kind": job.kind,
                       "result": measured, "memory": memory,
                       "artifact_sha256": file_digest(job.artifact)
                       if job.artifact is not None and job.artifact.exists()
                       else None}
            if args.generated:
                receipt["input_receipt_sha256"] = input_sha
            temporary = receipt_path.with_suffix(".tmp")
            temporary.write_text(json.dumps(receipt, indent=2) + "\n")
            temporary.replace(receipt_path)
            print(json.dumps({"job": job.name, **receipt}), flush=True)
            if result.returncode:
                raise RuntimeError("job failed; inspect log")
            completed += 1
            if args.limit and completed >= args.limit:
                break


if __name__ == "__main__":
    main()
