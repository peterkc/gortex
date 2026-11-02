"""Prepare labelled language subsets for the unchanged evaluator."""
import argparse
import hashlib
import json
from pathlib import Path

import numpy as np

from artifacts import digest


def write(path: Path, value: dict) -> None:
    with path.open("x") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")


def owned(root: Path, name: str) -> Path:
    path = root / name
    if path.resolve() != path or root not in path.parents:
        raise ValueError("group input escaped root or used a path alias")
    return path


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--cut", type=Path, required=True)
    args = parser.parse_args()
    root = args.root.resolve()
    cut = json.loads(args.cut.read_text())
    original = owned(root, cut["score_command"][
        cut["score_command"].index("--config") + 1])
    if digest(original) != cut["config_pin"]:
        raise ValueError("changed parent config")
    config = json.loads(original.read_text())
    for key in ("documents", "queries", "vectors"):
        if digest(owned(root, config[key]["path"])) != config[key]["sha256"]:
            raise ValueError("changed parent input")
    query_path = owned(root, config["queries"]["path"])
    query_lines = query_path.read_bytes().splitlines(keepends=True)
    rows = [json.loads(line) for line in query_lines]
    vectors = np.load(owned(root, config["vectors"]["path"]),
                      allow_pickle=False)
    documents = config["documents"]["rows"]
    groups = {}
    for language, count in (("go", 50), ("python", 29)):
        indices = [i for i, row in enumerate(rows)
                   if row["language"] == language]
        if len(indices) != count:
            raise ValueError("wrong language denominator")
        output = args.cut.parent / language
        output.mkdir()
        queries = output / "queries.jsonl"
        with queries.open("xb") as stream:
            stream.write(b"".join(query_lines[i] for i in indices))
        path = output / "vectors.npy"
        with path.open("xb") as stream:
            np.save(stream, np.concatenate((
                vectors[:documents], vectors[[documents + i for i in indices]]
            )), allow_pickle=False)
        subset = {**config,
                  "queries": {"path": str(queries.relative_to(root)),
                              "sha256": digest(queries), "rows": count},
                  "vectors": {"path": str(path.relative_to(root)),
                              "sha256": digest(path),
                              "rows": documents + count}}
        target = output / "config.json"
        write(target, subset)
        pins = {key: subset[key]["sha256"] for key in
                ("documents", "queries", "vectors")}
        input_pin = hashlib.sha256(json.dumps(
            pins, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        command = list(cut["score_command"])
        for flag, value in (("--config", str(target)),
                            ("--receipt", str(output / "score.json")),
                            ("--config-pin", digest(target)),
                            ("--input-pin", input_pin)):
            command[command.index(flag) + 1] = value
        groups[language] = {
            "label": ("Go" if language == "go"
                      else "Python short-code control"),
            "queries": count, "indices": indices, "command": command,
            "parent_config_sha256": digest(original)}
    write(args.cut.parent / "groups.json", groups)


if __name__ == "__main__":
    main()
