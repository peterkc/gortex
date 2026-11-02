# Design C Check 6: vector contract prototype

This is a throwaway scan check, not a production backend or a model-quality test.
It implements exact F32 scans and exhaustive int8 candidate scans with original-F32
reranking. It does not implement ANN.

**Approval:** The lead reports that the user approved D28's synthetic fixture,
the synthetic-only int8 branch and amendment 4. Code and tiny tests are prepared
for the combined CI change. Measurement still requires the lead's recorded
approved PLAN/YAML fingerprint and separate approval of the batched run list.
No Go command or measurement may run on the Mac.

## Contract and scope

- 150,000 document vectors, 768 dimensions, at most four scan workers.
- Float64 cosine accumulation over the original F32 components.
- Ordered top-k logical vector IDs. Equal scores sort by ID, without an epsilon.
  This is the storage check's rule, not the accuracy evaluator's corpus-order rule.
- Every warm-up and measured answer is checked, including every returned F32 score.
  One omitted neighbor, wrong ID, changed score or reordered tie fails exactness.
- Nearest-rank p95 is `sorted[ceil(0.95*n)-1]`. The target is at most 50 ms.
  F32 timings measure the current scalar, single-accumulator float64 kernel.
  Bitwise score checks fix its summation order. A latency miss is specific to
  this kernel, not evidence against an optimized exact F32 scan.
- Requests run serially. Each request uses the explicitly selected 1–4 workers.
  Int8 selects one global candidate pool before original-F32 reranking.

The check exercises only G's vector-capability result contract against one
manifest-bound, immutable in-process corpus snapshot. Loading the fixture is not
Part 1's publication-aware O operation. B/S/P/O/T/R/C, graph predicates, parent
chunk aggregation, visibility masks, catalog transitions, Arrow mapping lifetime,
watch updates, daemon constructors and backend installation are **not exercised**.
No full-backend conformance or lifecycle parity is claimed.

## Existing input format and approved fixture

`gxstudy-v2` stores 384-dimensional graph-fixture vectors. It cannot represent the
chosen 768-dimensional model shape without a format change. This module leaves
that format unchanged. With the lead's explicit steer, it instead uses the
existing accuracy bridge's format:

- `documents.jsonl`: unique `id`, unique `parent_id`, and `repo`.
- `queries.jsonl`: unique `id` and `repo`; the ID names a document parent.
- `vectors.npy`: no-pickle, C-order, little-endian F32, rank two. Documents precede
  queries. Both NPY v1 and v2 headers are accepted in NumPy's normal field order.
- `manifest.json`: generator source revision and executable hash, seed, recipe,
  dimension, document/query counts, and SHA-256 for the three input files.

Body/language fields in existing JSONL rows are not consumed. Stored vectors must
be finite and L2-normalized within `1e-5` squared-norm error. That tolerance checks
input normalization; it is not a score or tie tolerance. Unknown NPY dtypes,
Fortran order, wrong shape, partial/trailing bytes, duplicate identities, bad
hashes and symlinked input paths refuse timing.

No saved public vector payloads or queries were found in the tracked study tree.
`study/storage/vector/RECOVERY.md` records their loss. D28 approves this
synthetic fixture instead of inference:

1. Use seed `20261102` and 32 independent cluster centers.
2. Generate each center/noise vector from a position-specific `math/rand` source:
   `seed + position*104729`. Components are uniform in `[-1,1)`, then normalized.
3. Adjacent document pairs share one position and therefore identical vectors.
   Their cluster is `(document_position/2) % 32`.
4. Form each F32 mixture as `0.8*center + 0.2*unit_noise`, compute its norm in
   float64, and round the normalized components to F32. Duplicate pairs create
   exact ties; shared clusters also create near-ties.
5. Save 256 held-out queries. Query noise positions start at `1000000000` and
   cycle through the same clusters. Centers use positions starting at `2000000000`.
   Query IDs are relevance-style labels; their vectors are not copied documents.

The generator is reproducible only at its pinned source, Go build and seed.
`study/storage/checks/run-policy.json` owns the seed and query-count pins; CI
checks them when generating the fixture. The binary requires the exact approved
manifest hash rather than a second hard-coded seed/query-count policy.
The query count and all actual hashes must be frozen before timing. This fixture
says nothing about embedding quality or real-vector quantization.

## CI checks and build

Use the CI-selected Go 1.27.1 toolchain, isolated caches/HOME/TMPDIR, `GOWORK=off`,
`GOTOOLCHAIN=local`, `GOFLAGS=-mod=readonly`, and `CGO_ENABLED=0`.
The module has no third-party dependency. It requires only the local `gxstudy`
module through `replace gxstudy => ../../harness`; no `go.sum` is needed.

```bash
go -C study/storage/checks/vector test -p=1 -v ./...
go -C study/storage/checks/vector vet -p=1 ./...
go -C study/storage/checks/vector build -p=1 -trimpath \
  -ldflags "-X main.sourceRevision=$GITHUB_SHA" \
  -o "$RUNNER_TEMP/storage/bin/vector-check" .
```

The tests use the harness's `conformance.Fixture()` plus small 768-dimensional
NPY/JSONL files. They do not create the full measurement fixture or run a benchmark.
Do not add a `-race` run on the Mac. Keep same-host measurement runs serial.

## Scenarios and tests

Each scenario is Given/When/Then in the normal Go suite:

- Given the harness fixture, when F32 scans with 1–4 workers, then ordered hits
  and scores equal the existing oracle (`TestHarnessVectorContract`).
- Given three identical vectors z/a/m competing for two slots, when input order
  reverses or worker count changes, then both scans return a/m. The int8 pool is
  also limited to two (`TestExactTiesAndInputOrder`). This correctness fixture
  proves cutoff ties; the paired latency corpus at k=10 need not split a tie.
- Given unequal near-ties, when the difference is below an epsilon-style cutoff,
  then score order still wins (`TestNearTiesUseNoEpsilon`).
- Given int8-colliding candidates, when pruning drops the original-F32 winner,
  then exactness fails; reranking the full candidate pair succeeds
  (`TestInt8PruningMustNotMasqueradeAsExact`).
- Given exact answers, when one neighbor, ID, score or order changes, then the
  comparison fails (`TestExactnessRejectsOneWrongResult`).
- Given invalid dimensions, nonfinite/zero vectors, bounds, cancellation or more
  than four workers, when scanned, then refuse. Given k=0, return no hits
  (`TestScanBoundsAndCancellation`). Caller inputs remain unchanged
  (`TestNoMutation`).
- Given one pinned seed, when rebuilding/reopening tiny saved files, then hashes,
  dimensions, row order and duplicate pairs remain the same. Existing output
  refuses replacement (`TestExistingNPYIdentityFormatRoundTrip`).
- Given altered hashes, aliases, malformed/truncated NPY, bad identities or a
  partial/reordered engine answer, when loaded or checked, then refuse or report
  non-exactness (`TestInputHashAndAliasRejection`,
  `TestNPYRejectsUnsupportedAndPartialInputs`, `TestIdentityRejections`,
  `TestEngineCheckerAndOmittedNeighbor`). Manifest fields, query targets and all
  engine identity fields have independent refusal cases
  (`TestManifestGuards`, `TestQueryTargetGuards`, `TestEngineIdentityGuards`).
- Given missing CLI arguments or approval drift, when invoked, then refuse before
  output creation. Given a failed component, retain a blocked result and receipt
  (`TestExplicitRunArgumentsRefuseBeforeOutput`,
  `TestEntrypointRequiresBinaryAndArguments`, `TestApprovalIdentityAndLimits`,
  `TestFailedComponentCannotProducePass`). `TestRunCLIRefusalGuards` checks the
  pinned generator, source, count and output-path refusals at the actual CLI
  boundary before output or child creation. Unknown scan modes also refuse
  before output (`TestMeasureUnknownModeCreatesNoOutput`). A later output
  refusal retains completed sample rows
  (`TestMeasurePreservesSamplesAfterLateFailure`).
- Given an inexact F32 result, block selection before int8 or ANN. Given exact
  F32 passes, keep F32. Given an exact F32 latency miss, synthetic int8 can never
  recommend int8. Only two latency misses expose ANN diagnostic eligibility;
  nothing runs ANN (`TestSelectionBranches`). The serialized summary identifies
  the scalar F32 kernel and blocks reference mismatches
  (`TestSummarizeScalarKernelAndF32Defect`). Warm-ups never enter p95
  (`TestNearestRankP95`).

Update/delete/remove-readd and concurrent publication scenarios do not apply to
this immutable vector-only snapshot. They remain untested lifecycle obligations.
No existing shared code or consumer changed, and no test was deleted.

## Measurement entrypoint

All binary, data and output paths must be explicit and absolute. Build and
approved fixture generation happen outside query timing. Generation is a target
run too: invoke the command below only through the accepted `limited-command`
launcher, under its own approved generator-request binding, memory cap and
absolute deadline. Require its completed receipt before admitting measurement.

```text
VECTOR_CHECK generate -out NEW_FIXTURE_DIRECTORY \
  -documents 150000 -queries 256 -seed 20261102
```

The measurement entrypoint uses the prebuilt binaries. It neither builds code nor
generates data. Numeric limits have no defaults. These are **proposed sizing
values**, not run approval:

```bash
bash study/storage/checks/vector/run.sh "$VECTOR_CHECK" \
  -data "$FIXTURE" -fixture-pin "$FIXTURE_MANIFEST_SHA256" \
  -out "$FRESH_OUTPUT" -launcher "$LIMITED_COMMAND" \
  -cgroup-root "$DELEGATED_CGROUP" \
  -approval "$RUN_APPROVAL_JSON" -approval-pin "$RUN_APPROVAL_SHA256" \
  -wall-time 10m -runner-bytes 268435456 \
  -checker-bytes 2147483648 -engine-bytes 2147483648 \
  -workers 4 -k 10 -candidates 128
```

The approval JSON is an operator-supplied record of the user's exact approval,
not an approval generated by this module. It must contain these closed fields:

```json
{
  "approved": true,
  "source_revision": "FULL_REVIEWED_SOURCE_OID",
  "executable_sha256": "VECTOR_CHECK_SHA256",
  "launcher_sha256": "ACCEPTED_LIMITED_COMMAND_SHA256",
  "fixture_sha256": "FIXTURE_MANIFEST_SHA256",
  "host": "EXACT_HOSTNAME",
  "operator": "NAMED_OPERATOR",
  "price_cap_usd": 0,
  "wall_time_seconds": 600,
  "memory_limits_bytes": {
    "runner": 268435456,
    "checker": 2147483648,
    "engine": 2147483648
  }
}
```

The example's placeholders must be replaced. The source revision must match the
reviewed build. The fixture must come from that same pinned generator executable.
Approval, manifest and executable drift refuse the run. An existing output
folder refuses adoption. The caller must already run inside the delegated
cgroup tree. The CI job owns last-resort cleanup if the outer controller is lost.

The controller runs five accepted `limited-command` scopes serially:

```text
F32 engine -> F32 checker -> int8 engine -> int8 checker -> result reducer
```

Runner bytes cap the small reducer. Checker bytes cap each independent reference
process. Engine bytes cap each scan process. The launcher's `engine` field is its
single external-process slot; the top-level receipt gives each slot its actual
role. The small outer controller and launcher stay outside target scopes so they
can preserve failure and cleanup evidence. Their enclosing job cap is the
operator's responsibility. They are not reported as engine memory.

`result.json` contains target values, exactness, p95, pass/miss/partial/blocked and
the branch taken. It never starts ANN. `run-receipt.json` binds source/build,
fixture recipe/seed/hashes, host, exact component commands, all numeric caps,
absolute deadline, approval hash, terminal component receipts and output hashes.
Each component receipt includes engine/reducer/checker PID, RSS, cgroup peak,
OOM/exit state and owned-scope removal. `result.partial.json` is never completion
proof by itself. Each successful query writes its existing sample JSON record
outside the timed interval to `<mode>-samples.partial.jsonl`. A hard deadline or
OOM can leave a truncated last row; earlier complete rows remain evidence, not a
complete result. Failed logs and partial files remain in the output folder.

An F32 reference mismatch blocks performance selection. Exact F32 passes keep
F32; int8 is extra evidence. A scalar F32 latency miss returns synthetic-only
int8 results to the user, even if int8 passes. Both latency misses mark ANN
eligible for a separately approved diagnostic, not implemented here. No retry,
cap increase, inference, native build or backend recommendation follows.

## Sizing estimates, not measurements

On the proposed four-vCPU/16-GB Ubuntu CI host, estimate 3–8 minutes for generator,
256-query scans and both independent reference passes. Request a 10-minute total
measurement bound. Each pass touches 115.2 million components per query; two
checkers also sort the complete score list. The checker reuses one score buffer
instead of retaining full-sort allocations between collections. Compilation is
separate.

The F32 matrix is 460,800,000 bytes. Int8 adds 115,200,000 bytes; norms, IDs,
queries and heap candidates add tens of MiB. Estimate 0.6–0.8 GiB peak engine RSS
and 1.1–1.3 GiB cgroup charge if input file cache is charged there. The checker
needs roughly 0.5–0.7 GiB RSS, potentially another input-file-sized cache charge.
The reducer/controller should stay below 64 MiB each. A one-GiB generator cap and
two-GiB engine/checker caps are proposals for the lead's approval request, not
measured peaks or defaults. A cap miss stops; it does not authorize a larger cap.
