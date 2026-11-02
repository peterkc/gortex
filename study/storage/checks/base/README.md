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

Nodes have three sorted lookup copies. Edges have outgoing/incoming sorted
copies and sparse CSR/CSC-style endpoint range directories. Zero-degree and
external endpoints do not create fake nodes. The prototype is GraphAr-style,
not GraphAr SDK/wire compatibility or the design's final dense N+1 layout.
Every table retains the complete source record as opaque JSON payload inside
Parquet. After writing, construction reads every table and compares a streaming
full-row digest with the input. Duplicate edges survive; duplicate node IDs
refuse sealing. All six harness record families persist.

External `sort` uses a 4 MiB buffer. Parquet row groups contain at most 1,024 rows.
Serving reads use sparse range lookup and bounded block caching, not a resident
graph. The shared serving cache payload budget is 256 MiB across all repositories:
256 MiB Parquet, or 128 MiB Parquet plus 128 MiB Pebble in the fallback. Bleve,
metadata and OS cache remain outside that budget and inside process/cgroup memory.
The Bleve build and query functions match unchanged reference B.

Every graph lookup copy, adjacency copy, range directory, seal and Pebble file
counts in graph serving bytes. Text, vectors and total bytes are separate.
B's mixed graph KV retains vector/text primary records; the comparison counts its
whole graph directory rather than subtracting estimated bytes. Scratch disk peak
and old/new-version coexistence are not measured by this base-only prototype.

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
Rebuild unchanged `sqlite-segment` (A) and `pebble-bleve` (B) against the same
harness revision. The harness now records engine self+joined-children build CPU
and hash-bound query timing logs. Run its existing test/vet suites too.

## One measurement entrypoint

The lead calls `./run.sh --config /absolute/run-config.json` on approved Linux CI.
No paths, limits or output directory have defaults. Existing output is refused.
The configuration must have exactly these fields. Values below are placeholders
or proposed bounds, not approved run limits:

```json
{
  "schema": "design-c-base-run/v1",
  "source_root": "/absolute/checkout",
  "source_revision": "<reviewed full source OID>",
  "recipe_sha256": "<approved generator/recipe SHA-256>",
  "approval_path": "/absolute/user-operator-approval.json",
  "approval_sha256": "<approval receipt SHA-256>",
  "wall_seconds": 10800,
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

The driver runs that full correctness protocol once per candidate, then uses the
accepted timing-only protocol at all three scaling points. Timing-only success is never full lifecycle correctness. The full-table
C digest check establishes tier1 serialization, while the disk-backed checker
validates every measured answer. Data retains the harness gxstudy-v2 format.
All host timings run serially. Builds/downloads occur before this entrypoint.

The accepted runner enforces independent runner/checker/engine cgroup hard limits.
The driver bounds the whole batch by `wall_seconds`. On expiry or interruption,
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

Check 1 requires each hit/miss p95 at tier1/tier10 to be at most 1.5x A. A miss
runs the bounded Pebble fallback. Check 5 includes its build CPU and bytes.
All five selection-table rows and CPU/bytes/tie/read-conflict subcases are tested.
The driver refuses a B fallback without a B pass.

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

For three fresh measured processes, allow roughly 90–180 minutes on the named
4-vCPU/16-GB CI host, including a possible side-index branch. This is an unmeasured
estimate based on up to four full tiny correctness runs and twelve tier timing
invocations, repeated full table sort/validation, Bleve construction and the
harness's disk-backed checker. Full vector/content queries inherited from the
fixture can dominate even though they add no Check 6 claim. New fixture sizes
could change the estimate materially. Report the actual terminal result.

Estimate engine peak at 1–3 GiB, runner below 512 MiB and checker below 1 GiB;
these are not measured values or an assurance that the proposed caps fit. The
main bounded buffers are the shared 256 MiB cache, 4 MiB sort, 1,024-row Parquet
groups and 1,000-document Bleve batches. Charged file cache, largest row payloads
and Bleve memory are the main unknowns. Missing approval or an OOM/deadline stops
that attempt rather than changing its data or limits.
