# D3 model runner preparation

The worker owns only `study/storage/pod-models/` and the model binding fragment.
The goal is three complete, hash-bound model results within 160 pod minutes.
The lead owns Linux CI, lock acceptance, staging, commits and pod execution.
No Mac model loading, dependency resolution, scorer execution or tests run here.

## Given/When/Then scenarios (before implementation)

1. Given the three evaluator-pinned models, when inputs are embedded, then query
   prefixes apply exactly once, documents have none, and fp32 L2 vectors retain
   original row order. CLS and last-attended-token pooling follow model settings.
2. Given variable token lengths and left/right padding, when batches are made,
   then caps include special tokens, truncation counts remain explicit, and
   pooled values exclude padding. Length grouping never changes output IDs.
3. Given missing/duplicate IDs, empty texts, wrong hashes/counts or an unsupported
   model/code pin, when preparation begins, then it refuses before GPU loading.
   Opaque symbol IDs are not paths. Escaping/symlinked paths refuse at the root.
4. Given complete model outputs or interrupted partial outputs, when restarted,
   then the runner refuses overwrite. The driver's verified complete-result
   resume is the only reuse path. Partial artifacts remain for lead review.
5. Given a deadline during a later model, when the supervisor kills/reaps its
   process group, then earlier models retain independent complete receipts.
   An unfinished model has no success receipt. Completed dataset files persist.
6. Given matching successful calibration at 69.369%, when vectors finish, then
   every mixed/TS-metadata/TS-body set is scored with the existing evaluator.
   Given failed/missing/mismatched calibration, vectors still finish, but metrics
   and ranking stay dependency_blocked with the reason, never a reduced sample.
7. Given nonfinite/zero/wrong-dimension embeddings, when a batch finishes, then
   it fails visibly and publishes no completed vector set or model receipt.
8. Given a calibration receipt with changed/missing metric bytes, when checked,
   then scoring remains blocked. Input/source/config pins and denominator must
   agree, not just a state label.

No existing code/caller changes. Removal/re-adding is a fresh job, not a cache
migration. No daemon or concurrent writers are part of this runner.

## Evidence and remaining checks

Source preparation is ready for lead review. Tests run only in Linux CI.
Model/runtime/memory/cost and rankings remain unmeasured until the one pod.

## Implementation and precedent

The runner follows `study/storage/neon-eval/evaluate.py` for pins, source/input
hash checks, exact cosine normalization, exclusive JSON writes and receipts.
It imports that sibling evaluator without importing torch or transformers.
Only GPU execution lazily imports those packages. Existing scorer tests and
model configurations remain unchanged. No Gortex graph or memory call was
made: this task's explicit file-only Mac restriction excludes local services.

For each model the runner independently embeds all five input files. It writes
three combined fp32 NPY matrices: mixed metadata, TypeScript metadata and
TypeScript body. Each matrix stores documents first, then queries, as required
by the evaluator. Every set has the complete ordered document/query IDs in a
separate hashed JSON. No sample/label filtering or cross-sample quality claim
is added. Length-sorted batches restore the original row order.

BGE runs first (40 minutes), Qwen second (60), GTE third (60). These are hard
suballocations totaling 160 minutes, not measured duration predictions. The
short-context model runs first to obtain a complete matched result early.
The supervisor owns absolute deadlines, process groups and reserves; the runner
adds no timer, retry or second observer. A completed model has its own receipt,
independent of later model failures. Deadline interruption leaves partial
artifacts without a completion receipt. No partial attempt is replayed.

Each admitted model records model/tokenizer revision, code revision, prefixes,
pooling, dimensions, L2 normalization and cap through the evaluator's recipe.
Raw and effective token totals, truncation row counts, maximum raw length,
batch count, padding side and batch limits are recorded per input. Inputs use
the recovered JSONL hashes in `configs/`. Qwen uses left padding and last
attended-token pooling. BGE/GTE use right padding and CLS. A token budget of
8,192 limits padded batches; a single longer Qwen input may exceed that budget
up to its documented 32,768-token cap. No adaptive cap, quantization, fallback
model, mixed precision, retries or instruction tuning is used.

GPU/runtime output records total model seconds, model-load seconds, CUDA runtime,
cuDNN, library versions, A10 device name and PyTorch peak allocated/reserved GPU
bytes. These are framework allocator peaks, not a claim about total device or
host RAM. Cost is model wall time at $1.29/hour and excludes uv setup and input
validation. Driver receipts retain whole-job time/cost, including environment
setup. Actual driver compatibility and performance remain unverified; missing
GPU/driver support fails visibly, without installing or changing a driver.

The calibration check requires the accepted source/config/input pins, completed
state, exit 0, clean handoff, actual metric hash, 333 queries and 69.369 percent.
A missing/failed/mismatched calibration leaves all vectors available and marks
rankings `dependency_blocked`. Successful calibration invokes the existing
`evaluate.py score` CLI separately for every set with the complete v1 schema,
corpus-order ties, all hashes and full row counts. Its four metrics and every
rank remain in the hashed score outputs. No target is imposed on D3 quality.

Pre-output identity/data rejections publish no receipt and return nonzero.
Handled compute/scorer failures retain a `failure.json` and `checks_failed`
receipt with exit 1. The runner has no detached child: scorer calls are joined;
the owning supervisor reaps the process group before accepting handoff. Other
unexpected failures remain incomplete and cannot pass driver validation.

## Package choice and evidence

The separate Python 3.12 uv application pins NumPy 1.26.4. The `gpu` extra pins
PyTorch 2.7.1+cu126, transformers 4.51.3 and sentence-transformers 4.1.0.
Transformers meets Qwen's documented >=4.51.0 and GTE's >=4.36.0 floor;
sentence-transformers accepts transformers >=4.41,<5 and torch >=1.11.
The explicit torch-only index is `https://download.pytorch.org/whl/cu126`.
Its published cp312 manylinux_2_28 x86_64 wheel is compatible with Ubuntu 22.04's
platform baseline. This is package metadata evidence, not an executed GPU check.
Other packages use PyPI. Resolution is restricted to Linux x86_64.

Only NumPy installs for CI tests: `gpu` is not selected. Lock resolution includes
the optional GPU dependency metadata but downloads no model weights and executes
no inference. `sources/manifest.json` records the public package metadata/wheel-index
GET URLs, Los Angeles fetch times and SHA256. The three package JSON files and
the CUDA wheel-index HTML are the exact saved responses. Existing revision-
addressed model cards/configs were read from A2's source bundle; the frozen
runtime recipe is `evaluate.py:MODELS`. GTE alone permits remote code, and only
the exact model/code repository and revision pair passes the literal guard.
Tokenizer loading never permits remote code.

`uv.lock` is deliberately absent. The lead must resolve it in Linux CI, retrieve
`ci-models-lock.txt`, review/copy it as `uv.lock` here, and bind its bytes in the
accepted setup/source receipt. Do not resolve it on the held Mac. The pod uses
`uv run --frozen --extra gpu` with that lock. It intentionally does not pass
`--no-config`, so this explicit project's CUDA index/source mapping is honored.
HOME/cache paths remain the driver's isolated `/work` environment.

## Linux CI step

Insert `CI-step.txt` immediately after the existing evaluator step and before
artifact upload. It reuses that step's checksum-verified uv 0.13.5 executable;
it does not fetch or install a second toolchain. The existing upload wildcard
`ci-*.txt` includes the source/check receipt and generated lock bytes. Exact step:

```yaml
- name: D3 inference contracts on tiny NumPy fixtures only
  shell: bash
  env:
    OPENBLAS_NUM_THREADS: "2"
    OMP_NUM_THREADS: "2"
    MKL_NUM_THREADS: "2"
    UV_PYTHON: python3.12
    UV_PYTHON_DOWNLOADS: never
    UV_CACHE_DIR: ${{ runner.temp }}/storage/uv-cache
  run: |
    set -euo pipefail
    uv="$RUNNER_TEMP/storage/evaluator-tools/uv-x86_64-unknown-linux-gnu/uv"
    project="study/storage/pod-models"
    "$uv" lock --project "$project"
    "$uv" lock --check --project "$project"
    cp "$project/uv.lock" "$RUNNER_TEMP/storage/ci-models-lock.txt"
    "$uv" run --frozen --project "$project" python --version \
      | tee "$RUNNER_TEMP/storage/ci-models.txt"
    sha256sum "$project/pyproject.toml" "$project/uv.lock" \
      "$project/run_models.py" "$project/test_models.py" \
      study/storage/neon-eval/evaluate.py \
      | tee -a "$RUNNER_TEMP/storage/ci-models.txt"
    "$uv" run --frozen --project "$project" python -m unittest discover \
      -s "$project" -p test_models.py -v 2>&1 \
      | tee -a "$RUNNER_TEMP/storage/ci-models.txt"
```

## Local-only pod staging handoff

The binding fragment is `tmp/pod-run/bindings/model_a4.json` in the local research
hub. The lead merges it with sibling fragments; it is not a complete schedule.
Replace each job's `source_pin: LEAD` and argv `--source-pin LEAD` with the final
reviewed commit. The fragment deliberately cannot pass driver preflight before
that replacement. Model configs currently bind the already accepted calibration
source `89b5aed457b25616b1345a82291c1cff1a1dd195` and its existing input/config pins.
If that calibration binding changes, update its expected pins in all three model
configs and regenerate each config SHA256 in the fragment and argv.

During approved setup, stage this complete application, configs and CI-resolved
lock under `/work/control/pod-models/`. Stage the exact evaluator source under
sibling `/work/control/neon-eval/` and all five JSONL files under
`/work/data/evaluator/`. The runner verifies its own and evaluator's SHA256,
config bytes, input hash mapping and every input file before GPU loading.
The config pin is SHA256 of exact config JSON bytes. The input pin hashes
compact sorted-key JSON of basename -> file SHA256. Do not change paths after
hashing a config. Source edits require refreshed runner/config/binding hashes.

## Verification boundary

All scenarios have focused NumPy/stdlib fixtures except forced process-group
termination, whose supervisor behavior belongs to the existing driver tests.
The output-reservation fixtures check both partial and completed replay refusal
and preserved bytes. No existing test was deleted. Shared scorer behavior and
all existing callers remain unchanged.

Allowed local syntax compilation of both Python files exited 0 with empty
output using Python 3.11.15. This is not Python 3.12 or execution evidence.
Manual source review found no line over 79 characters. Tests, lock resolution,
model compatibility, inference, GPU memory, runtime, cost and actual rankings
are **not checked** here. Linux CI and the same authorized pod must supply them.
No Go, daemon, pod, paid action, commit, push or other worker's file was touched.

Final source sizes: `run_models.py` 348 lines;
`test_models.py` 213 lines, 13 statically counted test methods.
Final runner SHA256: `6637fd26ad1bfd99cc713187f3c9326f681a38ce4010a7d89bfbd58e3d87221c`.
Evaluator SHA256: `43972a8a6d8b13133b70491343a5c33fface8bf8d4ca156cfefbd7997bd53a4b`.
Config hashes and binding arguments match by static comparison. The complete
binding sums to 160 minutes. This comparison runs no project code or tests.

Receipt-path correction: application receipts live at
`/work/results/model_a4/<job id>/receipt.json`, not the supervisor's reserved
`/work/aux/<job id>/complete.json`. Calibration is now
`/work/results/calibration/calibration-coderank/receipt.json`; its config and
identity pins are unchanged. All three model config hashes and binding argv
were refreshed. Runner code and the NumPy-only CI step are unchanged.
