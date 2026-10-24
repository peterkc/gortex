"""Prepare public beads inputs without embedding calls."""

import argparse
from collections import Counter
import hashlib
import importlib.util
import json
from pathlib import Path
import sqlite3
from collections.abc import Mapping, Sequence

STUDY = Path.home() / ".prime/research/gortex/storage-architecture/tmp"
QUALITY = STUDY / "embed-quality"
DESTINATION = STUDY / "vector/hybrid-followup/real-100k-beads"
CORPORA = STUDY / "corpora"
SYMBOLS = 100_000
QUERIES = 1_000
TEXT_CAP = 110_000


def read_rows(path: Path) -> list[dict]:
    with path.open() as stream:
        return [json.loads(line) for line in stream]


def write_rows(path: Path, rows: Sequence[Mapping]) -> None:
    with path.open("w") as stream:
        for row in rows:
            stream.write(json.dumps(row, ensure_ascii=False) + "\n")


def source_rules():
    provenance_path = DESTINATION.parent / "source-provenance.json"
    expected = json.loads(provenance_path.read_text())
    source = QUALITY / "build_dataset.py"
    actual = hashlib.sha256(source.read_bytes()).hexdigest()
    if actual != expected["build_dataset.py"]["sha256"]:
        raise RuntimeError("embed-quality builder changed; inspect before use")
    spec = importlib.util.spec_from_file_location("quality_rules", source)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def sample(rows: Sequence[Mapping], wanted: set[str], count: int,
           digest) -> list[dict]:
    if len(wanted) > count or len(rows) < count:
        raise RuntimeError("sample size cannot retain all query targets")
    selected = [dict(row) for row in rows if row["id"] in wanted]
    remaining = [row for row in rows if row["id"] not in wanted]
    remaining.sort(key=lambda row: digest("corpus:" + row["id"]))
    selected.extend(dict(row) for row in remaining[:count - len(selected)])
    return sorted(selected, key=lambda row: row["id"])


def prepare() -> None:
    rules = source_rules()
    store = CORPORA / "beads/run-v3r/store.sqlite"
    sql = """SELECT id, kind, name, file_path, start_line, end_line,
                    language, signature, doc
             FROM nodes WHERE view_gen=0 AND language='go'
             AND kind NOT IN ('file', 'import')
             AND COALESCE(data_class, '') IS NOT 'content'"""
    with sqlite3.connect(f"file:{store}?mode=ro", uri=True) as connection:
        connection.row_factory = sqlite3.Row
        rows = [dict(row) for row in connection.execute(sql)]
    sources = {}
    candidates = []
    excluded = Counter()
    seen = set()
    for row in sorted(rows, key=lambda row: rules.digest(row["id"])):
        if not row["doc"]:
            excluded["no_doc"] += 1
            continue
        path = row["file_path"]
        if path not in sources:
            source = CORPORA / "beads" / path
            sources[path] = (source.read_text(errors="replace")
                             if source.is_file() else "")
        text = rules.first_sentence(row["doc"])
        reason = rules.query_rejection(row, text, sources[path])
        if reason:
            excluded[reason] += 1
            continue
        if text in seen:
            excluded["duplicate_query"] += 1
            continue
        seen.add(text)
        candidates.append((row, text))
    if len(candidates) < QUERIES:
        raise RuntimeError("fewer than 1000 eligible doc-comment queries")
    chosen = candidates[:QUERIES]
    selected = sample(rows, {row["id"] for row, _ in chosen}, SYMBOLS,
                      rules.digest)
    selected = [{**row, "repo": "beads"} for row in selected]
    old_symbols = read_rows(QUALITY / "symbols.jsonl")
    old_beads = {row["id"] for row in old_symbols if row["repo"] == "beads"}
    if not old_beads.issubset({row["id"] for row in selected}):
        raise RuntimeError("expanded sample is not nested around old 10k")
    old_requests = {row["id"]: row for row in
                    read_rows(QUALITY / "strip-requests.jsonl")
                    if row["id"].startswith("beads:")}
    old_stripped = {row["id"]: row for row in
                    read_rows(QUALITY / "stripped-files.jsonl")
                    if row["id"].startswith("beads:")}
    requests = {}
    cached = {}
    for row in selected:
        path = row["file_path"]
        key = "beads:" + path
        if key in requests or key in cached:
            continue
        source = CORPORA / "beads" / path
        if not source.is_file():
            continue
        body = sources.get(path)
        if body is None:
            body = source.read_text(errors="replace")
        prior = old_requests.get(key)
        if (prior and prior["body"] == body and key in old_stripped):
            cached[key] = old_stripped[key]
        else:
            requests[key] = {"id": key, "operation": "strip",
                             "language": "go", "body": body}
    queries = [{"repo": "beads", "language": "go",
                "id": "beads::" + row["id"], "text": text}
               for row, text in chosen]
    DESTINATION.mkdir(exist_ok=True)
    write_rows(DESTINATION / "symbols.jsonl", selected)
    write_rows(DESTINATION / "queries.jsonl", queries)
    write_rows(DESTINATION / "strip-requests.jsonl", list(requests.values()))
    write_rows(DESTINATION / "stripped-cached.jsonl", list(cached.values()))
    stats = {"primary_eligible_symbols": len(rows),
             "query_candidates": len(candidates), "queries": len(queries),
             "sampled_symbols": len(selected), "nested_old_symbols": 10000,
             "query_exclusions": dict(excluded),
             "cached_stripped_files": len(cached),
             "new_strip_requests": len(requests), "text_cap": TEXT_CAP}
    (DESTINATION / "dataset-stats.json").write_text(
        json.dumps(stats, indent=2) + "\n"
    )
    print(json.dumps(stats))


def stripped_files() -> dict[str, dict]:
    rows = read_rows(DESTINATION / "stripped-cached.jsonl")
    rows += read_rows(DESTINATION / "stripped-new.jsonl")
    if any(row.get("error") for row in rows):
        raise RuntimeError("textbuilder failed to strip a selected file")
    return {row["id"]: row for row in rows}


def make_chunks() -> None:
    files = stripped_files()
    requests = []
    for row in read_rows(DESTINATION / "symbols.jsonl"):
        start, end = row["start_line"], row["end_line"]
        if start <= 0 or end - start + 1 <= 60:
            continue
        file_row = files.get("beads:" + row["file_path"], {})
        source = file_row.get("text", "")
        body = "".join(source.splitlines(keepends=True)[start - 1:end])
        if body:
            requests.append({"operation": "chunk", "body": body,
                             "language": "go", "id": "beads::" + row["id"]})
    write_rows(DESTINATION / "chunk-requests.jsonl", requests)
    print(json.dumps({"chunk_requests": len(requests)}))


def corpus_rows(symbols: Sequence[Mapping], chunks: Mapping) -> list[dict]:
    corpus = []
    for row in symbols:
        sid = "beads::" + row["id"]
        meta = (f'{row["kind"]} {row["name"]} '
                f'{row["signature"] or ""} {row["file_path"]}')
        pieces = chunks.get(sid, {}).get("chunks", [])
        if len(pieces) > 1:
            for piece in pieces:
                text = f'{row["kind"]} {row["name"]} {piece["Text"]}'
                corpus.append({"id": sid + f'#chunk{piece["WindowIndex"]}',
                               "parent_id": sid, "repo": "beads",
                               "text": text})
        else:
            text = meta + " " + pieces[0]["Text"] if pieces else meta
            corpus.append({"id": sid, "parent_id": sid, "repo": "beads",
                           "text": text})
    return corpus


def finish() -> None:
    symbols = read_rows(DESTINATION / "symbols.jsonl")
    chunk_rows = read_rows(DESTINATION / "chunks.jsonl")
    if any(row.get("error") for row in chunk_rows):
        raise RuntimeError("textbuilder failed to chunk a selected symbol")
    corpus = corpus_rows(symbols, {row["id"]: row for row in chunk_rows})
    if len(corpus) > TEXT_CAP:
        raise RuntimeError("follow-up 110k text cap exceeded; no API calls")
    if len({row["id"] for row in corpus}) != len(corpus):
        raise RuntimeError("duplicate corpus record IDs")
    old = {row["id"]: row for row in read_rows(QUALITY / "corpus.jsonl")
           if row["repo"] == "beads"}
    by_id = {row["id"]: row for row in corpus}
    if any(key not in by_id or by_id[key]["text"] != row["text"]
           for key, row in old.items()):
        raise RuntimeError("old 10k text identity drift; inspect before calls")
    write_rows(DESTINATION / "corpus.jsonl", corpus)
    stats_path = DESTINATION / "dataset-stats.json"
    stats = json.loads(stats_path.read_text())
    stats.update({"embedded_texts": len(corpus),
                  "utf8_bytes": sum(len(row["text"].encode())
                                    for row in corpus),
                  "old_vector_records_reusable": len(old)})
    stats_path.write_text(json.dumps(stats, indent=2) + "\n")
    print(json.dumps(stats))


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("stage", choices=("prepare", "chunks", "finish"))
    stage = parser.parse_args().stage
    match stage:
        case "prepare":
            prepare()
        case "chunks":
            make_chunks()
        case "finish":
            finish()


if __name__ == "__main__":
    main()
