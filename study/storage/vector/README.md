# Recovered vector study source

This source was recovered after the Mac reboot removed the original scratch tree.
Read [RECOVERY.md](RECOVERY.md) before using it. The restored historical
[Mac report](MAC-ACCURACY-REPORT.md) is not a new measurement or proof that its
old datasets/results still exist.

## Scope and layout

- `hybrid-followup/`: self-contained Go exact/int8/USearch tools, parallel scan,
  metric helpers, Python preparation/export/API source and matrix controller.
- `adapters/sqlite-vecflat/`: SQLite plus a persisted exact-F32 vector file.
  Its own `internal/vecindex` wrapper was previously shared by the study.
- `adapters/sqlite-usearch/`: SQLite IDs plus native USearch2.26.5 F32/cosine,
  M16/Add128/Search128. `View` is the default; the historical optional Load
  build-tag source is also retained.
- `scripts/build_usearch.py`: reconstructed pinned CPU-only CMake setup.
- `scripts/build_cli.py`: reconstructed Go build command planner.

No third-party source, model weights, vector files, corpora or native binaries
are vendored here. Module versions remain pinned. The two USearch `go.sum`
files and Python `uv.lock` were not recovered; see the documented limits.

## Persistent paths

All data, receipts, caches, generated fixtures, source downloads and build output
belong under this local-only scratch root, not in the checkout or OS temporary tree:

```text
$HOME/.prime/research/gortex/storage-architecture/tmp/
  vector/hybrid-followup/  exports, indexes, results, matrix receipts
  native/usearch/          verified download, native source/build/library/header
  cache/go-build/          Go build cache
  cache/go-mod/            Go module cache
  cache/tmp/               temporary files
  bin/                    optional individually built tools/adapters
```

The hybrid Go/Python defaults were adapted to this root. Explicit export inputs
and destinations must also use it. Unit-test temporary files follow `TMPDIR`;
set it to the shown persistent scratch directory before future tests.

## Commands are plans, not clearance

**Ask the lead for the Go slot before any Go build, test, vet, module resolution
or native build.** Data generation and hosted embedding require separate approval.
A script's `--execute` flag does not grant that approval. No build or regeneration
was performed during recovery.

These standard-library scripts only print commands unless `--execute` is supplied:

```text
python3 study/storage/vector/scripts/build_usearch.py
python3 study/storage/vector/scripts/build_cli.py ann
python3 study/storage/vector/scripts/build_cli.py sqlite-usearch
python3 study/storage/vector/scripts/build_cli.py sqlite-vecflat
```

The CMake plan pins USearch2.26.5, source SHA256 and one compiler process.
NumKong/OpenMP are off. It requires existing tools and does not install them.
Future Go module resolution must be approved and must regenerate/verify the
missing USearch checksum files before reproducibility is claimed.

The matrix controller requires Python3.12+. It uses the standard library. Its
plan modes create only job metadata under persistent scratch, not an index:

```text
python3 study/storage/vector/hybrid-followup/run.py accuracy
python3 study/storage/vector/hybrid-followup/run.py timing
python3 study/storage/vector/hybrid-followup/run.py latency
```

Do not execute these matrices without new clearance. No datasets were restored.
Timing execution remains restricted to a cleared Linuxarm64 VM. Mac accuracy
uses the historical memorystatus/Swapouts rules and logs failed windows.

The retained `embed_voyage.py` source is **not approved to run**. Its old paid
ledger and vectors are absent. An absent ledger must not be treated as a fresh
approved token budget. The old6M approval was for the completed historical task.

## Verification status

Recovery checks parse Python source, run the explicitly approved `gofmt` parser,
check relative module links and compare surviving historical source hashes.
They do not compile/link, fetch dependencies, run tests, rebuild data or call
providers. Prior passing checks in the historical report belong to the lost run.
