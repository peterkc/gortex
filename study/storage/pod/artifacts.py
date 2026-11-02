"""Bounded plain-tar evidence without recompressing gzip members."""
import argparse
import os
import tarfile
from collections.abc import Sequence
from pathlib import Path

import hashlib
import json


GIB = 1024 ** 3


def digest(path: Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 ** 2), b""):
            value.update(block)
    return value.hexdigest()


def write_json(path: Path, value: object) -> None:
    with path.open("x") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")


MAX_PAYLOAD = 8 * GIB
MAX_FILE = 4 * GIB
MAX_MEMBERS = 1024
TAR_OVERHEAD = 16 * 1024 ** 2



def verify(manifest: Path) -> None:
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


def validate_payload(candidates: Sequence[Path]) -> None:
    sizes = [path.stat().st_size for path in candidates]
    if (len(candidates) > MAX_MEMBERS or any(size > MAX_FILE for size in sizes)
            or sum(sizes) > MAX_PAYLOAD):
        raise ValueError("payload exceeds stated 8 GiB / 1024-member bound")


def extract(bundle: Path, destination: Path) -> None:
    if destination.exists():
        raise ValueError("extraction requires a fresh destination")
    with tarfile.open(bundle, "r:") as archive:
        members, seen, total = [], set(), 0
        for member in archive:
            name = Path(member.name)
            if (not member.isfile() or name.is_absolute()
                    or ".." in name.parts or "\\" in member.name
                    or len(name.parts) < 2 or name.parts[0] != "evidence"
                    or name.as_posix() != member.name
                    or member.name in seen
                    or not 0 <= member.size <= MAX_FILE):
                raise ValueError("unsafe archive member")
            total += member.size
            if len(members) >= MAX_MEMBERS + 1 or total > (
                    MAX_PAYLOAD + TAR_OVERHEAD):
                raise ValueError("archive exceeds declared extraction bound")
            seen.add(member.name)
            members.append(member)
        destination.mkdir(mode=0o700)
        archive.extractall(destination, members=members, filter="data")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--verify", type=Path)
    parser.add_argument("--extract", type=Path)
    parser.add_argument("--destination", type=Path)
    parser.add_argument("--source", type=Path)
    parser.add_argument("--bundle", type=Path)
    parser.add_argument("--small-list", type=Path)
    parser.add_argument("--scope",
                        choices=("full-evidence", "partial-evidence"),
                        default="full-evidence")
    args = parser.parse_args()
    if args.extract:
        if (args.destination is None or args.verify or args.source
                or args.bundle or args.small_list):
            parser.error("extract requires only archive and fresh destination")
        extract(args.extract.resolve(), args.destination.resolve())
        return
    if args.destination:
        parser.error("destination requires extract")
    if args.verify:
        if args.source or args.bundle or args.small_list:
            parser.error("verify cannot also package")
        verify(args.verify.resolve())
        return
    if args.source is None or args.bundle is None:
        parser.error("source and bundle required")
    source, bundle = args.source.resolve(), args.bundle.resolve()
    if source == bundle or source in bundle.parents or bundle.exists():
        parser.error("fresh bundle must be outside the artifact directory")
    candidates = []
    small_files = []
    if args.small_list:
        small_files = json.loads(args.small_list.read_text())
        if (not isinstance(small_files, list)
                or any(not isinstance(n, str) for n in small_files)):
            parser.error("small-list must contain explicit relative paths")
        if len(set(small_files)) != len(small_files):
            parser.error("duplicate small-list path")
        for name in small_files:
            path = Path(name)
            if (path.is_absolute() or ".." in path.parts
                    or path.as_posix() != name or "\\" in name):
                parser.error("unsafe small-list path")
        candidates = [source / name for name in small_files
                      if (source / name).is_file()
                      and (source / name).stat().st_size <= 1024 ** 2]
    else:
        for directory, children, names in os.walk(source, followlinks=False):
            for name in [*children, *names]:
                if (Path(directory) / name).is_symlink():
                    parser.error("symlinked evidence")
            candidates.extend(Path(directory) / name for name in names)
    validate_payload(candidates)
    files = {}
    for path in candidates:
        if path.is_symlink() or path.resolve() != path or not path.is_file():
            parser.error("non-regular evidence")
        files[str(path.relative_to(source))] = digest(path)
    manifest = (bundle.with_suffix(".manifest.json") if args.small_list
                else source / "file-manifest.json")
    write_json(manifest, {
        "schema": "pod-file-manifest/v1", "files": files,
        "scope": "diagnostics-only" if args.small_list else args.scope,
        "omitted": (sorted(set(small_files) - set(files))
                    if args.small_list else []),
        "self_hash": "covered by external bundle SHA-256",
    })
    with tarfile.open(bundle, "x") as archive:
        for name in sorted(files):
            archive.add(source / name, arcname="evidence/" + name,
                        recursive=False)
        archive.add(manifest, arcname="evidence/file-manifest.json")
    maximum = (32 * 1024 ** 2 if args.small_list
               else MAX_PAYLOAD + TAR_OVERHEAD)
    if bundle.stat().st_size > maximum:
        parser.error("archive exceeds declared copy bound")
    bundle.with_suffix(bundle.suffix + ".sha256").write_text(
        digest(bundle) + "  " + bundle.name + "\n")


if __name__ == "__main__":
    main()
