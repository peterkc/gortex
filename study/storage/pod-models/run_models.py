"""Pinned D3 fp32 inference and calibrated same-pod retrieval metrics."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
import sys
import time
from collections.abc import Mapping, Sequence
from dataclasses import asdict
from importlib.metadata import version
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "neon-eval"))
IMPORT_FAILURE = ""
try:
    import numpy as np
    import evaluate as ev
except ImportError as error:
    IMPORT_FAILURE = str(error)

D3 = ("bge-large-en", "qwen3-embedding-0-6b", "gte-large-en")
GTE_PAIR = ("Alibaba-NLP/gte-large-en-v1.5",
            "104333d6af6f97649377c2afbde10a7704870c7b",
            "Alibaba-NLP/new-impl",
            "40ced75c3017eb27626c9d4ea981bde21a2662f4")
SETS = (("mixed-metadata", "corp_.jsonl", "qs_eq.jsonl", "metadata"),
        ("ts-metadata", "docs_.jsonl", "qrows_.jsonl", "metadata"),
        ("ts-body", "body_.jsonl", "qrows_.jsonl", "body"))


def model_options(name: str, model: ev.Model) -> dict:
    if name not in D3 or model != ev.MODELS[name]:
        raise ev.RunError("unsupported model identity")
    remote = bool(model.code_repo or model.code_revision)
    if remote and (model.repo, model.revision, model.code_repo,
                   model.code_revision) != GTE_PAIR:
        raise ev.RunError("remote code requires the pinned GTE pair")
    options = {"revision": model.revision, "trust_remote_code": remote}
    if remote:
        options["code_revision"] = model.code_revision
    return options


def texts(rows: Sequence[Mapping], prefix: str) -> list[str]:
    ids = [row.get("id") for row in rows]
    if (not rows or any(not isinstance(i, str) or not i for i in ids)
            or len(set(ids)) != len(ids)):
        raise ev.RunError("empty sample or missing/duplicate row IDs")
    if any(not isinstance(row.get("text"), str) or not row["text"].strip()
           for row in rows):
        raise ev.RunError("missing/empty input text")
    return [prefix + row["text"] for row in rows]


def batches(lengths: Sequence[int], budget: int = 8192,
            max_rows: int = 32) -> list[list[int]]:
    if not lengths or min(lengths) < 1 or budget < 1 or max_rows < 1:
        raise ev.RunError("invalid batching lengths or limits")
    groups = []
    current = []
    for index in sorted(range(len(lengths)), key=lengths.__getitem__):
        if current and (len(current) == max_rows
                        or lengths[index] * (len(current) + 1) > budget):
            groups.append(current)
            current = []
        current.append(index)
    return [*groups, current] if current else groups


def pooling_indices(mask: np.ndarray, method: str) -> np.ndarray:
    if (mask.ndim != 2 or not mask.size
            or not np.isin(mask, [0, 1]).all() or not mask.any(axis=1).all()):
        raise ev.RunError("invalid attention mask")
    if method == "cls":
        return np.zeros(len(mask), dtype=np.int64)
    if method != "last-token":
        raise ev.RunError("unsupported pooling")
    return mask.shape[1] - 1 - np.argmax(mask[:, ::-1], axis=1)


def restore_order(groups: Sequence[Sequence[int]],
                  values: Sequence[np.ndarray]) -> np.ndarray:
    indices = [index for group in groups for index in group]
    if sorted(indices) != list(range(len(indices))):
        raise ev.RunError("missing/duplicate batch row index")
    return np.concatenate(values)[np.argsort(indices)].copy()


def token_stats(lengths: Sequence[int], cap: int) -> dict:
    return {"rows": len(lengths), "raw_tokens": sum(lengths),
            "effective_tokens": sum(min(n, cap) for n in lengths),
            "truncated_rows": sum(n > cap for n in lengths),
            "max_raw_tokens": max(lengths), "cap": cap}


def begin_output(path: Path) -> None:
    if path.exists():
        raise ev.RunError("output exists; preserve complete/partial evidence")
    path.mkdir(parents=True)


def write_json(path: Path, value: Mapping) -> None:
    with path.open("x") as stream:
        json.dump(value, stream, indent=2, allow_nan=False)
        stream.write("\n")


def calibration(config: Mapping, root: Path) -> str:
    expected = config["calibration"]
    path = ev.owned_path(expected["receipt"], root)
    if not path.is_file():
        return "calibration receipt missing; rankings dependency_blocked"
    try:
        receipt = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as error:
        return f"calibration receipt unreadable: {error}"
    if not isinstance(receipt, dict):
        return "calibration receipt malformed"
    if (receipt.get("state") != "completed"
            or receipt.get("clean_handoff") is not True
            or receipt.get("exit_code") != 0
            or any(receipt.get(k) != expected[k]
                   for k in ("source_pin", "config_pin", "input_pin"))):
        return "calibration failed or identity mismatch"
    metric_path = path.with_suffix(".metrics.json")
    files = receipt.get("files", {})
    if not isinstance(files, dict):
        return "calibration result manifest malformed"
    relative = str(metric_path.relative_to(root))
    if (not metric_path.is_file()
            or files.get(relative) != ev.digest(metric_path)):
        return "calibration metrics missing or hash mismatch"
    try:
        observed = json.loads(metric_path.read_text())
    except (OSError, json.JSONDecodeError) as error:
        return f"calibration metrics unreadable: {error}"
    if not isinstance(observed, dict):
        return "calibration metrics malformed"
    metrics = observed.get("metrics", {})
    if (not isinstance(metrics, dict)
            or observed.get("observed_percent") != "69.369"
            or observed.get("expected_r10_percent") != "69.369"
            or metrics.get("queries") != 333):
        return "calibration target or denominator mismatch"
    return ""


def score_config(name: str, sample: str, policy: str, specs: Mapping,
                 vector: Path, count: int, root: Path) -> dict:
    return {"schema": "retrieval-evaluator/v1", "model": name,
            "sample": sample, "text_policy": policy,
            "tie_policy": "corpus-order", "documents": specs["documents"],
            "queries": specs["queries"], "vectors": {
                "path": str(vector.relative_to(root)),
                "sha256": ev.digest(vector), "rows": count},
            "evaluator_sha256": ev.digest(Path(ev.__file__)),
            "expected_r10_percent": None}


def load_inputs(config: Mapping, root: Path) -> dict:
    rows = {}
    hashes = {}
    for name, spec in config["inputs"].items():
        path = ev.owned_path(spec["path"], root)
        hashes[name] = ev.digest(path)
        if hashes[name] != spec["sha256"]:
            raise ev.RunError(f"{name}: input hash mismatch")
        rows[name] = [json.loads(line) for line
                      in path.read_text().splitlines()]
        if len(rows[name]) != spec["rows"]:
            raise ev.RunError(f"{name}: row-count mismatch")
        texts(rows[name], "")
    for _, docs, queries, _ in SETS:
        parsed_docs = ev.parse_rows(rows[docs], "document")
        parsed_queries = ev.parse_rows(rows[queries], "query")
        if isinstance(parsed_docs, ev.Invalid):
            raise ev.RunError(parsed_docs.reason)
        if isinstance(parsed_queries, ev.Invalid):
            raise ev.RunError(parsed_queries.reason)
        owners = {row.parent: row.repo for row in parsed_docs}
        if (any(owners[row.parent] != row.repo for row in parsed_docs)
                or any(owners.get(q.parent) != q.repo
                       for q in parsed_queries)):
            raise ev.RunError("missing or conflicting same-repository labels")
    return rows


def embed(rows: Sequence[Mapping], prefix: str, model, tokenizer,
          recipe: ev.Model, torch) -> tuple[np.ndarray, dict]:
    started = time.monotonic()
    # Count full tokens separately; never count padded positions as throughput.
    tokenized = tokenizer(texts(rows, prefix), truncation=False,
                          padding=False)["input_ids"]
    lengths = [len(ids) for ids in tokenized]
    groups = batches([min(n, recipe.max_tokens) for n in lengths])
    batch_vectors = []
    for indices in groups:
        encoded = tokenizer([prefix + rows[i]["text"] for i in indices],
                            max_length=recipe.max_tokens, padding=True,
                            truncation=True, return_tensors="pt").to("cuda")
        with torch.inference_mode():
            hidden = model(**encoded).last_hidden_state
            mask = encoded["attention_mask"].cpu().numpy()
            last = torch.as_tensor(pooling_indices(mask, recipe.pooling),
                                   device="cuda")
            selected = hidden[torch.arange(len(indices), device="cuda"), last]
            pooled = selected.float().cpu().numpy()
        unit = ev.unit_vectors(pooled, len(indices), "model batch")
        if (isinstance(unit, ev.Invalid)
                or pooled.shape[1] != recipe.dimensions):
            raise ev.RunError("nonfinite/zero/wrong-dimension model output")
        batch_vectors.append(unit.astype(np.float32))
    torch.cuda.synchronize()
    stats = {**token_stats(lengths, recipe.max_tokens),
             "seconds": time.monotonic() - started,
             "batches": len(groups), "padded_token_budget": 8192,
             "max_batch_rows": 32}
    return restore_order(groups, batch_vectors), stats


def require_t4(name: str | None) -> str:
    if name is None or "T4" not in name.split():
        raise ev.RunError("required T4 GPU unavailable")
    return name


def compute(config: Mapping, rows: Mapping, args: argparse.Namespace,
            output: Path, blocked: str, started: float) -> None:
    root = args.root.resolve()
    name = config["model"]
    recipe = ev.MODELS[name]
    options = model_options(name, recipe)
    import torch
    from transformers import AutoModel, AutoTokenizer

    device_name = require_t4(torch.cuda.get_device_name()
                             if torch.cuda.is_available() else None)
    torch.set_num_threads(2)
    torch.backends.cuda.matmul.allow_tf32 = False
    torch.backends.cudnn.allow_tf32 = False
    torch.cuda.reset_peak_memory_stats()
    tokenizer = AutoTokenizer.from_pretrained(
        recipe.repo, revision=recipe.revision, trust_remote_code=False,
        padding_side="left" if recipe.pooling == "last-token" else "right")
    model = AutoModel.from_pretrained(
        recipe.repo, **options, torch_dtype=torch.float32).to("cuda").eval()
    load_seconds = time.monotonic() - started
    inference = {}
    arrays = {}
    for filename in config["inputs"]:
        prefix = (recipe.query_prefix if filename in {"qs_eq.jsonl",
                  "qrows_.jsonl"} else recipe.document_prefix)
        arrays[filename], inference[filename] = embed(
            rows[filename], prefix, model, tokenizer, recipe, torch)
    for sample, docs, queries, policy in SETS:
        vector = output / (sample + ".npy")
        partial = vector.with_suffix(".partial.npy")
        np.save(partial, np.concatenate([arrays[docs], arrays[queries]]))
        partial.rename(vector)
        ids = output / (sample + ".ids.json")
        write_json(ids, {"documents": [r["id"] for r in rows[docs]],
                         "queries": [r["id"] for r in rows[queries]]})
        if not blocked:
            specs = {"documents": config["inputs"][docs],
                     "queries": config["inputs"][queries]}
            scoring = score_config(name, sample, policy, specs, vector,
                                   len(rows[docs]) + len(rows[queries]), root)
            score_path = output / (sample + ".score.json")
            write_json(score_path, scoring)
            score_receipt = output / (sample + ".score-receipt.json")
            completed = subprocess.run([
                sys.executable, ev.__file__, "score", "--root", str(root),
                "--config", str(score_path), "--receipt", str(score_receipt),
                "--source-pin", args.source_pin, "--config-pin",
                ev.digest(score_path), "--input-pin", ev.json_pin({
                    key: scoring[key]["sha256"] for key in
                    ("documents", "queries", "vectors")})], check=False)
            if completed.returncode:
                raise ev.RunError(f"{sample}: evaluator failed")
    seconds = time.monotonic() - started
    write_json(output / "runtime.json", {
        "model": asdict(recipe), "variant": "fp32", "inputs": config["inputs"],
        "inference": inference, "load_seconds": load_seconds,
        "total_seconds": seconds,
        "cost_usd": (seconds / 3600 * args.hourly_usd
                     if args.hourly_usd is not None else None),
        "cost_basis": "bound hourly rate; model wall time; excludes uv setup",
        "gpu": device_name,
        "cuda_runtime": torch.version.cuda,
        "cudnn_version": torch.backends.cudnn.version(),
        "peak_allocated_bytes": torch.cuda.max_memory_allocated(),
        "peak_reserved_bytes": torch.cuda.max_memory_reserved(),
        "versions": {p: version(p) for p in
                     ("torch", "transformers", "sentence-transformers")},
        "rankings": "dependency_blocked" if blocked else "completed",
        "reason": blocked, "padding_side": tokenizer.padding_side,
        "environment": {k: os.environ.get(k) for k in
                        ("HF_HOME", "UV_CACHE_DIR", "HOME")}})
    del model
    torch.cuda.empty_cache()


def run_model(args: argparse.Namespace) -> int:
    if IMPORT_FAILURE:
        raise ImportError(IMPORT_FAILURE)
    root = args.root.resolve()
    config_path = ev.owned_path(str(args.config), root)
    if not ev.is_pin(args.source_pin, (40, 64)):
        raise ev.RunError("supply final source pin, not LEAD")
    if ev.digest(config_path) != args.config_pin:
        raise ev.RunError("config_pin mismatch")
    config = json.loads(config_path.read_text())
    if (config["runner_sha256"] != ev.digest(Path(__file__))
            or config["evaluator_sha256"] != ev.digest(Path(ev.__file__))):
        raise ev.RunError("runner/evaluator source mismatch")
    if ev.json_pin({n: s["sha256"] for n, s in config["inputs"].items()}) != (
            args.input_pin):
        raise ev.RunError("input_pin mismatch")
    name = config["model"]
    model_options(name, ev.MODELS[name])
    receipt = ev.owned_path(str(args.receipt), root)
    output = receipt.parent
    rows = load_inputs(config, root)
    blocked = calibration(config, root)
    begin_output(output)
    code = 0
    reason = {}
    state = "dependency_blocked" if blocked else "completed"
    try:
        compute(config, rows, args, output, blocked, time.monotonic())
    except (RuntimeError, OSError, ValueError, ev.RunError) as error:
        write_json(output / "failure.json", {"error": str(error)})
        state, code = "partial", 1
        reason = {"reason_code": "compute", "reason": str(error)}
    files = {str(p.relative_to(root)): ev.digest(p)
             for p in output.iterdir() if p.is_file()}
    write_json(receipt, {"source_pin": args.source_pin,
                         "config_pin": args.config_pin,
                         "input_pin": args.input_pin, "exit_code": code,
                         "state": state, "clean_handoff": True,
                         "files": files, **reason})
    return code


def run(args: argparse.Namespace) -> int:
    try:
        return run_model(args)
    except Exception as error:
        root = args.root.resolve()
        receipt = args.receipt.resolve()
        if receipt.exists():
            raise
        if not receipt.is_relative_to(root):
            raise RuntimeError("receipt escapes declared root") from error
        receipt.parent.mkdir(parents=True, exist_ok=True)
        reason_code = "import" if isinstance(error, ImportError) else "input"
        failure = receipt.with_suffix(".error.json")
        write_json(failure, {"reason_code": reason_code, "reason": str(error)})
        files = {str(p.relative_to(root)):
                 hashlib.sha256(p.read_bytes()).hexdigest()
                 for p in receipt.parent.iterdir() if p.is_file()}
        write_json(receipt, {"source_pin": args.source_pin,
                            "config_pin": args.config_pin,
                            "input_pin": args.input_pin, "exit_code": 1,
                            "state": "partial", "clean_handoff": True,
                            "reason_code": reason_code, "reason": str(error),
                            "files": files})
        return 1


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ("root", "config", "receipt"):
        parser.add_argument("--" + flag, type=Path, required=True)
    for flag in ("source-pin", "config-pin", "input-pin"):
        parser.add_argument("--" + flag, required=True)
    parser.add_argument("--hourly-usd", type=float)
    args = parser.parse_args(argv)
    if args.hourly_usd is not None and not 0 <= args.hourly_usd < float("inf"):
        parser.error("--hourly-usd must be finite and nonnegative")
    try:
        if args.root.resolve() != Path("/work") or sys.platform != "linux":
            raise RuntimeError("pod-only execution requires Linux and /work")
        return run(args)
    except Exception as error:
        print(json.dumps({"state": "checks_failed", "error": str(error)}),
              file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
