# D14 accuracy pod runbook: 2,048 versus 8,192 tokens

**Preparation only. No pod has been rented or approved by this document.**
The lead operates one Prime Intellect Nebius **on-demand CPU, 8 vCPU / 32 GB**
pod. The full paid attempt, including setup, copy-out and deletion, is limited
to **three hours and $2 total**. Do not use the caller-map runbook, T4/CUDA
`pod-models/run_models.py`, a standard CI runner or the local Mac for inference.
A full pair may not finish. Preserve a partial result and stop rather than shrink
the corpus, raise a limit or rent again.

Read [`../pod/LIFECYCLE.md`](../pod/LIFECYCLE.md) first. It owns Prime CLI 0.17.0,
required offer pins, the single create/terminate owner, price refusal, reported
versus observed shape, detached execution and verified copy-out. This document
adds the accuracy inputs, commands and acceptance checks. The lead supplies a
fresh reviewed full fork commit containing these scripts. `b1989926` is historical
rebuild evidence, not the final run commit.

## Fixed inputs and outstanding approval

- Frozen corpus ID:
  `dd6bddf4e92f1357c2d0f49bbd602c26d30c44d08b19d83aeb8dec3de4f3b401`.
  Use the retained artifact from CI run **41443670385**. Retain the later rebuild
  proof **b587d444 at b1989926** as a separate receipt. A rebuild under a changed
  harness/Python binding can have a different freeze ID even when text bytes
  match. Do not substitute it for the selected freeze.
- Go source `go1.23.2`, commit
  `ed07b321aef7632f956ce991dd10fdd7e1abd827`, tree
  `b5db4f10f3238e4c0de0645a9e31f9a123e071f9`.
- CPython source `v3.12.7`, commit
  `0b05ead877f909b7efe712db758012d9dbece7ce`, tree
  `c572705d83b94fe0c9c3080bc3fab4ac2c2ee32f`.
- Exactly **958 documents / 79 queries**: Go 500/50, Python 458/29. Go carries
  the long bodies. Every Python result must say **Python short-code control**.
  Preserve counts, provenance, ordered IDs, complete licenses/notices, manifest,
  canonical freeze bytes and freeze receipt. See `ACCURACY-CORPUS.md` for quotas
  and deterministic selection. No rebuild, resampling, padding or denominator
  change belongs to this run.
- CodeRankEmbed revision
  `3c4b60807d71f79b43f3c4363786d9493691f8b1`; use the exact bytes pinned in
  `../pod-cost/cpu-cost-v1.json`. The payload uses these relative paths:

- `model/model.onnx`
  SHA-256: `76cd0054e173a5928b3de15bb31f806f995705d221b77db6b5150f10c8907333`
- `model/weights.bin`
  SHA-256: `bdb9f7182272f817aab9e8ceb88891f4fd104c35183f0c2420ac35808aa127af`
- `tokenizer/tokenizer.json`
  SHA-256: `91f1def9b9391fdabe028cd3f3fcc4efd34e5d1f08c3bf2de513ebb5911a1854`
- `tokenizer/tokenizer_config.json`
  SHA-256: `7809f768ee3614618b3f1b91dcbfab4f6a9d4b79fb1ad5d17feb65a7c1bb5b7a`
- `tokenizer/special_tokens_map.json`
  SHA-256: `5d5b662e421ea9fac075174bb0688ee0d9431699900b90662acd44b2a350503a`
- `tokenizer/vocab.txt`
  SHA-256: `07eced375cec144d27c900241f3e339478dec958f92fddbc551f295c992038a3`
- `tokenizer/config.json`
  SHA-256: `5ff856a41d0f53ef2d74520627d464bd75c2efd8f26f381bd528654895c29b6c`

Before rental, the lead reads the pinned model/tokenizer configuration and model
limit documentation. Record evidence that the **model and tokenizer support
8,192 tokens including special tokens**, with configuration hashes and the
source reference, in `model-limit.json`. The lead's reviewed interpretation at
this pinned revision is: README line 11 advertises 8,192 context; the pinned
`sentence_bert_config.json` sets total tokenized length, including CLS/SEP, to
8,192; tokenizer `model_max_length` and model `n_positions` agree. The model's
`max_trained_positions=2048` affects DynamicNTK only when rotary scaling is set;
it is null here. Tokenizer `max_length=512` is a call default; this pipeline
supplies its cut. Record the source hashes and this interpretation. Training
positions were 2,048: quality beyond that is this experiment's question, not a
configuration promise. Do not infer support merely because a
CLI accepts `-cap 8192`. If the configuration needs interpretation or disagrees
with the claimed limit, stop and return the question to the lead.

Use Linux x86-64 **ORT 1.31.0**, not the macOS ARM wheel in `cpu-cost-v1.json`.
The CPU wheel is
`onnxruntime-1.31.0-cp312-cp312-manylinux_2_28_x86_64.whl`, SHA-256
`5e129d6c56abd53e659cb70f00a108d6824086470ff99c2e47a82e5786563db3`,
as published by `https://pypi.org/pypi/onnxruntime/1.31.0/json`.
Its `manylinux_2_28` tag requires glibc at least 2.28. Ubuntu 22.04's published
libc6 package is glibc 2.35, but check the actual image in the first pod check.
Verify the trusted wheel hash and each extracted member against its RECORD.
The acquired C library member SHA-256 is
`21901b1b02233484b80888c16f4f5473742f796bf5528d5d0c4073dbe9802001`.
The CPU provider helper SHA-256 is
`086ec1d5388f64153d9c63470d126693db9a182c8ce236d3a1119068471b8a0d`.
Its standard CPU system-library dependencies remain image checks, not staged
CUDA dependencies. Stage `runtime/libonnxruntime.so.1.31.0`, needed CPU shared
dependencies, and `runtime/receipt.json` with version, platform, download URL/hash, extracted
member path/hash and verification method. A hash first measured from an
unverified download is not a trusted download pin. No CUDA dependency is allowed.
The Go probe uses yalue **1.36.0** from its module lock and is built inside the pod.

Obtain the fresh offer through pi-gcp. The user approves the exact short ID,
cloud ID, context/account, image, region, disk, total hourly price, numeric
resource/deadline/file-limit profile and reviewed fork/input/script/config
fingerprints before create. The selected US CPU offer exposes only
`ubuntu_22_cuda_12` (Ubuntu 22.04).
Require x86-64, root login, glibc at least 2.28, and the true writable cgroup v2
root at `/sys/fs/cgroup`. Use the pinned standalone CPython 3.12 below, not an
assumed system Python. The CUDA-named image does not authorize CUDA dependencies;
this remains a CPU-only workload. A delegated namespace root is not sufficient
for this path.
No existing provider/image proof of that root layout has been supplied. The
execution JSON must bind reviewed prior evidence, or the user must explicitly
approve the lifecycle's bounded first-in-pod-check fallback and its cost risk.
The latter first stages and verifies the pinned standalone CPython archive,
extracts it under the private session directory, and stages the cgroup helper.
It uses that interpreter before any bulk upload. The first check must observe
root UID, x86-64, Ubuntu 22.04, the pinned Python, glibc at least 2.28, and the
true writable cgroup root. None of these facts is inferred from the image name.
Create, ACTIVE and first SSH keep their original 900-second readiness allowance.
The approved
300-second first-root window starts at the first successful SSH contact. Both
clocks are recorded once; retries never restart the window. Owner and helper
must stay on the same always-on host with the original lease state.

Without contact by readiness, the root question is unanswered and the owner
requests deletion. After contact, missing/refused proof ends at contact plus
300 seconds. Readiness plus check is at most 20 minutes under normal operation.
At the expected 0.274/hour compute-plus-disk total, its estimate is about 0.0913.
Using the conservative status-basis bound of 0.284/hour gives about 0.0947;
with the 120-second cleanup/deletion reserve it is 22 minutes and about 0.1041.
The terminate call's
30-second limit uses that reserve; it adds no further allowance. These estimates
are not invoices;
hourly rounding and separate fees can make the charge higher. Confirm the basis
in final run approval. The original three-hour/$2 limits are unchanged.

If an early terminate attempt fails, the timer issues no second terminate.
Use read-only verification and immediate resource-owner escalation instead.
The session's later cgroup-limit setup remains an independent exit-78 backstop.
This staging preparation made no Prime/account API call. This option adds no namespace-root support or
permission bypass.
The November 2, 3:17 PM PST receipt has FI and US 8/32 CPU rows, both at
0.264/hour compute. Select US short ID `840aa0`, cloud `cpu-d3_8vcpu-32gb`, CLI
`location=US` (raw data center `us-central1`), with exactly one match. These facts
must still be refreshed before rental. Use the 100-GB default boot disk: the
raw offer says it is not included in compute price. `pricePerUnit=0.0001` does
not state its unit; the user-approved working assumption is USD per GB-hour.
That adds 0.010/hour, giving expected total 0.274/hour. Record the assumption
and source in the execution JSON, not as verified billing. Keep the explicit
image/disk create flags and request echoes. Never pass `pods create --env`:
CLI 0.17 filters out this CPU image when that flag is used. `env.sh` is uploaded
and sourced separately, not sent as that CLI flag.
Receipt collection was read-only; no pod was rented. This staging work uses
those retained files and makes no Prime/account call.

The two preparation receipts use these field names:

```text
model-limit.json:
  schema=model-limit/v1, model_revision=<fixed revision>
  config_sha256=<pinned tokenizer/config.json SHA>
  tokenizer_config_sha256=<pinned tokenizer_config.json SHA>
  maximum_tokens=<verified integer at least 8192>
  includes_special_tokens=true, source_reference=<inspected source>
runtime/receipt.json:
  schema=linux-ort/v1, version=1.31.0, platform=linux-x86_64
  download_url=<trusted HTTPS URL>, download_sha256=<trusted SHA>
  member_path=<wheel/release member>, member_sha256=<staged library SHA>
  verification=<source pin plus RECORD/release-checksum method>
```

`admission.py` checks the complete reviewed harness export and pinned Go/uv
archives, archive manifest, frozen input bindings, model bytes
and these receipts before create. `accuracy.py` repeats validation before
inference. Structure/hash checks do not replace source/limit/trust review. The
stdlib-only preparation check does not load a model or run inference.

## Approved resource profile and schedule

The lead obtains the user's approval for these numeric limits before measurement.
They are not a claim that this workload fits.

| Scope                     | Hard memory | Concurrency |
|---------------------------|-------------|-------------|
| Private study parent      | 26 GiB      | Entire pod session |
| Independent controller    | 0.5 GiB     | Always outside the payload |
| Setup / prepare           | 4 GiB       | One active payload |
| Body or query inference   | 24 GiB      | One active payload |
| Assembly / language split | 4 GiB       | One active payload |
| Each evaluator score      | 4 GiB       | One active payload |

**Do not add the phase limits as concurrent consumers.** The script runs setup,
prepare, body, query, assembly and scores serially, then repeats at the other cut.
At the largest phase, 24 + 0.5 = **24.5 GiB**, leaving 1.5 GiB inside the 26-GiB
parent. The observed minimum is **28 GiB** (30,064,771,072 bytes), leaving at least
2 GiB outside that parent for the OS. A nominal 32-GB offer is not proof of
MemTotal or inherited cgroup capacity. Require at least eight CPUs in affinity and no known inherited CPU quota below
eight, that memory floor and **32 GiB free disk after input extraction**. Record
raw affinity and each ancestor `cpu.max`. This does not authorize a larger offer
or higher price. Refuse insufficient effective capacity.
No swap is allowed in the parent, controller or payload scopes. Use eight
intra-op threads and one inter-op thread for both cuts. No cost/thread sweep or
extra inference warm-up is part of this accuracy comparison.

The controller is the shell/Python/native cleanup supervisor, not an unaccounted
child of the inference scope. Bootstrap has its own 4-GiB sibling scope before
the native limiter exists. Each later process tree uses `limited-command` with
`memory.max`, `memory.swap.max=0`, `memory.oom.group=1`, one absolute deadline,
peak/events and owned cleanup receipts. A 256-MiB per-file `RLIMIT_FSIZE` applies
to each later phase and its descendants. The archive limits are separate.
Controller peak/events are explicitly a **before-pack snapshot**, not a sampled
engine peak or a claim to its full lifetime maximum.

The unchanged 256-MiB file cap has a conservative token-export bound. The frozen
quotas have 900 bodies at most 2,048 tokens, 48 at most 4,096, and 10 at most
8,192, including special tokens. `cpu_cost.batches` makes widths above 2,048
singleton under its 4,000,000 quadratic budget; shorter rows pad no further than
2,048. Three flat arrays at indent 2 need at most 30 bytes per signed-int64 value,
including indentation, comma and newline. Allow another 2 MiB for batch metadata:
`(900*2048 + 48*4096 + 10*8192)*3*30 + 2*1024**2 = 193052672` bytes, below 256 MiB.
The 79 queries at most 512 tokens are smaller. `admission.py` requires the approved
bound to cover this source-derived value. Retain its source/approval reference;
do not raise the cap after a file-limit failure.

Paid time starts before create. The example setup ends at minute 30 and all
accuracy phases share the nonrefreshing deadline at minute **160**. Both are
derived from approved JSON durations, not hardcoded by the shell. Readiness has
its own approved allowance (example 900 seconds) inside setup. Actual upload
bytes, a conservative referenced link rate, margin, the distinct first-root
allowance and bootstrap reserve must fit the same setup window before create.
The first-root allowance is added only for first-in-pod-check mode; the existing
prior-evidence readiness bound remains unchanged.

Approve `expected_evidence_bytes`, `evidence_size_reference`, a measured
`minimum_download_mbps` and `download_reference` too. A proposed 768-MiB normal
forecast has these modeled components for both cuts: 386,105,344 bytes of body
token JSON, 11,474,944 of query token JSON, 18,628,608 of NPY scalar payload and
50,970,624 of Go JSON scalar allowance. Their subtotal is 467,179,520 bytes.
The remaining approximately 322 MiB is forecast room for corpus/provenance,
notices, headers, bindings and normal logs; those bytes have not been measured.
The forecast is not a hard failure-output bound. Each of 16 child stages can
leave files subject to its separate 256-MiB file cap. Source references are
`admission.py`'s token bound, `build_accuracy_corpus.py`'s 512-token query cap,
`accuracy_bridge.py`'s 768-dimensional vectors, and `groups.py`'s subset copies.
At 10 Mbit/s, this forecast needs 836 seconds including margin/handshakes, so
admission refuses a 420-second copy. At 100 Mbit/s it needs 111 seconds. The
actual marker byte count still obeys the hard archive bound. If the conservative
rate estimate exceeds the remaining time, the host records a warning and tries
the copy under the unchanged deadline. It still verifies size, external hash,
manifest and scope. No download shortfall authorizes more paid time.

An outer controller timeout uses the analysis deadline plus at most 30 seconds
of kill grace. Small packing gets 30 seconds, full/partial packing 240, each
with five seconds of kill grace. The host observes completion for at most six
minutes after analysis and always leaves its copy reserve. Full copy gets at
most seven minutes, diagnostics one minute. The cutoff at minute **178**
requests deletion, leaving two minutes inside the three-hour bound. Shorter
approved durations must preserve these reserves. Every transport timeout also
shortens at that cutoff; no reconnect refreshes any clock.
Apply the $0.50/hour ceiling to compute plus assumed disk, not compute alone.
Availability total must equal the approved quoted total. Status rate component
basis is unknown: raw status must not exceed that total, and raw plus assumed
disk must not exceed the ceiling. Retain its raw value with `basis: unknown,
conservative`. Use `min(ceiling, quoted total + assumed disk)` for conservative
budget/duration estimates: 0.284/hour here, or 1.136 for three hours plus one
billing-contingency hour. At the full $0.50 ceiling, those four hours give $2.
The contingency grants no runtime extension. API/host failures can
still prevent a literal billing guarantee; use the lifecycle's escalation rule.

## Stage and pin before create

Prepare a fresh public-only staging directory with this layout:

```text
harness/study/storage/{harness,onnx,neon-eval,pod-cost,pod}/...
corpus/{documents.jsonl,queries.jsonl,freeze.json,freeze-canonical.json,...}
model/{model.onnx,weights.bin}
tokenizer/{tokenizer.json,tokenizer_config.json,special_tokens_map.json,vocab.txt,config.json}
runtime/{libonnxruntime.so.1.31.0,receipt.json,...CPU dependencies...}
model-limit.json
go.tar.gz
uv.tar.gz
python.tar.gz
```

Export only those harness subtrees from the lead's clean reviewed commit with
`git archive`; record the archive command and exact commit in the preparation
receipt. Include these new pod helpers in that committed source, not unreviewed
worktree bytes. Do not send the original projects' full checkouts: their frozen
text/provenance/license files already define the corpus. No private files,
authentication tokens or `.git` directories belong in the staging directory.
The corpus artifact's builder/license binding is retained unchanged, even though
the inference harness commit is later.

Pin Go **1.27.3 linux/amd64**, SHA-256
`65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40`, from
`https://go.dev/dl/go1.27.3.linux-amd64.tar.gz`. This satisfies the native module's
Go 1.27.1 minimum without an automatic toolchain download. Use the repository's
existing uv **0.13.5** Linux GNU archive, SHA-256
`f68d291c75783122dad58ab77def0e13f3233f358731db5d6d7e8715603b928f`, from
`https://github.com/astral-sh/uv/releases/download/0.13.5/uv-x86_64-unknown-linux-gnu.tar.gz`.
Stage these before the paid interval. Also stage `python.tar.gz` from the
Astral python-build-standalone **20261026** release:
`cpython-3.12.16+20261026-x86_64-unknown-linux-gnu-install_only_stripped.tar.gz`,
SHA-256 `ce4f6d0a6cfdb3bed6e44c2f9affd85943c76a42e25ad3a259002f31d6fb8a20`.
Source: `https://github.com/astral-sh/python-build-standalone/releases/tag/20261026`.
Verify against that release's published `SHA256SUMS` before use. It contains the
`python/` prefix and `python/bin/python3.12`; retain its acquisition receipt.

The host separately uploads this same pinned archive before the root check,
verifies its hash, and extracts it in the owned session directory. Its upload
is counted in transport feasibility in addition to the bulk payload bytes.
Bootstrap consumes the original first-contact root allowance. All remote early
probes, session startup, cleanup and uv sync use the staged interpreter. Do not
install system `python3.12-venv` or rely on an Ubuntu 22.04 package for Python
3.12. Keep other package installation/sync inside the pod, charged to setup.
Do not change either Python lock file to install ORT.

Use `artifacts.py --source STAGING --bundle PRIVATE/inputs.tar` from the generic
lifecycle. Its manifest binds every staged byte. Its external hash is the
approved `payload_sha256`. The reviewed session is
`../pod/accuracy_session.sh`; hash it into `session_sha256`.

Prepare a complete private `env.sh`, with the real approved 40-hex commit:

```bash
export HARNESS_COMMIT='REQUIRED_REVIEWED_FULL_FORK_COMMIT'
export LC_ALL=C
export DEBIAN_FRONTEND=noninteractive
export GOTOOLCHAIN=local
export GOWORK=off
export GOFLAGS='-mod=readonly -p=2'
export GOMAXPROCS=2
export GOMEMLIMIT=3GiB
export CGO_ENABLED=1
export GOOS=linux
export GOARCH=amd64
export CGO_CFLAGS='-O2 -g -fno-strict-aliasing'
export CGO_CXXFLAGS='-O2 -g -fno-strict-aliasing'
export PYTHONDONTWRITEBYTECODE=1
export UV_PYTHON_DOWNLOADS=never
export OMP_NUM_THREADS=8
export OPENBLAS_NUM_THREADS=1
export MKL_NUM_THREADS=1
export TOKENIZERS_PARALLELISM=false
export HF_HUB_OFFLINE=1
export TRANSFORMERS_OFFLINE=1
```

Hash it into `environment_sha256`. All later shell boundaries source it.
The measured phases use explicit `uv --offline run --frozen`; bridge and score
have separate environments synchronized from their existing locks in setup.
The native allowlist drops credentials, `OUT`, `GITHUB_*`, `RUNNER_*` and Prime
selectors. Pass paths through argv; do not depend on dropped environment names.

Complete and approve the execution JSON from the lifecycle, including the profile
above and fresh quote. Validate it locally without Prime. Before create, the
lead accepts independent review and fresh CI of the helpers and tiny identity,
language-subset, resource-refusal and lifecycle/archive fixtures on that commit.
No model inference or local Go test is needed for this preparation review.

## Execute once through the existing bridge and evaluator

Start the lifecycle owner with a retained host handle. After its `RENTED` and `validated.json` receipts,
run `host_run.py` exactly as documented in the lifecycle, with the approved
`accuracy_session.sh`, `env.sh` and `inputs.tar`. Do not use an interactive shell.
The session verifies the upload/manifest and shape before setup, then shape again
after extraction. The script records the original SSH membership/capacity before migration.
`cgroup_setup.py` creates the private parent at the true unified root, enables
memory before creating leaves, applies limits, then moves the controller. The
second memory-capacity anchor is that actual root ancestor, not the old SSH
scope which is no longer an ancestor. It does not mistake bootstrap's 4-GiB
limit for host RAM. CPU quota observation still walks all actual ancestors.
The same helper is exercised by the occupied-nonroot Linux CI integration test.
Unsupported namespace/container roots refuse; no EBUSY workaround skips limits.
Phase limits have separate receipts. It builds these in the pod under the bootstrap cap:

```text
cd harness/study/storage/harness
go build -trimpath -p=1 -o /.../bin/limited-command ./cmd/limited-command
cd harness/study/storage/onnx
go build -trimpath -p=1 -o /.../bin/onnxprobe .
```

The actual script uses absolute paths. It checks and records the pinned Go/Cgo environment and GCC target before
either build, then records binary hashes,
and uses the same built binaries for both cuts. `accuracy.py` then performs the following sequence for cut 2048 and cut
8192. Every command runs through `limited.py`, which hash-binds argv, host, fork
source, launcher, frozen data, policy and the nonrefreshing absolute deadline to
the existing native launcher. A nonzero result stops the session immediately.

1. Verify canonical freeze hash, all bound corpus files, language denominators,
   model revision and all seven model/tokenizer file hashes.
2. Write `accuracy-bridge/v1` manifest with those bindings, probe/runtime hashes,
   host, token cut, 24-GiB inference cap, eight threads and a 7,800-second probe
   guard. The guard is an additional limit; it never refreshes the shared deadline.
3. Prepare under 4 GiB:
   `uv --offline run --frozen --project .../pod-cost python .../onnx/accuracy_bridge.py prepare --root ROOT --manifest MANIFEST --manifest-pin SHA --output FRESH_PREPARED`.
4. Execute `prepared.json`'s exact `corpora.body.command`, then its
   `corpora.query.command`, each under 24 GiB. These are separate fresh Go probe
   processes with `-model coderank`, `-path`, `-library`, `-tokens`, `-output`,
   matching `-corpus body|query`, `-cap 2048|8192`, `-no-prepack`, `-threads 8`,
   `-guard 7800s`, `-repeat 1` and `-planned-repeats 1`. No mmap or new engine.
5. Assemble under 4 GiB:
   `... accuracy_bridge.py assemble --root ROOT --prepared PREPARED_JSON --prepared-pin SHA --body-report BODY_JSON --query-report QUERY_JSON --output FRESH_ASSEMBLED`.
6. Execute `cut.json`'s hash-bound existing evaluator `score_command`, using the
   pinned absolute uv executable plus `--offline`, under 4 GiB. It invokes
   `study/storage/neon-eval/evaluate.py score` with root, config, receipt,
   source/config/input pins. There is no new scoring implementation.
7. Under 4 GiB, `groups.py` prepares Go and Python query subsets using original
   query-line bytes/order and the corresponding already-generated query vectors.
   **All 958 document vectors stay in original order.** It records query indices,
   labels, parent config hash and fresh subset config/vector/input hashes.
   Score each subset through the same unchanged evaluator under 4 GiB.
   This produces denominators 50 and 29; it does not resample the corpus or run
   more inference. Both cuts derive the same subsets from the same frozen rows.

### Identity, recipe and scoring acceptance

Documents have unique `id`, `parent_id`, `repo`; queries have unique `id`, `repo`,
and name their relevant parent in that same repository. Go token batches restore
vectors by `indices`, not batch arrival/order. The assembled pickle-free NPY is
**(1,037, 768)**: all 958 document vectors, then all 79 query vectors in file
order. Do not accept missing, duplicate, shuffled, partial or wrong-cut output.
Identity relies on the pinned Go probe restoring indices; do not guess a new
ordering for an unlabelled report. The bridge binds exact token-file hashes and validates completed counts, runtime,
model, corpus, cap, thread/prepacking/repeat policy and dimension.

Use CLS pooling then L2 normalization. Query prefix is exactly
`Represent this query for searching relevant code: `, including its final space.
Bodies receive no query prefix. Padding is right to longest, PAD/type ID 0 and
attention mask 0. The existing tokenizer/batch export pattern remains unchanged.
Only the cap changes between cuts; model, texts, IDs, labels, query prefix,
normalization, candidates, ties and denominators remain fixed.

The unchanged evaluator normalizes in float64 for cosine, takes the maximum
chunk score per parent, and restricts candidates to the query's repository.
`corpus-order` exact ties keep the first parent occurrence in document order.
There is no epsilon tie. Report its `r10` and `mrr10` as **R@10 and MRR@10**.
Rank 11 contributes zero to both. Report both metrics overall (79 queries),
Go (50), and **Python short-code control** (29), for both cuts. No minimum gain
was selected. A result informs design; it is not a fabricated accuracy threshold.

## Copy-back and stop rules

The session creates diagnostics first on success or failure, including exit 78.
Its small list includes score metrics/receipts and bindings, with omissions
explicitly recorded. It also packages retained evidence after a failure as
`partial-evidence`; completed 2,048 work survives a later 8,192 OOM. Only both
cuts and all six successful scores produce `full-evidence`. The atomic completion
marker reports exit, scope and actual archive bytes after packing. A normal
exit-78 session still attempts partial packaging; only failed packaging reduces
it to diagnostics-only or none. An earlier host root-precheck refusal has no
study bundle and records its local exit-78 refusal instead. The live log stays
outside the
manifest; the bundle contains only its explicit partial 1-MiB tail. Evidence includes:

- Both cuts' bridge manifests, token exports, preparation receipts and Go reports.
- Document-then-query NPY/config/cut receipts and overall/language score receipts.
- Language indices, labels, subset NPY/configs and parent bindings.
- Frozen corpus, canonical/freeze receipts, counts, ordered IDs, provenance and
  complete licenses/notices, unchanged from the selected artifact.
- Input-file manifest, model-limit proof and Linux runtime acquisition receipt.
- Launcher/probe hashes, checked Go environment, GCC version, phase bindings,
  policies, file limits,
  native peak/OOM/exit/removal receipts, bootstrap/controller facts and host shape.
- Session exit and bounded partial log tail. The host separately retains approval,
  quote, full CLI output, requested/observed/omitted pins, SSH observations,
  terminate command/exit, complete active/history JSON, cost and duration.

The controller's private empty cgroup directories remain until pod deletion;
all payload scopes must be killed/empty/removed first. The controller exits
normally within its own limit. The host owner deletes the whole pod after copy,
then proves that deletion. The host copy path checks the reviewed verifier HEAD,
external hash, fresh extraction, manifest and scope before reporting a verified
copy; the lead retains and accepts that evidence after deletion. No path in
this runbook asks an operator to remove an unproved shared cgroup or resource.

A lost SSH acknowledgement or probe permits short read-only reconnection, never
a second session launch. The helper ignores host SSH config, uses explicit
identity/known_hosts and keepalives, and never holds one connection for the
analysis lifetime. Read-only probes also check the session's own atomic PID;
a dead session stops promptly, and a missing PID gets only the original
30-second startup grace. A permanently lost channel still preserves the host
cutoff. The producer rechecks a completed marker after process exit; uncertain
PID facts permit more bounded read-only observations, not a fresh session.
After a host helper failure, use the lifecycle's `--observe-only` path
only after that helper has exited and while the original pod remains live.
A local observation refusal before the first probe keeps a receipt and requests
no deletion. One corrected rerun is allowed; the owner's original cutoff is not
reset. A done/refused rental exits immediately without a probe or cleanup.
After the first probe attempt, copy/failure again requests cleanup. It cannot
undo deletion already requested or recover bytes from a deleted pod. Copies use
a fresh private recovery child without overwriting, uploading or relaunching.
Diagnostic/log decoding failures do not prevent a valid bulk copy attempt;
log-tail fallback preserves raw bytes, and failed attempts retain separate errors.

Stop on any shape mismatch, stale/over-ceiling quote, wrong pin, unconfirmed
8,192 support, OOM, deadline, file/disk limit, child/cleanup failure, partial cut,
wrong ID/order/label, unrecovered transport loss or rate/time breach. Preserve the failure.
On OOM, do not raise memory, lower the cap, drop long bodies or create a replacement.
Do not resume a half pair or mix source/corpus/runtime changes. No paid retry is
authorized. If only diagnostics copy succeeds, label it partial and make no
comparative claim. Missing deletion proof requires immediate resource-owner escalation. Later
`lifecycle.py verify` is read-only; never issue another uncertain terminate.
History `total_cost` units are not verified and cannot enforce the $2 cap. Keep
that audit value separate from rate/time estimates and deletion proof. Final
invoice and storage inclusion remain unverified unless an authoritative billing
source confirms them. Choose the durable private destination named in the
approved execution JSON before rent, not an ephemeral session folder.

## Verification scenarios and current limit

Before rental, fresh CI must check: correct pins give the expected commands;
wrong/stale/missing pins refuse before effects; concurrent timer/manual stop
issues one terminate; uncertain create/terminate never replays; SSH loss does not
restart; copied or changed/extra files and unsafe archive members are rejected;
low CPU/RAM/disk refuses; both cuts preserve IDs/order; language grouping retains
all documents and exact original query indices/bytes, including interleaved
languages; a missing language or wrong denominator refuses. Repeated output
paths must refuse rather than overwrite. A source update requires a new reviewed
fingerprint; deleting/re-adding the same frozen input does not change its bytes.
The impossible same-short-ID collision fixture was removed because the CLI
already discards those rows; exact matching and distinct-ID ambiguity tests
remain. Do not claim a hidden spot/price-twin guard. C2 fixtures also cover
unknown-identity polling without FIFO events, all four terminal signals,
transient deletion-read errors without effect replay, the first-root deadline,
copy-only recovery, PID startup/death, byte tails and download-window refusal.
All new unittest modules must run in fresh CI; they have not run locally.

Only static/syntax inspection is allowed during preparation. It cannot prove
8,192 fit, throughput, cloud deletion or final cost. Exact offer pins, final
reviewed commit, Linux ORT pin/limit proof, numeric-limit approval and fresh
CI/independent review remain pre-rental requirements. The lead records acceptance
and actual results; this document does not claim the accuracy action is complete.
