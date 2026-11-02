# Design C graph fixtures

Reuse the unchanged public tier1/tier10 pair. Do not regenerate a passing input.
The release and all runs still need the user's approval. This directory contains
code and pins only. It publishes no data and changes no product behavior.

```text
User approves public assets and exact CI run
  -> lead publishes the reviewed release
  -> fetch.sh runs fetch.py under limited-command
  -> archive SHA-256 -> safe extraction -> all manifest/file SHA-256 values
  -> result.json pass -> consumers may use the pair
Mismatch, interruption or missing receipt -> keep partial evidence and STOP
```

## Pinned pair

- **tier1:** K-client-go. 81,179 nodes, 448,952 edges, 694 queries.
  10 files, 516,258,893 bytes. Manifest SHA-256:
  `9c1a7fe9ebbee64b94f4c904795ada4942622f0efcd4e7657b6318922cdef736`.
- **tier10:** six Kubernetes and four Pallets repositories. 82,088 nodes,
  414,089 edges, 752 queries. 73 files, 436,311,531 bytes. Manifest SHA-256:
  `8cde7d90cfd0af9125e8d7152675d41c7c075f99e8fcc6805e6141854a1873bb`.

Their near-equal node counts are intentional. The older 423,567-node tier10 is
not this input. `fixture-pins.json` records every repository manifest hash,
full public source revision, source-store hash, count and archive identity.
Both use `gxstudy-v2`, seed `20261022`, and no synthetic cross-repository links.
The original source indexer was `73933568f89c51898f54380160200b46d0a068f8`.
The README's Gortex-source revision `76c080e5` describes an older derived recipe.
It is not the provenance of this pair.

## Prepared release assets, not yet published

Repository: `peterkc/gortex`. Intended tag: `storage-design-c-fixtures-v1`.
The tag remains an explicit `fetch.py` argument. The pin file has no release-tag
field, and no unchecked field was added. Publication still needs the user's
approval and the lead's separate publication action.

The lead prepared tier1 on the Mac because the verified source files exist only
locally; a Linux CI host cannot read them. Packaging used Python's standard-library
`tarfile`, not BSD tar, so it did not copy xattrs or create AppleDouble entries.
The tar uses USTAR with exactly the ten verified files in byte-sorted order.
It contains no directory entries. Every entry has mode `0644`, uid/gid `0`, empty
uname/gname and mtime `0`. The lead compressed it with zstd **1.5.7**, `-19 -T1`.
No dataset or manifest bytes changed.

The lead checked all ten file hashes before and after packaging, the unchanged
516,258,893-byte file total, absence of `._` members, and equality of the zstd
round trip with the tar. The prepared file sizes and SHA-256 values were also
checked read-only during release preparation. No fetch or extraction ran here.

Current pins in `fixture-pins.json`:

- **tier1.tar.zst:** 36,669,062 bytes, SHA-256
  `c802fd028b25d83caeca5f62835dd2279dc08e20ef80e6fb7138b010e9ec24f5`.
  Its tar is 516,270,080 bytes, SHA-256
  `6e4ca8bd35518fe2175a13ff74b82244eb1773af6f7fa4ad6eef36616da7dbbd`.
- **tier10.tar.zst:** unchanged at 42,551,184 bytes, SHA-256
  `5064655da1ac9d24db23d4474110275b532a0f20733cc2faf4cfd751fb05da6c`.
  Its tar is 436,469,760 bytes. It contains 73 files and 11 directory entries.

Total compressed download: 79,220,246 bytes, about 75.6 MiB. The tier1 root
manifest remains
`9c1a7fe9ebbee64b94f4c904795ada4942622f0efcd4e7657b6318922cdef736`.
`extract_tier` accepts a files-only archive: it requires every allowed file and
the exact byte total, but does not require directory entries. Its final
`tarfile.extractall(..., filter="data")` creates the parent directories. The
strict refusal of links, extra files and AppleDouble members is unchanged.

**Historical tier1 packaging, not for upload:** the former archive was
36,857,489 bytes with SHA-256
`5ac6b1e83fc04b6e021c7a78ccdf52d002e06555f657ba1e9da02dd7b616b3fb`;
its tar was 516,300,800 bytes. That tar contained twelve 163-byte AppleDouble
`._` files (1,956 extra bytes) carrying `com.apple.provenance`. The strict
extractor refuses that packaging. Those identities remain historical evidence,
not accepted release pins. The previous proposed GNU-tar-on-Linux procedure
was not used because CI could not see the local verified data.

Versioned release assets avoid re-indexing and whole-corpus generator memory.
An Actions cache can accelerate later reads, but it is not the source of truth:
entries can be
evicted. A CI artifact needs a pinned run/artifact identity and has retention
limits. Neither avoids the same hash and publication checks.

Before publication, the lead must approve the complete public-content and
license inventory. The data includes upstream test keys and public email
addresses. Do not strip them silently; that would change the input hashes.
Include `NOTICE.txt`, the source inventory and the exact source license texts
with the release. Kubernetes uses Apache-2.0. The four Pallets projects use
BSD-3-Clause. Werkzeug also includes Silk icon attribution under CC-BY-2.5 or
CC-BY-3.0. Preserve it. The local worker report contains the proposed NOTICE and
all detected source locations. This paragraph is not permission to upload.

## CI fetch entrypoint

Prerequisites: Linux, Python 3.12+, `zstd`, and the accepted harness
`cmd/limited-command` binary. Build that binary outside all timed work with the
harness's Go 1.27.1 toolchain. Run from a leaf inside a job-owned delegated
cgroup-v2 root. Its cleanup owner must remain outside the worker tree and apply
the accepted ownership/inode and `cgroup.kill` cleanup procedure on every exit.
The script neither creates the parent cgroup nor administers Gortex.

The lead supplies every argument. `OUT` must not exist. `DATA` must be
`OUT/data`; it is explicit so consumers cannot select an incidental data root.
The approval file is a hash-bound record, not automatic proof of approval.
The lead checks its meaning before starting.

```sh
bash study/storage/checks/fixtures/fetch.sh "$ABSOLUTE_PYTHON" \
  --release-tag storage-design-c-fixtures-v1 \
  --source-revision "$REVIEWED_FULL_HEAD" \
  --launcher "$BIN/limited-command" \
  --approval "$APPROVAL_RECEIPT" \
  --cgroup-root "$DELEGATED_CGROUP" \
  --out "$OUT" --data-dir "$OUT/data" \
  --runner-memory-bytes "$APPROVED_RUNNER_BYTES" \
  --checker-memory-bytes "$APPROVED_CHECKER_BYTES" \
  --engine-memory-bytes "$APPROVED_FETCH_BYTES" \
  --wall-seconds "$APPROVED_WALL_SECONDS"
```

No path, numeric limit, deadline or release tag defaults. No automatic retry,
resumption, cap increase or regeneration. The worker downloads only the two
named assets, checks lengths and SHA-256 values, then extracts regular files
under the expected tier paths. It refuses links, duplicate members, traversal,
unexpected files, existing outputs and expansion beyond pinned bytes.

Before use, it checks both root manifest pins, every file hash in both root and
repository manifests, source identities, count sums and byte totals. It records
all file hashes in `verified.json`. The accepted launcher records PID, process
RSS, cgroup peak/OOM/limit/deadline state and cleanup in `limited/receipt.json`.
`result.json` binds those output hashes and labels the branch
`reuse-unchanged-assets` or `stop-no-retry`. Pass means fixture identity only,
not a storage target or daemon-parity pass.

The external launcher enforces only the engine process-tree limit. Its binding
schema still requires runner/checker byte values; those components do not run
here and have no measured memory. The small Python controller remains outside
that tree. HOME, XDG and TMPDIR are isolated, and no inherited Prime/RLM/Gortex
selectors reach the worker.

Planning estimate on a 4-vCPU/16-GB Ubuntu runner: 2–5 minutes for download,
decompression and hashing, with a proposed 600-second hard deadline. Process RSS
should be below 100 MiB; charged tar/data file cache can be much larger. Propose
2,147,483,648 bytes for the fetch tree, subject to user approval. This is not a
measured peak or a promise of completion. Retaining both compressed archives,
tars and data needs about 2.0 GB before logs and runtime tools.

## Derived 100-repository point: single-dispatch measure job

No existing tier100 was found in the known study data roots. After the lead
accepts the verified pair and the user approves the exact run, one dispatch runs
generation and timing. The measure job generates tier100 first from the ten
verified tier10 parts, with seed `20261022` and synthetic links off. It writes
and uploads the generated manifest before any timing starts, then runs timing.
No intervening lead review or second dispatch is required.

Use the accepted, pinned harness `cmd/gen`, built before timing with Go 1.27.1.
Do not change the generator or add another data format.

Keep this input order, matching tier10's manifest:

```sh
sources="$DATA/tier10/K-apimachinery,$DATA/tier10/K-component-base"
sources="$sources,$DATA/tier10/K-kube-aggregator,$DATA/tier10/K-metrics"
sources="$sources,$DATA/tier10/K-cli-runtime,$DATA/tier10/K-controller-manager"
sources="$sources,$DATA/tier10/P-flask,$DATA/tier10/P-click"
sources="$sources,$DATA/tier10/P-werkzeug,$DATA/tier10/P-jinja"
"$BIN/gen" -sources "$sources" -repos 100 -seed 20261022 \
  -synthetic-cross-repo=false -out "$FRESH_TIER100"
```

Run that exact argument array through `cmd/limited-command`, not a bare shell
benchmark. Bind its executable/source, the two parent manifest hashes, ordered
source list, seed and flags, hostname, approval and fresh output to the accepted
RunBinding/data-receipt/deadline-policy protocol. Use serial runs. Suggested
starting proposal: engine cap 10,737,418,240 bytes and deadline 1,200 seconds.
Runner/checker are not launched by this external operation. Their required
binding fields and the outer cleanup owner's budget remain explicit.

Estimate: 2–10 minutes and 3–8 GiB process RSS, with substantial uncertainty.
The old single 81,179-node generation took 13.3 seconds and 3.15 GiB on the Mac;
the older 423,567-node generation took 45.4 seconds and 12.38 GiB. Those are
capacity clues, not Linux timing comparisons. The current generator retains
whole input/derived datasets, oracle segments and answers. File-less nodes repeat,
and charged file cache also counts. A cap or deadline miss stops the attempt;
no larger cap or substitute fixture follows automatically.

Label every output **derived-100-repository fixture: 10 real source parts plus
90 correlated derived top-ups**, not 100 real repositories. The first ten slots
retain complete inputs. The remaining slots cycle through them with the existing
rank-based file fractions. No synthetic links are injected, so no invented
Go/Pallets cross-group policy appears. This is a storage fan-out/scaling fixture,
not evidence of realistic 100-repository dependency topology or daemon parity.

Before any timing, the measure job saves the generated root manifest SHA-256,
all repository manifest and record hashes, total/per-repository counts, query
count, generator source/binary hashes, command, limits and terminal launcher
receipt. It checks count sums and all manifest files, records the immutable
manifest pin in the timing run bindings, and uploads the manifest. The accepted
disk-backed checker also validates the dataset when each check consumes it.

The lead reviews the uploaded manifest after the run. If the lead rejects it,
only tier100 results are invalidated; tier1/tier10 results remain subject to
their own checks. Do not predeclare a guessed tier100 hash or counts, and do not
change the unchanged-pair pin file to accept a new result silently. A later
fetch path needs a reviewed new pin and archive identity.

## Missing/corrupt input fallback

A fetch failure stops. The lead decides whether a new input is permitted. Never
replace this pair automatically with the harness README's old recipe.

For an approved fresh re-index, retain the public repository revisions in
`fixture-pins.json`, but build the Gortex indexer from upstream
`7096db70df5ab7d83e20a28d9ccfd0994812acf9`, with Go **1.27.3** as required by
that root module. Use isolated CI-only state and a separately bounded indexing
owner. The earlier `73933568` recipe used shared K/P workspaces, one restart and
one on-demand analysis pass per workspace. Reproducing those completion
conditions needs its own explicit CI indexing procedure; this fetch script does
not create a daemon or claim to reproduce it.

After the indexing owner supplies stopped, checkpointed database copies and
source-matched public clone roots, use accepted `cmd/export` with explicit roots
mapping `{prefix: {id, tree, revision}}`. Export all K or P group parts, then:

```sh
"$BIN/export" -db "$K_COPY" -roots "$K_ROOTS" -repo K -out "$K_EXPORT"
"$BIN/export" -db "$P_COPY" -roots "$P_ROOTS" -repo P -out "$P_EXPORT"
"$BIN/gen" -sources "$K_EXPORT/K-client-go" -repos 1 -seed 20261022 \
  -synthetic-cross-repo=false -out "$FRESH_TIER1"
# Use the same ten labels/order above, now under K_EXPORT and P_EXPORT.
"$BIN/gen" -sources "$NEW_TEN_SOURCE_PATHS" -repos 10 -seed 20261022 \
  -synthetic-cross-repo=false -out "$FRESH_TIER10"
```

Every export/generation also uses the accepted limited-command protocol with
separate explicit budgets. Label these as **changed indexer inputs**, not recovered
unchanged data. Hash and freeze them before timing. Changed counts may lose the
near-equal-node purpose; the lead must resolve that before comparative claims.
No re-index is needed for the verified pair, so no additional indexer wrapper or
daemon lifecycle was added here.

## Focused CI checks

From this directory, run `python3.12 -B -m unittest -v test_fetch`.
There are no new Go dependencies or Go module in this fixture-fetch slice.
No formatter, type checker or Python linter is declared for it.

Given/When/Then cases in `test_fetch.py` cover:

- Valid tiny gxstudy-v2 files: verification/extraction preserve bytes and counts.
- Repeated verification: no input changes. Repeated extraction: visible refusal.
- Dropped edge or wrong root manifest pin: refusal before accepted use.
- Different source revision or inconsistent counts, even after resealing: refusal.
- Extra file, symlink, duplicate archive member or traversal: refusal before use.
- Missing memory/deadline/path arguments: no defaults and visible refusal.

This slice does not alter shared graph code or adapters. Product edits,
remove/re-add, restart, CAS and pin/retire behavior remain the check workers'
conformance scope, not a fetch-test claim. Archive/data integrity is covered here;
actual Linux download, launcher containment and fresh CI results remain required.
