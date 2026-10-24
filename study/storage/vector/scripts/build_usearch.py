"""Plan or build the pinned native USearch library after slot clearance."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import urllib.request

VERSION = "2.26.5"
ARCHIVE_SHA256 = (
    "96f1ed59f32303e612b2e72c393475696743b49e0832d33e65ad97aacf61d655"
)
URL = ("https://codeload.github.com/unum-cloud/USearch/tar.gz/refs/tags/"
       "v" + VERSION)
SCRATCH = Path.home() / ".prime/research/gortex/storage-architecture/tmp"
NATIVE = SCRATCH / "native/usearch"
SOURCE = NATIVE / "source" / ("USearch-" + VERSION)
BUILD = NATIVE / "build"
ARCHIVE = NATIVE / ("usearch-v" + VERSION + "-source.tar.gz")


def digest(path: Path) -> str:
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--execute", action="store_true")
    args = parser.parse_args()
    commands = [
        ["cmake", "-S", str(SOURCE), "-B", str(BUILD),
         "-DCMAKE_BUILD_TYPE=Release", "-DUSEARCH_BUILD_LIB_C=ON",
         "-DUSEARCH_BUILD_TEST_CPP=OFF", "-DUSEARCH_BUILD_BENCH_CPP=OFF",
         "-DUSEARCH_BUILD_TEST_C=OFF", "-DUSEARCH_USE_NUMKONG=OFF",
         "-DUSEARCH_USE_OPENMP=OFF"],
        ["cmake", "--build", str(BUILD), "--target", "usearch_static_c",
         "--parallel", "1"],
    ]
    print(json.dumps({"execute": args.execute, "version": VERSION,
                      "source_url": URL, "archive_sha256": ARCHIVE_SHA256,
                      "commands": commands, "output": str(NATIVE)}, indent=2))
    if not args.execute:
        return
    if shutil.which("cmake") is None:
        raise RuntimeError("CMake must already be installed")
    NATIVE.mkdir(parents=True, exist_ok=True)
    temporary = SCRATCH / "cache/tmp"
    temporary.mkdir(parents=True, exist_ok=True)
    environment = dict(os.environ, TMPDIR=str(temporary))
    if not ARCHIVE.exists():
        pending = ARCHIVE.with_suffix(".part")
        with urllib.request.urlopen(URL, timeout=120) as response:
            with pending.open("xb") as output:
                shutil.copyfileobj(response, output)
        if digest(pending) != ARCHIVE_SHA256:
            raise RuntimeError("source archive SHA256 mismatch; kept .part")
        pending.rename(ARCHIVE)
    if digest(ARCHIVE) != ARCHIVE_SHA256:
        raise RuntimeError("source archive SHA256 mismatch")
    if SOURCE.exists() or BUILD.exists():
        raise RuntimeError("source/build exists; reconcile before reuse")
    SOURCE.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(ARCHIVE, "r:gz") as archive:
        archive.extractall(SOURCE.parent, filter="data")
    for command in commands:
        subprocess.run(command, env=environment, check=True)
    candidates = [path for path in (
        BUILD / "libusearch_static_c.a",
        BUILD / "c/libusearch_static_c.a",
    ) if path.is_file()]
    if len(candidates) != 1:
        raise RuntimeError("expected exactly one native static library")
    shutil.copyfile(candidates[0], NATIVE / "libusearch_c.a")
    shutil.copyfile(SOURCE / "c/usearch.h", NATIVE / "usearch.h")
    receipt = {"version": VERSION, "archive_sha256": ARCHIVE_SHA256,
               "commands": commands, "outputs": {
                   name: digest(NATIVE / name)
                   for name in ("libusearch_c.a", "usearch.h")}}
    (NATIVE / "build-receipt.json").write_text(
        json.dumps(receipt, indent=2) + "\n")


if __name__ == "__main__":
    main()
