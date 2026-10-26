# Bounded CodeRank CPU cost preparation

## Scope and status

Source preparation only. The lead owns review, CI, commits and pod acceptance.
No tokenization, model/library loading, tests or measurements ran on the Mac.
A1 and A2 remain accepted and closed. Only this directory and the `cpu_cost`
binding are owned here. The total pod block is at most 60 minutes.

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
   accepted timing. A fatal unexpected error remains fatal, not a clean skip.
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

## Reviewed failure scenarios (before the repair)

- Given a reference or probe that exits with an error, is killed, or finishes
  without a complete report, when the runner receives the outcome, then it
  raises a fatal worker error. No clean skip receipt is written; A9 owns cleanup.
- Given a completed 200-row reference and Go report with valid matching inputs,
  when one cosine is below 0.9999, then the recorded gate failure permits a skip.
- Given a missing/hash-mismatched library, a recorded failed successor check,
  refused time admission or recorded input/export mismatch, when the runner
  records that clean outcome, then only the corresponding skip reason is used.
- Given an invalid, partial or malformed worker report, when it is read, then
  it cannot stand in for the full evidence required by any permitted skip.

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
absolute monotonic deadline; a missing receipt fails closed with identity skip.
It does not start a new 60-minute clock. It subtracts 30 seconds for final
hashing/receipt work. Dependency installation, hashing, native probe build,
tokenization, references, warm-ups and measured repeats all remain inside the
parent's single 60-minute job allocation. The parent's absolute guard is still
authoritative if inline tokenization or finalization cannot return in time.

Remaining time is divided among remaining planned cases. Probes receive both
the native soft guard and a subprocess hard timeout. A native hard timeout
kills/waits the direct child, preserves logs and raises a fatal worker error.
It does not produce a clean skip receipt. Crashes, signal exits, missing reports,
malformed reports and incomplete worker reports are fatal too. The driver's
A9 path owns cleanup; planned-matrix.json alone is not a terminal result.
No hidden retries. Parent group cleanup
still detects/reaps descendants and refuses an unsafe handoff. Only complete, matching 200-row reference/Go reports with a cosine below
0.9999 permit a gate skip. Invalid/nonfinite/missing vectors are fatal, not gate
skips. A missing/hash-mismatched library, a recorded normal successor-check
failure, refused time admission or recorded input/export mismatch permits the
corresponding skip. Unknown worker outcomes never do. Case-level partial results are never
full-corpus results. A partial/reduced matrix gives the driver's permitted
`skipped/time` state; individual completed repeats remain in the raw evidence.

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

## Phase-1 completion

The lead accepted the original 363/184-line size after restart and requested
this failure-classification repair. The repair is written. It keeps worker
fatal errors separate from the five permitted, recorded skip outcomes. The
shared driver and binding were not edited. This is source preparation, not CI
or pod acceptance.

### Regression tests written for the repair

- `test_fatal_worker_crash_even_with_completed_report`: reference/probe exit 7
  remains fatal even if a completed-status JSON file already exists.
- `test_fatal_worker_killed_or_timed_out_and_reaped`: reference/probe SIGKILL and
  a hard timeout are fatal; the timed-out direct child is reaped.
- `test_fatal_worker_reportless_success`: reference/probe exit 0 without a report
  is fatal.
- `test_fatal_worker_incomplete_or_malformed_report`: missing status, partial
  status and malformed JSON cannot become clean skips.
- `test_skip_gate_only_complete_matching_reports`: only a complete, matched
  cosine rejection gives `gate`; missing reference vectors remain fatal.
- `test_skip_library_missing_or_hash_mismatch`: missing or changed pinned bytes
  give `library` without replacing the bytes.
- `test_skip_check_has_complete_recorded_failure`: a normal failed successor
  check writes a completed check record before giving `check`.
- `test_skip_time_admission_refusal`: expired admission refuses launch and gives
  `time`; a killed running worker is tested separately as fatal.
- `test_skip_identity_receipt_and_partial_replay_refusal`: an input-config pin
  mismatch gives a hashed clean `identity` receipt; replay still refuses.

The original combined deadline test was split across the time-admission and
fatal-kill/reaping tests. Its reaping coverage was retained, not deleted. The
probe-flag test now expects a fatal recipe mismatch instead of an identity skip.
The other original matrix/batch/row/cosine/symbol-cost cases remain unchanged.

### Remaining lead steps

1. Review this repair and run the existing proposed tiny Linux CI step. No Mac
   tests, tokenizer/model loads, Go execution or uv execution are permitted here.
2. Retrieve/review `ci-cpu-cost-lock.txt` as `uv.lock`, verify it in CI, and fill
   `source_files["control/pod-cost/uv.lock"]` with its SHA-256.
3. Refresh the binding's two config-pin occurrences after these source/config
   edits. This request was confined to `pod-cost/`, so the shared-hub binding
   was deliberately NOT changed. The new config SHA-256 is computed below.
4. Commit/review/pin the source and replace the binding's two source-pin `LEAD`
   values. Stage the exact public inputs/control files and use only the accepted
   driver's single-session admission. Actual library/reference/matrix work
   remains pod-only inside the same 60-minute allocation.

A1/A2 stay closed. No commit, workflow edit, pod action or paid action is part
of this repair.

### Repair verification

- Runner: 413 lines. Tests: 290 lines, 16 methods.
- Syntax only: Python 3.11.15, `-m py_compile cpu_cost.py
  test_cpu_cost.py`, exit 0, empty output. Target Python 3.12 and
  all runtime tests remain unrun locally.
- New config SHA-256: `98c80908218b1fdb1e68a556e598fba1f76348e3248f26bc6d28ac73d95e942a`.
  The runner source hash in the config matches the saved source.
- No test was run. No dependency resolution, model/tokenizer/reference/probe
  execution, Go build/test, Gortex call, pod action or publication occurred.
- Final files remain uncommitted for lead review and Linux CI.
