"""Embed public inputs with a durable six-million-token cap."""

import argparse
import base64
from collections.abc import Mapping, Sequence
import fcntl
import hashlib
import json
import os
from pathlib import Path
import tempfile
import time

import numpy as np
import requests

ROOT = (Path.home() / ".prime/research/gortex/storage-architecture/tmp"
        / "vector/hybrid-followup")
DATA = ROOT / "real-100k-beads"
QUALITY = ROOT.parent.parent / "embed-quality"
CAP = 6_000_000
MODEL = "voyage-code-4"
DIMS = 1024


def atomic_json(path: Path, value: Mapping) -> None:
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent,
                                     delete=False) as stream:
        temporary = Path(stream.name)
        try:
            json.dump(value, stream, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        except BaseException:
            temporary.unlink(missing_ok=True)
            raise
    try:
        temporary.replace(path)
        descriptor = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    finally:
        temporary.unlink(missing_ok=True)


def read_rows(path: Path) -> list[dict]:
    with path.open() as stream:
        return [json.loads(line) for line in stream]


def key(text: str, kind: str) -> str:
    value = {"text": text, "input_type": kind, "model": MODEL,
             "dims": DIMS, "output_dtype": "float", "truncation": True}
    encoded = json.dumps(value, sort_keys=True).encode()
    return hashlib.sha256(encoded).hexdigest()


def token_bound(text: str) -> int:
    return min(len(text.encode()) + 64, 32_064)


def batch_rows(rows: Sequence[Mapping]):
    batch = []
    bound = 0
    for row in rows:
        extra = token_bound(row["text"])
        if batch and (len(batch) >= 128 or bound + extra > 110_000):
            yield batch, bound
            batch, bound = [], 0
        batch.append(row)
        bound += extra
    if batch:
        yield batch, bound


def reserve(ledger: Mapping, identity: str, bound: int) -> dict:
    if ledger["pending"] is not None:
        raise RuntimeError("unreconciled paid attempt; no further calls")
    if ledger["charged_or_reserved_tokens"] + bound > CAP:
        raise RuntimeError("six-million-token cap would be exceeded")
    return {**ledger,
            "charged_or_reserved_tokens":
            ledger["charged_or_reserved_tokens"] + bound,
            "attempted_calls": ledger["attempted_calls"] + 1,
            "pending": {"identity": identity, "bound": bound}}


def settle(ledger: Mapping, identity: str, used: int) -> dict:
    pending = ledger["pending"]
    if pending is None or pending["identity"] != identity or used < 0:
        raise RuntimeError("paid receipt does not match reservation")
    return {**ledger,
            "charged_or_reserved_tokens":
            ledger["charged_or_reserved_tokens"] + used - pending["bound"],
            "confirmed_tokens": ledger["confirmed_tokens"] + used,
            "pending": None}


def decode_vectors(result: Mapping, count: int) -> np.ndarray:
    rows = sorted(result["data"], key=lambda row: row["index"])
    if [row["index"] for row in rows] != list(range(count)):
        raise RuntimeError("incomplete or duplicate provider indices")
    vectors = np.stack([
        np.frombuffer(base64.b64decode(row["embedding"], validate=True),
                      dtype="<f4")
        if isinstance(row["embedding"], str)
        else np.asarray(row["embedding"], dtype="<f4")
        for row in rows
    ])
    if vectors.shape != (count, DIMS) or not np.isfinite(vectors).all():
        raise RuntimeError("provider returned invalid float32 vectors")
    if np.any(np.sum(vectors.astype(np.float64) ** 2, axis=1) == 0):
        raise RuntimeError("provider returned a zero vector")
    return vectors


def input_plan(kind: str) -> tuple[list[dict], dict[str, int], list[dict]]:
    name = "corpus.jsonl" if kind == "document" else "queries.jsonl"
    rows = read_rows(DATA / name)
    cached_rows = read_rows(QUALITY / name)
    cache = {}
    for index, row in enumerate(cached_rows):
        cache.setdefault(key(row["text"], kind), index)
    needed = {}
    for index, row in enumerate(rows):
        identity = key(row["text"], kind)
        if identity in cache:
            continue
        if identity not in needed:
            needed[identity] = {"identity": identity, "text": row["text"],
                                "destinations": []}
        needed[identity]["destinations"].append(index)
    return rows, cache, list(needed.values())


def save_vectors(path: Path, vectors: np.ndarray) -> None:
    with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as stream:
        temporary = Path(stream.name)
        try:
            np.save(stream, vectors)
            stream.flush()
            os.fsync(stream.fileno())
        except BaseException:
            temporary.unlink(missing_ok=True)
            raise
    try:
        temporary.replace(path)
    finally:
        temporary.unlink(missing_ok=True)


def execute_kind(kind: str, session: requests.Session, ledger: dict) -> dict:
    rows, cache, needed = input_plan(kind)
    destination = DATA / "voyage-code-4"
    cache_matrix = np.load(QUALITY / "voyage-code-4" / f"{kind}.npy",
                           mmap_mode="r")
    if cache_matrix.shape != (len(read_rows(QUALITY /
                                          ("corpus.jsonl" if kind == "document"
                                           else "queries.jsonl"))), DIMS):
        raise RuntimeError("cached source matrix/metadata mismatch")
    output_path = destination / f"{kind}.npy"
    pending_path = destination / f"{kind}.assembling.npy"
    matrix = np.lib.format.open_memmap(pending_path, mode="w+", dtype="<f4",
                                       shape=(len(rows), DIMS))
    reused = 0
    for index, row in enumerate(rows):
        cached = cache.get(key(row["text"], kind))
        if cached is not None:
            matrix[index] = cache_matrix[cached]
            reused += 1
    for number, (batch, bound) in enumerate(batch_rows(needed)):
        identities = [row["identity"] for row in batch]
        identity = hashlib.sha256(json.dumps(identities).encode()).hexdigest()
        receipt_path = destination / f"{kind}-{number:04d}.json"
        vector_path = destination / f"{kind}-{number:04d}.npy"
        if receipt_path.exists():
            receipt = json.loads(receipt_path.read_text())
            if receipt["identity"] != identity:
                raise RuntimeError("paid batch input drift")
            vectors = np.load(vector_path)
            digest = hashlib.sha256(vector_path.read_bytes()).hexdigest()
            if receipt["sha256"] != digest:
                raise RuntimeError("paid vector file checksum mismatch")
            pending = ledger["pending"]
            if pending is not None and pending["identity"] == identity:
                ledger = settle(ledger, identity, receipt["tokens"])
                atomic_json(ROOT / "voyage-spend-ledger.json", ledger)
        else:
            ledger = reserve(ledger, identity, bound)
            atomic_json(ROOT / "voyage-spend-ledger.json", ledger)
            payload = {"model": MODEL, "input_type": kind,
                       "input": [row["text"] for row in batch],
                       "output_dimension": DIMS, "output_dtype": "float",
                       "encoding_format": "base64", "truncation": True}
            start = time.perf_counter()
            response = session.post(
                "https://api.voyageai.com/v1/embeddings", json=payload,
                timeout=(15, 180),
            )
            if response.status_code != 200:
                raise RuntimeError(
                    f"Voyage HTTP {response.status_code}; reservation retained"
                )
            result = response.json()
            used = result["usage"]["total_tokens"]
            if type(used) is not int or used < 0:
                raise RuntimeError("invalid provider token count")
            if used > bound:
                ledger = {**ledger,
                          "charged_or_reserved_tokens":
                          ledger["charged_or_reserved_tokens"] + used - bound,
                          "confirmed_tokens":
                          ledger["confirmed_tokens"] + used,
                          "pending": {"identity": identity, "bound": used,
                                      "violation": "provider_token_bound"}}
                atomic_json(ROOT / "voyage-spend-ledger.json", ledger)
                raise RuntimeError("provider token bound violated; STOP calls")
            vectors = decode_vectors(result, len(batch))
            save_vectors(vector_path, vectors)
            receipt = {"identity": identity, "tokens": used, "bound": bound,
                       "seconds": time.perf_counter() - start,
                       "texts": len(batch), "model": MODEL,
                       "sha256": hashlib.sha256(vector_path.read_bytes())
                       .hexdigest()}
            atomic_json(receipt_path, receipt)
            ledger = settle(ledger, identity, used)
            atomic_json(ROOT / "voyage-spend-ledger.json", ledger)
            if used > bound or ledger["charged_or_reserved_tokens"] > CAP:
                raise RuntimeError("provider token bound violated; STOP calls")
            print(kind, number, len(batch), used, flush=True)
        if vectors.shape != (len(batch), DIMS):
            raise RuntimeError("saved paid batch shape mismatch")
        for row, vector in zip(batch, vectors, strict=True):
            for index in row["destinations"]:
                matrix[index] = vector
    matrix.flush()
    del matrix
    descriptor = os.open(pending_path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    pending_path.replace(output_path)
    info = {"kind": kind, "rows": len(rows), "reused_rows": reused,
            "new_unique_texts": len(needed), "dimensions": DIMS,
            "matrix_sha256": hashlib.sha256(output_path.read_bytes())
            .hexdigest()}
    atomic_json(destination / f"{kind}-complete.json", info)
    return ledger


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--execute", action="store_true")
    args = parser.parse_args()
    if not args.execute:
        planned = {}
        for kind in ("document", "query"):
            rows, cache, needed = input_plan(kind)
            batches = list(batch_rows(needed))
            planned[kind] = {"rows": len(rows),
                             "reused_rows": sum(key(row["text"], kind)
                                                in cache for row in rows),
                             "new_unique_texts": len(needed),
                             "batches": len(batches),
                             "sum_conservative_bounds": sum(bound for _, bound
                                                            in batches)}
        print(json.dumps({"execute": False, "token_cap": CAP,
                          "inputs": planned}, indent=2))
        return
    if (os.environ.get("DOPPLER_PROJECT") != "prime"
            or os.environ.get("DOPPLER_CONFIG") != "dev_host"):
        raise RuntimeError("launch via doppler run -p prime -c dev_host")
    token = os.environ["VOYAGE_API_KEY"]
    destination = DATA / "voyage-code-4"
    destination.mkdir(exist_ok=True)
    with (ROOT / "voyage-ledger.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        ledger_path = ROOT / "voyage-spend-ledger.json"
        if ledger_path.exists():
            ledger = json.loads(ledger_path.read_text())
        else:
            if (any(destination.glob("document-*.json"))
                    or any(destination.glob("query-*.json"))):
                raise RuntimeError("paid receipts exist without spend ledger")
            ledger = {"charged_or_reserved_tokens": 0,
                      "confirmed_tokens": 0, "attempted_calls": 0,
                      "pending": None, "cap_tokens": CAP}
        if ledger["cap_tokens"] != CAP:
            raise RuntimeError("ledger cap drift")
        with requests.Session() as session:
            session.headers["Authorization"] = "Bearer " + token
            for kind in ("document", "query"):
                ledger = execute_kind(kind, session, ledger)
        print(json.dumps({"complete": True, "ledger": ledger}))


if __name__ == "__main__":
    main()
