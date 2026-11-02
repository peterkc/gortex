# Design C base checks (throwaway)

This module implements the Check 1/2/5 prototype. It is not a product backend.
No measurement, test, build or module download runs on the Mac.

## Storage and exposed API

`study/checks/base` is importable as package `base`. `Backend{SideIndex: false}`
implements the existing `gxstudy/adapter.Backend`. `SideIndex: true` selects the
bounded fallback. `Build` creates a private sealed directory. `Open` returns an
`adapter.Segment`; `OpenVersion(ctx, ref)` returns `*base.Segment`.

The reader implements all `adapter.Segment` methods. `NodeRows`, `EdgeRows`,
`FileRows`, `SymbolRows`, `ContentRows` and `VectorRows` take a context and return
`iter.Seq2[dataset.<record>, error]`. Keep the segment open while consuming them.
`TextIndex(name string) (bleve.Index, error)` accepts `symbols` or `content`.
Its borrowed wrapper refuses writes and `Advanced`; its `Close` does not close
the segment-owned index. Borrowed indexes expire at `Segment.Close`.
`BuildText(ctx, path, rows)` and `SearchText(ctx, index, text, k)` expose the
same Bleve mapping and query policy to the delta prototype. A row producer must
call its supplied `add(id, text)` callback synchronously and return its errors.

Nodes have three sorted lookup copies. Edges have outgoing/incoming sorted
copies and sparse CSR/CSC-style endpoint range directories. Zero-degree and
external endpoints do not create fake nodes. The prototype is GraphAr-style,
not GraphAr SDK/wire compatibility or the design's final dense N+1 layout.
Every table retains the complete source record as opaque JSON payload inside
Parquet. After writing, construction reads every table and compares a streaming
full-row digest with the input. Duplicate edges survive; duplicate node IDs
refuse sealing. All six harness record families persist.

Each neutral record family is decoded once. That pass fans out lookup and
adjacency copies, feeds Bleve and the optional side store, validates vectors,
and counts records. External `sort` uses a 4 MiB buffer. Parquet row groups contain at most 1,024 rows.
Serving reads use sparse range lookup and bounded block caching, not a resident
graph. The shared serving cache payload budget is 256 MiB across all repositories:
256 MiB Parquet, or 128 MiB Parquet plus 128 MiB Pebble in the fallback. Bleve,
metadata and OS cache remain outside that budget and inside process/cgroup memory.
Each open segment retains parsed metadata and file handles for its nine tables
and nine range directories. Queries create private readers, not new file/footer
parses. This is 18 retained Parquet handles per repository before Bleve/Pebble
handles. The tier100 memory and file-descriptor cost remains unmeasured. Cache
charges use allocated block capacity, including short final blocks.
The Bleve mapping and query functions remain identical to reference B.

Every graph lookup copy, adjacency copy, range directory, seal and Pebble file
counts in graph serving bytes. Text, vectors and total bytes are separate.
B now routes existing key prefixes into physically separate `graph`,
`text-records` and `vectors` Pebble stores. All stores share one 256 MiB cache.
Graph files, raw text plus Bleve files, and vector files have independent byte
inventories. Shared seals count as graph metadata. The three categories sum to
all artifact bytes. No compressed SST prefix share is estimated or subtracted.
The C fallback counts both its Parquet lookup copies and its Pebble side store.
Scratch peak and real product old/new-version coexistence remain unmeasured.

## CI fixture commands

Resolve the pinned module graph in CI setup before readonly checks. These commands
are not permission to run them locally or start a measurement:

```text
cd study/storage/checks/base
go test -p=1 ./...
go vet -p=1 ./...
python3 -B -m unittest -v test_measure

go build -p=1 -o <bin>/parquet-base ./cmd/parquet
go build -p=1 -o <bin>/parquet-side ./cmd/side
go build -p=1 -o <bin>/base-fixture ./cmd/fixture
```

Use Go 1.27.1, `GOTOOLCHAIN=local`, `GOWORK=off`, `GOFLAGS=-mod=readonly` and the
CI job's isolated build/module caches. The leaf module pins Parquet v0.30.1,
Bleve v2.6.1 and Pebble v2.1.7. Pure Go is used; no native Parquet library is needed.
Rebuild `sqlite-segment` (A) and the repaired `pebble-bleve` (B) against the same
harness revision. A's implementation is unchanged. The harness records engine
self+joined-children build CPU and hash-bound query timing logs. C records complete
per-table read-back digests, row counts and CPU separately. The comparison removes
only those validated verification intervals from C's raw build CPU. Source
decoding, fanout, sorting, indexing, compression, hashing and sync/seal remain
construction. Raw and adjusted CPU are both retained. Run its existing test/vet suites too.

## One measurement entrypoint

The lead calls `./run.sh --config /absolute/run-config.json` on approved Linux CI.
No paths, limits or output directory have defaults. Existing output is refused.
Put the run JSON and approval receipt in a dedicated input-only directory. The
output must be outside source, fixture and configuration directories. Aliased
parents, symlinks and `..` output paths are refused. File hashes and directory
entries protect inputs, including added or removed files.

The configuration must have exactly these fields. The proposed phase limits are
9,000 seconds main and 2,700 seconds conditional side. These and the example memory
limits need the run owner's approval before execution:

```json
{
  "schema": "design-c-base-run/v1",
  "source_root": "/absolute/checkout",
  "source_revision": "<reviewed full source OID>",
  "recipe_sha256": "<approved generator/recipe SHA-256>",
  "approval_path": "/absolute/base-policy/user-operator-approval.json",
  "approval_sha256": "<approval receipt SHA-256>",
  "base": {"main_wall_seconds": 9000, "side_wall_seconds": 2700},
  "runs": 3,
  "safety_timeout_seconds": 900,
  "memory_limits_bytes": {
    "runner": 536870912,
    "checker": 1073741824,
    "engine": 4294967296
  },
  "output_dir": "/absolute/fresh-base-results",
  "cgroup_root": "/sys/fs/cgroup/<job-owned-delegated-root>",
  "construction_selection_approved": true,
  "binaries": {
    "a": {"path": "/absolute/bin/sqlite-segment", "sha256": "<SHA-256>"},
    "b": {"path": "/absolute/bin/pebble-bleve", "sha256": "<SHA-256>"},
    "c": {"path": "/absolute/bin/parquet-base", "sha256": "<SHA-256>"},
    "c_side": {"path": "/absolute/bin/parquet-side", "sha256": "<SHA-256>"}
  },
  "data": {
    "tiny": {"path": "/absolute/tiny", "sha256": "<manifest SHA-256>"},
    "tier1": {"path": "/absolute/tier1", "sha256": "<manifest SHA-256>"},
    "tier10": {"path": "/absolute/tier10", "sha256": "<manifest SHA-256>"},
    "tier100": {"path": "/absolute/tier100", "sha256": "<manifest SHA-256>"}
  }
}
```

Prepare the tiny tier from the existing pinned harness
`conformance.Fixture` source; no exported corpus or Gortex installation is needed.
From this module, in CI setup before timing:

```sh
go build -p=1 -o "$RUNNER_TEMP/base-fixture" ./cmd/fixture
"$RUNNER_TEMP/base-fixture" -out "$RUNNER_TEMP/base-tiny" \
  -source-revision "$GITHUB_SHA"
sha256sum "$RUNNER_TEMP/base-tiny/manifest.json"
```

Use `base-tiny` and its printed manifest digest for `data.tiny` in the run config.
`GITHUB_SHA` must be the reviewed checkout commit. The producer records that full
revision, the canonical fixture-content SHA-256 and seed 20261022. It labels the
source synthetic, retains the harness gxstudy-v2 format and writes complete
queries/expected answers and all file checksums. Missing/invalid revision or an
existing output refuses before overwriting anything. The fixture has three Go
functions, including two in one file for the full update/publication protocol.
Its tests check disk-backed expected answers, deterministic manifests, delta
readiness and invalid-input/overwrite rejection. All execution remains CI-only.

The driver runs the full tiny correctness protocol once per candidate. It then
uses three fresh timing-only processes per candidate per scaling point. Timing-only
success is not full lifecycle correctness. Complete tier1 C verification witnesses
establish serialization; the disk-backed checker validates every measured answer.
Data retains gxstudy-v2. Builds and downloads finish before timing. Processes run
serially, with three rotating A-B-C, B-C-A and C-A-B rounds per tier.

Each existing ID/name/file miss receives exactly 20 measured calls per timing
process. Warmup calls do not count. A lookup hit/miss stratum needs at least 20
measured samples across the three processes. Memoized semantic checks use the full
source/query/policy/answer envelope, omitting only elapsed time and warmup state.
They do not cache serving answers or remove measurements.

The accepted runner enforces independent runner/checker/engine cgroup hard limits.
The driver bounds main and conditional side separately with the required `base`
limits. There is no separately adjustable total; the maximum is their sum. On
expiry or interruption,
it sends SIGTERM to the runner and allows the existing 30-second cleanup join.
If that join fails, it kills only its runner and refuses completion. The CI job
owner must stay outside the worker tree, retain the job cgroup identity and apply
the accepted job-level cleanup procedure on every exit. Never retry, reuse the
output directory or raise a failed cap automatically.

`result.json` contains source/config/script/executable/input/output hashes,
approval binding, host, numeric limits, complete fixture manifests/counts,
component receipts, inventories, all lookup hit/miss ratios, base read timings,
pass/miss/partial/blocked outcomes and the selected miss/table branch. Component
receipts remain authoritative. Failed attempts retain partial evidence.
After each successful timing process, the driver inventories and hashes every
serving file, saves construction/verification CPU, byte categories and directory
identity, then removes only its expected identity-matched segment directories.
The inventory and removal-intent/completion receipts, JSON, query samples and logs
remain. Successful tiny correctness artifacts remain. Failed serving artifacts
remain, and failed inventory/cleanup stops further work. Aggregation uses saved
inventories rather than reading deleted indexes.

Check 1 requires each hit/miss p95 at tier1/tier10 to be at most 1.5x A. Compare
same-round process p95s: all three ratios at or below a ceiling pass, all three
above miss, and crossing ratios are partial. There is no epsilon. A conclusive
Check 1 miss alone starts the bounded Pebble side branch. Its three processes per
tier reuse same-index A/B controls from this unchanged job. This is control reuse,
not contemporaneous controls or a statistical confidence claim. Missing or
invalid controls stop the comparison, not start more runs.

Check 5 retains the 300 graph B/edge ceiling and the applicable B-relative read
ceiling of 1.0. Every multi/traversal operation uses the same paired rule, including
identical-code searches. Opposite read directions require consistency across all
three pairs before they become a read trade-off. Construction ties use the sum of
per-tier median adjusted CPU, then total bytes; unresolved ties return to the user.

Only complete, hash-valid terminal evidence with typed `semantic` check failures
can reject C for a wrong answer and fall back to independently passing B. An
`operation_error`, OOM, deadline, invalid hash, missing receipt or unclassified
failure blocks the comparison. A failed A/B correctness control invalidates it.
All selection-table rows, numeric boundaries and these rejection paths have
regression test source. The driver never treats unknown C evidence as a B pass.

A is the plan's read-only SQLite segment reference. The existing adapter is
`study/storage/adapters/sqlite-segment` (modernc SQLite v1.60.1), serving with
`mode=ro&immutable=1` and `query_only(1)`. It is not a candidate or a new SQLite
layer. Report construction as **storage-build CPU on neutral records, equal
adapter work**. The plan's **0.50 of same-host current-main cold-index CPU** has
no equal whole-product path here and remains **unmeasured: needs product
integration (D18, stage 1)**. Never compute that target from an A/C adapter ratio.

D29 approves using storage-build CPU for the both-pass tie-break while judging
correctness/read/disk targets. Set `construction_selection_approved` to true
under that approval, as in the example. The whole-product 0.50 target remains
unmeasured. False still forces comparison-unavailable and no recommendation.

## Estimate and limits

The main proposal allows 150 minutes for 27 fresh timing processes and three
tiny correctness processes. The conditional side proposal adds 45 minutes for
nine timing processes and one tiny correctness process. The maximum is 195
minutes, not the old 180-minute estimate. These are unmeasured planning bounds on
the named 4-vCPU/16-GB CI host, not evidence that the workload fits them. The base
job also needs its separately budgeted setup/fixture phases. Full vector/content
queries can dominate even though these checks make no Check 6 claim.

Estimate engine peak at 1–3 GiB, runner below 512 MiB and checker below 1 GiB;
these are not measured values or an assurance that the proposed caps fit. The
main bounded buffers are the shared 256 MiB cache, 4 MiB sort, 1,024-row Parquet
groups and 1,000-document Bleve batches. Charged file cache, resident Parquet
metadata, largest row payloads and Bleve memory remain unknowns. Missing approval
or an OOM/deadline stops the attempt rather than changing its data or limits.

### Disk admission and retention

After fixture freeze, let `S`, `N` and `E` be the maximum input bytes, node count
and edge count across tier1, tier10 and tier100. Read tier1/tier10 from the reviewed
pins and tier100 from its frozen inventory. Require this conservative free-space
floor before starting the batch:

```text
estimated_one_working_set = 8*S + 300*E + 256*N
required_free_bytes = 2*estimated_one_working_set + 1_073_741_824
```

`8*S` budgets C's fanout/sort input and output beside a final Parquet/Bleve set.
It also allows A/B journal and SST staging. `300*E` keeps the graph serving target
visible; `256*N` allows lookup/range/index metadata. Factor 2 covers unmeasured
expansion and coexistence, and 1 GiB retains logs, receipts and tiny evidence.
This is an admission floor, not a worst-case proof or a claim that every format
meets 300 B/edge. Fixture inputs already occupy disk, so do not add them again to
required *free* bytes.

With successful serial cleanup, peak disk is retained fixture inputs plus saved
evidence plus the largest single candidate/tier construction working set,
including C sort scratch and final indexes. A conclusive C semantic failure can
retain one failed C set while A/B finish; include that retained set as well.
Resource, inventory or cleanup failures stop instead. Actual tier100 bytes,
working-set peaks and a 24-GiB fit remain unmeasured. If the floor fails, stop
before measurement rather than dropping evidence or raising a cap.
