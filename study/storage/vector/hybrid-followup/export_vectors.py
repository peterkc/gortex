"""Export stored vectors without scoring or ANN builds."""

import argparse
import hashlib
import json
from pathlib import Path

import numpy as np


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--destination", type=Path, required=True)
    args = parser.parse_args()
    args.destination.mkdir(exist_ok=True)
    records_by_kind = {}
    for kind, name in (("document", "corpus.jsonl"),
                       ("query", "queries.jsonl")):
        with (args.source / name).open() as stream:
            records_by_kind[kind] = [json.loads(line) for line in stream]
    repos = sorted({row["repo"] for row in records_by_kind["document"]})
    results = {}
    for repo in repos:
        destination = args.destination / repo
        destination.mkdir(exist_ok=True)
        files = {}
        for kind, output_name in (("document", "records.json"),
                                  ("query", "query-records.json")):
            records = records_by_kind[kind]
            matrix = np.load(args.source / "voyage-code-4" / f"{kind}.npy",
                             mmap_mode="r")
            if matrix.shape != (len(records), 1024):
                raise RuntimeError("vector/metadata shape mismatch")
            positions = [i for i, row in enumerate(records)
                         if row["repo"] == repo]
            selected = [{**{key: records[i][key]
                            for key in ("id", "repo", "parent_id")
                            if key in records[i]}, "key": i}
                        for i in positions]
            metadata_path = destination / output_name
            metadata_path.write_text(json.dumps(selected) + "\n")
            files[output_name] = {
                "bytes": metadata_path.stat().st_size,
                "sha256": hashlib.sha256(metadata_path.read_bytes())
                .hexdigest(),
            }
            path = destination / f"{kind}.f32"
            with path.open("wb") as stream:
                for start in range(0, len(positions), 256):
                    matrix[positions[start:start + 256]].tofile(stream)
            files[path.name] = {"rows": len(positions), "dimensions": 1024,
                                "bytes": path.stat().st_size,
                                "sha256": hashlib.sha256(path.read_bytes())
                                .hexdigest()}
        (destination / "input-receipt.json").write_text(
            json.dumps(files, indent=2) + "\n"
        )
        results[repo] = files
    print(json.dumps(results))


if __name__ == "__main__":
    main()
