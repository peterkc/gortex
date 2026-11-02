# One owned CPU pod lifecycle

**Preparation only. This document grants no spend approval.** The lead operates
one approved Nebius on-demand CPU offer with Prime CLI **0.17.0**. The complete
attempt includes provisioning, setup, analysis, copy and deletion. It must stay
within three hours and $2. These helpers do not use `run.py`, `test_driver.py`
or caller-map tooling.

```text
Approve exact inputs -> local validation -> one fresh availability read
  -> arm owner -> create once -> validate request/status -> publish RENTED
  -> short detached launch -> read-only completion probes -> verified copy
  -> terminate once -> bounded active/history proof -> lead acceptance
       ^ timer, signals and manual stop share the same locked owner
```

## 1. Approve the exact execution

Obtain a fresh resource-owner receipt for context/account, short ID, cloud ID,
provider, region, on-demand status, CPU/RAM, image, boot disk and hourly price.
Approve the reviewed commit, input/script/config hashes, numeric profile and
private durable output destination. Keep CLI installation and context unchanged
until deletion is verified. An archived quote is not approval or availability.

Use `--no-group-similar` JSON. Default grouping hides CPU rows. Filter the fresh
result by approved cloud, provider, location, numeric `price_value`, CPU shape
and Available stock. Use the quoted default boot disk; other sizes need a
separate reviewed price/selection contract. Accept only an explicitly present `is_spot: null` or
`false`; missing, true or malformed values refuse. Require exactly one match.
Empty output fails once. Do not retry it, choose the next cheapest row or change
location. Create with the selected `--id`, not the CLI's broad `--cloud-id`
alternative. Its first-match OR selection can select another location.

The read-only November 2 receipt observed one matching Nebius FI 8/32 CPU row
at 0.264/hour, SKU count 1 and default boot disk 100 GB. These are observations,
not permanent pins. Refresh them before use. Availability displays `CPU NODE`,
while the status formatter uses raw type plus count (`CPU_NODE x1` in the
source-derived fixture). There was no live status sample. Do not call that
fixture observed allocation evidence.

CLI 0.17.0 short IDs omit spot and price. Even no-group output deduplicates short
IDs. The CLI has no atomic offer/price lock or create `--max-price`. Post-create
checks stop a mismatch, but cannot prevent its initial charge. Host/API outages
can prevent a literal billing guarantee. The user's approval must accept these
limits; otherwise do not rent. Confirm boot-disk/other fee inclusion separately.
The current path requires the approved total rate to match the selected and
reported rate. Unknown or separately charged fees are not a zero-cost assumption.

Prepare private execution JSON outside the checkout. All keys are mandatory;
replace paths and placeholders before validation. The numeric example is a
proposal, not permission to execute it.

```json
{
  "cli_context": "REQUIRED_APPROVED_CONTEXT",
  "provider": "nebius",
  "offer_id": "REQUIRED_FRESH_SHORT_ID",
  "cloud_id": "REQUIRED_FRESH_CLOUD_ID",
  "location": "REQUIRED_APPROVED_LOCATION",
  "gpu_label": "CPU_NODE x1",
  "gpu_count": 1,
  "image": "REQUIRED_APPROVED_IMAGE",
  "disk_gb": 100,
  "vcpus": 8,
  "memory_gb": 32,
  "quoted_total_hourly": "REQUIRED_FRESH_TOTAL",
  "quote_checked_epoch": 0,
  "quote_reference": "REQUIRED_RECEIPT_REFERENCE",
  "approval_reference": "REQUIRED_RUN_APPROVAL",
  "harness_commit": "REQUIRED_REVIEWED_FULL_FORK_COMMIT",
  "max_seconds": 10800,
  "cutoff_seconds": 10680,
  "setup_seconds": 1800,
  "ready_seconds": 900,
  "bootstrap_seconds": 300,
  "analysis_seconds": 9600,
  "copy_seconds": 420,
  "minimum_upload_mbps": 100,
  "upload_reference": "REQUIRED_RECENT_CONSERVATIVE_LINK_MEASUREMENT",
  "token_json_max_bytes": 193052672,
  "token_size_reference": "REQUIRED_REVIEWED_FROZEN_QUOTA_EXPORT_BOUND",
  "budget": "2.00",
  "hourly_ceiling": "0.50",
  "contingency_seconds": 3600,
  "minimum_memory_bytes": 30064771072,
  "minimum_free_bytes": 34359738368,
  "payload_path": "REQUIRED_CANONICAL_PRIVATE_PATH/inputs.tar",
  "session_path": "REQUIRED_CANONICAL_PRIVATE_PATH/session.sh",
  "environment_path": "REQUIRED_CANONICAL_PRIVATE_PATH/env.sh",
  "ssh_key_path": "REQUIRED_CANONICAL_PRIVATE_KEY_PATH",
  "destination": "REQUIRED_CANONICAL_DURABLE_PRIVATE_FRESH_DIRECTORY",
  "payload_sha256": "REQUIRED_64_HEX",
  "session_sha256": "REQUIRED_64_HEX",
  "environment_sha256": "REQUIRED_64_HEX"
}
```

The quote must be no older than 15 minutes. Positive integer/time/hash checks and
D14 bounds reject before create. Three hours at 0.50/hour is 1.50. One further
billing-hour contingency makes 2; it grants no runtime extension. Even a short
run refuses a quote above 0.50/hour. Deletion has at least a two-minute reserve.
Shorter approved times must also reserve controller cleanup, packing and copy.

## 2. Validate public inputs before rental

Use a clean reviewed checkout. Stage only public source and approved inputs;
exclude `.git`, credentials, execution JSON and private host dumps. Include the
reviewed session and complete nonsecret `env.sh`. See the
[accuracy runbook](../onnx/ACCURACY-POD-RUNBOOK.md) for exact inputs and limits.

`artifacts.py` packages plain tar with an exact SHA-256 manifest and external tar
hash. Limits: 1,024 payload files, 4 GiB per file, 8 GiB total and 16 MiB tar
overhead. It rejects links, traversal, devices, duplicates and noncanonical names.
Keep the fresh bundle outside its source. Do not stage an existing manifest.

```bash
python3.12 -B study/storage/pod/artifacts.py   --source "$STAGED_PUBLIC_INPUTS" --bundle "$PRIVATE/inputs.tar"
python3.12 -B -c 'import sys; sys.path.insert(0,"study/storage/pod"); from pathlib import Path; from lifecycle import configuration; from admission import validate; validate(configuration(Path(sys.argv[1])))' "$PRIVATE/execution.json"
```

The second command is cloud-free. `rent` repeats it before any account operation.
It checks clean reviewed HEAD, canonical regular inputs, transport hashes,
private key, fresh output path outside the checkout, archive member/manifest
hashes, complete reviewed harness export, pinned Go/uv archives, frozen corpus,
model bytes and prepared 8,192/Linux-runtime proofs.
The lead still reviews the trust/source meaning of those proofs.

Upload admission uses actual transport bytes, the referenced minimum uplink,
25% margin and 30 seconds of handshakes. Readiness + upload + bootstrap reserve
must fit setup. Bootstrap reserve is not a new paid extension. A roughly 0.7-GB
payload takes 560 seconds at 10 Mbit/s even before margin, so that link can be
refused. Do not replace this calculation with a larger arbitrary timeout or
invent a public URL for local ONNX exports.

The output directory is reserved before create and must remain empty until
copy. The lead chooses an approved local-only directory in the existing Prime
Gortex research hub, for example
`research/gortex/storage-architecture/artifacts/accuracy/<execution-id>/`.
Check ownership, ignore rules, privacy and free disk before selecting its exact
canonical path. Do not leave accepted bundles only in an ephemeral session or
upstream checkout. Keep the bundle, hash, approval and deletion receipts.

## 3. Keep one host deletion owner

Use an always-on host. On a Mac, wrap the retained owner with `caffeinate -i`.
Keep the lid open; do not force sleep, shut down or change networks during the
attempt. `caffeinate` does not protect against those actions. Do not run this
owner through the pod's SSH channel or daemonize/duplicate it. Retain its managed
process handle and full stdout/stderr. Set `PRIME_DISABLE_VERSION_CHECK=1`.

```bash
PRIME_DISABLE_VERSION_CHECK=1 caffeinate -i python3.12 -B study/storage/pod/lifecycle.py rent   --config "$PRIVATE/execution.json" --state "$PRIVATE/lease"
```

On an always-on Linux host omit `caffeinate -i`. State must be new, absolute and
private. Wait for **RENTED** and `validated.json`, not merely `session.json`.
The latter carries identity for cleanup before validation permits connection.
The owner arms its cutoff before the single create. The create call has a
separate 90-second bound and a last cancellation/deadline check before spawn.
An expired/cancelled call records not-created; an uncertain outcome never retries.

Global context/plain flags and explicit offer, name, CPU, RAM, disk, image and
`--yes` make create noninteractive. Pass boot-disk size explicitly; do not allow
a Typer disk prompt. No attached disk is created. The complete list identifies
exactly one owned nonce name. Request-summary quantity/provider/team/shape pins
and status fields are checked before publishing validation. Echoed request
fields are not observed allocation. Missing live fields remain listed.

Signals, manual stop and timer use the same locked owner. CLI subprocesses have
separate sessions so terminal Ctrl-C goes through that owner. After uncertain
create or early reconciliation failure, the process keeps its handlers and FIFO
service until the lease is resolved or its one terminate attempt is issued.
It does not return with only an unprotected timer. An unresolved identity is an
immediate resource-owner escalation; do not kill a guess or abandon that owner.

## 4. Launch once, observe and copy

```bash
python3.12 -B study/storage/pod/host_run.py   --state "$PRIVATE/lease" --destination "$APPROVED_DURABLE_DESTINATION"   --ssh-key "$APPROVED_PRIVATE_SSH_KEY" --session "$PRIVATE/session.sh"   --environment "$PRIVATE/env.sh" --payload "$PRIVATE/inputs.tar"
```

Arguments must match the approved JSON. ACTIVE readiness and first SSH contact
share the approved readiness allowance. SSH/SCP ignore user configuration with
`-F /dev/null`, use BatchMode/IdentitiesOnly, a private known_hosts file and
keepalives every 30 seconds (six missed replies). First-use host-key trust is
explicit; if unacceptable, obtain a separately reviewed host-key path first.

Initial `mkdir -p` observations and pinned staging uploads are idempotent and
bounded by the original setup/upload clock. A failed upload has at most one
same-byte re-transfer before any launch. It cannot refresh the allowance.
The helper writes an exclusive local launch receipt, then sends one short
`nohup setsid` start command. It does not wait on that SSH channel. A lost or
uncertain acknowledgement permits only observation, never another launch.

The session writes an atomic `complete.json` after cleanup/packing, with exit
status and `full-evidence`, `partial-evidence`, `diagnostics-only` or `none` scope.
Short read-only probes can reconnect until the copy reserve. Permanently lost
transport still leaves the independent cutoff armed and requests stop when the
host helper exits. Do not restart a half pair or rerun host launch on that state.

Copy diagnostics first, including exit 78. Small bundles list explicit paths,
omissions and JSON metrics/bindings; each file is at most 1 MiB and the bundle
32 MiB. Full or partial evidence gets the approved copy window (at most 420
seconds), diagnostics 60 and log-tail fallback 15. All shorten at the cutoff.
Timed-out transport groups are killed and reaped. An intact first cut survives
second-cut OOM in partial evidence; it cannot become an accepted comparison.

Copy verification checks the reviewed checkout again, external tar hash, fresh
safe extraction, exact manifest and expected scope. A changed verifier or bad
copy refuses acceptance. The host helper requests stop after copy/refusal/error.
If stop IPC has lost its reader, preserve the primary error and escalate the
cleanup failure; do not start another terminate owner.

## 5. Prove deletion independently of cost audit

```bash
python3.12 -B study/storage/pod/lifecycle.py stop --state "$PRIVATE/lease"
```

The locked owner marks the lease attempted before one `terminate --yes`, and
executes it before journal writes. It never replays an uncertain effect. Then
it reads complete active/history sets within the remaining deletion reserve.
Prove the ID absent from active and present once with `terminated_at`. A delayed
history entry permits more read-only observations, not another terminate.

A later read-only reconciliation is available:

```bash
python3.12 -B study/storage/pod/lifecycle.py verify --state "$PRIVATE/lease"
```

This mode cannot create or terminate. Observe the owner's terminal result too:
a stop request is not proof. `done.json` state deleted records observed deletion;
not-created proves no create subprocess began. Missing proof requires immediate
resource-owner escalation with ID/name, logs and uncertain-effect receipt.
Never create a replacement automatically.

History `total_cost` units are unverified: a real 1.1-hour record at 1.29/hour
contained 12961.0. Preserve that raw value for audit only. Do not divide it by a
guessed factor or use it for the $2 check. Time/rate acceptance uses the approved
rate, ceiling and host cutoff, not history totals. Termination's rendered UTC
second supplies a conservative duration upper edge for later verification.
A missing/unparseable time fails duration proof without erasing deletion proof.
Rate-times-duration is an estimate, not an invoice; storage inclusion and final
billing need authoritative confirmation. Unknown/missing cost never becomes a
verified invoice or prevents recording observed deletion.

## Verification boundary

Before paid use, the lead requires fresh independent review and CI on the final
commit, including fake process/clock refusals, signal/FIFO ownership, uncertain
effects, delayed deletion, source-format fixtures, transport recovery, partial
copy and real occupied-nonroot Linux cgroup setup. Static checks cannot prove
cloud behavior, fit, throughput or billing. No local study test, Go build or
account operation belongs to this preparation work.
