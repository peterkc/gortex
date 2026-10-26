# Bounded CodeRank CPU cost preparation

## Scope and status

Source preparation only. The lead owns review, CI, commits and pod acceptance.
No tokenization, model/library loading, tests or measurements ran on the Mac.
A1 and A2 remain accepted and closed. This review round owns this directory
and the adjacent driver folder. The lead owns all shared binding updates.
The total pod block is at most 60 minutes.

## Given / When / Then (written before implementation)

1. Given pinned public rows and tokenizer files, when pod precompute runs, then
   metadata8192/body512/body2048/body8192 retain all 5000/4842 rows, while
   query8192 retains all 333 rows with one query per batch. Token IDs, row IDs,
   unpadded counts, caps, batching and hashes remain linked.
2. Given a cap-specific 200-row token file, when Python ORT creates the reference
   and Go embeds it, then the gate checks every matched vector at cosine >=0.9999.
   One opposite/nonfinite/missing vector rejects the configuration and cost block.
   An acceptable mean cannot hide one failing row.
3. Given a gate-passing configuration, when timings run, then each fresh process
   receives the pinned external export, no-prepack, CLS, inter-op 1, exact token
   policy and thread count. All 20 throughput and five query cases stay visible.
4. Given time for warm-up and three measured repeats, when a case finishes, then
   its full denominators and three repeats are completed. Less time leaves an
   explicit partial/skipped case and actual repeat count, not full coverage.
5. Given missing or changed inputs/library/source/check receipts, when execution
   starts, then it records only a permitted identity/library/check skip and no
   accepted timing. A worker failure becomes partial after verified reaping,
   not a skip. Unverified teardown requires an unclean handoff.
6. Given an existing partial result directory, when the command is repeated,
   then it refuses replacement/replay and preserves all prior bytes. Only the
   parent driver may reuse verified complete receipts with matching identities.
7. Given an expiring absolute driver deadline or a non-returning probe, when the
   guard expires, then no later probe starts. Available output remains partial,
   all cases remain listed, and A7/deletion time is not borrowed.
8. Given a successful tiny probe fixture, when the CLI produces its receipt,
   then pins, exit/state and all result-file hashes satisfy the existing driver.
   The job receipt is outside the driver's own auxiliary receipt directory.

## Current evidence and decisions

- Reuse `study/storage/onnx/main.go` at accepted preparation head `89b5aed4`.
  Its flags, JSON counters, CLS pooling, inter-op 1 and Linux VmHWM are unchanged.
- Use the PLAN's O2-derived external fp32 graph and weights, not a new export.
- Tokenization runs on the pod and counts inside its 60-minute allocation.
- Do not reuse saved Mac vectors: their available records do not establish all
  required token/cap/export/input identities for this complete matrix. Generate
  a Python ORT 1.31.0 reference on the first 200 exact rows of each policy instead.
- Reuse the original tokenizer/batch recipe: Transformers 4.45.1, tokenizers
  0.20.3, fixed first 200 then stable length order, <=16 rows and batch*length^2
  <=4,000,000 unless one long row; longest-in-batch right padding. Queries remain
  single-row batches in original order. Record each policy separately.
- Gortex localization/recall was not run under the file-only Mac hold. Evidence
  here comes from current source and the approved PLAN, not graph/memory claims.
- The driver's prospective and live admission remain authoritative. Do not
  allocate a pod or launch this binding from this preparation task.

## Failure outcomes

- A crashed, killed, timed-out, reportless or malformed reference/probe raises
  `FatalWorker` locally. After reaping verification, the block writes a partial
  receipt with a reason. It never changes that failure into a clean skip.
- Native exit 1 is accepted only with a partial report naming the probe's
  `runner guard exceeded` outcome, consistent row/vector counters, token hash
  and runtime recipe. The case stays partial; the next case may run inside the
  same deadline. It never becomes a complete repeat or a full-symbol cost.
- A missing live child check or failed reaping verification writes an unclean
  partial receipt. The driver owns full process-group verification and A9.
- Complete matched cosine rejection, library identity failure, a recorded
  normal successor-check failure, refused admission or recorded input/export
  mismatch retain the five permitted skip codes. Missing required input/source
  files and unexpected input/config/import errors instead remain partial.

## Runtime recipe and provenance

The configuration is `cpu-cost-v1.json`. Its input manifest binds all eleven
files: the three recovered JSONL inputs, five original tokenizer/config files,
the external model graph and weights, and the original `artifacts.json`.
On the pod stage them under the exact `data/cpu-cost/` paths in that manifest.
Tokenizer files come from the accepted `tmp/onnx/models/coderank/source/`.
Model files come from `tmp/onnx/models/coderank/external/`. Do not regenerate them.

The exact Linux CPython 3.12 ORT wheel is pinned by URL and SHA-256 in the config
and as a direct dependency in `pyproject.toml`:

- Version: `onnxruntime==1.31.0`.
- Wheel: `onnxruntime-1.31.0-cp312-cp312-manylinux_2_28_x86_64.whl`.
- Wheel SHA-256:
  `5e129d6c56abd53e659cb70f00a108d6824086470ff99c2e47a82e5786563db3`.
- Metadata source: <https://pypi.org/pypi/onnxruntime/1.31.0/json>.
  The exact response, URL, retrieval time in PDT and SHA-256 are in `sources/`.
  This was the only network request. No wheel or weights were downloaded here.

The shared-library member is `onnxruntime/capi/libonnxruntime.so.1.31.0`.
Its individual bytes/hash are not available from PyPI's release metadata alone.
The runner verifies the installed member against the wheel's SHA-256 RECORD
entry, requires its path under `/work`, and records the member SHA-256 in
`library.json` BEFORE Go loads it. The reviewed uv lock and direct wheel pin
carry the wheel-to-member provenance. This is a pinned container/member chain,
not a claim that the individual Linux `.so` was fetched or inspected on the Mac.
Missing version/member/RECORD/hash yields the permitted library skip.

The runtime uses Transformers 4.45.1, tokenizers 0.20.3, huggingface-hub 0.36.2
and NumPy 1.26.4, matching the original tokenizer recipe. No Torch or remote
model implementation is loaded. Precompute uses local files only. For each of
five policies, `tokens-manifest.json` links source-order row IDs, unpadded token
counts, unique symbols and the full/gate token-file hashes. Each first-200 gate
file uses exactly the full-run token batches before the 200-row boundary.
Python ORT references use the same external graph, CLS then L2, CPU provider,
intra-op 16/inter-op 1 and disabled prepacking. References are reused only
within their exact token policy; every Go thread/policy gets its own gate.

Each case's first-200 gate probe is its warm-up (`-repeat 0`). Every measured
repeat uses the full corpus in a fresh Go process (`-repeat 1/2/3`). Native
p50/p95 are nearest-rank over 333 single-query calls in EACH completed repeat;
no pooled p95 is invented. Raw reports retain vectors for reconciliation.
`matrix.json` omits those large vectors but preserves the native counters,
load time, RSS, rates and query statistics. Raw probe
`minutes_per_10k_symbols` is row-based; the matrix renames it to
`minutes_per_10k_text_rows`, then computes a separate unique-symbol estimate
only for completed document repeats. It makes no full-symbol cost estimate
from a partial corpus or query-latency run.

## Deadlines, failures and resume

The existing driver remains the process-group owner. It starts job `cpu-cost`,
then writes `/work/aux/cpu-cost/process.json`. The runner requires that receipt's
absolute monotonic deadline; a missing receipt gives a partial outcome.
It does not start a new 60-minute clock. It subtracts 30 seconds for final
hashing/receipt work. Dependency installation, hashing, native probe build,
tokenization, references, warm-ups and measured repeats all remain inside the
parent's single 60-minute job allocation. The parent's absolute guard is still
authoritative if inline tokenization or finalization cannot return in time.

Remaining time is divided among remaining planned cases. Each probe receives
its native soft guard and a subprocess hard timeout. A hard timeout kills and
waits for the direct worker. The runner then verifies its remaining children
before writing the receipt. The driver separately verifies its private group,
including adopted descendants. No failed process is retried.

A native soft-guard partial case permits the next case when time remains. A
reduced matrix, including zero completed cases, is partial with reason `time`,
not skipped. A later admission refusal cannot erase that attempted partial
outcome. Completed, gate-matched measured repeats remain explicit; the
`accepted_timings` flag does not claim complete matrix coverage. The repeat
estimate scales measured inference by planned rows / measured rows, but does
not scale session-load time. The first estimate uses the 200-row gate; later
estimates use the completed full repeat.

`planned-matrix.json` is written first. Therefore even an interrupted setup has
all 25 planned cases visible. Existing output directories are never replaced,
even if a receipt is missing. Only the accepted parent driver reuses verified
complete attempts; an ambiguous attempt requires lead reconciliation.

The job receipt is `/work/results/cpu_cost/cpu-cost/receipt.json`, outside the
supervisor's own `/work/aux/cpu-cost/complete.json`. All runner-generated files,
including the native probe binary, live beside the job receipt and are hashed
there. The receipt itself is hashed by the outer supervisor, not recursively
included in its own file map. Fatal unexpected exceptions do not emit a clean
receipt. This task adds no supervisor, daemon, workload overlap or pod rental.

## Checks and coverage

- Local syntax only: Python 3.11.15, `-m py_compile cpu_cost.py test_cpu_cost.py`,
  exit 0 with empty output. This does not validate target Python 3.12 or behavior.
- Python source/test line widths are <=79. No formatter/linter was installed.
- Synthetic tests cover the full 25-case inventory; padding/order/200 boundary;
  query batching; invalid/duplicate rows; per-input versus mean cosine rejection;
  the inclusive threshold; symbol versus chunk costs; direct-child deadline and
  reap; real subprocess probe arguments/identity; pinned skip receipts; and
  completed/partial-output replay refusal. They have NOT run on the Mac.
- No tests, uv lock/sync/run, tokenizer/scorer/model execution, Go build/test,
  Gortex call, daemon, weights download, pod or paid action ran locally.
- Native tokenization, the shared-library load, Python reference inference,
  full-corpus matrix and full runner completion need reviewed Linux/pod evidence.
- No shared implementation changed. Existing Go callers/pooling remain unchanged.
  Nested/escaped filesystem paths use resolve-and-containment checks; additional
  explicit symlink fixtures and an end-to-end timed-matrix fixture remain review
  items, not claimed coverage.

## Proposed Linux CI step (not installed)

Place this AFTER the accepted evaluator step, which already installs and
checksum-verifies uv 0.13.5 at the referenced tools path. The job is verified as
`ubuntu-24.04`. Its existing artifact glob collects the two new `ci-cpu-cost*.txt`
files. Lock resolution and tiny tests occur only in CI, not on the Mac. No model
or tokenizer execution belongs in this step.

```yaml
- name: CPU cost orchestration on tiny synthetic fixtures only
  env:
    OPENBLAS_NUM_THREADS: "2"
    OMP_NUM_THREADS: "2"
    MKL_NUM_THREADS: "2"
  run: |
    set -euo pipefail
    uv="$RUNNER_TEMP/storage/evaluator-tools/uv-x86_64-unknown-linux-gnu/uv"
    project="$RUNNER_TEMP/storage/cpu-cost-project"
    cp -R study/storage/pod-cost "$project"
    if test ! -f "$project/uv.lock"; then
      "$uv" lock --project "$project"
    fi
    "$uv" lock --check --project "$project"
    cp "$project/uv.lock" "$RUNNER_TEMP/storage/ci-cpu-cost-lock.txt"
    sha256sum "$project/pyproject.toml" "$project/uv.lock" \
      "$project/cpu_cost.py" "$project/test_cpu_cost.py" \
      | tee "$RUNNER_TEMP/storage/ci-cpu-cost.txt"
    "$uv" run --project "$project" python -m unittest discover \
      -s "$project" -p test_cpu_cost.py -v 2>&1 \
      | tee -a "$RUNNER_TEMP/storage/ci-cpu-cost.txt"
```

## Phase-1 review fixes

The current shared failure rule supersedes the prior fatal-to-A9 policy.
`FatalWorker` remains a local exception name, not a session-stop classification.
The original 16 tests remain. No test was deleted. The new cases cover:

- Worker crash, kill, timeout, missing report and malformed report each produce
  a hashed partial receipt after reaping. Both reference/probe helper variants
  remain covered by the earlier rejection tests.
- Unverified reaping produces `clean_handoff: false` and a process reason.
- A valid native soft-guard report is partial, not a worker crash; an unknown
  partial reason still rejects the report.
- A guard-partial case releases the next case. Zero completed cases and a
  subsequent admission refusal stay partial/time, never completed or skipped.
- Repeat admission scales inference by row count without scaling load time.

The exact method names are in `test_cpu_cost.py`. The lock pin now matches the
committed `uv.lock`; the runner pin matches the saved source. The lead updates
both config-pin occurrences in the shared binding, commits the fixes and runs
Linux CI. No Mac test or model execution is authorized here. Runtime acceptance
and the immutable checkout pin remain lead-owned.

## Source-only verification

- Production diff: 71 added + 18 removed = 89 changed lines against 792c1a97.
- Runner: 466 lines. Tests: 483 lines, 21 methods (five new; 16 retained).
- Python 3.11.15 py_compile passed with exit 0 and empty output. This is syntax
  evidence only. Linux/Python 3.12 runtime checks remain unrun locally.
- Config SHA-256: `7cd5b69612cd592a7ac25361946aa43ae91852800501ba4e8ba0710fcb435b94`.
- Committed lock SHA-256: `b49f13da093039d0645f48916061ac28a67541f4f9e89e2d6f3d18364386be3d`.
- No commit, push, shared binding edit, test, uv, Go build or model run occurred.
