# Design C base checks (throwaway)

This module implements the Check 1/2/5 prototype. It is not a product backend.
Measurements run only on approved Linux CI. Focused local tests use the approved
isolated environment; construction CPU cases compile locally and run in Linux CI.

## Selected CI steps

Manual dispatch takes one named step or an approved batch:

- `bytes`: build A, B, plain C and C with the side index at tier1, without queries
  or timing fields. Every serving file counts. A is labeled **mixed file:
  graph+text+vectors, upper bound for graph bytes**. Its existing SQLite driver's
  `dbstat` graph-table/index page share is a separate diagnostic, not graph-only
  file accounting.
- `check2`: build/open plain C at tier1 and check every unchanged query once.
  No warmup, repeated query timing, update or catalog phases run. Storage-build
  CPU remains reported separately.
- `check1-plain` or `check1-side`: time only ID, name and file lookups at tier1
  and tier10, with A and B controls and the named C variant.
- `check5-tier1`, `check5-tier10` or `check5-tier100`: measure one tier's base
  reads, bytes and construction CPU. The required `c_variant=plain|side` comes
  from Check 1. These jobs do not rerun Check 1 or select a format.
- `check4`: run the delta check without Check 3. No selection runs Check 6.

The two allowed batches are `check2,check1-plain,check4` and
`check5-tier1,check5-tier10,check5-tier100`. Empty, unknown, duplicate or other
step sets fail preflight before measurement jobs start.

Reduce the three saved Check 5 results with the saved selected Check 1 result:

```sh
python3 -B measure.py --combine-check5 tier1-result.json tier10-result.json \
  tier100-result.json --lookup-result check1-result.json --out selection.json
```

This command performs no measurements. It preserves the existing combined
verdict and selection table. Results must share the source revision, policy and
selected C variant. The old combined path without `step` remains for regression
tests only; the workflow always supplies a selected step.

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

Nodes have dense integer local IDs and one canonical binary payload per ID.
One sorted logical-ID lookup maps to those IDs. Name, qualified-name and file
lookups hold local IDs, not node payloads. Each edge payload appears once;
outgoing and incoming tables contain references. Kind-specific dense N+1 offsets
include zero-degree nodes and explicit unresolved endpoints. The graph ID columns
are fixed-width and uncompressed. Strings and open-ended metadata live in the
canonical payload area. All serving files, including offsets, lookup copies,
seal metadata and the optional bounded Pebble side index, count in inventory.
The side index holds local IDs only. No whole-graph serving map replaces a lookup.

After writing, construction reads every table and compares a streaming full-row
digest with the input. Duplicate edges survive; duplicate logical node IDs refuse
sealing. Open rejects duplicate canonical local IDs and truncated offset
directories before returning a reader. All six harness record families persist.

Each neutral record family is decoded once. That pass fans out lookup and
adjacency copies, feeds Bleve and the optional side store, validates vectors,
and counts records. External `sort` uses a 4 MiB buffer. Parquet row groups contain at most 1,024 rows.
Serving reads use sparse range lookup and bounded block caching, not a resident
graph. The shared serving cache payload budget is 256 MiB across all repositories:
256 MiB Parquet, or 128 MiB Parquet plus 128 MiB Pebble in the fallback. Bleve,
metadata and OS cache remain outside that budget and inside process/cgroup memory.
Each open segment retains parsed metadata and file handles for its 14 tables,
three range directories (names, paths and terminals), and two dense offset files.
Outgoing/incoming range directories exist only while building the dense offsets;
other unused directories are not written. Queries create private readers, not
new file/footer parses. This is 19 retained handles before Bleve/Pebble handles.
The first open validates local IDs and every offset; later opens of the same
absolute path and seal digest reuse successful process-local validation. Every
open still checks the seal, artifact sizes and schemas. A new path or seal digest
needs fresh validation. Failed validation is never cached. Sealed files must
remain immutable. Offset validation reads 64-KiB blocks. The tier100 memory and file-descriptor cost remains unmeasured. Cache
charges use allocated block capacity, including short final blocks.
The Bleve mapping and query functions remain identical to reference B.

Every graph lookup copy, adjacency copy, range directory, seal and Pebble file
counts in graph serving bytes. Text, vectors and total bytes are separate.
B routes existing key prefixes into physically separate `graph`, `text-records`
and `vectors` Pebble stores. All stores share one 256 MiB cache. Each store and
C's side store bulk-build with WAL disabled and non-sync batches, then Flush,
Close and whole-file seal fsync. Unpublished builds have no WAL recovery promise.
Pebble v2.1.7 still creates an active WAL with writes disabled. Close emits its
11-byte EOF trailer. After successful Flush/Close, one shared helper validates
empty or exact EOF-only content and removes it from the identity-captured build
store before sealing. It rejects real records, malformed/trailing content and
aliases. Size alone never permits removal. Sealed stores must hold no `.log`
entry; byte accounting never hides a stray WAL.
Graph files, raw text plus Bleve files, and vector files have independent byte
inventories. Shared seals count as graph metadata. The three categories sum to
all artifact bytes. No compressed SST prefix share is estimated or subtracted.
The C fallback counts both its Parquet lookup copies and its Pebble side store.
B's unused K/kind index is omitted; Kind remains in every full node record.
This B is `dense-local-id/v2`, single-decode and WAL-disabled, not the older
JSON/string-key Check 5 B or single-store/two-pass options-study B. Historical restoration hashes in
`adapters/RECOVERY.md` describe that older version, not these Check 5 numbers.
B still Flushes and Closes without a forced final full-range compaction. Its
sealed LSM shape, bytes and reads can depend on background compaction timing.
This is disclosed in `result.json`, not silently treated as a fixed LSM shape.
The tier100 cost of 300 DBs, native file/metadata caches, open handles, concurrent
memtables and batches is unmeasured, like C's retained metadata/handles.
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

The configuration must have exactly these fields. The proposed envelopes are
12,540 seconds main (including finalization and runner join) and 3,600 seconds
conditional side. These and the example memory limits need the run owner's
approval before execution:

```json
{
  "schema": "design-c-base-run/v1",
  "source_root": "/absolute/checkout",
  "source_revision": "<reviewed full source OID>",
  "recipe_sha256": "<approved generator/recipe SHA-256>",
  "approval_path": "/absolute/base-policy/user-operator-approval.json",
  "approval_sha256": "<approval receipt SHA-256>",
  "base": {"main_wall_seconds": 12540, "side_wall_seconds": 3600},
  "job": {
    "started_unix_seconds": 0,
    "timeout_seconds": 21000,
    "cleanup_upload_seconds": 1200
  },
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

The driver passes `-min-query-samples 20`. Each judged stratum needs at least
20 measured samples in **each** fresh process, because each process's p95 decides
the ratio. Pooled samples cannot satisfy this requirement. The existing discarded
warmup counts each stratum, then each query repeats `ceil(20 / warmup_count)`
times, with a minimum of one. Without warmup observations, a judged query repeats
20 times. Hits, misses and design-specific read strata use this rule; shared
Bleve text is observational and is not repeated. Ordinary runner callers retain
the default one-call behavior.

Repetitions use the same warmed keys consecutively. They are not independent cold
keys or statistical confidence evidence. A GC pause is still possible. Warmup
calls do not count. Lookup directions use separate `name_hit` and `name_miss`
strata; the combined `name` p95 is reported but does not decide. Memoized semantic
checks use the full source/query/policy/answer envelope, omitting only elapsed
time and warmup state. They do not cache serving answers or remove measurements.

### Checker-only edge index

After native sampling stops, the checker builds one incoming/outgoing endpoint
index per immutable source. It retains source byte offsets and lengths, not edge
payloads or candidate answers. Each lookup reads the original full JSONL rows in
source order. Kind filters, duplicates and BFS fanout rules are unchanged. Text,
vector and node reference reads still scan. In-memory data and update overlays
use the independent scan path. Changing the complete dataset descriptor closes
the old view before opening another one.

Construction checks the manifest, record hashes, edge count and byte size. It
rejects invalid JSON, UTF-8 and an unterminated last line, as the scan reader does.
Every reference call refuses changed file identities, sizes or modification
times. These guards assume the frozen input tree stays immutable; they are not
a defense against an actor that rewrites bytes and forges filesystem metadata.
The checker closes its read handles at shutdown. None of this work enters native
build/query duration or construction CPU. Scan parity tests cover each endpoint
lookup and BFS frontier, including a deliberately dropped index entry.

The accepted runner enforces independent runner/checker/engine cgroup hard limits.
The driver bounds main and conditional side with the required `base` limits and
clamps both to one absolute first-step clock in `job`. The workflow records epoch
seconds in its first step, before checkout. It replaces the zero placeholder in
the example; missing, zero, future or expired clocks refuse execution. Setup
cannot reset this clock. The gap from GitHub job start to that first step is
expected to be seconds, not measured; the one-minute margin also covers this gap.
No token permission is added to fetch a timing value.

On expiry or interruption, the driver sends SIGTERM to the runner and allows the
existing 30-second cleanup join.
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
ceiling of 1.0. Design-specific multi/traversal reads keep that unchanged ceiling
and need at least 20 observations in every paired process. `symbols` and
`multi_symbols` run identical Bleve mapping/query and fanout code in B and C.
Their timings remain reported separately, but cannot reject a format or set a
read direction. `multi_name` remains judged: its fanout calls different native
Parquet/Pebble lookup paths. No noise margin is introduced.

Opposite design-specific read directions require consistency across all three
pairs before they become a read trade-off. Construction ties use the sum of
per-tier median adjusted CPU, then total bytes; unresolved ties return to the user.
CPU comparison still ranks the observed medians exactly, not statistical equality.
A validated disk miss remains conclusive even if latency sampling is partial.
Partial per-metric details remain visible; B must independently pass before it
can be selected. Resource, identity and hash failures still block first.

Only complete, hash-valid terminal evidence with typed `semantic` check failures
can reject C for a wrong answer and fall back to independently passing B. An
`operation_error`, OOM, deadline, invalid hash, missing receipt or unclassified
failure blocks the comparison. The complete runner failure multiset must be
explained by typed semantic check records. Extra input-change, crash, release,
reset or unknown failures block, even beside a semantic mismatch. The driver does
not classify the contents of their human error text. A failed A/B correctness
control invalidates it.
All selection-table rows, numeric boundaries and these rejection paths have
regression test source. The driver never treats unknown C evidence as a B pass.

A is the plan's read-only SQLite segment reference. The existing adapter is
`study/storage/adapters/sqlite-segment` (modernc SQLite v1.60.1), serving with
`mode=ro&immutable=1` and `query_only(1)`. It is not a candidate or a new SQLite
layer. Report construction as **storage-build CPU on neutral records, equal
adapter work**. The plan's **0.50 of same-host current-main cold-index CPU** has
no equal whole-product path here and remains **unmeasured: needs product
integration (D29)**. Never compute that target from an A/C adapter ratio.

D29 approves using storage-build CPU for the both-pass tie-break while judging
correctness/read/disk targets. Set `construction_selection_approved` to true
under that approval, as in the example. The whole-product 0.50 target remains
unmeasured. False still forces comparison-unavailable and no recommendation.

## Estimate and limits

The rerun has a 209-minute main envelope for 27 fresh timing processes, three
tiny correctness processes, runner join and finalization. The conditional side allowance remains 60 minutes for
nine timing processes and one tiny correctness process. No queries, tiers,
samples, fresh runs or crash trials are removed. The crash-child safety limit
remains 900 seconds; it is not a whole timing-process timeout.

Let `P` be the exact total bytes of the unique protected files in the initial
snapshot, including all four fixture tiers, source and policy files. The driver
reserves `F = max(60, ceil(9*P / 67_108_864))` seconds for the closing protected
hashes, output hashes and `result.json`. This assumes 64 MiB/s hashing and allows
one protected pass plus output bytes up to `8*P`, using the existing capacity
estimate's input multiplier of eight. This is a planning allowance, not a hard
bound on retained failed artifacts or a measured hash rate. The 60-second floor
covers small fixtures and result serialization. Slower hashing, larger retained
outputs or many small files can still exhaust it; the wrapper remains a backstop.
The result records `P` and `F`. No free-disk-based allowance is charged.

The complete admission sum is **60 preparation + (209 - F/60 - 0.5) main work
+ 60 conditional side + 0.5 runner join + F/60 finalization + 20 cleanup/upload
= 349 minutes < 350-minute job timeout < GitHub 360 minutes**.
Preparation is fetch 10, tiny generation 5, tier100 generation 20 and setup 25.
Both work deadlines subtract `F + 30` seconds from their phase envelopes and the
329-minute absolute cutoff from the first step, before checkout/setup. An
exhausted budget refuses work. The wrapper still kills at 329 minutes plus
30 seconds; work plus join plus finalization ends by minute 329, before that kill.
The separate one-minute margin covers the expected job-start gap. The remaining
20 minutes stay reserved for owned cleanup and uploads, not more measurement.

For protected bytes of tier1 alone (516,258,893), `F` is 70 seconds; for tier10
alone (436,311,531), it is 60 seconds; the unchanged pair gives 128 seconds.
Tier100 is not frozen yet. Ten copies of tier10 would be 4,363,115,310 bytes and
586 seconds alone, or 713 seconds with the unchanged pair. This projection is
not a tier100 pin. At that combined size, main work is 11,797 seconds and the
reserve is 5.7% of its 12,540-second envelope. Source, policy and tiny bytes add
to `P`; the driver uses the actual frozen bytes, not this projection.

Indexed-checker planning ranges are 1–4 minutes per tier1 process, 2–6 at tier10
and 3–10 at tier100. Main totals 54–180 minutes; conditional side adds 18–60.
These are unmeasured estimates, not a completion guarantee. Node/text/vector
reference scans and answer payloads remain. The one-host comparison and the full
100-repository workload stay unchanged.

Estimate engine peak at 1–3 GiB, runner below 512 MiB and checker below 1 GiB;
these are not measured values or an assurance that the proposed caps fit. The
main bounded buffers are the shared 256 MiB cache, 4 MiB sort, 1,024-row Parquet
groups and 1,000-document Bleve batches. Checker span metadata may add roughly
100–200 MiB, but its peak and speedup have not been measured; the 1 GiB checker
cap and engine cache/answer-cache rules are unchanged. Charged file cache, resident Parquet
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
