"""Prepare local tokens and bind one cut's Go vectors for evaluate.py.

No inference or comparative claim is made here. The run owner supplies a
pinned manifest and enforces its host/resource approval outside this bridge.
"""
import argparse
import hashlib
import importlib.util
import json
import sys
from collections import Counter
from collections.abc import Mapping, Sequence
from dataclasses import asdict, dataclass
from pathlib import Path

import numpy as np


STORAGE = Path(__file__).resolve().parents[1]


def sibling(name: str, relative: str):
    spec = importlib.util.spec_from_file_location(name, STORAGE / relative)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


cpu = sibling("accuracy_cpu_cost", "pod-cost/cpu_cost.py")
ev = sibling("accuracy_evaluate", "neon-eval/evaluate.py")


@dataclass(frozen=True, slots=True)
class Invalid:
    reason: str


class BridgeError(Exception):
    """A refused run identity or incomplete inference output."""


def unique_object(pairs: Sequence[tuple[str, object]]) -> dict:
    value = {}
    for key, entry in pairs:
        if key in value:
            raise BridgeError(f"duplicate JSON key: {key}")
        value[key] = entry
    return value


def read_json(path: Path):
    return json.loads(path.read_text(), object_pairs_hook=unique_object)


def write_json(path: Path, value: object) -> None:
    with path.open("x") as stream:
        json.dump(value, stream, indent=2, allow_nan=False)
        stream.write("\n")


def owned(value: str, root: Path) -> Path:
    if not isinstance(value, str) or not value:
        raise BridgeError("missing file path")
    try:
        return ev.owned_path(value, root)
    except ev.RunError as error:
        raise BridgeError(str(error)) from error


def verify_files(files: Mapping, root: Path) -> None:
    if not isinstance(files, Mapping) or not files:
        raise BridgeError("missing file bindings")
    resolved = set()
    for name, pin in files.items():
        path = owned(name, root)
        if path in resolved:
            raise BridgeError("aliased file binding")
        resolved.add(path)
        if not ev.is_pin(pin, (64,)) or ev.digest(path) != pin:
            raise BridgeError(f"file hash mismatch: {name}")


def manifest(path: Path, pin: str, root: Path) -> dict:
    if ev.digest(path) != pin:
        raise BridgeError("manifest hash mismatch")
    value = read_json(path)
    fields = {"schema", "source_pin", "sample", "cap", "host", "limits",
              "documents", "queries", "tokenizer", "model", "model_data",
              "probe", "runtime", "files"}
    if not isinstance(value, dict) or set(value) != fields:
        raise BridgeError("manifest: expected complete accuracy-bridge/v1")
    if (value["schema"] != "accuracy-bridge/v1"
            or not ev.is_pin(value["source_pin"], (40, 64))
            or type(value["cap"]) is not int
            or not 2048 <= value["cap"] <= 8192):
        raise BridgeError("manifest: invalid schema/source/cut")
    for key in ("sample", "host"):
        if not isinstance(value[key], str) or not value[key].strip():
            raise BridgeError(f"manifest: missing {key}")
    limits = value["limits"]
    if (not isinstance(limits, dict)
            or set(limits) != {"memory_bytes", "seconds", "threads"}
            or any(type(n) is not int or n < 1 for n in limits.values())):
        raise BridgeError("manifest: positive memory/seconds/threads required")
    verify_files(value["files"], root)
    for key in ("documents", "queries", "model", "model_data", "probe",
                "runtime"):
        owned(value[key], root)
        if value[key] not in value["files"]:
            raise BridgeError(f"manifest: unbound {key}")
    tokenizer = owned(value["tokenizer"], root)
    if not tokenizer.is_dir():
        raise BridgeError("tokenizer: expected local directory")
    # Reject links and subdirectories: only the bound local tokenizer files
    # may be visible to from_pretrained; no cache or remote lookup is used.
    for entry in tokenizer.iterdir():
        name = str(entry.relative_to(root))
        if (entry.is_symlink() or not entry.is_file()
                or name not in value["files"]):
            raise BridgeError("tokenizer: unbound, linked or nested file")
    if not any(tokenizer.iterdir()):
        raise BridgeError("tokenizer: empty directory")
    return value


def corpus_rows(path: Path, role: str) -> tuple[list, list[str], dict]:
    rows = [json.loads(line, object_pairs_hook=unique_object)
            for line in path.read_text().splitlines()]
    parsed = ev.parse_rows(rows, "document" if role == "body" else "query")
    texts = cpu.input_texts(rows, len(rows), role)
    if isinstance(parsed, ev.Invalid) or isinstance(texts, cpu.Invalid):
        reason = (parsed.reason if isinstance(parsed, ev.Invalid)
                  else texts.reason)
        raise BridgeError(reason)
    if any(not isinstance(row.get("language"), str)
           or not row["language"].strip() for row in rows):
        raise BridgeError(f"{role}: missing language")
    return rows, texts[1], dict(Counter(row["language"] for row in rows))


def token_count(batches: object, count: int, cap: int,
                role: str) -> int | Invalid:
    if not isinstance(batches, list) or not batches:
        return Invalid("missing token batches")
    seen, tokens = set(), 0
    for batch in batches:
        if not isinstance(batch, dict):
            return Invalid("invalid token batch")
        indices, shape = batch.get("indices"), batch.get("shape")
        if (not isinstance(indices, list) or not indices
                or not isinstance(shape, list) or len(shape) != 2
                or any(type(n) is not int for n in shape)
                or shape[0] != len(indices) or not 1 <= shape[1] <= cap
                or (role == "query" and len(indices) != 1)):
            return Invalid("token shape/cap mismatch")
        for index in indices:
            if (type(index) is not int or not 0 <= index < count
                    or index in seen):
                return Invalid("duplicate or out-of-range token index")
            seen.add(index)
        inputs = batch.get("inputs")
        names = {"input_ids", "attention_mask", "token_type_ids"}
        if not isinstance(inputs, dict) or set(inputs) != names:
            return Invalid("missing token inputs")
        for name, values in inputs.items():
            if (not isinstance(values, list)
                    or len(values) != shape[0] * shape[1]
                    or any(type(n) is not int or n < 0 for n in values)):
                return Invalid("token input shape/type mismatch")
        mask = inputs["attention_mask"]
        if any(n not in (0, 1) for n in mask):
            return Invalid("invalid attention mask")
        if any(not any(mask[i:i + shape[1]])
               for i in range(0, len(mask), shape[1])):
            return Invalid("empty token row")
        tokens += sum(mask)
    if seen != set(range(count)):
        return Invalid("missing token indices")
    return tokens


def probe_vectors(report: object, batches: object, count: int, cap: int,
                  role: str, token_pin: str) -> np.ndarray | Invalid:
    total = token_count(batches, count, cap, role)
    if isinstance(total, Invalid):
        return total
    expected = {"status": "completed", "reason": "", "corpus": role,
                "token_cap": cap, "token_file_sha256": token_pin,
                "planned_texts": count, "completed_texts": count,
                "planned_unpadded_tokens": total,
                "completed_unpadded_tokens": total,
                "batch_count": len(batches), "model": "coderank",
                "runtime": "1.31.0"}
    if not isinstance(report, dict) or any(
            report.get(key) != value for key, value in expected.items()):
        return Invalid("partial, wrong-cut or mismatched probe report")
    vectors = ev.unit_vectors(report.get("vectors"), count, role)
    if isinstance(vectors, ev.Invalid):
        return Invalid(vectors.reason)
    if vectors.shape[1] != 768:
        return Invalid("probe vector dimension mismatch")
    # The Go probe already restores Vectors[index]. Do not apply the token
    # permutation again, or corpus-order ties will silently change.
    return np.asarray(report["vectors"], dtype=np.float32)


def source_hashes() -> dict:
    paths = (Path(__file__), Path(cpu.__file__), Path(ev.__file__),
             STORAGE / "pod-cost/pyproject.toml", STORAGE / "pod-cost/uv.lock")
    return {str(path.resolve().relative_to(STORAGE)): ev.digest(path)
            for path in paths}


def prepare(args: argparse.Namespace) -> None:
    root = args.root.resolve()
    config_path = owned(str(args.manifest), root)
    config = manifest(config_path, args.manifest_pin, root)
    corpora = {}
    for role, key in (("body", "documents"), ("query", "queries")):
        rows, texts, languages = corpus_rows(owned(config[key], root), role)
        corpora[role] = (rows, texts, languages)
    parents = {row["parent_id"]: row["repo"]
               for row in corpora["body"][0]}
    if any(parents.get(row["id"]) != row["repo"]
           for row in corpora["query"][0]):
        raise BridgeError("query: missing same-repository label")
    from transformers import AutoTokenizer

    tokenizer = AutoTokenizer.from_pretrained(
        owned(config["tokenizer"], root), local_files_only=True,
        trust_remote_code=False)
    exports = {}
    for role, (rows, texts, languages) in corpora.items():
        encoded = tokenizer(texts, padding=False, truncation=True,
                            max_length=config["cap"])
        batches = cpu.batches(encoded, role)
        total = token_count(batches, len(rows), config["cap"], role)
        if isinstance(total, Invalid):
            raise BridgeError(total.reason)
        exports[role] = (batches, [row["id"] for row in rows], languages)
    output = owned(str(args.output), root)
    output.mkdir()  # A fresh directory is the no-overwrite boundary.
    receipt = {"schema": "accuracy-prepared/v1", "manifest": str(
        config_path.relative_to(root)), "manifest_pin": args.manifest_pin,
        "sources": source_hashes(),
        "corpora": {}}
    for role, (batches, ids, languages) in exports.items():
        path = output / (role + "-tokens.json")
        write_json(path, batches)
        receipt["corpora"][role] = {
            "path": str(path.relative_to(root)), "sha256": ev.digest(path),
            "ids": ids, "languages": languages,
            "command": [str(owned(config["probe"], root)), "-model",
                        "coderank", "-path", str(owned(config["model"], root)),
                        "-library", str(owned(config["runtime"], root)),
                        "-tokens", str(path), "-output",
                        str(output / (role + "-report.json")), "-corpus", role,
                        "-cap", str(config["cap"]), "-no-prepack", "-threads",
                        str(config["limits"]["threads"]), "-guard",
                        str(config["limits"]["seconds"]) + "s",
                        "-repeat", "1", "-planned-repeats", "1"]}
    write_json(output / "prepared.json", receipt)


def assemble(args: argparse.Namespace) -> None:
    root = args.root.resolve()
    prepared_path = owned(str(args.prepared), root)
    if ev.digest(prepared_path) != args.prepared_pin:
        raise BridgeError("prepared receipt hash mismatch")
    prepared = read_json(prepared_path)
    if (not isinstance(prepared, dict)
            or set(prepared) != {"schema", "manifest", "manifest_pin",
                                 "sources", "corpora"}
            or prepared["schema"] != "accuracy-prepared/v1"
            or set(prepared["corpora"]) != {"body", "query"}):
        raise BridgeError("incomplete prepared receipt")
    config_path = owned(prepared["manifest"], root)
    config = manifest(config_path, prepared["manifest_pin"], root)
    if prepared["sources"] != source_hashes():
        raise BridgeError("bridge/toolchain source changed or missing")
    arrays, rows_by_role, reports = {}, {}, {}
    for role, key in (("body", "documents"), ("query", "queries")):
        spec = prepared["corpora"][role]
        rows, _, languages = corpus_rows(owned(config[key], root), role)
        if (spec["ids"] != [row["id"] for row in rows]
                or spec["languages"] != languages):
            raise BridgeError("corpus ID/order/language mismatch")
        tokens = owned(spec["path"], root)
        if ev.digest(tokens) != spec["sha256"]:
            raise BridgeError("token hash mismatch")
        path = owned(str(getattr(args, role + "_report")), root)
        report = read_json(path)
        vectors = probe_vectors(report, read_json(tokens), len(rows),
                                config["cap"], role, spec["sha256"])
        if isinstance(vectors, Invalid):
            raise BridgeError(vectors.reason)
        if (owned(report["model_path"], root) != owned(config["model"], root)
                or report["intra_op_threads"] != config["limits"]["threads"]
                or report["inter_op_threads"] != 1
                or report["guard_seconds"] != config["limits"]["seconds"]
                or report["disable_prepacking"] is not True
                or report["mapped"] is not False
                or report["repeat"] != 1 or report["completed_repeats"] != 1
                or report["planned_repeats"] != 1):
            raise BridgeError("probe command/recipe mismatch")
        arrays[role], rows_by_role[role] = vectors, rows
        reports[role] = {"path": str(path.relative_to(root)),
                         "sha256": ev.digest(path)}
    dataset = ev.parse_dataset(rows_by_role["body"], rows_by_role["query"],
                               arrays["body"], arrays["query"])
    if isinstance(dataset, ev.Invalid):
        raise BridgeError(dataset.reason)
    output = owned(str(args.output), root)
    output.mkdir()
    vector_path = output / "vectors.npy"
    with vector_path.open("xb") as stream:
        np.save(stream, np.concatenate((arrays["body"], arrays["query"])),
                allow_pickle=False)
    evaluator = {"schema": "retrieval-evaluator/v1",
                 "model": "coderank-reference", "sample": config["sample"],
                 "text_policy": "body", "tie_policy": "corpus-order",
                 "expected_r10_percent": None,
                 "evaluator_sha256": ev.digest(Path(ev.__file__))}
    for key, role in (("documents", "body"), ("queries", "query")):
        evaluator[key] = {"path": config[key],
                          "sha256": config["files"][config[key]],
                          "rows": len(rows_by_role[role])}
    evaluator["vectors"] = {"path": str(vector_path.relative_to(root)),
                            "sha256": ev.digest(vector_path),
                            "rows": sum(len(rows) for rows in
                                        rows_by_role.values())}
    evaluator_path = output / "config.json"
    write_json(evaluator_path, evaluator)
    inputs = {key: evaluator[key]["sha256"]
              for key in ("documents", "queries", "vectors")}
    receipt = {"schema": "accuracy-cut/v1", "state": "ready_to_score",
               "prepared_pin": args.prepared_pin,
               "recipe": asdict(ev.MODELS["coderank-reference"]),
               "manifest_pin": prepared["manifest_pin"], "binding": config,
               "sources": prepared["sources"], "reports": reports,
               "corpora": prepared["corpora"], "config_pin": ev.digest(
                   evaluator_path), "input_pin": ev.json_pin(inputs),
               "files": {str(path.relative_to(root)): ev.digest(path)
                         for path in (vector_path, evaluator_path)}}
    receipt["score_command"] = [
        "uv", "run", "--frozen", "--project", str(STORAGE / "neon-eval"),
        "python", ev.__file__, "score", "--root", str(root), "--config",
        str(evaluator_path), "--receipt", str(output / "score.json"),
        "--source-pin", config["source_pin"], "--config-pin",
        receipt["config_pin"], "--input-pin", receipt["input_pin"]]
    write_json(output / "cut.json", receipt)


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    for name in ("prepare", "assemble"):
        command = commands.add_parser(name)
        command.add_argument("--root", type=Path, required=True)
        command.add_argument("--output", type=Path, required=True)
        if name == "prepare":
            command.add_argument("--manifest", type=Path, required=True)
            command.add_argument("--manifest-pin", required=True)
        else:
            command.add_argument("--prepared", type=Path, required=True)
            command.add_argument("--prepared-pin", required=True)
            command.add_argument("--body-report", type=Path, required=True)
            command.add_argument("--query-report", type=Path, required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "prepare":
            prepare(args)
        else:
            assemble(args)
    except (BridgeError, OSError, ValueError, KeyError, TypeError) as error:
        print(json.dumps({"state": "rejected", "error": str(error)}),
              file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
