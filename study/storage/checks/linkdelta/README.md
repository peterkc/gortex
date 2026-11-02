# Design C checks 3 and 4 (throwaway)

This module measures name/scope linking and sealed Arrow/Parquet deltas over the
shared C Parquet base in `../base`. It is not a production adapter or proof of
daemon parity. Input is the harness `gxstudy-v2` format. Internal facts and row
envelopes are derived scratch, not a replacement input format. Do not run Go or
measurements on the Mac.

## Scenarios

- Given per-file declarations and facts without target IDs, when the symbol
  table links them, then full edges, payload and duplicate multiplicity equal
  the clean input. Nested scopes and reversed enumeration preserve the result.
- Given valid duplicate names or dangling targets, when linking runs, then
  projected declaration ordinals distinguish the names and raw dangling IDs stay
  unchanged. Collision and `unresolved_kept` counts remain visible.
- Given malformed ambiguous or missing projected lookup keys, when linking runs,
  then witnesses name the facts and candidates. It reports partial, never guesses.
  A new definition or changed dependency resolves against fresh declarations.
- Given a no-op, body-only update, deletion, rename or remove/re-add, when either
  delta form replaces a file, then graph and vector answers match clean output.
  Incoming dangling edges remain, as the harness contract requires.
- Given an old pin, stale writer, interrupted publish or restart, when a new
  pointer is published, then the old reader stays readable. Stale CAS refuses.
  Retirement refuses active/previous roots, ancestors and unreferenced live pins.
- Given a dropped edge, removed tombstone, corrupt payload, duplicate node ID or
  alias/symlinked payload path, when checking runs, then it refuses or fails.
- Given no paths, limits, approval binding or a fresh output directory, when the
  entrypoint starts, then it refuses rather than guessing or overwriting evidence.
- Given native Bleve alias search, when delta-local statistics change ranking,
  then the report lists every differing ordered top-k case against a clean C
  rebuild. Native match/mask correctness still fails on invalid or removed hits.

## Representation and measured scope

Arrow IPC uses arrow-go v18.7.0. Parquet uses parquet-go v0.30.1, matching the
shared C reader. Bleve v2.6.1 uses the existing adapter's exact mapping, analyzer,
BM25 policy and `unsafe_batch=false`. Both delta forms contain the same typed
ownership columns, tombstones and opaque full-record JSON. Both are uncompressed.
Each also has sealed small symbol/content Bleve indexes. All serving files count
in bytes. Close, payload fsync, seal fsync and directory fsync precede publication.
The reference harness catalog supplies durable guarded pointer replacement.

Real measurements require `base_layout="c-parquet"` and use
`base.Backend{SideIndex:false}`. The baseline query calls go directly to that
reader. Contract tests use that same C path; there is no second JSON serving
base. A missing C text-index API visibly blocks the run. Text build/search
reuse `base.BuildText` and `base.SearchText`, including their mapping and analyzer.

The overlay merges typed C lookups with delta ID/name/file and both adjacency
indexes. Newest-mask maps make each visibility decision constant-time. Delta
records are decoded once at open, not during every query. Vector merging requests
base top-k plus the masked-ID upper bound, filters old IDs, then merges exact delta
scores with the same score/ID tie order. Native search uses a Bleve alias of base
plus deltas. Each member filters only its superseded document IDs, so replacement IDs in a
newer member remain visible. Ordinary native rank differences are diagnostic,
not quietly converted to slower formula queries. Full delta-marker coverage is
still an exact correctness requirement.

Check 3 removes `edge.to` only for declared targets. Its projected lookup key
contains target name, repository, file scope, kind, span and a per-file declaration
ordinal assigned during preparation. Facts do not retain declared target IDs.
Dangling facts preserve their full original edge, including the raw target ID,
and count as `unresolved_kept`. Original and extended key collisions are counted.
Missing or ambiguous supplied lookup keys produce witnesses and a partial result.
The scope/span/ordinal projection is stronger than real extractor knowledge.
Imports, overloads, method sets and language resolver rules are untested.
Successful synthetic parity is not production-linker completeness. Preparation
joins consumed declarations and partitions facts once with a bounded external
sort. Package fallback reads only its own facts and consumed declarations. It does
not rescan the global fact corpus per package.

The delta batch alternates the harness's `runner.MakeChange` mutation and the
original complete output of that same file. The two independently prepared clean
disk sources cover every odd/even checkpoint. This measures edit churn and
physical chain length, not growth in distinct changed files. No tier query is
dropped. Additional delta identity and exact text-marker probes join the workload.
Warm-up and measured observations retain `elapsed_ns`, state, variant, repeat,
query and full-answer SHA256. Full answer blobs are content-addressed outside the
timed call and stored once; no answer cache replaces a timed query. A separate
capped checker streams pinned `expected.json` for unchanged original queries.
Those generator-format entries supply golden answers, not lexical match sets.
Preparation computes lexical membership from the same source/token rule as the
oracle, including explicit empty sets. A missing prepared set blocks verification,
not a candidate correctness miss. Native lexical results must have the full
expected top-k count even when their rank order or selected IDs differ.
Oracle tokenization accepts Unicode letters and decimal digits (Nd). Bleve accepts
letters and all number categories (N), including other numbers (No) and letter
numbers (Nl), such as `²`, `½` and `Ⅻ`. These rules can yield different matches and
counts, so a correct native base can fail against the oracle on such text. This
limit is not exhaustively checked against the frozen corpus. The occupancy rule
remains strict; delta and wrapper answers compare with clean C's same analyzer.
Aligning shared tokenizers is outside this module.
Non-lexical reuse binds the complete query and
unchanged hash-pinned data to the approved golden answer; it does not assume that
the generator and current harness revisions are identical. Current clean C still
must match exactly. Incompatible semantics fail rather than regenerate fixtures.
Changed-state and added-probe references are computed once, on disk, and shared
across both formats. Repeated complete checks reuse a reference/query/answer-bound
validation marker. Native ordered top-k pairs use disk scratch, not a retained
answer corpus. Original workload queries must target `ds[0]`; `delta_repo` must
name that repository. Both workload files must be pinned by the tier manifest.

The one-file CPU and visibility window includes delta/text construction, seal,
fsync, catalog verification/CAS, pinned open and a file read. It excludes source
extraction and daemon/watch routing. Read ratios compare per-operation p95 with a
clean C rebuild of exactly the same edited data, on the same host, serially, after
the same warm-up. Every checkpoint also measures the zero-layer wrapper against
that exact raw C reader. Raw samples reproduce all per-operation p95 and A/A
ratios. A/A is not subtracted and the 1.10 bound is unchanged. Read serving is
format-independent by construction; write/seal, open and bytes distinguish the
codecs. The worst operation determines the 10% crossing. No observed
crossing means a lower bound only; the threshold remains unresolved. A crossing
at the smallest size supplies no passing threshold. The selected checkpoint must
also have a positive delta/base byte ratio at most 2. The approved Design C target
contract says "Change-layer delta bytes before compaction at most twice
sealed-store bytes" (Prime plan `agent/plans/gortex-design-c/PLAN.md:765–766`).
Line 790 applies that contract to Checks 1 to 6. This ratio counts all layer bytes
through the selected checkpoint over the delta repository's sealed base bytes.
If both forms pass, the lead selects the simplest form; code does not invent that
judgment. A passing form
cannot be selected as the sole pass while the other form is blocked or absent.
A measured miss of the other form permits that selection.
CPU maxima and visibility p95 apply to the prefix that the chosen threshold or
lower-bound checkpoint would serve. Later-chain edit maxima remain diagnostics,
not failures of an earlier compaction regime. Report-level `MaxEditCPU` and
`Visibility` describe the whole measured chain and are diagnostic. If no read
regime passes, the edit-bound user-stop rule uses the smallest completed
checkpoint. Raw per-edit samples preserve both.

A base's hashes and ownership-key metadata are admitted once per run owner.
Later opens still validate the seal digest, exact identity, safe paths and every
recorded size. Sealed files must remain immutable; same-size external tampering
is detected on fresh adoption, not promised on every trusted read. Content chunk
tombstones are explicit. No edit scans C text bodies to discover its exclusions.
The ownership-key file's construction, bytes and resident indexes are counted.

## CI build and checks (not Mac commands)

From this module, with the repository-selected Go 1.27.1 environment:

```text
GOWORK=off GOFLAGS=-mod=readonly go test -p=1 ./...
GOWORK=off GOFLAGS=-mod=readonly go vet -p=1 ./...
GOWORK=off GOFLAGS=-mod=readonly go build -p=1 -o <outside-checkout>/linkdelta .
```

Build the accepted `study/storage/harness/cmd/limited-command` outside timing too.
The module's sums are seeded from pinned cached module sums. No local Go command
resolved them. Read-only CI must fail visibly if the manifest still needs work.

## Measurement entrypoint

The lead must first obtain review, fresh CI and approval for the exact run.
`measure.sh` is non-interactive and has no defaults:

```text
study/storage/checks/linkdelta/measure.sh \
  /absolute/bin/linkdelta /absolute/operator-config.json CONFIG_SHA256
```

Every field below is required. Replace placeholders and `null` with the approved
values; this example deliberately cannot run as written. Memory values are bytes.
`link_memory_limit_bytes` is the fixed 1.5 GiB target, not a run approval.

```json
{
  "schema": "linkdelta-measure/v1",
  "base_layout": "c-parquet",
  "data": "/absolute/verified-tier",
  "out": "/absolute/fresh-run-output",
  "launcher": "/absolute/bin/limited-command",
  "cgroup_root": "/sys/fs/cgroup/approved-delegated-root",
  "source_revision": "<reviewed-full-source-OID>",
  "approval": "/absolute/run-approval-receipt",
  "approval_sha256": "<approval-file-sha256>",
  "delta_repo": "<repository-ID-in-tier>",
  "delta_sizes": [],
  "query_repeats": null,
  "wall_seconds": null,
  "memory_limits_bytes": {"runner": null, "checker": null, "engine": null},
  "link_memory_limit_bytes": 1610612736
}
```

The controller, preparation/checkers and engines enter separate accepted-launcher
cgroups. Runner/checker limits remain explicit. The link engine gets the exact
1.5 GiB hard cap. The controller deadline covers the complete serial run.
All workers stop 60 seconds before it, leaving a fixed finalization reserve.
The 60 seconds is an unmeasured engineering allowance. Finalization hashes the
whole tier input tree and all finalized outputs. It can exceed the allowance and
lose `result.json` at the controller deadline. Retained progress and the outer
terminal receipt remain necessary; no full-tier completion time is guaranteed.
A wall budget of 60 seconds or less is refused; the outer approved cap does not
increase. The launcher verifies its own executable, command, hostname, policy
and input hashes. `inputs.json` additionally binds the worker executable, source declaration,
config, data manifest and approval hashes. The lead must verify that the source
declaration matches the reviewed build receipt; a supplied OID alone is not proof.

A cleanup owner must remain **outside all worker cgroups**. Use the accepted CI
job's existing owned-root cleanup. If the controller is killed, it cannot clean
sibling engine scopes itself. Do not run this script without that external owner.
The launcher records each scope's terminal state, peak, OOM events and removal.
Only a measured link memory miss permits the package-decomposition branch.
Semantic partial/parity misses do not. A non-OOM link crash is blocked, not a
measured miss, and cannot trigger fallback. Check 3 reports collision counts and
fact shares beside its result; missing collision evidence blocks it. Check 4
still runs after a contained Check 3 failure or semantic miss. Arrow and Parquet
also proceed independently after contained, cleaned failures. Lost approval,
cancellation, worker deadline expiry, invalid bindings or uncertain cleanup stop further launches. These are independent measurements,
not retries. The controller rechecks the approval and deadline before each launch.

Keep `run-receipt.json` and the controller scope's `receipt.json` as terminal
records. `result.json` contains target values/statuses/branches, all component
receipt hashes and all result/output hashes. A controller error preserves partial
outputs and labels uncompleted targets blocked. Each completed edit and checkpoint
is appended and synchronized immediately in `delta-*/edits.jsonl` and
`delta-*/checkpoints.jsonl`; completed checkpoints also synchronize their answer
spool. Progress synchronization is outside the timed operation. A worker deadline
keeps these records and leaves the controller its finalization reserve. Controller
OOM or storage failure can still prevent the inner summary; the outer terminal
receipt remains authoritative. Checker crashes, OOM and incomplete reports block
verification. Only a structured semantic mismatch report makes it a miss.
`Complete` records collection,
not acceptance: inspect every target status and branch before claiming a pass. Native rank differences live in `native-rank-differences-*.jsonl`, with
counts, match rate and hashes in `verify-*.json`. Protected input hashes are checked
on every observed controller exit and again by the outer terminal owner. If a
killed controller never produced its frozen snapshot, the terminal record says
`not checked`, not unchanged. The live controller log is excluded from the inner
output hashes. The outer terminal receipt owns its final hash after launch/close.
Engine process RSS and cgroup peak (including charged cache) have separate labels.
Nothing is removed or retried after an uncertain effect.

## Remaining limits

Full catalog transactions, cross-process leases, interrupted retirement recovery,
linked multi-repository publication, daemon paths, language invalidation and
production watcher timing are untested. The lifecycle is a single-process study
owner around the accepted reference catalog, not a product contract refactor.
This check does not establish any base B/C selection or whole-product speedup.
