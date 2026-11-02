"""Run the approved D14 cut pair serially through the existing tools.

Call only inside the separately capped controller from the reviewed runbook.
No cloud operations, corpus rebuilds or paid retries are implemented here.
"""
import argparse
import json
import os
import shutil
import socket
import subprocess
from collections import Counter
from collections.abc import Mapping, Sequence
from pathlib import Path

from artifacts import digest


FREEZE = "dd6bddf4e92f1357c2d0f49bbd602c26d30c44d08b19d83aeb8dec3de4f3b401"
REVISION = "3c4b60807d71f79b43f3c4363786d9493691f8b1"
GIB = 1024 ** 3
HERE = Path(__file__).resolve().parent


def write(path: Path, value: dict) -> None:
    with path.open("x") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")


def verify_proofs(pins: Mapping, limit: Mapping, runtime: Mapping,
                  library_pin: str) -> None:
    expected = {
        "schema": "model-limit/v1", "model_revision": REVISION,
        "config_sha256": pins["inputs"][
            "data/cpu-cost/tokenizer/config.json"],
        "tokenizer_config_sha256": pins["inputs"][
            "data/cpu-cost/tokenizer/tokenizer_config.json"],
        "includes_special_tokens": True,
    }
    maximum = limit.get("maximum_tokens")
    reference = limit.get("source_reference")
    if (any(limit.get(k) != v for k, v in expected.items())
            or limit.get("includes_special_tokens") is not True
            or type(maximum) is not int or maximum < 8192
            or not isinstance(reference, str) or not reference.strip()):
        raise ValueError("missing or mismatched 8192-token proof")
    download = runtime.get("download_sha256")
    if (runtime.get("schema") != "linux-ort/v1"
            or runtime.get("version") != "1.31.0"
            or runtime.get("platform") != "linux-x86_64"
            or runtime.get("member_sha256") != library_pin
            or not isinstance(runtime.get("member_path"), str)
            or not runtime["member_path"].strip()
            or not isinstance(download, str) or len(download) != 64
            or any(c not in "0123456789abcdef" for c in download)
            or not isinstance(runtime.get("download_url"), str)
            or not runtime["download_url"].startswith("https://")
            or not isinstance(runtime.get("verification"), str)
            or not runtime["verification"].strip()):
        raise ValueError("missing or mismatched Linux ORT proof")


def verify_inputs(root: Path) -> None:
    corpus = root / "corpus"
    freeze = json.loads((corpus / "freeze.json").read_text())
    canonical = corpus / "freeze-canonical.json"
    if (digest(canonical) != FREEZE or freeze["freeze_id"] != FREEZE
            or json.loads(canonical.read_text())
            != {k: v for k, v in freeze.items() if k != "freeze_id"}):
        raise ValueError("wrong frozen corpus")
    for name, pin in freeze["files"].items():
        path = corpus / name
        if (path.resolve() != path or corpus not in path.parents
                or digest(path) != pin):
            raise ValueError("frozen input path/hash mismatch")
    for name, expected in (("documents", {"go": 500, "python": 458}),
                           ("queries", {"go": 50, "python": 29})):
        rows = [json.loads(line) for line in
                (corpus / (name + ".jsonl")).read_text().splitlines()]
        if Counter(r["language"] for r in rows) != expected:
            raise ValueError("wrong frozen language denominators")
    pins = json.loads((root / "harness/study/storage/pod-cost/"
                       "cpu-cost-v1.json").read_text())
    if pins["model_revision"] != REVISION:
        raise ValueError("model revision mismatch")
    verify_proofs(
        pins, json.loads((root / "model-limit.json").read_text()),
        json.loads((root / "runtime/receipt.json").read_text()),
        digest(root / "runtime/libonnxruntime.so.1.31.0"))
    for name, pin in pins["inputs"].items():
        relative = name.removeprefix("data/cpu-cost/")
        if relative.startswith(("model/", "tokenizer/")):
            path = root / relative
            if digest(path) != pin:
                raise ValueError("model/tokenizer hash mismatch")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--source", required=True)
    parser.add_argument("--deadline", required=True)
    parser.add_argument("--cgroup-root", type=Path, required=True)
    args = parser.parse_args()
    root = args.root.resolve()
    verify_inputs(root)
    storage = root / "harness/study/storage"
    results = root / "results"
    results.mkdir()
    uv = str(root / "uv-x86_64-unknown-linux-gnu/uv")
    bridge = storage / "onnx/accuracy_bridge.py"
    data = root / "corpus/freeze.json"
    launcher = root / "bin/limited-command"

    def launch(label: str, memory: int, command: Sequence[str],
               environment: str = "bridge-env") -> None:
        os.environ["UV_PROJECT_ENVIRONMENT"] = str(root / environment)
        subprocess.run([
            str(Path(shutil.which("python3.12")).resolve()), "-B",
            str(HERE / "limited.py"), "--launcher", str(launcher),
            "--data", str(data), "--cgroup-root", str(args.cgroup_root),
            "--output", str(results / (label + "-limits")),
            "--source", args.source, "--deadline", args.deadline,
            "--memory-bytes", str(memory),
            "--file-bytes", str(256 * 1024 ** 2),
            "--", *command,
        ], check=True)

    for cap in (2048, 8192):
        output = results / ("cap" + str(cap))
        output.mkdir()
        files = ["corpus/documents.jsonl", "corpus/queries.jsonl",
                 "model/model.onnx", "model/weights.bin", "bin/onnxprobe",
                 "runtime/libonnxruntime.so.1.31.0"]
        files += [str(p.relative_to(root)) for p in
                  sorted((root / "tokenizer").iterdir())]
        manifest = output / "manifest.json"
        write(manifest, {
            "schema": "accuracy-bridge/v1", "source_pin": args.source,
            "sample": FREEZE, "cap": cap, "host": socket.gethostname(),
            "limits": {"memory_bytes": 24 * GIB, "seconds": 7800,
                       "threads": 8},
            "documents": "corpus/documents.jsonl",
            "queries": "corpus/queries.jsonl", "tokenizer": "tokenizer",
            "model": "model/model.onnx", "model_data": "model/weights.bin",
            "probe": "bin/onnxprobe",
            "runtime": "runtime/libonnxruntime.so.1.31.0",
            "files": {name: digest(root / name) for name in files},
        })
        prepared = output / "prepared"
        prefix = [uv, "--offline", "run", "--frozen", "--project",
                  str(storage / "pod-cost"), "python", str(bridge)]
        launch(str(cap) + "-prepare", 4 * GIB, [
            *prefix, "prepare", "--root", str(root), "--manifest",
            str(manifest), "--manifest-pin", digest(manifest),
            "--output", str(prepared)])
        receipt_path = prepared / "prepared.json"
        receipt = json.loads(receipt_path.read_text())
        for role in ("body", "query"):
            launch(str(cap) + "-" + role, 24 * GIB,
                   receipt["corpora"][role]["command"])
        assembled = output / "assembled"
        launch(str(cap) + "-assemble", 4 * GIB, [
            *prefix, "assemble", "--root", str(root),
            "--prepared", str(receipt_path), "--prepared-pin",
            digest(receipt_path), "--body-report",
            str(prepared / "body-report.json"), "--query-report",
            str(prepared / "query-report.json"), "--output", str(assembled)])
        cut = json.loads((assembled / "cut.json").read_text())
        score = cut["score_command"]
        launch(str(cap) + "-score", 4 * GIB,
               [uv, "--offline", *score[1:]], "score-env")
        launch(str(cap) + "-groups", 4 * GIB, [
            uv, "--offline", "run", "--frozen", "--project",
            str(storage / "pod-cost"), "python", str(HERE / "groups.py"),
            "--root", str(root), "--cut", str(assembled / "cut.json")])
        groups = json.loads((assembled / "groups.json").read_text())
        for language in ("go", "python"):
            command = groups[language]["command"]
            launch(str(cap) + "-score-" + language, 4 * GIB,
                   [uv, "--offline", *command[1:]], "score-env")


if __name__ == "__main__":
    main()
