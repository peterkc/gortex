"""Build the frozen public body corpus. No inference or network access."""
import argparse
import ast
import hashlib
import inspect
import io
import json
import subprocess
import sys
import tokenize
from collections import Counter
from collections.abc import Mapping, Sequence
from pathlib import Path, PurePosixPath


PREFIX = "Represent this query for searching relevant code: "
QUOTAS = ((2048, 450, 25), (4096, 40, 20), (8192, 10, 5))
SOURCES = {
    "go": ("https://github.com/golang/go", "go1.23.2",
           "ed07b321aef7632f956ce991dd10fdd7e1abd827"),
    "python": ("https://github.com/python/cpython", "v3.12.7",
               "0b05ead877f909b7efe712db758012d9dbece7ce"),
}
RECIPE = {
    "schema": "accuracy-corpus-recipe/v1",
    "header": "{kind} {name}\n{signature}\n{path}\n\n{body}",
    "blanking": "comments and all Python docstrings; preserve LF and spans",
    "spans": "UTF-8 decoded byte offsets; line numbers start at one",
    "query": "first nonempty paragraph; delimiters/common indent only",
    "query_chars": [30, 400], "query_tokens_max": 512,
    "query_prefix": PREFIX,
    "tokens": ("untruncated complete header/body or prefixed query; "
               "special tokens"),
    "duplicate_queries": "first canonical parent ID owns the exact query text",
    "order": ("language, bucket; targets first then fill; "
              "SHA256(lang LF bucket LF ID)"),
    "quotas": QUOTAS,
}


class CorpusError(Exception):
    """Refused corpus input or incomplete frozen selection."""


def canonical(value: object) -> bytes:
    return json.dumps(value, sort_keys=True, ensure_ascii=False,
                      separators=(",", ":"), allow_nan=False).encode("utf-8")


def digest(content: bytes) -> str:
    return hashlib.sha256(content).hexdigest()


def file_hash(path: Path) -> str:
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def unique_object(pairs: Sequence[tuple[str, object]]) -> dict:
    result = {}
    for key, value in pairs:
        if key in result:
            raise CorpusError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def read_json(path: Path):
    return json.loads(path.read_text(encoding="utf-8"),
                      object_pairs_hook=unique_object)


def write_json(path: Path, value: object) -> None:
    with path.open("xb") as stream:
        stream.write(canonical(value) + b"\n")


def safe_path(name: str) -> PurePosixPath:
    path = PurePosixPath(name)
    if (not name or path.is_absolute() or str(path) != name
            or any(part in (".", "..") for part in path.parts)
            or "\\" in name or any(ord(c) < 32 for c in name)):
        raise CorpusError(f"noncanonical or escaping source path: {name!r}")
    return path


def git_output(repository: Path, *args: str, data=None) -> bytes:
    result = subprocess.run(["git", "-C", str(repository), *args],
                            input=data, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=120, check=False)
    if result.returncode:
        raise CorpusError("Git input failed: " + result.stderr.decode(
            "utf-8", errors="replace"))
    return result.stdout


def source_files(repository: Path, source: Mapping) -> tuple[dict, bytes]:
    commit, tree = source["commit"], source["tree"]
    head = git_output(repository, "rev-parse", "HEAD").decode().strip()
    actual_tree = git_output(repository, "rev-parse", "HEAD^{tree}")
    if head != commit or actual_tree.decode().strip() != tree:
        raise CorpusError("source commit/tree mismatch")
    listing = git_output(repository, "ls-tree", "-rz", "--full-tree", commit)
    entries, seen = [], set()
    for entry in listing.split(b"\0"):
        if not entry:
            continue
        metadata, raw_path = entry.split(b"\t", 1)
        mode, kind, oid = metadata.decode().split()
        path = raw_path.decode("utf-8")
        safe_path(path)
        if path in seen:
            raise CorpusError("duplicate/aliased source path")
        seen.add(path)
        if not (eligible_path(path, source["language"]) or path == "LICENSE"):
            continue
        if mode not in ("100644", "100755") or kind != "blob":
            raise CorpusError(f"linked/submodule/nonregular source: {path}")
        entries.append((path, oid))
    # One batch reads only object IDs from the pinned tree. Working files and
    # submodule contents never supply corpus bytes.
    payload = git_output(repository, "cat-file", "--batch", data=(
        "".join(oid + "\n" for _, oid in entries).encode()))
    files, license_bytes, offset = {}, None, 0
    for path, oid in entries:
        end = payload.index(b"\n", offset)
        got_oid, kind, size_text = payload[offset:end].decode().split()
        size = int(size_text)
        if (got_oid != oid or kind != "blob" or size > 16 * 1024 * 1024
                or len(payload) > 1024 ** 3):
            raise CorpusError("source object identity/type/size mismatch")
        content = payload[end + 1:end + 1 + size]
        if (len(content) != size
                or payload[end + 1 + size:end + 2 + size] != b"\n"):
            raise CorpusError("incomplete source object")
        offset = end + 2 + size
        if path == "LICENSE":
            license_bytes = content
        else:
            files[path] = content
    if offset != len(payload) or license_bytes is None:
        raise CorpusError("source objects incomplete or missing LICENSE")
    return files, license_bytes


def eligible_path(path: str, language: str) -> bool:
    parts = PurePosixPath(path).parts
    if language == "go":
        return (len(parts) > 1 and parts[0] == "src"
                and "vendor" not in parts and "testdata" not in parts
                and path.endswith(".go"))
    return (len(parts) > 1 and parts[0] == "Lib"
            and parts[1] != "test" and path.endswith(".py"))


def paragraph(documentation: str) -> str:
    cleaned = inspect.cleandoc(documentation).strip("\n")
    lines = []
    for line in cleaned.splitlines():
        if not line.strip():
            if lines:
                break
            continue
        lines.append(line)
    return "\n".join(lines).strip()


def offsets(source: bytes) -> list[int]:
    values = [0]
    for line in source.splitlines(keepends=True):
        values.append(values[-1] + len(line))
    return values


def python_extract(path: str, content: bytes) -> list[dict]:
    # Decode according to the file's Python coding cookie, then freeze UTF-8
    # offsets into decoded UTF-8. Original bytes have their own hash.
    encoding, _ = tokenize.detect_encoding(io.BytesIO(content).readline)
    source = content.decode(encoding)
    raw = source.encode("utf-8")
    lines, starts = source.splitlines(keepends=True), offsets(raw)
    tree = ast.parse(source, filename=path)
    blank = bytearray(raw)

    def position(line, column):
        return starts[line - 1] + len(lines[line - 1][:column].encode("utf-8"))

    def erase(start, end):
        for i in range(start, end):
            if blank[i] not in (10, 13):
                blank[i] = 32

    tokens = list(tokenize.generate_tokens(io.StringIO(source).readline))
    source_comments = []
    for token in tokens:
        if token.type == tokenize.COMMENT:
            start, end = position(*token.start), position(*token.end)
            source_comments.append(raw[start:end].decode())
            erase(start, end)
    for node in ast.walk(tree):
        body = getattr(node, "body", None)
        if (isinstance(node, (ast.Module, ast.ClassDef, ast.FunctionDef,
                              ast.AsyncFunctionDef)) and body
                and isinstance(body[0], ast.Expr)
                and isinstance(body[0].value, ast.Constant)
                and isinstance(body[0].value.value, str)):
            doc = body[0]
            start = starts[doc.lineno - 1] + doc.col_offset
            end = starts[doc.end_lineno - 1] + doc.end_col_offset
            source_comments.append(raw[start:end].decode())
            erase(start, end)
    result = []
    comments_text = "\n".join(source_comments)

    def visit(node, names):
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            first = min([node.lineno]
                        + [n.lineno for n in node.decorator_list])
            start = starts[first - 1] + (node.col_offset if
                                        first == node.lineno else 0)
            end = starts[node.end_lineno - 1] + node.end_col_offset
            depth, colon = 0, None
            for token in tokens:
                if token.start < (node.lineno, 0):
                    continue
                if token.type == tokenize.OP:
                    if token.string in ("(", "[", "{"):
                        depth += 1
                    elif token.string in (")", "]", "}"):
                        depth -= 1
                    elif token.string == ":" and depth == 0:
                        colon = position(*token.end)
                        break
            if colon is None:
                raise CorpusError(f"missing function signature: {path}")
            signature_start = starts[node.lineno - 1] + node.col_offset
            result.append({
                "path": path, "name": ".".join((*names, node.name)),
                "kind": "method" if names else "function",
                "start": start, "end": end, "line": first,
                "end_line": node.end_lineno,
                "signature": bytes(blank[signature_start:colon]).decode(),
                "body": bytes(blank[start:end]).decode(),
                "documentation": ast.get_docstring(node, clean=False) or "",
                "notice": source[:starts[tree.body[0].lineno - 1]],
                "source_comments": comments_text,
            })
            return  # Nested definitions stay inside this body only.
        child_names = names
        if isinstance(node, ast.ClassDef):
            child_names = (*names, node.name)
        for child in ast.iter_child_nodes(node):
            visit(child, child_names)

    visit(tree, ())
    return result


def go_extract(executable: Path, path: str, content: bytes) -> list[dict]:
    request = canonical({"path": path, "source": content.decode("utf-8")})
    result = subprocess.run([str(executable)], input=request + b"\n",
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            timeout=30, check=False)
    if result.returncode:
        raise CorpusError(f"Go parse failed for {path}: "
                          + result.stderr.decode("utf-8", errors="replace"))
    extracted = json.loads(result.stdout, object_pairs_hook=unique_object)
    return [{**row, "source_comments": extracted["source_comments"]}
            for row in extracted["declarations"]]


def candidates(files: Mapping[str, bytes], source: Mapping,
               extractor: Path) -> list[dict]:
    result = []
    for path in sorted(files):
        raw = files[path]
        extracted = (python_extract(path, raw)
                     if source["language"] == "python"
                     else go_extract(extractor, path, raw))
        for declaration in extracted:
            identity = {key: declaration[key] for key in
                        ("path", "name", "start", "end")}
            identity.update(repo=source["repo"], commit=source["commit"],
                            tree=source["tree"])
            parent = digest(canonical(identity))
            text = RECIPE["header"].format(**declaration)
            result.append({
                **declaration, **identity, "id": parent, "parent_id": parent,
                "language": source["language"], "text": text,
                "source_sha256": digest(raw),
                "body_sha256": digest(text.encode()),
                "query": paragraph(declaration["documentation"]),
            })
    return result


def measure(rows: Sequence[Mapping], tokenizer) -> list[dict]:
    result = []
    for row in rows:
        body = tokenizer(row["text"], truncation=False, padding=False,
                         add_special_tokens=True)["input_ids"]
        query = row["query"]
        query_size = None
        if 30 <= len(query) <= 400:
            query_size = len(tokenizer(
                PREFIX + query, truncation=False, padding=False,
                add_special_tokens=True)["input_ids"])
        result.append({**row, "tokens": len(body), "query_tokens": query_size})
    return result


def bucket(tokens: int) -> int | None:
    return next((upper for upper, _, _ in QUOTAS if tokens <= upper), None)


def select(rows: Sequence[Mapping], quotas=QUOTAS,
           languages=("go", "python")) -> tuple[list, list, dict]:
    ids, unique_text, prepared, rejected = set(), set(), [], Counter()
    for row in sorted(rows, key=lambda r: r["id"]):
        parent = row["id"]
        if parent in ids or row["parent_id"] != parent:
            raise CorpusError("duplicate ID or conflicting parent ownership")
        ids.add(parent)
        valid = (30 <= len(row["query"]) <= 400
                 and row["query_tokens"] is not None
                 and row["query_tokens"] <= 512)
        if valid and row["query"] in unique_text:
            valid = False
            rejected["duplicate_query_text"] += 1
        elif valid:
            unique_text.add(row["query"])
        else:
            rejected["query_length"] += 1
        prepared.append({**row, "labelled": valid})
    counts = {"population": {}, "availability": {}, "rejected": dict(rejected)}
    documents, queries, shortfalls = [], [], []
    for language in languages:
        population = [r for r in prepared if r["language"] == language]
        counts["population"][language] = {
            "total": len(population),
            **{f"above_{n}": sum(r["tokens"] > n for r in population)
               for n in (2048, 4096, 8192)},
        }
        counts["availability"][language] = {}
        previous = 0
        for upper, document_count, query_count in quotas:
            available = [r for r in population
                         if previous < r["tokens"] <= upper]
            available.sort(key=lambda r: digest(
                f"{language}\n{upper}\n{r['id']}".encode()))
            labelled = [r for r in available if r["labelled"]]
            counts["availability"][language][str(upper)] = {
                "documents": len(available), "queries": len(labelled),
                "required_documents": document_count,
                "required_queries": query_count,
            }
            if len(available) < document_count or len(labelled) < query_count:
                shortfalls.append(f"{language}:{upper}")
            targets = labelled[:query_count]
            target_ids = {r["id"] for r in targets}
            chosen = targets + [r for r in available
                                if r["id"] not in target_ids]
            documents.extend(chosen[:document_count])
            queries.extend(targets)
            previous = upper
    counts["shortfalls"] = shortfalls
    selected = {"documents": len(documents), "queries": len(queries),
                "by_language": {}}
    for language in languages:
        ds = [r for r in documents if r["language"] == language]
        qs = [r for r in queries if r["language"] == language]
        selected["by_language"][language] = {
            "documents": len(ds), "queries": len(qs),
            "documents_by_bucket": {
                str(upper): sum(bucket(r["tokens"]) == upper for r in ds)
                for upper, _, _ in quotas},
            "queries_by_bucket": {
                str(upper): sum(bucket(r["tokens"]) == upper for r in qs)
                for upper, _, _ in quotas},
        }
    counts["selected"] = selected
    return documents, queries, counts


def tokenizer_files(directory: Path, pins: Mapping) -> dict:
    if directory.is_symlink() or not directory.is_dir():
        raise CorpusError("tokenizer must be a real local directory")
    expected = {Path(name).name: pin for name, pin in pins.items()
                if name.startswith("data/cpu-cost/tokenizer/")}
    if (len(expected) != 5
            or {p.name for p in directory.iterdir()} != set(expected)):
        raise CorpusError("expected exactly five pinned tokenizer files")
    for name, pin in expected.items():
        path = directory / name
        if path.is_symlink() or not path.is_file() or file_hash(path) != pin:
            raise CorpusError(f"tokenizer hash/type mismatch: {name}")
    return expected


def input_manifest(path: Path, pin: str) -> dict:
    if file_hash(path) != pin:
        raise CorpusError("input manifest hash mismatch")
    manifest = read_json(path)
    if (set(manifest) != {"schema", "sources"}
            or manifest["schema"] != "accuracy-sources/v1"):
        raise CorpusError("expected accuracy-sources/v1")
    sources = manifest["sources"]
    if len(sources) != 2 or {s["language"] for s in sources} != set(SOURCES):
        raise CorpusError("exactly Go and Python sources required")
    for source in sources:
        language = source["language"]
        repo, tag, commit = SOURCES[language]
        expected_tree = {
            "go": "b5db4f10f3238e4c0de0645a9e31f9a123e071f9",
            "python": "c572705d83b94fe0c9c3080bc3fab4ac2c2ee32f",
        }[language]
        if (set(source) != {"language", "repo", "tag", "commit", "tree"}
                or (source["repo"], source["tag"], source["commit"])
                != (repo, tag, commit) or source["tree"] != expected_tree):
            raise CorpusError(f"invalid pinned source: {language}")
    return manifest


def emit(output: Path, documents: Sequence[Mapping],
         queries: Sequence[Mapping],
         counts: Mapping, bindings: Mapping, licenses: Mapping) -> str:
    if counts["shortfalls"]:
        missing = ", ".join(counts["shortfalls"])
        raise CorpusError("quota shortfall: " + missing)
    fields = ("id", "parent_id", "repo", "language", "text")
    files = {}
    for name, rows, keys in (("documents.jsonl", documents, fields),
                             ("queries.jsonl", queries,
                              ("id", "repo", "language", "text"))):
        with (output / name).open("xb") as stream:
            for row in rows:
                value = {k: (row["query"] if name == "queries.jsonl"
                             and k == "text" else row[k]) for k in keys}
                stream.write(canonical(value) + b"\n")
        files[name] = file_hash(output / name)
    with (output / "provenance.jsonl").open("xb") as stream:
        for row in documents:
            stream.write(canonical({k: v for k, v in row.items()
                                    if k not in ("text", "body")}) + b"\n")
    files["provenance.jsonl"] = file_hash(output / "provenance.jsonl")
    write_json(output / "ordered-ids.json", {
        "documents": [r["id"] for r in documents],
        "queries": [r["id"] for r in queries]})
    license_manifest = []
    for language, content in sorted(licenses.items()):
        name = language + "-LICENSE"
        (output / name).write_bytes(content)
        files[name] = digest(content)
        notices = {
            r["path"]: {
                "preamble": r["notice"],
                "comments_and_docstrings": r.get("source_comments", "")}
            for r in documents if r["language"] == language}
        license_manifest.append({"language": language, "license": name,
                                 "sha256": files[name],
                                 "file_notices": notices})
    write_json(output / "source-license.json", {
        "sources": bindings["sources"], "licenses": license_manifest,
        "review": "Complete notices retained; no license classifier applied."})
    for name in ("counts.json", "ordered-ids.json", "source-license.json"):
        files[name] = file_hash(output / name)
    frozen = {"schema": "accuracy-freeze/v1", "files": files,
              "bindings": bindings, "recipe": RECIPE}
    # This file is the exact canonical material whose digest names the freeze.
    freeze_bytes = canonical(frozen)
    (output / "freeze-canonical.json").write_bytes(freeze_bytes)
    freeze_id = digest(freeze_bytes)
    write_json(output / "freeze.json", {"freeze_id": freeze_id, **frozen})
    return freeze_id


def build(args: argparse.Namespace) -> None:
    manifest = input_manifest(args.manifest, args.manifest_pin)
    storage = Path(__file__).resolve().parents[1]
    pins_path = storage / "pod-cost/cpu-cost-v1.json"
    pins = read_json(pins_path)
    token_pins = tokenizer_files(args.tokenizer, pins["inputs"])
    args.output.mkdir()  # Existing output, even partial, is never resumed.
    try:
        from transformers import AutoTokenizer

        tokenizer = AutoTokenizer.from_pretrained(
            args.tokenizer, local_files_only=True, trust_remote_code=False)
        rows, licenses, bound_sources = [], {}, []
        for source in sorted(manifest["sources"], key=lambda s: s["language"]):
            repository = args.sources / source["language"]
            files, license_bytes = source_files(repository, source)
            licenses[source["language"]] = license_bytes
            license_pin = digest(licenses[source["language"]])
            bound_sources.append({**source, "license_sha256": license_pin})
            rows.extend({**row, "license_sha256": license_pin} for row in
                        candidates(files, source, args.go_extractor))
        documents, queries, counts = select(measure(rows, tokenizer))
        write_json(args.output / "counts.json", counts)
        checkout = Path(__file__).resolve().parents[3]
        revision = git_output(checkout, "rev-parse", "HEAD").decode().strip()
        bindings = {"sources": bound_sources, "tokenizer": token_pins,
                    "harness_revision": revision,
                    "input_manifest_sha256": args.manifest_pin,
                    "builder_sha256": file_hash(Path(__file__)),
                    "extractor_sha256": file_hash(args.go_extractor),
                    "python_environment": {
                        name: file_hash(pins_path.parent / name)
                        for name in ("pyproject.toml", "uv.lock")}}
        emit(args.output, documents, queries, counts, bindings, licenses)
    except (CorpusError, OSError, ValueError, SyntaxError,
            tokenize.TokenError, subprocess.SubprocessError) as error:
        write_json(args.output / "diagnostics.json", {
            "state": "rejected", "error": str(error)})
        raise


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("manifest", "sources", "tokenizer", "go-extractor", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--manifest-pin", required=True)
    args = parser.parse_args(argv)
    try:
        build(args)
    except (CorpusError, OSError, ValueError, SyntaxError,
            tokenize.TokenError, subprocess.SubprocessError) as error:
        print(json.dumps({"state": "rejected", "error": str(error)}),
              file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
