"""Exact saved-vector retrieval; model inference belongs to the pod owner."""
import argparse
import hashlib
import json
import math
import sys
from collections.abc import Mapping, Sequence
from dataclasses import asdict, dataclass
from pathlib import Path

import numpy as np


@dataclass(frozen=True, slots=True)
class Model:
    repo: str
    revision: str
    dimensions: int
    max_tokens: int
    query_prefix: str
    pooling: str
    document_prefix: str = ""
    normalization: str = "l2"
    code_repo: str = ""
    code_revision: str = ""


MODELS = {
    "qwen3-embedding-0-6b": Model(
        "Qwen/Qwen3-Embedding-0.6B",
        "97b0c614be4d77ee51c0cef4e5f07c00f9eb65b3", 1024, 32768,
        "Instruct: Given a web search query, retrieve relevant passages "
        "that answer the query\nQuery:", "last-token"),
    "bge-large-en": Model(
        "BAAI/bge-large-en-v1.5",
        "d4aa6901d3a41ba39fb536a557fa166f842b0e09", 1024, 512,
        "Represent this sentence for searching relevant passages: ", "cls"),
    "gte-large-en": Model(
        "Alibaba-NLP/gte-large-en-v1.5",
        "104333d6af6f97649377c2afbde10a7704870c7b", 1024, 8192, "", "cls",
        code_repo="Alibaba-NLP/new-impl",
        code_revision="40ced75c3017eb27626c9d4ea981bde21a2662f4"),
    "coderank-reference": Model(
        "nomic-ai/CodeRankEmbed",
        "3c4b60807d71f79b43f3c4363786d9493691f8b1", 768, 8192,
        "Represent this query for searching relevant code: ", "cls"),
}
TIES = ("corpus-order", "descending-parent-ID")


@dataclass(frozen=True, slots=True)
class Invalid:
    reason: str


@dataclass(frozen=True, slots=True)
class Row:
    id: str
    parent: str
    repo: str


@dataclass(frozen=True, slots=True)
class Dataset:
    documents: tuple[Row, ...]
    queries: tuple[Row, ...]
    document_vectors: np.ndarray
    query_vectors: np.ndarray


def parse_rows(rows: Sequence[object], role: str) -> tuple[Row, ...] | Invalid:
    if not rows:
        return Invalid(f"{role}: empty sample")
    parsed = []
    seen = set()
    fields = ("id", "repo", "parent_id") if role == "document" else (
        "id", "repo")
    for index, row in enumerate(rows):
        if not isinstance(row, Mapping) or any(
                not isinstance(row.get(key), str) or not row[key].strip()
                for key in fields):
            return Invalid(
                f"{role} row {index}: missing identifier/repository")
        if row["id"] in seen:
            return Invalid(f"{role}: duplicate ID {row['id']}")
        seen.add(row["id"])
        parent = row["parent_id"] if role == "document" else row["id"]
        parsed.append(Row(row["id"], parent, row["repo"]))
    return tuple(parsed)


def unit_vectors(values: object, count: int,
                 role: str) -> np.ndarray | Invalid:
    try:
        array = np.asarray(values)
    except (TypeError, ValueError):
        return Invalid(f"{role}: ragged/nonnumeric vectors")
    if (array.ndim != 2 or array.shape[0] != count
            or array.shape[1] == 0):
        return Invalid(f"{role}: row-count/shape mismatch")
    if array.dtype.kind not in "fiu":
        return Invalid(f"{role}: nonnumeric vectors")
    copied = array.astype(np.float64, copy=True)
    if not np.isfinite(copied).all():
        return Invalid(f"{role}: nonfinite vectors")
    scale = np.abs(copied).max(axis=1, keepdims=True)
    if (scale == 0).any():
        return Invalid(f"{role}: zero vectors")
    scaled = copied / scale
    normalized = scaled / np.linalg.norm(scaled, axis=1, keepdims=True)
    normalized.flags.writeable = False
    return normalized


def parse_dataset(document_rows: Sequence[object],
                  query_rows: Sequence[object], documents: object,
                  queries: object) -> Dataset | Invalid:
    docs = parse_rows(document_rows, "document")
    qs = parse_rows(query_rows, "query")
    if isinstance(docs, Invalid):
        return docs
    if isinstance(qs, Invalid):
        return qs
    owners = {}
    for row in docs:
        if row.parent in owners and owners[row.parent] != row.repo:
            return Invalid("parent ID spans repositories")
        owners[row.parent] = row.repo
    if any(owners.get(row.parent) != row.repo for row in qs):
        return Invalid("query: missing same-repository label")
    dv = unit_vectors(documents, len(docs), "document")
    qv = unit_vectors(queries, len(qs), "query")
    if isinstance(dv, Invalid):
        return dv
    if isinstance(qv, Invalid):
        return qv
    if dv.shape[1] != qv.shape[1]:
        return Invalid("document/query dimension mismatch")
    return Dataset(docs, qs, dv, qv)


def retrieval(dataset: Dataset, tie_policy: str) -> dict:
    if tie_policy not in TIES:
        raise RunError("unsupported tie policy")
    ranks = []
    for query, vector in zip(dataset.queries, dataset.query_vectors):
        indices = [i for i, row in enumerate(dataset.documents)
                   if row.repo == query.repo]
        scores = dataset.document_vectors[indices] @ vector
        parents = {}
        for index, score in zip(indices, scores):
            parent = dataset.documents[index].parent
            parents[parent] = max(
                parents.get(parent, -math.inf), float(score))
        if tie_policy == "corpus-order":
            order = sorted(parents, key=parents.__getitem__, reverse=True)
        else:
            order = sorted(
                parents, key=lambda p: (parents[p], p), reverse=True)
        ranks.append(order.index(query.parent) + 1)
    count = len(ranks)
    return {
        "queries": count, "unique_parents": len({
            row.parent for row in dataset.documents}),
        "tie_policy": tie_policy,
        "r1": sum(rank == 1 for rank in ranks) / count,
        "r10": sum(rank <= 10 for rank in ranks) / count,
        "mrr10": sum(1 / rank for rank in ranks if rank <= 10) / count,
        "ndcg10": sum(1 / math.log2(rank + 1) for rank in ranks if rank <= 10)
        / count, "ranks": ranks,
    }


class RunError(Exception):
    """A refused input or output identity at the CLI boundary."""


def digest(path: Path) -> str:
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def json_pin(value: Mapping) -> str:
    return hashlib.sha256(json.dumps(
        value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def is_pin(value: object, lengths: tuple[int, ...]) -> bool:
    return (isinstance(value, str) and len(value) in lengths
            and all(char in "0123456789abcdef" for char in value))


def owned_path(value: str, root: Path) -> Path:
    path = Path(value)
    path = path if path.is_absolute() else root / path
    if not path.resolve().is_relative_to(root.resolve()):
        raise RunError("path escapes declared root")
    return path.resolve()


def load_config(path: Path) -> dict | Invalid:
    config = json.loads(path.read_text())
    fields = {"schema", "model", "sample", "text_policy", "tie_policy",
              "documents", "queries", "vectors", "evaluator_sha256",
              "expected_r10_percent"}
    if not isinstance(config, dict) or set(config) != fields:
        return Invalid("config: expected the complete v1 schema")
    if config["schema"] != "retrieval-evaluator/v1":
        return Invalid("config: unsupported schema")
    for key in ("model", "sample", "text_policy", "tie_policy"):
        if not isinstance(config[key], str) or not config[key].strip():
            return Invalid(f"config: invalid {key}")
    if (config["model"] not in MODELS or config["tie_policy"] not in TIES
            or config["text_policy"] not in {"metadata", "body"}):
        return Invalid("config: unsupported model/text/tie policy")
    for key in ("documents", "queries", "vectors"):
        spec = config[key]
        if (not isinstance(spec, dict)
                or set(spec) != {"path", "sha256", "rows"}
                or not isinstance(spec["path"], str) or not spec["path"]
                or not is_pin(spec["sha256"], (64,))
                or type(spec["rows"]) is not int or spec["rows"] < 1):
            return Invalid(f"config: invalid {key} file identity")
    expected = config["expected_r10_percent"]
    if expected is not None and (
            not isinstance(expected, str) or len(expected.split(".")) != 2
            or not expected.replace(".", "").isascii()
            or not expected.replace(".", "").isdigit()
            or len(expected.split(".")[1]) != 3
            or not 0 <= float(expected) <= 100):
        return Invalid("config: expected R@10 must be percent with 3 decimals")
    return config


def run(args: argparse.Namespace) -> int:
    root = args.root.resolve()
    receipt = owned_path(str(args.receipt), root)
    metrics_path = receipt.with_suffix(".metrics.json")
    if any(path.exists() or path.is_symlink()
           for path in (receipt, metrics_path)):
        raise RunError("output exists; preserve it for lead reconciliation")
    if not is_pin(args.source_pin, (40, 64)):
        raise RunError("source_pin: supply the final reviewed pin, not LEAD")
    config_path = owned_path(str(args.config), root)
    if digest(config_path) != args.config_pin:
        raise RunError("config_pin mismatch")
    config = load_config(config_path)
    if isinstance(config, Invalid):
        raise RunError(config.reason)
    if config["evaluator_sha256"] != digest(Path(__file__)):
        raise RunError("evaluator source hash mismatch")
    actual_inputs = {}
    paths = {}
    for key in ("documents", "queries", "vectors"):
        paths[key] = owned_path(config[key]["path"], root)
        actual_inputs[key] = digest(paths[key])
        if actual_inputs[key] != config[key]["sha256"]:
            raise RunError(f"{key}: input hash mismatch")
    if json_pin(actual_inputs) != args.input_pin:
        raise RunError("input_pin mismatch")
    documents = [json.loads(line) for line in
                 paths["documents"].read_text().splitlines()]
    queries = [json.loads(line) for line in
               paths["queries"].read_text().splitlines()]
    if (len(documents) != config["documents"]["rows"]
            or len(queries) != config["queries"]["rows"]):
        raise RunError("configured sample row-count mismatch")
    try:
        vectors = np.load(paths["vectors"], allow_pickle=False)
    except ValueError as error:
        raise RunError("vectors: invalid non-pickled NPY") from error
    if not isinstance(vectors, np.ndarray):
        vectors.close()
        raise RunError("vectors: expected one NPY array, not an archive")
    expected_shape = (len(documents) + len(queries),
                      MODELS[config["model"]].dimensions)
    if (vectors.shape != expected_shape
            or config["vectors"]["rows"] != expected_shape[0]):
        raise RunError("vector row-count/dimension mismatch")
    dataset = parse_dataset(documents, queries, vectors[:len(documents)],
                            vectors[len(documents):])
    if isinstance(dataset, Invalid):
        raise RunError(dataset.reason)
    metrics = retrieval(dataset, config["tie_policy"])
    observed = f"{100 * metrics['r10']:.3f}"
    expected = config["expected_r10_percent"]
    code = int(expected is not None and observed != expected)
    result = {"sample": config["sample"], "text_policy": config["text_policy"],
              "model": asdict(MODELS[config["model"]]), "metrics": metrics,
              "normalization": "float64 scaled L2 cosine",
              "expected_r10_percent": expected, "observed_percent": observed}
    metrics_path.parent.mkdir(parents=True, exist_ok=True)
    with metrics_path.open("x") as stream:
        json.dump(result, stream, indent=2, allow_nan=False)
        stream.write("\n")
    completion = {"source_pin": args.source_pin, "config_pin": args.config_pin,
                  "input_pin": args.input_pin, "exit_code": code,
                  "state": "checks_failed" if code else "completed",
                  "clean_handoff": True,
                  "files": {str(metrics_path.relative_to(root)):
                            digest(metrics_path)}}
    with receipt.open("x") as stream:
        json.dump(completion, stream, indent=2)
        stream.write("\n")
    return code


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    model = commands.add_parser("model-config")
    model.add_argument("model", choices=tuple(MODELS))
    score = commands.add_parser("score")
    for flag in ("config", "receipt", "root"):
        score.add_argument("--" + flag, type=Path, required=True)
    for flag in ("source-pin", "config-pin", "input-pin"):
        score.add_argument("--" + flag, required=True)
    args = parser.parse_args(argv)
    if args.command == "model-config":
        print(json.dumps(asdict(MODELS[args.model]), indent=2))
        return 0
    try:
        return run(args)
    except (RunError, OSError, json.JSONDecodeError) as error:
        print(json.dumps({"state": "rejected", "error": str(error)}),
              file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
