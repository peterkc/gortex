# A2 retrieval evaluator — local-only preparation

Source preparation is ready for lead review. Linux CI and the 69.369% pod
calibration have **not** run. No model was loaded or executed.

## Scope and implementation

The standalone Python 3.12 uv application uses NumPy 1.26.4 and stdlib unittest.
It scores saved matrices only. `model-config NAME` prints pinned settings for
the pod's inference owner; it does not download or run a model. Apply each
query prefix exactly once and use the same revision for model and tokenizer.
The later inference owner must record runtime, memory, cost and vector hashes.
No hosted Neon account, service-parity claim or provider integration is added.

The defining local precedents are `tmp/onnx/py/{common,run_embeddings,score}.py`
and its pyproject. Those files remain unchanged. The scorer normalizes in
float64, filters candidates by repository, collapses chunks using their maximum
score, and reports all query ranks. Corpus-order ties use the first parent
occurrence. Descending-parent-ID ties use reverse, exact string order. IDs are
opaque: the `../` inside an external-call symbol ID is not a filesystem path.

The CLI accepts a complete `retrieval-evaluator/v1` config. It binds sample,
text policy, model, dimensions, row counts, input file hashes, evaluator source
hash, and tie policy. A successful driver receipt binds the source/config/input
pins and the metrics file hash. Invalid input returns exit 2 without a new
completion receipt or metrics. Existing/partial outputs refuse overwrite.
A valid score that misses the calibration target preserves observed ranks and
metrics, writes `checks_failed` with exit 1, and cannot become a new baseline.

## Given/When/Then scenarios (recorded before implementation)

1. Given normalized or scaled vectors with known parent ranks 1 and 2,
   when scored, then R@1=0.5, R@10=1, MRR@10=0.75 and
   NDCG@10=(1+1/log2(3))/2. Inputs remain unchanged.
2. Given ranks 1, 10 and 11, when scored, then rank 11 contributes zero
   to all cutoff-10 metrics and the full query denominator stays three.
3. Given duplicate chunks and interleaved parents, when collapsed,
   then each parent appears once with its maximum chunk score.
4. Given equal parent scores, when the policy changes, then corpus-order
   preserves first parent occurrence and descending-parent-ID uses reverse ID.
   Reordering the corpus changes only the corpus-order tie result.
5. Given higher-scoring rows in another repository, when scored,
   then those rows cannot change a query's same-repository rank.
6. Given missing labels, duplicate document/query IDs, malformed rows or an
   empty sample, when parsed, then a visible error replaces all metrics.
7. Given zero, nonfinite, nonnumeric, ragged or wrong-shaped vectors,
   when parsed, then no partial metrics are accepted. Row/dimension mismatch
   and conflicting cross-repository parent identities also refuse.
8. Given a valid synthetic on-disk sample and matching source/config/input
   hashes, when the CLI runs, then its metrics and completion receipt agree
   with the driver's required pins, file hashes, state and exit code.
9. Given an input/config/source identity mismatch or an escaping path,
   when the CLI runs, then it returns an error without a completion receipt.
10. Given a completed output or leftover partial output, when restarted,
    then it refuses overwrite and preserves the earlier bytes for review.
11. Given a valid score that fails the saved-precision calibration target,
    when the CLI runs, then it preserves observed metrics and returns a
    checks_failed receipt with exit 1, not a new baseline or a success.
12. Given a D3 model name, when its config is requested, then the pinned
    repository, revision, prefix, pooling, dimensions and truncation are
    explicit. GTE also pins its separately hosted code revision.

No shared scorer is changed: the surviving ONNX scorer and its callers remain
read-only. The new scorer has only its CLI and focused tests as callers.
Symlinks apply to the file boundary, not opaque symbol IDs. Removal/re-adding
is a fresh invocation with no persistent cache. No daemon lifecycle applies.

## Intervention and evidence boundary

Reuse the array/JSONL layout of `tmp/onnx/py/run_embeddings.py` and the explicit
parent scoring in `tmp/onnx/py/common.py`, without importing their model stack
or Mac probes. NumPy 1.26.4 is the existing ONNX study dependency. A standalone
Python 3.12 uv application needs only NumPy and stdlib unittest, not a build
backend, model SDK or a scoring framework. Exact cosine normalizes in float64;
the surviving ONNX scorer instead multiplies already-normalized float32 rows.
The later pod calibration must expose any resulting discrepancy.

Only source preparation, file identities, public documentation reads and syntax
compilation are permitted on this Mac. Synthetic execution, dependency/lock
resolution and verification belong to reviewed Linux fork CI. Calibration and
model inference are not run or claimed here. The lead owns acceptance.

## File-only sample reconciliation

These are separate samples, not interchangeable score denominators:

- Mixed metadata: `corp_.jsonl`, 31,092 rows, 30,000 unique parents.
  `qs_eq.jsonl` contains 1,000 queries: 334 Go, 333 Rust, 333 TypeScript.
- TypeScript metadata: `docs_.jsonl`, 5,000 distinct row IDs and 4,842 parents.
  `qrows_.jsonl` contains 333 distinct query/label IDs.
- TypeScript body: `body_.jsonl`, 4,842 rows and the same parent set.

Every TS metadata row equals its matching mixed-corpus row. TS queries equal
the ordered TypeScript subset of the mixed queries. All labels are present in
the same-repository parent sets. Document/query row IDs are unique within each
file. Body and metadata preserve their different text boundaries. Both retained
metadata corpora keep each parent's chunks contiguous (zero parent reentries),
so first-parent and first-winning-chunk corpus-order tie conventions coincide
for these existing inputs. Interleaved synthetic chunks define first-parent
order explicitly for new inputs.

The surviving producer explicitly reads `docs_.jsonl`, then `qrows_.jsonl`,
restores length-grouped batches to original row indices, and writes the matrix
in that order. Its saved complete receipt says metadata/count 5,333/start 0/end
null. The NPY header is little-endian float32, C order, shape 5,333×768; its
16,383,104 bytes equal the 128-byte header plus that payload. Thus rows 0–4,999
map to metadata and rows 5,000–5,332 map to queries. **The array has no embedded
IDs or historical input hashes.** This association is reconstructed from the
surviving producer, files and report, not retroactive cryptographic provenance.
The hashes below freeze this reconstructed input set for the later pod replay.

The saved `retrieval.json` baseline contains 333 ranks, 4,842 parents and
R@10 0.6936936936936937. Its reported percent at three decimals is 69.369.
That is existing evidence, not a score recomputed by A2. No dot products,
norms or scorer calls ran during reconciliation.

- `docs_.jsonl` SHA256: `555c48d42999399364ac6ee023d2a4b6aa71af9a6a30f2e03da6cfaa2cf31bb1`.
  Ordered IDs SHA256 (UTF-8 IDs joined with LF, final LF):
  `731a131f407a5ad5468999bcc616778f31f382d55f741132ff7c7fa5469aaeb5`.
- `body_.jsonl` SHA256: `128bc20f9e8b9640668d48d783dd924e39c920edf13aeff3fc7b1908dd9b0e30`.
  Ordered IDs SHA256 (UTF-8 IDs joined with LF, final LF):
  `98f5df34fc5ed23ba25a424782dc1d3f59d159de22e6e19e2076005e382fc8be`.
- `qrows_.jsonl` SHA256: `3315ad5265d3459491af01a568f2b442a4caaadaf238ca47c04e505541a1680f`.
  Ordered IDs SHA256 (UTF-8 IDs joined with LF, final LF):
  `9fc8d4bb6b03a46870428051aab5cf1c7a6df8710a7c1cc9239968d6566c841b`.
- `corp_.jsonl` SHA256: `e40db9f4cad191854c672a4222f8faa0f8c2b25ed8136d3cc8b7fb7eaafc4887`.
  Ordered IDs SHA256 (UTF-8 IDs joined with LF, final LF):
  `83457dd312712c35bde2f924a1741426aad287f7078ffad05b8e4b3dbce62e86`.
- `qs_eq.jsonl` SHA256: `c62560a8d080b9ae9efdcc41a7c05dc76e87894256d6a50c424ba5abca9933a7`.
  Ordered IDs SHA256 (UTF-8 IDs joined with LF, final LF):
  `6155dd4992c3992ad238e2da9725a59bc7971c3463ca6ce1b08b70ee981e74e2`.
- `vectors/coderank-fp32-metadata.npy` SHA256:
  `7fab16ef9c2e27b3948ca0197b1b60b5d9b4924bbfaacf1e847131c97a024636`.

Producer source SHA256:
- `tmp/onnx/py/run_embeddings.py`: `f946dc532ef9cc71a3d3646b765f8723c7c5e997b0ca9cdd23266864891f9a10`.
- `tmp/onnx/py/score.py`: `3638244aa2bb11fa7bf549d3ab1aec06a4a7e453283be5f7b9ed9e0b01450004`.
- `tmp/onnx/py/common.py`: `9ac96a6c723577e9a495bafc4a10555e536b07d599167a2d91cb8864a06127d1`.

## D3 model identities and source evidence

`sources/manifest.json` records all 18 fetched public files, their requested
URLs, PDT fetch times and SHA256. Requests were unauthenticated GETs only.
The source bundle contains the Neon page, three API metadata records, three
revision-addressed cards, ten small configuration files and the GTE code-repo
metadata. It contains no weights, tokenizer payloads or downloaded Python code.
The first page's timestamp uses its immediate file-write time because metadata
capture encountered a restored-name collision; the body was kept, not refetched.

Neon's page confirms three 1,024-dimensional aliases but does not establish
which HF revision its hosted service uses. These are PLAN's open-weight
candidates, not proven Neon checkpoints. Configurations use native card/settings:

- `qwen3-embedding-0-6b` → `Qwen/Qwen3-Embedding-0.6B`.
  Revision: `97b0c614be4d77ee51c0cef4e5f07c00f9eb65b3`.
  Pooling: last-token; dimensions: 1024; truncation: 32768 tokens.
  Canonical model-config SHA256: `813fbce54b27e77a7a51156203deaf928be738516f5cf5064c7546db9c1981c9`.
- `bge-large-en` → `BAAI/bge-large-en-v1.5`.
  Revision: `d4aa6901d3a41ba39fb536a557fa166f842b0e09`.
  Pooling: cls; dimensions: 1024; truncation: 512 tokens.
  Canonical model-config SHA256: `3679c1a533745bea63631e76e55784f34a0d09bef589a5398010b56aa1d4a98e`.
- `gte-large-en` → `Alibaba-NLP/gte-large-en-v1.5`.
  Revision: `104333d6af6f97649377c2afbde10a7704870c7b`.
  Pooling: cls; dimensions: 1024; truncation: 8192 tokens.
  Canonical model-config SHA256: `98ef81c5a4c32581ed9bf81b9df6767c155e455a1f9de4199b6ba3a3a207a6e5`.

Qwen uses the exact `query` prompt in its pinned
`config_sentence_transformers.json`: `Instruct: Given a web search query,
retrieve relevant passages that answer the query\nQuery:`. The newline is
literal and there is no added space after the final colon. This deliberately
uses the native default rather than tuning a new instruction on these labels.
BGE uses `Represent this sentence for searching relevant passages: `, including
the trailing space, from the card's English retrieval table. GTE uses no prefix.
Documents use no prefix for any of the three. All comparison vectors use L2
normalization, including GTE, whose card makes normalization optional.

**GTE requires `trust_remote_code=True` on the pod.** Its config points to
`Alibaba-NLP/new-impl`, pinned to
`40ced75c3017eb27626c9d4ea981bde21a2662f4` as `code_revision`. The pod inference
will execute that pinned third-party code. A2 fetched metadata only, not `.py`.
The CodeRank reference settings describe the saved fp32 vectors, not an
approved fresh CodeRank inference recipe.

## Project identities and verification

Preparation checkout identities, read from Git metadata without running Git:

- Fork `study/storage-harness`: `601e22858769d60ce66f8c43bcfd0ee4244303f1`.
- Prime `research/gortex-storage-study`:
  `0da4b3c5626133767ed099ea6442db48d3497ed2`.

These are the preparation bases, not a commit containing A2. Full Git status was
not run under the hold. New target directories were absent before creation.
The workflow has an exact byte baseline and a one-step structural comparison.

Source snapshots:
- `pyproject.toml`: `f7b184e27a47e6b323063ab94b9c7070ae52aa0c606533867d39beb4079fca19`.
- `evaluate.py`: `43972a8a6d8b13133b70491343a5c33fface8bf8d4ca156cfefbd7997bd53a4b`.
- `test_evaluate.py`: `5caff7291fdbc65ff23e14ff498befab19f20c0891c08e2fe3586f8dfe174b0f`.
- `calibration-v1.json`: `1fd49167243d0f5dd510bbc2d14c6b1576477660ee37f8aaae82f3805ac0050e`.
- Canonical calibration input pin: `a9159f625bb67f720b35be9fe218262873a134c25fc47119b3886b4517e41966`.
- Python source sizes: 331 production lines and
  283 test lines; 13 test methods, counted statically.
- uv 0.13.5 Linux GNU archive SHA256:
  `f68d291c75783122dad58ab77def0e13f3233f358731db5d6d7e8715603b928f`.
- Pod Python comes from the accepted driver's managed
  `cpython-3.12.15-linux-x86_64-gnu` installation.

`uv.lock` is intentionally **pending Linux CI**, not fabricated on the Mac.
The step resolves it only when absent, runs `uv lock --check`, and includes
its exact bytes as `ci-evaluator-lock.txt` in the existing CI artifact.
The lead must retrieve that file, review/copy it as `uv.lock` into both project
copies, record its hash here, and verify it in CI before phase acceptance.
The actual CI interpreter and source/test/lock hashes are captured in
`ci-evaluator.txt`; none is claimed from an unrun job.

Allowed local verification: `py_compile` on both source/test mirrors exited 0
with empty output, using Python 3.11.15 and an owned bytecode-cache prefix.
This is syntax evidence only, not a Python 3.12 environment or test pass.
Manual/AST checks found no source/test line above 79 characters. No formatter,
linter, type checker, uv command, unit test, scorer, model or Go command ran.
No existing test was deleted. Mirrors are checked byte-identical.

Gortex localization and memory calls were not made: the Mac file-only hold
excludes their local MCP/native process. Direct source reads carry the evidence;
no graph claim or recalled repository memory was used.

## Exact Linux CI step

Fork source: `study/storage/neon-eval/`. Only the public application, tests,
pyproject and this report are mirrored. Real JSONL/matrices, the local calibration
config and fetched source bundle are not included. The existing single-job
workflow gains exactly one step, named:

`Exact evaluator entrypoint on tiny synthetic vectors only`

It checksum-verifies uv, stages the project under
`$RUNNER_TEMP/storage/evaluator-layout/`, resolves/checks the lock there, and runs
this exact command from that staged root:

```sh
uv run --project research/gortex/storage-architecture/tmp/neon-eval python -m unittest discover -s research/gortex/storage-architecture/tmp/neon-eval -p test_evaluate.py -v
```

The complete new step is in `.github/workflows/storage-screen.yml`.
Parsed comparison confirms all existing triggers, concurrency, job properties
and other steps are unchanged. OpenBLAS/OMP/MKL are capped at two threads.
No model library, weights or real corpus is needed for the fixtures.

## Exact pod calibration handoff (not executed)

The shared-hub output is `tmp/pod-run/bindings/calibration.json`. It is a
`calibration` jobs fragment, not the complete six-block bindings object.
Merge it with the other owner fragments. Replace both `LEAD` occurrences with
the reviewed full source pin; preflight intentionally rejects that placeholder.
Its one job allocates 15 minutes under the existing supervisor's absolute
calibration deadline and cleanup reserve. No second timer or pod is introduced.

During the approved setup, the owner must stage:

1. The reviewed evaluator, pyproject, CI-generated lock and `calibration-v1.json`
   under `/work/control/neon-eval/`. The existing driver's old source checkout
   is not assumed to contain this later evaluator.
2. The exact metadata/query/matrix files from the reconciliation above under
   `/work/data/evaluator/`, preserving their listed basenames and hashes.
3. The final source pin in the binding and argv. If the evaluator/config changes,
   regenerate the evaluator hash, config pin and binding before a run.

The config pin is the SHA256 of the exact calibration JSON bytes. The input pin
is SHA256 of the compact, sorted-key JSON mapping documents/queries/vectors to
their SHA256 values. Paths are not substituted after hashing the config.

```sh
/work/toolchain/uv-x86_64-unknown-linux-gnu/uv --no-config run --frozen \
  --python /work/python/cpython-3.12.15-linux-x86_64-gnu/bin/python3.12 \
  --project /work/control/neon-eval \
  python /work/control/neon-eval/evaluate.py score \
  --config /work/control/neon-eval/calibration-v1.json \
  --receipt /work/aux/calibration-coderank/complete.json \
  --root /work \
  --source-pin LEAD \
  --config-pin 1fd49167243d0f5dd510bbc2d14c6b1576477660ee37f8aaae82f3805ac0050e \
  --input-pin a9159f625bb67f720b35be9fe218262873a134c25fc47119b3886b4517e41966
```

The target is exactly `69.369` percent at three decimals. A mismatch preserves
baseline/discrepancy evidence and blocks dependent model rankings, not the
independent storage/catalog/vector and eligible ONNX-cost work. Retrieval,
model runtime, memory and cost remain unmeasured by A2.

## File-impact notes and remaining work

The owned worktree lacked the shared hub's private `tmp/.gitignore`. A local
project `.gitignore` containing `*` keeps every owned artifact ignored without
editing shared-main or tracked configuration. Added `sources/` follows the
lead's later documentation approval. `calibration-v1.json` carries the concrete
input contract needed by the binding. `.checks/` is syntax-check bytecode only.

The fork has the evaluator, test, pyproject, report and one workflow step.
No driver, probe, accepted input, model, resource helper or upstream file changed.
Everything remains uncommitted. Remaining work belongs to the lead: review,
Linux CI/lock retrieval and verification, final source pin, setup staging,
then the separately authorized pod calibration and whole-phase acceptance.
