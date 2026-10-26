# Frozen v8 runner preparation

Scope: Linux smoke P/R and baseline K/P/R, using fresh owned state and
one start/restart per set. No exports, product changes, local execution,
retries, retracks, or binary rebuilds. The lead owns runtime acceptance.

## Given/When/Then scenarios (before implementation)

- Given the exact frozen binary and all 23 full public clones at their recorded
  tags, when a clean smoke/baseline attempt receives each matching lead go,
  then it tracks each member once, restarts once, verifies ready/idle and
  active whole-index node coverage, records counts/bridges/timing/RSS/bytes,
  and stops its owned daemon and socket.
- Given a changed binary, commit, tag, shallow clone, missing member, duplicate
  member, escaped/symlinked path, existing socket/PID, or wrong daemon identity,
  when preflight runs, then it rejects before tracking or adopting that state.
- Given an interrupted attempt directory, when invoked again, then it refuses
  replay and preserves the partial files. The enclosing driver alone may reuse
  verified completed receipts; this runner never repeats an attempt.
- Given a mismatched/missing go or an expired block/set deadline, when waiting
  or running, then no unapproved launch occurs, partial evidence remains, and
  the receipt is blocked/failed rather than complete.
- Given a stale/history-only generation or a same-sized wrong node set, when
  completion is checked, then it fails exact node coverage. At most one native
  whole-index trigger may repair a missing current generation.
- Given stopped clean semantic rejection, when returning to the driver, then
  clean_handoff is true with a non-success result. Given an owned survivor,
  socket, wrong daemon, or cleanup failure, then clean_handoff is false so A9
  is required. Unknown processes are never signalled or removed.
- Given P's generated-only dependency pairs, when reporting coverage, then
  they stay separate from runtime pairs. Given R history, then v4 is used and
  no v5 R result is invented. Every historical count difference is listed;
  causal explanation remains a lead source-review task, not a guessed label.

Shared-code callers: these are new standalone files. The existing driver and
batch B evidence remain unchanged. Updates, removals and re-adding repositories
are excluded because this method permits fresh tracks and one unchanged restart
only. Input duplication, alias/symlink paths, ordering, rejection, interruption
and partial output are covered by tiny synthetic tests.

## Staging and review

The setup owner stages full public clones at
`/work/inputs/batch-b/<K|P|R>/<label>` and the binary at
`/work/inputs/v8/gortex-21baab3b-linux-amd64`. These are pod-only paths.
Every initial/restart request is in `/work/control/v8-go/`. The lead must tell
pi-gortex the exact request and write a matching `.go.json`; copying the request
and adding `lead_go: true` and `pi_gortex_notified: true` is the protocol.
Never pre-authorize future requests. Requests bind a fresh random nonce.

Precedents: batch B `scripts/gortex_op.py`, `commands.json`, completion and
read-only count receipts; the existing pod driver's receipt contract.

Verification: file inspection and py_compile only on the Mac. Unit tests are
written for Linux CI and are not run locally. No runtime success is claimed.

## Delivered files and integration

- `run.py`: 454 lines; stdlib-only Linux runner. The lead approved about
  450 runner lines without narrowing evidence. Fresh output roots are
  `/work/smoke` and `/work/baseline`. It reserves 60 seconds inside the block
  for shutdown and result hashing. Each set also retains its 45-minute bound.
- `test_run.py`: 192 lines of tiny synthetic SQLite, approval, identity,
  deadline/partial-output, and owned-process cleanup fixtures. No Gortex binary
  is launched. Linux CI is required; these tests were not run on the Mac.
- `inputs.json`: public K/P/R identities, declared/runtime pair scopes and
  historical v4/v5 count records. All 23 identities match batch B; R has no v5.
- Local bindings: `bindings/smoke.json` and `bindings/baseline_a7.json` in the
  lead's pod-run directory. Both argv and binding `source_pin` retain `LEAD`
  for the final accepted commit. The configuration pin is the runner file's
  SHA-256; the input pin is `inputs.json`'s SHA-256.

Job receipts use `/work/results/smoke/v8-smoke/receipt.json` and
`/work/results/baseline_a7/v8-baseline/receipt.json`, outside the driver's
`/work/aux` supervisor receipts. Approval request names include the mode to
avoid reusing smoke approvals for baseline: for example,
`/work/control/v8-go/smoke-P-start.request.json`. A baseline launch requires
both a completed clean smoke receipt and matching source/configuration/input
pins. The enclosing driver owns completed-result reuse; an existing attempt
root always refuses replay.

The foreground daemon remains in the driver's owned process group. The runner
uses its actual Popen child for stop, never a PID taken from an untrusted file.
It checks the socket/PID file and same-user processes carrying its exact socket
environment marker after stopping. Unknown processes are never signalled.
Before starting, the lead must verify the staged receipt's D6 source/parents/
ancestor composition against the frozen receipt. Binary SHA-256 is checked on
pod before any binary call; version and daemon PID/socket/version are checked
at runtime. Requests also name resolver semantics 6 and the frozen source.

Native track command receipts hold wall times. `native-times.json` retains
warmup/analysis log durations, completion errors, final load and stopped backend
file sizes. `rss.json` holds five-second daemon VmRSS/load samples with PIDs.
Read-only SQLite backup copies prove active generation coverage by exact node
ID sets, not only counts or completion logs. The final counts retain every
historical difference. The lead must explain these from inspected source;
the runner does not invent causes or make cross-platform speed claims.

The setup owner's staging receipt still needs all 23 public clone identities,
full history and the accepted frozen-build composition. The runner verifies
the used clones' HEAD/tag/full-history/clean state before each set's start.
No source clone is edited; run-owned clones use `--no-hardlinks` and lazy fetch
is disabled. No export, dependency install, daemon, model or paid work ran here.

### Exact CI step for the existing Ubuntu 24.04 workflow

```yaml
- name: Frozen-v8 tiny Linux fixtures without Gortex
  shell: bash
  run: |
    set -euo pipefail
    python3.12 --version | tee "$RUNNER_TEMP/storage/ci-v8.txt"
    python3.12 -m unittest discover -s study/storage/pod-v8 -p test_run.py -v 2>&1 \
      | tee -a "$RUNNER_TEMP/storage/ci-v8.txt"
```

The existing workflow already uploads `ci-*.txt`; no workflow edit was made.

### Local file-only verification

- `python3 -m py_compile study/storage/pod-v8/run.py study/storage/pod-v8/test_run.py`
  passed. This compiles syntax only; it does not import or execute the runner.
- Source/test lines stay at or below PEP 8's 79-character code limit.
- JSON bindings have only the driver's required job fields. Both allocations
  fit their block exactly, with one serial job per block.
- Runtime daemon behavior, synthetic tests and pod measurements are not checked
  locally. The Mac execution hold remains in force.
