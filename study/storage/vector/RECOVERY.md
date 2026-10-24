# Vector source recovery

The source and final report were recovered after the Mac reboot removed the old
OS-temporary study tree. No datasets, embeddings, indexes or native binaries were
recreated. The recovered code is not a fresh benchmark or build result.

## Authority and target

The lead owns this checkout and authorised only `study/storage/vector/` edits.
The recovery target was branch `study/storage-harness`, HEAD
`d4623808a6c1a2a26c1e7ddb28b0fb93a8378f16`. No commit, push, branch/ref mutation,
staging, daemon start or indexing was performed. `gofmt` was separately approved.
No `go` command, native compiler/build, data generator or provider call ran.

## Restored exactly before path adaptation

`recovered-source-hashes.json` records 29 historical SHA256 matches:

- All 13 Go files under `hybrid-followup/cmd` and `hybrid-followup/internal`.
- The five Python files: `run.py`, `prepare_dataset.py`, `embed_voyage.py`,
  `export_vectors.py`, `test_followup.py`.
- `hybrid-followup/go.mod`, `hybrid-followup/pyproject.toml`,
  and `hybrid-followup/METRICS.md`.
- All six Go files in `adapters/sqlite-usearch/`, including its tests, View default
  and optional historical Load build tag. Its original `go.mod` also matched
  before changing the relative harness path.
- The final `MAC-ACCURACY-REPORT.md` text. This report is still byte-identical.

Full string literals, logged edits and surviving in-memory source snapshots were
used. Historical gofmt steps were replayed with the approved parser/formatter.
No third-party source was copied from a module cache or vendored.

## Reconstructed or intentionally adapted

These files must not be described as unchanged historical source:

1. **Persistent paths.** `run.py`, `prepare_dataset.py`, `embed_voyage.py`,
   `cmd/ann/main.go` and `cmd/timing/main.go` now default all study outputs to
   `$HOME/.prime/research/gortex/storage-architecture/tmp/`. `run.py` sends Go
   caches and temporary files there. The tiny timing fixture uses an explicit
   persistent temporary directory instead of the OS default. Details are in
   `path-adaptations.json`. The source fingerprint therefore changes.
2. **Relative harness links.** Both adapter modules now resolve `gxstudy` through
   `../../../harness`, matching this checkout layout. Only the link changed in
   the USearch module; no build or module resolution checked it.
3. **Flat adapter.** Backend, driver and tests were replayed from the original
   SQLite base plus logged vector-sidecar, checksum and missing-parent edits.
   Its tests match the historical native adapter's shared test hash. A final
   historical flat-backend hash was not available for an exact comparison.
4. **Flat wrapper relocation.** Its own formerly shared `vecindex` wrapper is now
   `adapters/sqlite-vecflat/internal/vecindex/index.go`. The import changed to
   `sqlite-vecflat/internal/vecindex`; its old local `vectorbench` module edge
   was removed. The public coder/hnsw pin remains the historical
   `v0.6.2-0.20260622133054-36cab6028fed`. This wrapper is study code, not vendored
   coder/hnsw source. The original Go checksum file was recovered with the
   accepted pin reconciliation; it can contain now-unused checksums.
5. **Build scripts.** `scripts/build_usearch.py` and `scripts/build_cli.py` are
   reconstructed command planners, not byte-exact historical scripts. The old
   commands lived in the worker transcript. The native plan pins USearch2.26.5,
   the original source archive URL and SHA256, Release, NumKong/OpenMP off,
   static C target and one compiler process. The Go plan uses the pinned module
   files, one compile process and persistent scratch caches. Neither was run.
6. **Documentation.** This file and the recovery README are new. The historical
   Mac report is not edited to hide the data loss or its pre-reboot claims.

## Gaps and unavailable evidence

- `hybrid-followup/go.sum` was not recovered: the original 400-byte file had
  SHA256 `e65280f3e88d8c04dc538fd6d52b22248dd67a33cecde83c26a1f36b8849b638`.
  The exact USearch module checksum lines were not present in the transcript.
- `adapters/sqlite-usearch/go.sum` was not recovered: its original 4,382-byte
  SHA256 was `3f6feae5e0ced956e1aa70dafaf9eceaf615e116b9cb6302a8d40193f069d71e`.
  Do not treat a future download as historical integrity verification without
  checking these original fingerprints or authoritative module records.
- Python `uv.lock` and the Mac `.venv` were not recovered. The last recorded
  environment used CPython3.14.7, numpy2.5.4 and requests2.34.2. The recovered
  pyproject itself has the original Python3.12 floor and unbounded direct names;
  it is not a replacement for the missing lock.
- Embed-quality's external text-builder/helper code is outside this worker's
  restore. Preparation still needs that separately recovered implementation
  and its matching provenance/cache files before any reproducibility claim.
- Source/cache provenance JSON, old API spend ledger/batch receipts, raw NPY/F32
  matrices, exact-reference caches, native graphs, build caches and binaries
  were not restored. The old API approval is not permission to re-embed.
- Raw result files referenced by the historical Mac report are not on disk.
  The report text and lead's notes preserve numbers, not the old executable
  environment or the full verification chain. No current report claims that
  those lost files were revalidated.
- Linux native linking/runtime, saved-index cross-open and clean latency were
  never established by the Mac run and remain unverified. No local race test ran.

## Recovery checks

`recovery-checks.json` records current checks. They cover Python AST syntax,
Go parsing/formatting through `gofmt`, expected source files, historical digest
comparisons, persistent-path inspection and existing relative harness links.
They do not import/run recovered modules, fetch dependencies, compile/link, run
unit tests or execute any matrix. The lead must grant the Go slot before such
checks, and separately approve data generation or embedding.

Gortex read-only localisation failed because its daemon was unavailable after
reboot. No daemon, tracking or index operation was attempted. Workspace memory
could not be surfaced; direct reads of the current harness contract and this
worker's transcript supplied recovery evidence instead.

Stop here: recovery is handed back to the lead. No build or data work is queued.
