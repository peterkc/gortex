"""Hash all payload files and package them outside the source directory."""
import argparse
import os
import tarfile
from pathlib import Path

from run_limited import digest, write_json


def verify(manifest: Path) -> None:
    import json
    entries = json.loads(manifest.read_text())["files"]
    source = manifest.parent.resolve()
    actual = set()
    for directory, children, names in os.walk(source, followlinks=False):
        for name in [*children, *names]:
            path = Path(directory) / name
            if path.is_symlink():
                raise ValueError("symlinked evidence")
        for name in names:
            path = Path(directory) / name
            if path != manifest:
                actual.add(str(path.relative_to(source)))
    if actual != set(entries):
        raise ValueError("manifest does not cover the exact payload set")
    for name, expected in entries.items():
        path = source / name
        if (path.resolve() != path or source not in path.parents
                or digest(path) != expected):
            raise ValueError("payload path/hash mismatch: " + name)
    print("Manifest verified; all payload files match")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--verify", type=Path)
    parser.add_argument("--source", type=Path)
    parser.add_argument("--bundle", type=Path)
    args = parser.parse_args()
    if args.verify:
        if args.source or args.bundle:
            parser.error("verify cannot also package")
        verify(args.verify.resolve())
        return
    if args.source is None or args.bundle is None:
        parser.error("source and bundle required for packaging")
    source, bundle = args.source.resolve(), args.bundle.resolve()
    if bundle == source or source in bundle.parents or bundle.exists():
        parser.error("fresh bundle must be outside the artifact directory")
    files = {}
    for directory, children, names in os.walk(source, followlinks=False):
        for name in [*children, *names]:
            path = Path(directory) / name
            if path.is_symlink():
                parser.error("symlinked evidence is refused")
        for name in names:
            path = Path(directory) / name
            if not path.is_file():
                parser.error("non-regular evidence file")
            files[str(path.relative_to(source))] = digest(path)
    manifest = source / "file-manifest.json"
    write_json(manifest, {"schema": "seam-file-manifest/v1", "files": files,
                         "self_hash": "covered by external bundle SHA-256"})
    with tarfile.open(bundle, "x:gz") as archive:
        for name in sorted([*files, manifest.name]):
            archive.add(source / name, arcname="evidence/" + name,
                        recursive=False)
    bundle.with_suffix(bundle.suffix + ".sha256").write_text(
        digest(bundle) + "  " + bundle.name + "\n")


if __name__ == "__main__":
    main()
