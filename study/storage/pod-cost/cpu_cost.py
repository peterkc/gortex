"""Bounded Mac CodeRank CPU study; the parent driver owns admission."""
import argparse
import base64
import hashlib
import importlib.metadata
import json
import math
import os
import platform
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
import subprocess
import sys
import time


POLICIES = (("metadata", 8192, "docs_.jsonl", 5000),
            ("body", 512, "body_.jsonl", 4842),
            ("body", 2048, "body_.jsonl", 4842),
            ("body", 8192, "body_.jsonl", 4842),
            ("query", 8192, "qrows_.jsonl", 333))
PREFIX = "Represent this query for searching relevant code: "


@dataclass(frozen=True, slots=True)
class Invalid:
    reason: str


class FatalWorker(RuntimeError):
    """Unexpected worker failure; the parent must stop the session."""


class GateFailure(FatalWorker):
    reason_code = "gate"


class HostSafetyStop(FatalWorker):
    reason_code = "host-stop"


class Skip(RuntimeError):
    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code


def external_tool(name: str) -> str:
    path = Path(os.environ["STUDY_MANIFEST"])
    if digest(path) != os.environ["STUDY_MANIFEST_SHA"]:
        raise RuntimeError("tool manifest changed")
    manifest = json.loads(path.read_text())
    tool = Path(manifest["tools"][name])
    if (not tool.is_absolute() or tool.resolve() != tool
            or digest(tool) != manifest["files"].get(str(tool))):
        raise RuntimeError("unbound or changed external tool")
    return str(tool)


def digest(path: Path) -> str:
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def save(path: Path, value: object) -> None:
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("x") as stream:
        if path.exists() or path.is_symlink():
            raise FileExistsError(path)
        json.dump(value, stream, allow_nan=False)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)
    descriptor = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def matrix() -> list[dict]:
    return [{"corpus": corpus, "cap": cap, "threads": thread,
             "status": "skipped", "reason": "time", "repeats": [],
             "warmups": 0, "actual_repeats": 0}
            for corpus, cap, _, _ in POLICIES
            for thread in (1, 2, 4, 8, 16)]


def batches(encoded: Mapping[str, Sequence[Sequence[int]]],
            corpus: str) -> list[dict]:
    lengths = [len(row) for row in encoded["input_ids"]]
    order = (list(range(len(lengths))) if corpus == "query" else
             list(range(min(200, len(lengths))))
             + sorted(range(200, len(lengths)), key=lengths.__getitem__))
    result, position = [], 0
    while position < len(order):
        end = min(position + (1 if corpus == "query" else 16), len(order))
        if position < 200:
            end = min(end, 200)
        while end > position + 1 and (end - position) * max(
                lengths[i] for i in order[position:end]) ** 2 > 4_000_000:
            end -= 1
        indices = order[position:end]
        width = max(lengths[i] for i in indices)
        inputs = {name: [value for i in indices for value in
                        [*rows[i], *([0] * (width - lengths[i]))]]
                  for name, rows in encoded.items()}
        result.append({"indices": indices, "shape": [len(indices), width],
                       "inputs": inputs})
        position = end
    return result


def cosine_gate(actual: Sequence[Sequence[float]],
                expected: Sequence[Sequence[float]]) -> dict | Invalid:
    if not actual or len(actual) != len(expected):
        return Invalid("missing reference/Go rows")
    scores = []
    for left, right in zip(actual, expected):
        if (not left or len(left) != len(right)
                or not all(type(x) in (int, float) and math.isfinite(x)
                           for x in (*left, *right))):
            return Invalid("shape or nonfinite vector")
        a, b = math.hypot(*left), math.hypot(*right)
        if a == 0 or b == 0:
            return Invalid("zero vector")
        scores.append(math.fsum((x / a) * (y / b)
                                for x, y in zip(left, right)))
    return {"count": len(scores), "minimum": min(scores),
            "scores": scores, "passed": min(scores) >= 0.9999}


def child(argv: Sequence[str], log: Path, end: float,
          cwd: Path, env: Mapping[str, str]) -> int:
    remaining = end - time.monotonic()
    if remaining <= 0:
        raise Skip("time", "absolute deadline; no spawn")
    with log.open("x") as stream:
        try:
            return subprocess.run(argv, cwd=cwd, env=env, stdout=stream,
                                  stderr=subprocess.STDOUT,
                                  timeout=remaining).returncode
        except subprocess.TimeoutExpired as error:
            raise Skip("time", "child killed/reaped at deadline") from error


def completed_worker(argv: Sequence[str], output: Path, end: float,
                     cwd: Path, env: Mapping[str, str], *,
                     partial: bool = False) -> dict:
    code = child(argv, output.with_suffix(".log"), end, cwd, env)
    if code == 78:
        raise HostSafetyStop("native probe host-safety stop")
    if code not in ((0, 1) if partial else (0,)) or not output.is_file():
        raise FatalWorker(f"worker exit {code}; complete report required")
    try:
        report = json.loads(output.read_text())
    except json.JSONDecodeError as error:
        raise FatalWorker("worker report is malformed") from error
    status = "partial" if code == 1 else "completed"
    if not isinstance(report, dict) or report.get("status") != status:
        raise FatalWorker("worker report is incomplete")
    return report


def successor_check(argv: Sequence[str], log: Path, end: float,
                    cwd: Path, env: Mapping[str, str]) -> None:
    code = child(argv, log, end, cwd, env)
    if code not in (0, 1):
        raise FatalWorker(f"successor check abnormal exit {code}")
    save(log.with_suffix(".json"), {"status": "completed", "argv": list(argv),
                                  "exit_code": code, "passed": code == 0})
    if code:
        raise Skip("check", "recorded successor check failed")


def verify_library(path: Path, expected: str, root: Path) -> None:
    if not path.is_relative_to(root):
        raise FatalWorker("library escaped /work")
    if not path.is_file() or digest(path) != expected:
        raise Skip("library", "pinned shared library missing/hash mismatch")


def checked_gate(warm: Mapping, reference: Mapping, token_sha: str) -> dict:
    if (warm.get("status") != "completed"
            or reference.get("status") != "completed"
            or len(warm["vectors"]) != 200
            or len(reference["vectors"]) != 200
            or any(len(row) != 768 for row in
                   [*warm["vectors"], *reference["vectors"]])):
        raise GateFailure("invalid or incomplete 200-row gate report")
    if (reference["runtime"] != "1.31.0"
            or reference["intra_op_threads"] != 16
            or reference["inter_op_threads"] != 1):
        raise GateFailure("reference runtime/recipe mismatch")
    if (reference["token_sha256"] != token_sha
            or warm["token_file_sha256"] != token_sha):
        raise Skip("identity", "recorded gate token identity mismatch")
    gate = cosine_gate(warm["vectors"], reference["vectors"])
    if isinstance(gate, Invalid):
        raise GateFailure(gate.reason)
    if not gate["passed"]:
        raise GateFailure("recorded per-input cosine below 0.9999")
    return gate


def reference(model: Path, tokens: Path, output: Path) -> None:
    import numpy as np
    import onnxruntime as ort

    if ort.__version__ != "1.31.0":
        raise RuntimeError("reference ORT version mismatch")
    options = ort.SessionOptions()
    options.intra_op_num_threads, options.inter_op_num_threads = 16, 1
    options.add_session_config_entry("session.disable_prepacking", "1")
    session = ort.InferenceSession(str(model), sess_options=options,
                                   providers=["CPUExecutionProvider"])
    vectors = np.full((200, 768), np.nan, dtype=np.float32)
    for batch in json.loads(tokens.read_text()):
        feed = {name: np.asarray(values, dtype=np.int64).reshape(
                    batch["shape"])
                for name, values in batch["inputs"].items()}
        hidden = session.run(["last_hidden_state"], feed)[0]
        cls = hidden[:, 0, :].copy()
        cls /= np.linalg.norm(cls, axis=1, keepdims=True)
        vectors[batch["indices"]] = cls
    save(output, {"status": "completed", "token_sha256": digest(tokens),
                  "vectors": vectors.tolist(), "intra_op_threads": 16,
                  "inter_op_threads": 1, "runtime": ort.__version__})


def input_texts(rows: Sequence[Mapping], count: int,
                corpus: str) -> tuple[list[str], list[str]] | Invalid:
    if len(rows) != count or any(not isinstance(r, Mapping) for r in rows):
        return Invalid("row count/schema mismatch")
    ids = [row.get("id") for row in rows]
    if (any(not isinstance(x, str) or not x for x in ids)
            or len(set(ids)) != count
            or any(not isinstance(r.get("text"), str) for r in rows)):
        return Invalid("duplicate/missing IDs or invalid text")
    return ids, [(PREFIX if corpus == "query" else "") + r["text"]
                 for r in rows]


def precompute(root: Path, output: Path) -> dict:
    from transformers import AutoTokenizer

    tokenizer = AutoTokenizer.from_pretrained(
        root / "data/cpu-cost/tokenizer", local_files_only=True,
        trust_remote_code=False)
    result = {}
    for corpus, cap, filename, count in POLICIES:
        rows = [json.loads(line) for line in
                (root / "data/cpu-cost" / filename).read_text().splitlines()]
        parsed = input_texts(rows, count, corpus)
        if isinstance(parsed, Invalid):
            raise Skip("identity", parsed.reason)
        ids, texts = parsed
        encoded = tokenizer(texts, padding=False, truncation=True,
                            max_length=cap)
        values = batches(encoded, corpus)
        name = f"{corpus}-{cap}"
        path, gate = output / f"{name}.json", output / f"{name}-gate.json"
        save(path, values)
        save(gate, [batch for batch in values
                    if all(i < 200 for i in batch["indices"])])
        result[name] = {"ids": ids,
                        "symbols": len({r.get("parent_id", r["id"])
                                        for r in rows}),
                        "counts": [len(x) for x in encoded["input_ids"]],
                        "sha256": digest(path), "gate_sha256": digest(gate)}
    return result


def probe(args: argparse.Namespace, case: Mapping, tokens: Path,
          output: Path, repeat: int, end: float, env: Mapping) -> dict:
    guard = max(0.001, end - time.monotonic() - 2)
    argv = [str(tokens.parent / "onnxprobe"), "-model", "coderank",
            "-path", str(args.root / "data/cpu-cost/model/model.onnx"),
            "-library", str(args.library), "-tokens", str(tokens),
            "-output", str(output), "-no-prepack", "-threads",
            str(case["threads"]), "-corpus", case["corpus"], "-cap",
            str(case["cap"]), "-repeat", str(repeat), "-planned-repeats",
            "3", "-guard", f"{guard}s"]
    report = completed_worker(argv, output, end, args.root, env, partial=True)
    required = {"runtime", "disable_prepacking", "inter_op_threads",
                "intra_op_threads", "token_file_sha256", "vectors",
                "planned_texts", "completed_texts", "seconds", "load_seconds"}
    if (not required <= report.keys()
            or type(report["completed_texts"]) is not int
            or not 0 <= report["completed_texts"] <= report["planned_texts"]
            or len(report["vectors"]) != report["planned_texts"]
            or sum(row is not None for row in report["vectors"])
            != report["completed_texts"]
            or (report["status"] == "completed" and
                report["completed_texts"] != report["planned_texts"])
            or (report["status"] == "partial" and
                report.get("reason") != "runner guard exceeded")):
        raise FatalWorker("incomplete probe report")
    if (report["runtime"] != "1.31.0" or not report["disable_prepacking"]
            or report["inter_op_threads"] != 1
            or report["intra_op_threads"] != case["threads"]):
        raise GateFailure("probe runtime/recipe mismatch")
    if report["token_file_sha256"] != digest(tokens):
        raise Skip("identity", "recorded probe token identity mismatch")
    return report


def repeat_metrics(report: Mapping, symbols: int, corpus: str) -> dict:
    result = {k: v for k, v in report.items()
              if k not in {"vectors", "minutes_per_10k_symbols"}}
    result["minutes_per_10k_text_rows"] = report["minutes_per_10k_symbols"]
    if corpus != "query" and report["status"] == "completed":
        result["minutes_per_10k_symbols"] = (
            report["seconds"] * 10000 / symbols / 60)
    return result


def reap_children() -> bool:
    while True:
        try:
            pid, _ = os.waitpid(-1, os.WNOHANG)
        except ChildProcessError:
            return True
        if pid == 0:
            return False


def repeat_seconds(estimate: Mapping, planned_rows: int) -> float:
    return (estimate["load_seconds"] + 2 + estimate["seconds"]
            * planned_rows / estimate["completed_texts"])


def execute(args: argparse.Namespace) -> int:
    root, output = args.root.resolve(), args.receipt.parent.resolve()
    if not output.is_relative_to(root) or output.exists():
        raise RuntimeError("escaped/existing output; preserve for lead")
    output.mkdir(parents=True)
    cases, failure, state = matrix(), None, "partial"
    save(output / "planned-matrix.json", cases)
    started = time.monotonic()
    env = {**os.environ, "TOKENIZERS_PARALLELISM": "false", "USE_TORCH": "0",
           "USE_TF": "0", "HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1"}
    os.environ.update(env)
    try:
        if not args.config.is_file():
            raise Skip("identity", "missing configuration")
        config = json.loads(args.config.read_text())
        if digest(args.config) != args.config_pin:
            raise Skip("identity", "config pin mismatch")
        controller = root / "control/pod-cost/cpu_cost.py"
        if Path(__file__).resolve() != controller:
            raise Skip("identity", "frozen CPU controller refused")
        if any(not (root / p).resolve().is_relative_to(root / "control")
               for p in config["source_files"]):
            raise Skip("identity", "frozen CPU source binding refused")
        for relative, expected in {**config["inputs"],
                                   **config["source_files"]}.items():
            path = (root / relative).resolve()
            if not path.is_file():
                raise Skip("identity", f"missing input/source: {relative}")
            if not path.is_relative_to(root) or digest(path) != expected:
                raise Skip("identity", f"input/source mismatch: {relative}")
        input_pin = hashlib.sha256(json.dumps(
            config["inputs"], sort_keys=True, separators=(",", ":")
        ).encode()).hexdigest()
        if input_pin != args.input_pin:
            raise Skip("identity", "input pin mismatch")
        process_path = root / "aux/cpu-cost/process.json"
        if not process_path.is_file():
            raise FatalWorker("missing driver deadline receipt")
        process = json.loads(process_path.read_text())
        end = process["absolute_deadline"] - 30
        if not math.isfinite(end) or end <= time.monotonic():
            raise Skip("time", "no time before absolute driver deadline")
        if sys.platform != "darwin" or platform.machine() != "arm64":
            raise GateFailure("only Darwin arm64 is admitted")
        if config["ort"]["sha256"] != (
                "aaab9b3af536b06ca27ab5e35e3d429c9"
                "7457ce76cf298af103f687e8b9975c0"):
            raise Skip("library", "pinned Mac ORT wheel required")
        try:
            distribution = importlib.metadata.distribution("onnxruntime")
        except importlib.metadata.PackageNotFoundError as error:
            raise Skip("library", "ORT package unavailable") from error
        libraries = [p for p in (distribution.files or ())
                     if p.name.startswith("libonnxruntime.")
                     and p.name.endswith(".dylib")]
        if distribution.version != "1.31.0" or len(libraries) != 1:
            raise Skip("library", "ORT 1.31.0 shared library unavailable")
        entry = libraries[0]
        if entry.hash is None:
            raise Skip("library", "shared library has no RECORD identity")
        args.library = distribution.locate_file(entry).resolve()
        expected = base64.urlsafe_b64decode(entry.hash.value + "==").hex()
        if entry.hash.mode != "sha256":
            raise Skip("library", "pinned shared library SHA-256 missing")
        verify_library(args.library, expected, root)
        save(output / "library.json", {"path": str(args.library),
             "sha256": expected, "wheel": config["ort"]})
        successor_check(
            [external_tool("go"), "build", "-p=1", "-o",
             str(output / "onnxprobe"), "."], output / "build.log", end,
            root / "control/onnx", env)
        setup = time.monotonic()
        tokens = precompute(root, output)
        save(output / "tokens-manifest.json", {
            "policies": tokens, "seconds": time.monotonic() - setup,
            "setup_seconds": time.monotonic() - started,
            "recipe": config["recipe"]})
        for position, case in enumerate(cases):
            case["status"], case["reason"] = "partial", "in-progress"
            case_end = min(end, time.monotonic()
                           + (end - time.monotonic()) / (25 - position))
            name = f"{case['corpus']}-{case['cap']}"
            reference_path = output / f"{name}-reference.json"
            gate_tokens = output / f"{name}-gate.json"
            if not reference_path.exists():
                command = [sys.executable, __file__, "reference",
                           str(root / "data/cpu-cost/model/model.onnx"),
                           str(gate_tokens), str(reference_path)]
                completed_worker(command, reference_path,
                                 case_end, root, env)
            warm = probe(args, case, gate_tokens,
                         output / f"warm-{position}.json", 0, case_end, env)
            if warm["status"] == "partial":
                case["reason"] = "time"
                case["partial_warmup"] = repeat_metrics(
                    warm, tokens[name]["symbols"], case["corpus"])
                continue
            reference_data = json.loads(reference_path.read_text())
            gate = checked_gate(warm, reference_data, digest(gate_tokens))
            case["gate"], case["warmups"] = gate, 1
            for repeat in range(1, 4):
                estimate = (case["repeats"][-1] if case["repeats"] else warm)
                needed = repeat_seconds(estimate, len(tokens[name]["ids"]))
                if time.monotonic() + needed >= case_end:
                    case["admission_refusal"] = {
                        "requested_seconds": needed,
                        "remaining_seconds": case_end - time.monotonic()}
                    break
                report = probe(args, case, output / f"{name}.json",
                               output / f"case-{position}-{repeat}.json",
                               repeat, case_end, env)
                case["repeats"].append(repeat_metrics(
                    report, tokens[name]["symbols"], case["corpus"]))
                case["actual_repeats"] += int(report["status"] == "completed")
                if report["status"] == "partial":
                    break
            case["status"] = ("completed" if case["actual_repeats"] == 3
                              else "partial")
            case["reason"] = "" if case["status"] == "completed" else "time"
        state = ("completed" if all(c["status"] == "completed" for c in cases)
                 else "partial")
    except Skip as error:
        state = ("partial" if error.code == "time" and any(
            c["status"] == "partial" and c["reason"] == "time" for c in cases)
                 else "skipped")
        failure = {"reason_code": error.code, "reason": str(error)}
        for case in cases:
            if case["status"] == "skipped" or case["reason"] == "in-progress":
                case["reason"] = f"{error.code}: {error}"
    except Exception as error:
        state = "partial" if isinstance(error, GateFailure) else "fatal"
        failure = {"reason_code": getattr(error, "reason_code", "unexpected"),
                   "reason": str(error)}
        for case in cases:
            if case["reason"] == "in-progress":
                case["reason"] = str(error)
            elif case["status"] == "skipped":
                case["reason"] = "not admitted after block failure"
    try:
        clean = reap_children()
    except OSError:
        clean = False
    if not clean and state == "fatal":
        failure["teardown"] = "unverified"
    if not clean and state != "fatal":
        state, failure = "partial", {"reason_code": "process",
                                     "reason": "unverified child teardown"}
    summary = {"cases": cases, "failure": failure, "planned_repeats": 3,
               "cost_scope": "M1 Max CPU; precomputed tokens",
               "accepted_timings": failure is None and any(
                   c["actual_repeats"] for c in cases),
               "runner_seconds": time.monotonic() - started}
    save(output / "matrix.json", summary)
    files = {str(p.relative_to(root)): digest(p) for p in output.iterdir()
             if p.is_file()}
    code = 1 if state in {"partial", "fatal"} else 0
    save(args.receipt, {"state": state, "exit_code": code,
         "clean_handoff": clean and state != "fatal",
         "source_pin": args.source_pin, "config_pin": args.config_pin,
         "input_pin": args.input_pin, "files": files,
         **({} if state == "completed" else failure or
            {"reason_code": "time", "reason": "reduced coverage"})})
    return code


def main() -> int:
    if len(sys.argv) == 5 and sys.argv[1] == "reference":
        reference(*(Path(x) for x in sys.argv[2:]))
        return 0
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("root", "config", "receipt"):
        parser.add_argument("--" + name, type=Path, required=True)
    for name in ("source-pin", "config-pin", "input-pin"):
        parser.add_argument("--" + name, required=True)
    return execute(parser.parse_args())


if __name__ == "__main__":
    raise SystemExit(main())
