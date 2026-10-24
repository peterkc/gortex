# Gortex storage study harness, v2

Use this module to implement and compare storage adapters against one shared
workload. Only the in-memory oracle is included. Read [FORMAT.md](FORMAT.md) for
record/query semantics and `adapter/adapter.go` for the interface contract.

The module lives at `study/storage/harness/`. All disposable study data lives
under `~/.prime/research/gortex/storage-architecture/tmp/`, which is git-ignored.
Nothing is installed or published. Dataset contents and source mappings can be private; public output
labels do not make them safe to publish.

## Layout and checks

The source module and disposable artifacts are separate:

```text
study/storage/harness/   module gxstudy, Go 1.27.1, standard library only

~/.prime/research/gortex/storage-architecture/tmp/
  src/store.sqlite        Read-only corpus copy
  source-tree/            Receipt-matched source checkout
  exported/src/           Neutral source export
  data/tier{1,10,100}/     v2 records, manifests, queries and oracle answers
  bin/                    Locally built export, gen and oracle commands
  results/                One JSON per measured adapter/tier/run
  cache/                  Disposable Go caches and temporary files
```

Run project commands from the module directory. Build binaries and resolve any
candidate modules before benchmarking. No Go build or module download belongs
in timing. The exporter alone needs the installed `sqlite3` CLI.
All command-line input/output paths are required; the commands have no default
scratch directory. Use the root below for supplied paths and Go temporary files.

```sh
# Start at the repository checkout.
cd study/storage/harness
export GXH="$HOME/.prime/research/gortex/storage-architecture/tmp"
export GOTOOLCHAIN=local GOWORK=off GOMAXPROCS=2
export GOCACHE="$GXH/cache/go-build"
export GOMODCACHE="$GXH/cache/go-mod"
export TMPDIR="$GXH/cache/tmp"
mkdir -p "$GXH/bin" "$GOCACHE" "$GOMODCACHE" "$TMPDIR"

# Before each check, require kern.memorystatus_level >= 30.
sysctl -n kern.memorystatus_level
go vet -p=1 ./...
sysctl -n kern.memorystatus_level
go test -p=1 ./...

# Build only when separately authorized. These are not recovery checks.
go build -p=1 -o "$GXH/bin/export" ./cmd/export
go build -p=1 -o "$GXH/bin/gen" ./cmd/gen
go build -p=1 -o "$GXH/bin/oracle" ./cmd/oracle
```

Keep one Go process active at a time. Do not run the local race detector.
Generation, adapter runs and corpus protocols need separate clearance. Examples
below describe their interfaces; they do not authorize execution. Recovery
restores source only, not the lost datasets, binaries or measured results.

`conformance.CheckBackend(t, backend)` and
`conformance.CheckCatalog(t, backend, factory)` supply small shared tests for
adapter authors. They cover full node payload, absence and catalog CAS rules.
Separate hand-computed tests cover graph, ranked answers, ApplyChange and delta
mutation. Oracle end-to-end results are self-consistency checks, not independent
proof that the oracle is correct.

## Export and generate

Copy only the `.sqlite` file before export. Never pass the original store. The
exporter uses `sqlite3 -readonly file:<copy>?mode=ro&immutable=1`. Use the exact
source tree named by the indexing receipt and verify its revision/clean state.
The exporter reads one materialized node generation and rejects several rather
than guessing an active view. It accepts several real repository prefixes.

```sh
"$GXH/bin/export" -db "$GXH/src/store.sqlite" -tree "$GXH/source-tree" -repo src \
  -revision 76c080e56da3547f2b464522fd6acbe0accaa25e -out "$GXH/exported/src"
"$GXH/bin/gen" -source "$GXH/exported/src" -repos 1 -seed 20261022 -out "$GXH/data/tier1"
"$GXH/bin/gen" -source "$GXH/exported/src" -repos 10 -seed 20261022 -out "$GXH/data/tier10"
"$GXH/bin/gen" -source "$GXH/exported/src" -repos 100 -seed 20261022 -out "$GXH/data/tier100"
```

For a store with eight prefixes and eight clone trees, use an explicit mapping:

```sh
"$GXH/bin/export" -db <copied-group.sqlite> -roots <roots.json> \
  -repo group -out <fresh-export-dir>
```

`roots.json` maps each exact source prefix to a public label, clone and revision:

```json
{
  "<prefix-01>": {"id":"label-01","tree":"<clone-tree-01>","revision":"<commit-01>"},
  "<prefix-08>": {"id":"label-08","tree":"<clone-tree-08>","revision":"<commit-08>"}
}
```

Include all eight entries; the example omits entries 02–07. Labels must be unique.
Without an explicit entry, the label defaults to `group-000`, etc., the tree to
`-tree/<prefix>`, and revision to `-revision`. Eight prefixes produce eight parts.
Unowned nodes appear once, assigned to the lowest-labelled referencing real
repository. Unreferenced unowned nodes use the lowest-labelled part. Only two
different real endpoint owners make an edge cross-repository; an unowned node's
physical placement cannot inflate that count. See FORMAT for qualification rules.
Rebuild `bin/export` whenever its source changes.

For separately indexed stores, repeat `-source id,copy,tree,revision`:

```sh
"$GXH/bin/export" -out "$GXH/exported-real" \
  -source label-a,<copy-a.sqlite>,<tree-a>,<revision-a> \
  -source label-b,<copy-b.sqlite>,<tree-b>,<revision-b>
"$GXH/bin/gen" -sources "$GXH/exported-real/label-a,$GXH/exported-real/label-b" \
  -repos 10 -seed 20261022 -out "$GXH/data/real-plus-derived10"
```

Each real source fills a full slot. Extra slots are labelled derived top-ups,
cycling through the sources at the rank-based file fractions. Existing real cross
endpoints survive qualification and synthetic linking. These commands do not
index repositories or start Gortex. Commas in source-entry fields are unsupported.
Use new tier names to keep real-source tiers distinct from the derived baseline.

Synthetic cross-repository call links are enabled by default, including with
`-sources`. To preserve only the input cross-repository edges, add
`-synthetic-cross-repo=false` to `gen`. This skips link injection and adds no
synthetic cross-repository policy to the manifests. Existing source edges remain
unchanged. Rebuild `bin/gen` after changing its source.

## Link one adapter binary

Place an adapter in its own module, for example `study/storage/adapters/<name>/go.mod`:

```go
module studyadapter

go 1.27.1

require gxstudy v0.0.0
replace gxstudy => ../../harness
```

Register the backend in that binary, not through Go plugins or query-path RPC:

```go
func main() {
    b := mybackend.New()
    runner.Main(b, runner.ReferenceCatalog(b))
}
```

Catalog candidates pass their own `adapter.CatalogFactory`. The shared reference
catalog uses atomic JSON publication and process-level flock; it validates refs
through the candidate's Open/Close. The oracle command uses its own reference
seal verifier. Backend construction must not destroy shared state when the same
binary starts as a measurement or crash child.

`Build` must finish a sealed immutable version. Its ref has matching `Repo`, a
version/digest and an absolute path or descriptor accepted by `Open`. A server
may seal an immutable local descriptor, but it must validate the remote version.
Never put credentials in descriptors, capability notes or result JSON. `Open`
must not mutate sealed artifacts. Returned values and input records are immutable;
segments must support concurrent reads.

`NodeByID` alone returns ErrNotFound for absence. Unknown names, files, edges and
searches return empty,nil. Names sort by ID before the limit; zero means none and
negative means unlimited. File/edge order is unspecified; duplicate edges remain.
Full payload must round-trip. Ranked hits have unique nonempty IDs, finite scores,
descending score order and at most k entries. Use ID tie-breaking.

Capabilities distinguish native/emulated methods and list libraries and deployment.
`Unsupported` maps workload operation names such as `content`, `out` or
`multi_vector` to reasons. Declared unsupported queries are recorded and untimed;
other queries continue. Errors from supported methods fail checks. Build, Open,
BuildDelta and publication remain required lifecycle operations; BuildDelta may
emulate an update by rebuilding and must report that choice.

`Ranking` maps operation names to `RankPolicy`. `Mode` is `"harness"` by default
(also when omitted), or `"native"` for engine-native lexical scorers.

- **Harness scoring:** The existing BM25/cosine policy stays in force. The harness
  owns a minimum overlap of 0.8. Backend `MinOverlap` and `Exact` can only
  strengthen it. Default `multi_symbols` must match the exact local-IDF merge.
- **Native lexical scoring:** Use this for scorers such as FTS5 bm25 or Bleve
  BM25 with coordination. Correctness requires distinct, finite, descending-score
  hits, at most k, all satisfying FORMAT's lowercase Unicode OR-match rule.
  The result cannot be empty when the oracle has hits. Neither formula overlap
  is a correctness threshold. Backend `Exact`/`MinOverlap` apply only in harness
  mode. Query-specific delta marker coverage and required/forbidden IDs remain.

For example:

```go
Ranking: map[string]adapter.RankPolicy{
    "symbols": {Mode: "native"},
    "content": {Mode: "native"},
    "native_multi_symbols": {Mode: "native"},
},
```

If `multi_symbols` has no own entry, it inherits `symbols`' native scoring mode.
An explicit entry selects its own mode; omitted `Mode` means harness scoring.
Native multi APIs keep independent `native_multi_*` keys and never inherit the
base entry. The `native_` API prefix and `Mode: "native"` are different choices:
a native API can still claim harness-formula scoring. Native scoring is lexical
only; vector/cosine operations retain their existing policy.

In native mode, default fan-out merges each repository's native scores. An
untimed check validates every per-repository input and verifies the final top-k,
including scores and ID tie order, against those actual inputs. It does not
compare that merge to formula ranks. Input slice headers are captured into a
preallocated trace during the existing calls; validation/copying stays outside
the timer. Native multi APIs have no harness-owned merge inputs, so they use the
lexical match/structure check instead. Concurrent readers use the same checks.

`Result.local_quality` and `Result.global_quality` report overlap min, mean, p50
and count per measured operation, including native APIs. Both are quality only
for native lexical scoring. Single-repository references have the same scope;
multi search reports both local-IDF merge and global-IDF quality. Existing
`Result.overlap` remains the overlap against that operation's reference answer.
Delta/concurrent checks stay separate and do not weight these fixed-workload
summaries. Oracle-empty queries cannot return hits. `delta-vector` still requires
the inserted ID regardless of ANN overlap.

Exact checks keep the hit-count rule. At a full k cutoff they allow substitutions
only within a fixed relative score tolerance of 1e-5. Both a missing oracle hit
and its replacement must lie in that cutoff band. Replacement scores come from
an untimed oracle scan, not the backend's reported score. Raw overlap is still
reported even when an exact check accepts a cutoff tie.

Optional interfaces add measurements without replacing the default paths:

- `NativeMulti.OpenMulti` serves multi-name/incoming/search/vector requests.
- `NativeTraverser.NativeTraverse` can be implemented on a Segment or MultiRepo.
  Use the exact BFS semantics and caps in FORMAT. Missing native traversal is
  recorded as unsupported. The default BFS remains measurable.
- `ServerHooks` provides Reset, Release and ResourceUsage. The runner resets
  before and after each fresh-process run, releases every built ref after pins
  and segments close, and samples server RSS/disk after phases. These calls are
  outside timing. Reset must affect only this adapter's study namespace.

Native multi handles share the supplied segments' lifetimes; the interface has
no separate Close. Server hooks are optional, but without them server resource
usage and cleanup are not measured. `conformance` tests use disposable fixtures.

## Run protocol and timing

Use a fresh output directory. The normal protocol discards one full warm-up
process, then starts five distinct measured processes in sequence:

```sh
"$GXH/bin/oracle" -data "$GXH/data/tier1" -out "$GXH/results/oracle-new" \
  -runs 5 -crash-trials 12
```

A bounded smoke run can use `-runs 1 -warmup=false`. Do not use smoke results as
five-run measurements. `-safety-timeout` accepts a positive Go duration and
reaches every fresh-process run. Its default is `30s`. It bounds the whole
untimed commit-race pass and, separately, each crash child, including segment
verification Opens. It does not bound the full run or change serving latency
measurements. For a separately authorized large-corpus run, use
`-safety-timeout 15m` if the default cannot cover those Opens. The concurrent
reader handshake and minimum-sample timeouts remain fixed at 30 seconds.

Start servers and prepare native libraries before running.
Run candidates sequentially with no other heavy study work. `uptime` records
machine load before each child run; load is evidence, not a pass/fail threshold.

Each child verifies and loads its data outside timing. It calls GC and
`debug.FreeOSMemory` before phases and each repository build window. It then:

1. Times each build/seal, first open of all segments, and close/reopen.
2. Runs a seeded-shuffled query warm-up and a differently shuffled measured pass.
3. Times only raw adapter calls. Harness BFS bookkeeping stays inside explicitly
   emulated traversal timing. Canonicalization, validation and comparison happen
   after stopping the timer. They still consume CPU between requests.
4. Warms each optional native path with its own discarded seeded shuffle, then
   measures it with another shuffle. It checks a graph-changing one-file
   delta, and reports full rebuild versus delta mode and artifact bytes.
5. Runs eight catalog-aware readers through build/publication and an after-window.
6. Runs strict checkpoint and external-time process-crash recovery checks.
7. Collects process peak RSS and optional server usage, then releases owned refs.

Durations use time.Now/time.Since in nanoseconds. Percentiles are nearest rank:
`ceil(p*N)-1` after sorting. Every distribution has p50, p95, p99, mean and count.
Build/open/delta distributions can have one sample per run; aggregate the five
result files for across-run lifecycle statistics. No formatter, compiler, module
download or network setup is included in a timed window. Server requests that
implement an operation are part of that operation's timing.

First open is the first engine open after build, with no OS cache flush. It is
not a true machine-cold I/O measurement. Warm reopen includes whatever validation
the backend owns. Processes are fresh, but OS/engine caches and file write-back
can still persist between runs. The runner does not sync unrelated workloads.

## Readers, delta and publication

Eight timed readers run through build and publication. After commit, each must
complete at least two checked serving calls on the new ref. A 30-second timeout
fails the run if any reader misses that minimum. The idle window is 150 ms.
Requests Pin through the catalog. A per-version read cache opens outside its
mutex, with one opener per ref, so old versions remain readable while a new ref
opens. New-ref Pin/Open latency is a separate after-publication distribution.

`open_ns` measures verification Open/Close before publication. The reference
catalog caches that successful validation and does not reopen under Publish's
lock. The oracle command uses this same catalog path. `publish_ns` is only the
checkpoint interval before-write through after-dirsync. `precommit_ns` records
other time before that interval. Serving overlap uses the commit interval, not
the validation Open. A very short commit may have no timed overlap samples; that
is reported honestly rather than manufacturing a latency sample.

A separate untimed commit-race pass provides mandatory correctness coverage. At
each of the six ordered checkpoints, two dedicated readers must Pin, Open and
check changed/removed content before the writer proceeds. Precommit windows
require the old ref; postcommit windows require the new ref. Missing hooks,
insufficient checked samples, stale new-version data or a timeout fail the run.
This deliberately requires Pin/read progress while publication is paused. It is
not a publication latency measurement. Its safety limit is controlled by
`-safety-timeout`, with a 30-second default. The retained old pin is also reopened
and checked after commit, and Previous/new visibility remain checked.

The delta mutates a Go source file's graph records: rename, delete and add symbols,
change/add/remove owned edges, and remove/add vectors and text documents. Old-name,
removed-ID and edge checks are exact. Marker searches require the complete oracle
hit set. It is a record update, not parsing or source re-extraction. Its hash and
new source body are synthetic, and inherited opaque payload is not re-derived.

Catalog compare-and-set compares complete refs. Publish(old=nil) requires absence.
A missing expected-old entry in PublishSet requires absence; expected entries
outside Set are ignored. Failed CAS changes nothing. PublishSet is atomic. Pin
returns one coherent snapshot and retains complete versions until idempotent
release. Unknown Pin/Previous returns ErrNotFound. Recover is idempotent.

## Crash checks

A catalog declares `CommitPoint()` and calls these hooks in this order:

```text
before-write -> after-write -> after-sync -> before-rename
             -> after-rename -> after-dirsync
```

For a transactional database, map `before-rename`/`after-rename` to before/after
its atomic commit. The reference point is rename of `state.json`. The first four
stages must recover the old view; the last two must recover the new view. Hook
order violations fail. A completed PublishSet with nonempty expected-old is also
checked before crash trials.

With `-crash-trials 12`, a seeded permutation covers every hook for single-ref
and linked-set publication. Twelve more external-time trials disable hooks in
the child, repeatedly alternate the two complete sets, and send SIGKILL after a
seeded delay from the start handshake. The delay range uses the completed-set
publication time with a 1 ms minimum range. External trials may recover old or
new, but never a mixed/incomplete view. The parent confirms SIGKILL, calls Recover,
Pins the view and opens every visible ref. A test-only two-step catalog proves
that the external kill can expose and reject a mixed set.

Crash children are the same prebuilt adapter binary. Each child uses
`-safety-timeout`, with a 30-second default. This limit includes catalog creation
and full segment verification. stdin stays open through Wait so EOF cannot race
SIGKILL. Trials start from sealed segments; tier1 gets an extra small derived peer for linked sets.
These tests do not simulate power loss, interrupt Build, inject disk failures or
prove that every possible instruction boundary is safe. The reference catalog
leaves unpublished temp files and has no garbage collector or throughput claim.

## Result fields

Measured results are `<adapter>-<tier>-<run>.json`. A failed measured run writes
its result and stops later runs. Warm-up status is kept under `run-00/` but is
not a measured result. Never silently retry a failure or reuse an output path.

```text
Result
  pid, adapter, tier, run, started, uptime, go_version, cpus, manifest_sha256
  overlap {operation: {min,mean,p50,count}}
  local_quality, global_quality {operation: {min,mean,p50,count}}
  capabilities {operations, unsupported?, ranking?, libraries, deployment, notes}
  builds[] {repo, duration_ns, artifact_bytes, stats, ref}
  metrics {operation: {p50_ns,p95_ns,p99_ns,mean_ns,count}}
  checks[] {query_id,operation,status,mismatch,overlap_at_k?,local_overlap_at_k?,global_overlap_at_k?,native_scoring?,
            cutoff_tie_accepted?,detail?}
  delta {repo,duration_ns,artifact_bytes,stats,ref}, delta_checks[]
  concurrent
    idle, during_build, during_publish, after_publish: distributions
    pin_open_during_publish: distribution
    open_ns, precommit_ns, publish_ns, build_publish_ns, reader_passes, checks[]
    pin_open_after_publish, new_samples_per_reader
    commit_race_windows, commit_race_minimum
  crash[] {trial,kind,checkpoint,mode,allowed_view,delay_ns?,commit_point,
           killed,ok,recovered_view?,error?}
  server_usage? {phase: {rss_bytes,disk_bytes,notes?}}
  peak_rss_bytes, failures[], success
```

`stats={mode,bytes,notes?}` and `ref={repo,version,path,digest}`. Metric names include
`build_seal`, `open_first_no_cache_flush`, `open_warm`, `delta_build`, `id`, `name`,
`file`, per-kind `in_*`/`out_*`, `traverse2/3`, `symbols`, `content`, `vector`, and
multi variants. Native measurements use a `native_` prefix. Unsupported requests
have status `unsupported` and no latency sample. An absent optional native multi
interface produces no native multi measurements. Check status is not a quality
score: inspect overlap and the configured floor as well as success.

Artifact bytes sum local file lengths, not allocated blocks. Backend stats must
report server storage outside that directory. Peak self RSS includes datasets,
oracle validation data and the runner heap; it is not incremental engine RSS.
Server usage, when implemented, is separate. No real server adapter is included.

## Remaining fairness limits

- Derived tiers correlate names, code and dependency shapes. File-less nodes
  repeat across variants. Real-source tiers need separate interpretation.
- Full row payload is retained, but export uses JSON and base64, not source SQLite
  page layout. Generated columns stay source payload rather than being recomputed.
- Clustered vectors are synthetic and held-out queries use that same distribution.
  ANN overlap is not semantic relevance, and quality/latency depends on its floor.
- Global-IDF ranking and local-IDF fan-out differ by design. Harness-formula exact merge
  checks catch implementation faults. Native scorers instead check their own
  input-score merge. Both local/global quality report the ranking design cost.
- Finite fixed workloads and small timed commit-overlap counts limit tail claims.
  The separate counted race pass checks correctness, not latency. Validation
  and GC compete for CPU outside individual timing windows.
- Fresh processes do not flush OS caches or isolate the machine from other work.
  `uptime` must be read alongside results. Self RSS includes harness ownership.
- Lifecycle operations are required. Unsupported serving methods can be recorded,
  but a backend still needs valid Build/Open/BuildDelta and a publication path.
- Optional traversal/server hooks are exposed and fixture-tested; no native graph
  engine or real server resources have been validated here.
- SIGKILL sampling cannot prove atomicity for all interleavings. It covers known
  commit boundaries and seeded external times, not power-loss durability or GC.

## Resource condition for verification runs

Recovery checks are `go vet -p=1 ./...` and `go test -p=1 ./...`, in sequence,
with `GOMAXPROCS=2` and `GOWORK=off`. Read `kern.memorystatus_level` before each
command. Start only when it is at least 30. Otherwise report the check as blocked;
do not poll or silently retry. Do not run the local race detector.

Corpus generation, benchmarks and adapter protocols are outside recovery scope.
They need separate clearance and their current resource conditions. Rebuild
`bin/oracle` and `bin/gen` from this module before later use. Historical results
do not verify a rebuilt binary or regenerated workload.
