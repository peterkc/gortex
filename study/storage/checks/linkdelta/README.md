# Design C checks 3 and 4 (throwaway)

This module measures name/scope linking and sealed Arrow/Parquet deltas over the
shared C Parquet base in `../base`. It is not a production adapter or proof of
daemon parity. Input is the harness `gxstudy-v2` format. Internal facts and row
envelopes are derived scratch, not a replacement input format. Do not run Go or
measurements on the Mac.

## Scenarios written before implementation

- Given per-file declarations and facts without target IDs, when the symbol
  table links them, then full edges, payload and duplicate multiplicity equal
  the clean input. Nested scopes and reversed enumeration preserve the result.
- Given ambiguous aliases or missing target declarations, when linking runs,
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
reader. A JSON-row base exists only for tiny contract tests; the measurement CLI
cannot select it. Missing shared C row/text APIs visibly block the run.

The overlay uses C lookups and both adjacency directions. Typed C streams feed
exact vector merging. Native search uses a Bleve alias of base plus deltas.
Each member filters only its superseded document IDs, so replacement IDs in a
newer member remain visible. Ordinary native rank differences are diagnostic,
not quietly converted to slower formula queries. Full delta-marker coverage is
still an exact correctness requirement.

Check 3 removes `edge.to`, then records target qualified name (or name), target
repository and file scope from the declaration. Linking builds the lookup over
all declarations. Dangling or ambiguous facts become witnesses, and their share
is reported. File scope is stronger than real extractor knowledge in some
languages. Imports, overloads, method sets and language resolver rules are
untested. Successful synthetic parity is not production-linker completeness.
The fallback keeps only one source package's consumed target keys at a time,
rescanning declarations and facts to trade CPU/I/O for bounded memory.

The delta batch alternates the harness's `runner.MakeChange` mutation and the
original complete output of that same file. The two independently prepared clean
disk sources cover every odd/even checkpoint. This measures edit churn and
physical chain length, not growth in distinct changed files. No tier query is
dropped. Additional delta identity and exact text-marker probes join the workload.
Warm-up and measured answers are spooled and checked in another capped process.
Repeated complete checks reuse an on-disk memo keyed by state, query and answer.
Native ordered top-k parity pairs use disk scratch, not a retained answer corpus.

The one-file CPU and visibility window includes delta/text construction, seal,
fsync, catalog verification/CAS, pinned open and a file read. It excludes source
extraction and daemon/watch routing. Read ratios compare per-operation p95 with a
clean C rebuild of exactly the same edited data, on the same host, serially, after
the same warm-up. The worst operation determines the 10% crossing. No observed
crossing means a lower bound only; the threshold remains unresolved. A crossing
at the smallest size supplies no passing threshold. Delta serving bytes must
also fit the 2x bound at the proposed threshold/lower-bound checkpoint. If both
forms pass, the lead selects the simplest form; code does not invent that judgment.

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
1.5 GiB hard cap. The shared absolute deadline covers the complete serial run.
The launcher verifies its own executable, command, hostname, policy and input
hashes. `inputs.json` additionally binds the worker executable, source declaration,
config, data manifest and approval hashes. The lead must verify that the source
declaration matches the reviewed build receipt; a supplied OID alone is not proof.

A cleanup owner must remain **outside all worker cgroups**. Use the accepted CI
job's existing owned-root cleanup. If the controller is killed, it cannot clean
sibling engine scopes itself. Do not run this script without that external owner.
The launcher records each scope's terminal state, peak, OOM events and removal.
Only a clean completed link attempt or a contained, cleaned link OOM permits the
predeclared package-decomposition branch. Infrastructure failure is not a retry.

Keep `run-receipt.json` and the controller scope's `receipt.json` as terminal
records. `result.json` contains target values/statuses/branches, all component
receipt hashes and all result/output hashes. A controller error preserves partial
outputs and labels uncompleted targets blocked. A target miss is not protocol
success. Native rank differences live in `native-rank-differences-*.jsonl`, with
counts, match rate and hashes in `verify-*.json`. Protected input hashes are checked
again at the end. Nothing is removed or retried after an uncertain effect.

## Remaining limits

Full catalog transactions, cross-process leases, interrupted retirement recovery,
linked multi-repository publication, daemon paths, language invalidation and
production watcher timing are untested. The lifecycle is a single-process study
owner around the accepted reference catalog, not a product contract refactor.
This check does not establish any base B/C selection or whole-product speedup.
