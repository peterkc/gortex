# One owned CPU pod lifecycle

> **Sandbox venue:** use [Sandbox venue](#sandbox-venue) for the new
> Prime Sandbox profile. The pod procedure below stays available but is not
> used by that profile. Neither procedure grants spend approval.

**Preparation only. This document grants no spend approval.** The lead operates
one approved Nebius on-demand CPU offer with Prime CLI **0.17.0**. The complete
attempt includes provisioning, setup, analysis, copy and deletion. It must stay
within three hours and $2. These helpers do not use `run.py`, `test_driver.py`
or caller-map tooling.

```text
Approve exact inputs -> local validation -> one fresh availability read
  -> arm owner -> create once -> validate request/status -> publish RENTED
  -> bounded root precheck -> short detached launch -> read-only probes
  -> verified copy
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

The November 2, 3:17 PM PST read-only receipt has two 8/32 CPU rows at
0.264/hour compute: Nebius FI/eu-north1 (`3964f0`) and US/us-central1 (`840aa0`).
This study selects the US row: CLI `location` is `US`, not `us-central1`.
Require exactly one match after the location filter. Refresh these observations
before use; they are not permanent pins. Availability displays `CPU NODE`, while
status uses raw type plus count (`CPU_NODE x1` in the source-derived fixture).
There was no live status sample; do not call that fixture allocation evidence.

The raw offer lists only `ubuntu_22_cuda_12`. Its default disk is 100 GB and is
not included in compute price. `pricePerUnit=0.0001` has no stated time/size unit.
The approved working assumption is USD per GB-hour: 100 GB adds 0.010/hour and
expected total becomes 0.274/hour. Record this assumption and its receipt in
`disk_price_reference`; do not describe the unit or invoice as verified.

CLI 0.17.0 short IDs omit spot and price. Even no-group output deduplicates short
IDs. The CLI has no atomic offer/price lock or create `--max-price`. Post-create
checks stop a mismatch, but cannot prevent its initial charge. Host/API outages
can prevent a literal billing guarantee. The user's approval must accept these
limits; otherwise do not rent. Confirm boot-disk/other fee inclusion separately.
Availability compute plus approved assumed disk charge must equal the quoted
total. Status `cost_per_hour` has unknown component basis: require its raw value
not to exceed that total, and raw value plus assumed disk charge not to exceed
the hourly ceiling. Record the raw value and `basis: unknown, conservative`.
For duration/budget estimates, use the conservative hourly bound
`min(hourly_ceiling, quoted_total_hourly + assumed_disk_charge)`; it is 0.284
for this example, not an invoice. This covers either reading of the status rate.
Unknown fees remain unknown, not zero.

Prepare private execution JSON outside the checkout. All keys are mandatory;
replace paths and placeholders before validation. The numeric example is a
proposal, not permission to execute it.

```json
{
  "cli_context": "REQUIRED_APPROVED_CONTEXT",
  "team_id": "REQUIRED_APPROVED_BILLING_TEAM_ID",
  "provider": "nebius",
  "offer_id": "REQUIRED_FRESH_SHORT_ID",
  "cloud_id": "REQUIRED_FRESH_CLOUD_ID",
  "location": "REQUIRED_APPROVED_LOCATION",
  "gpu_label": "CPU_NODE x1",
  "gpu_count": 1,
  "image": "REQUIRED_APPROVED_IMAGE",
  "disk_gb": 100,
  "disk_hourly_per_gb": "0.0001",
  "disk_price_reference": "REQUIRED_RECEIPT_AND_USD_PER_GB_HOUR_ASSUMPTION",
  "vcpus": 8,
  "memory_gb": 32,
  "quoted_total_hourly": "REQUIRED_FRESH_COMPUTE_PLUS_ASSUMED_DISK_TOTAL",
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
  "upload_reference": "REQUIRED_RECENT_CONSERVATIVE_UPLINK_MEASUREMENT",
  "minimum_download_mbps": 100,
  "download_reference": "REQUIRED_RECENT_CONSERVATIVE_DOWNLINK_MEASUREMENT",
  "expected_evidence_bytes": 805306368,
  "evidence_size_reference": "REQUIRED_REVIEWED_NORMAL_OUTPUT_FORECAST",
  "cgroup_root": {
    "mode": "prior-evidence",
    "provider": "nebius",
    "cloud_id": "REQUIRED_FRESH_CLOUD_ID",
    "location": "REQUIRED_APPROVED_LOCATION",
    "image": "REQUIRED_APPROVED_IMAGE",
    "source_reference": "REQUIRED_REVIEWED_PROVIDER_OR_PRIOR_ROOT_PROOF",
    "evidence_path": "REQUIRED_CANONICAL_PRIVATE_EVIDENCE_FILE",
    "evidence_sha256": "REQUIRED_64_HEX"
  },
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
D14 bounds reject before create. Three hours at the 0.50/hour ceiling is 1.50.
One further billing-hour contingency makes 2; it grants no runtime extension.
The example's 0.284 conservative bound gives 1.136 for four hours, including
contingency. Even a short run refuses a quote above 0.50/hour. Deletion has at least a two-minute reserve.
Shorter approved times must also reserve controller cleanup, packing and copy.

### Cgroup venue evidence before approval

Availability and price receipts do not prove that a CPU pod is a VM or exposes
its true writable cgroup root. No such provider/image proof has been supplied.
For `prior-evidence`, the lead inspects the referenced provider document or
prior root observation. It must establish the exact provider, cloud, location
and image tuple. Admission binds those fields and the canonical evidence file's
SHA-256; it cannot judge whether its prose supports the claim.

If that proof is unavailable, the user can explicitly approve this replacement
`cgroup_root` object as part of the final run approval:

```json
{
  "mode": "first-in-pod-check",
  "probe_seconds": 300,
  "approval_reference": "REQUIRED_USER_APPROVAL_OF_UNKNOWN_ROOT_AND_CHECK_COST"
}
```

This is not existing venue evidence or permission to rent now. Create, ACTIVE
and the first successful SSH contact share the original `ready_seconds` window
(example: 900 seconds). The first-root check's 300 seconds starts at that first
successful contact, not at create. Its wall and monotonic timestamps are recorded
once in `ssh-contact.json`. A complete temporary file is atomically linked into
place without replacing an existing receipt. Retrying or restarting a helper
cannot refresh those clocks.
Run the owner and helper on the same always-on host. Do not copy a live lease to
another host with a different monotonic clock.

Before bulk input upload, the host stages the source-pinned standalone CPython
archive and reviewed cgroup helper. It checks the archive SHA-256, unpacks it in
the private session directory, and uses that interpreter for the root check and
later observations. No system Python or CLI `--env` is needed. First checks must
prove root UID, x86-64, Ubuntu 22.04, the pinned Python 3.12, glibc at least 2.28,
and the true writable cgroup root. These are observed requirements, not facts
inferred from the advertised image. Unsupported facts refuse before bulk input
upload or analysis. No CUDA runtime dependency is added. Without contact
by the readiness deadline, the owner requests deletion with the root question
unanswered. After contact, missing/refused root proof ends at contact plus the
approved probe allowance. The owner enforces those same deadlines even if the
helper disappears. Success permits the original full run, not a fresh clock.

Worst-case readiness plus root check is 900 + 300 = 1,200 seconds (20 minutes).
At the proposed 0.274/hour compute-plus-disk total, that is about 0.0913.
Using the 0.284 conservative status-basis bound gives about 0.0947. Including
120 seconds for cleanup/deletion observations gives 22 minutes and about 0.1041
at that conservative bound, under normal host/API operation. The terminate
command's 30-second limit is inside that reserve, not another allowance. These are
time/rate estimates, not billing caps, fresh quotes or invoices. Hourly rounding or separate fees can raise the
charge; confirm that basis in the final run approval. API/host failure can
defeat these windows. The three-hour/$2 full-attempt limits do not change.

Before any terminate attempt, the original hard cutoff still applies. After an
early attempt, the timer is cancelled; it does not issue another terminate if
that attempt fails. Use read-only `verify` and immediate resource-owner
escalation then. The later session still applies and verifies its cgroup limits;
failure there remains exit 78, followed by a cleanup request. No namespace-root
support or permission bypass is added.

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
hashes, complete reviewed harness export, pinned Go/uv/CPython archives,
frozen corpus, model bytes and prepared 8,192/Linux-runtime proofs.
The lead still reviews the trust/source meaning of those proofs.

Upload admission uses actual transport bytes, the referenced minimum uplink,
25% margin and 30 seconds of handshakes. Readiness + the separate first-root
allowance (when selected) + upload + bootstrap reserve must fit setup. Bootstrap
reserve is not a new paid extension. A roughly 0.7-GB
payload takes 560 seconds at 10 Mbit/s even before margin, so that link can be
refused. Do not replace this calculation with a larger arbitrary timeout or
invent a public URL for local ONNX exports.

Download admission uses the approved normal-output byte forecast and a separately
referenced minimum downlink, with the same 25% margin and 30-second allowance.
The proposed 768-MiB forecast needs 836 seconds at 10 Mbit/s, so it refuses a
420-second copy window before create. At 100 Mbit/s it needs 111 seconds.
The forecast is not a guarantee for failure logs or a hard archive maximum.
The published marker also carries actual archive bytes. Invalid or over-limit
archive bytes still refuse before transfer. If the conservative rate calculation
exceeds the remaining copy window, the host saves
`download-forecast-exceeded.json` and still attempts the copy. The actual link
can be faster than its pinned floor. The original copy/cutoff deadline, external
hash, exact copied size and manifest checks remain; no rental extension follows.

The output directory is reserved before create, with its device/inode recorded,
and must remain empty until the first copy. Recovery uses a fresh private child
under that same approved directory and preserves earlier files. The lead chooses
an approved local-only directory in the existing Prime
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
a Typer disk prompt. No attached disk is created. Create passes the required
billing `team_id` explicitly, without sharing flags. The request summary must
echo that team and must not report sharing. Other pod commands are user-scoped
in CLI 0.17.0; they have no team option. No global team selection is relied on.

Exactly one `Successfully created pod <id>` line supplies the
candidate identity. The ID is 8 to 64 letters, digits, `_` or `-`, starting
with a letter or digit. Status by ID must confirm its owned nonce name and
pinned team before identification for use. The list is an extra collision
check: zero matching rows are allowed, but a different ID with our name is refused.
For uncertain create, cleanup first uses the same stdout ID with status name
confirmation. Wrong billing is journaled but does not prevent deleting that
owned pod. A list-only fallback still requires status name/team confirmation.
Cleanup status/list reads use fresh receipt names for every attempt. Quantity,
provider and shape pins are checked before publishing validation. Echoed request
fields are not observed allocation. Missing live fields remain listed.

SIGINT, SIGTERM, SIGHUP, SIGQUIT, manual stop and timer use the same locked owner.
CLI subprocesses have
separate sessions so terminal Ctrl-C goes through that owner. After uncertain
create or early reconciliation failure, the process keeps its handlers and FIFO
service until the lease is resolved or its one terminate attempt is issued.
It leaves at most 15 seconds idle between unknown-identity reconciliation
attempts, even without a FIFO signal; it does not wait until the hard cutoff. It does not return with only an
unprotected timer. An unresolved identity is an immediate resource-owner
escalation; do not kill a guess or abandon that owner.

## 4. Launch once, observe and copy

```bash
python3.12 -B study/storage/pod/host_run.py   --state "$PRIVATE/lease" --destination "$APPROVED_DURABLE_DESTINATION"   --ssh-key "$APPROVED_PRIVATE_SSH_KEY" --session "$PRIVATE/session.sh"   --environment "$PRIVATE/env.sh" --payload "$PRIVATE/inputs.tar"
```

Arguments must match the approved JSON. ACTIVE readiness and first SSH contact
share the approved readiness allowance. In first-in-pod-check mode, the separate
root allowance starts only after that first successful SSH contact. SSH/SCP ignore
user configuration with
`-F /dev/null`, use BatchMode/IdentitiesOnly, a private known_hosts file and
keepalives every 30 seconds (six missed replies). First-use host-key trust is
explicit; if unacceptable, obtain a separately reviewed host-key path first.

Initial `mkdir -p` observations and pinned staging uploads are idempotent and
bounded by the original setup/upload clock. A failed upload has at most one
same-byte re-transfer before any launch. It cannot refresh the allowance.
The separately uploaded CPython archive is included in transport feasibility as
well as in the bulk payload. Bootstrap upload/extraction consumes the same
first-contact root window; it never refreshes it. Python hash/unpack has a
separate 30-second attempt limit, not the in-pod build reserve. SSH 255/timeouts
can retry until that original root deadline; other remote exits do not retry.
Each attempt retains a receipt. Extraction uses a unique private directory and
publishes only a complete, verified Python prefix; a retry can reuse that prefix
without unpacking into live files. Its interpreter supplies all session startup,
setup, cleanup and uv environment creation. Each failed upload
or probe keeps a separate error receipt; later attempts do not overwrite it.
The read-only root probe can retry SSH 255/timeouts until the same original
root deadline. Each error is retained; exit 78 never retries. This does not
repeat uploads, create, analysis or the root-check allowance.
The helper writes an exclusive local launch receipt, then sends one short
`nohup setsid` start command. It does not wait on that SSH channel. A lost or
uncertain acknowledgement permits only observation, never another launch.

The session writes an atomic `complete.json` after cleanup/packing, with exit
status, actual bulk archive size, and `full-evidence`, `partial-evidence`,
`diagnostics-only` or `none` scope. The session records its own PID atomically
before setup. Read-only probes check completion first, then launch/PID liveness.
A dead session stops immediately. A missing PID gets only the original 30-second
startup grace; it cannot idle until the analysis cutoff. A completed marker wins
over an already-exited PID; the producer rechecks after observing process exit.
Invalid or unproved PID facts are uncertainty, not death. They keep separate
receipts and permit only more read-only probes until the original deadline.
Short probes can reconnect until the copy reserve.
Permanently lost
transport still leaves the independent cutoff armed and requests stop when the
host helper exits. Do not restart a half pair or rerun host launch on that state.

If the original helper has failed and exited while the pod is still live, run
one replacement helper with the same arguments plus `--observe-only`. Keep the
original deletion owner running. This mode requires the existing validated
launch/connection receipts and unchanged approved output directory identity.
It makes no connect/account call, upload or launch. Copies go to a fresh private
`recovery-<id>/` child; the matching state receipt names that directory.

Before its first probe, a local argument/path/state refusal writes an
`observe-refusal-<id>.json` receipt and requests no cleanup. The original owner's
cutoff remains its responsibility; this is not a new clock. One corrected rerun
is allowed after that local refusal. Existing `done.json` or `rental-error.json`
refuses immediately, without probing or stopping anything. Do not run two
helpers concurrently or change source/input pins during recovery.

After its first probe attempt, the helper again requests stop on copy or failure.
It cannot undo an existing deletion request or retrieve remote bytes after
deletion. Even after the normal observation reserve expires, it can read one
existing completion marker before cutoff. Its copy attempt still uses only the
original remaining time; a conservative download warning is not a refusal.

Copy diagnostics first, including exit 78. Small bundles list explicit paths,
omissions and JSON metrics/bindings; each file is at most 1 MiB and the bundle
32 MiB. Full or partial evidence gets the approved copy window (at most 420
seconds), diagnostics 60 and log-tail fallback 15. All shorten at the cutoff.
Timed-out transport groups are killed and reaped. Log tails remain raw bytes,
including invalid UTF-8. A diagnostic or diagnostic-journal failure does not
veto an independent full/partial copy attempt. Normal verification still applies. An early root
precheck refusal has only its local exit-78 receipt, not fabricated study output.
An intact first cut survives
second-cut OOM in partial evidence; it cannot become an accepted comparison.

Copy verification checks the reviewed checkout again, external tar hash, fresh
safe extraction, exact manifest and expected scope. A changed verifier or bad
copy refuses acceptance. A normal helper requests stop after copy/refusal/error;
an observation helper does so only after its first probe attempt. If stop IPC
has lost its reader, preserve the error and escalate; do not start another
terminate owner. In particular, a failed cleanup request after root refusal
raises visibly instead of returning exit 78 as though cleanup were requested.

## 5. Prove deletion independently of cost audit

```bash
python3.12 -B study/storage/pod/lifecycle.py stop --state "$PRIVATE/lease"
```

The locked owner marks the lease attempted before one `terminate --yes`, and
executes it before journal writes. It never replays an uncertain effect. Then
it reads complete active/history sets within the remaining deletion reserve.
Prove the ID absent from active and present once with `terminated_at`. A delayed
history entry permits more read-only observations, not another terminate.
Transient CLI, JSON or read-timeout failures retain unique error receipts and
retry only these observations within the same deadline. A final rate/time proof
failure is not retried or converted into success.

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

## Sandbox venue

Use `sandbox.py` for the new venue, not `lifecycle.py rent` or `host_run.py`.
It uses Prime CLI 0.17.0 API transport. No SSH key, agent, pod quote, pod status
price or pod connection string is needed. Keep the reviewed CLI/SDK unchanged.

```text
Exact approval + local admission -> read-only image baseline
  -> arm owner -> create once -> confirm identity and use pins
  -> first API exec -> pinned Python -> actual root/runtime check
  -> same D14 session once -> observations -> verified copy
  -> delete once -> prove stopped -> read-only image receipt
  -> report new images for manual review, never auto-delete images
       ^ original cutoff and signals share the retained owner
```

The fixed profile is **8 vCPU, 32 GiB RAM, 40 GiB disk, amd64, zero GPU**.
The Ubuntu 22.04 Linux/amd64 manifest resolved on November 3, 2026 is:

```text
ubuntu@sha256:8a2321a92b438238f93b4346fff61bc4259c99fdd4d9b52ee86f153fee0c344f
```

Record its source index digest too:
`sha256:076e7bf82319619f28dd0aa9c02ab34b055ad37100395b4f49a2ffba04fb5e11`.
Metadata is not runtime proof. The guest must actually prove root, x86_64,
Ubuntu 22.04, CPython 3.12.16, glibc >=2.28 and the true writable cgroup-v2 root.
The existing preflight checks effective CPU, inherited memory and free disk.
A writable cgroup root remains unverified until that bounded guest check.

[Published prices](https://docs.primeintellect.ai/sandboxes/overview.md), valid
through December 22, 2026, are CPU 0.02/vCPU-hour, RAM 0.0125/GiB-hour and disk
0.0002/GiB-hour, in USD. The profile costs **0.568/hour** under the approved
**0.60/hour ceiling**. Three hours plus the retained one-hour contingency cost
2.272. Add the approved 0.05 image allowance: **2.322 <=3.00**. This is
**resources plus a bounded image allowance, not a provider-proved all-fee cap**.
The validator refuses these prices from December 23 UTC; renewed review is needed.

The host cutoff stays **10680 seconds**. The server timeout is **180 minutes**,
as a backstop, not a replacement for the owner or contingency. Readiness stays
900 seconds. One persisted first API exec starts the 300-second root window.
Retries never refresh it. Setup 1800, analysis 9600 and copy 420 stay fixed.
No idle timeout, custom Prime Image, checkpoint or volume is requested.

Bootstrap allows exactly these seven domains, with no deny rules:
`archive.ubuntu.com`, `security.ubuntu.com`, `proxy.golang.org`, `sum.golang.org`,
`storage.googleapis.com`, `pypi.org`, `files.pythonhosted.org`.
The driver emits and confirms this policy. Missing, extra, duplicate or wildcard
domains refuse use. The seven-domain egress allowlist stays open for the whole
run. Measured phases make no network calls by design; the sandbox does not
enforce that offline behavior. Apt, Go-module and locked Python setup stay
unchanged; this is not a deny-all run.

Copy `sandbox.example.json` to a private operator directory and fill every
`PENDING` field. Re-export the five existing harness subtrees, rebuild the
manifest/tar and bind `env.sh` to the new reviewed commit. `session.sh` must equal
the reviewed `accuracy_session.sh`. Only its admission block selects the sandbox
profile; default pod checks and all workload commands stay unchanged. Do not
reuse an older pod bundle, lease or result directory. State/results need fresh,
separate, canonical private paths outside the checkout.

From the reviewed clean checkout, use its native CPython 3.12 interpreter:

```sh
python3.12 -B study/storage/pod/sandbox.py check --config "$CONFIG"
python3.12 -B study/storage/pod/sandbox.py run --config "$CONFIG" --state "$FRESH_STATE"
```

`check` is local-only except the CLI version command. The example refuses use.
`run` requires separate execution approval. Keep its owner alive until deletion
is proved. Uncertain create/launch acknowledgements permit reconciliation and
observation only, never a rerun. Unproved identity/deletion retains the owner.

These are the owner's CLI shapes. **Do not issue an extra create or launch.**
The owner supplies the nonce, team, all seven allow flags and original deadlines.

```sh
prime --context "$CONTEXT" --plain sandbox create "$IMAGE" --name "$NONCE" \
  --label "$LABEL" --cpu-cores 8 --memory-gb 32 --disk-size-gb 40 \
  --arch amd64 --gpu-count 0 --timeout-minutes 180 --team-id "$TEAM" \
  --network-allow archive.ubuntu.com --network-allow security.ubuntu.com \
  --network-allow proxy.golang.org --network-allow sum.golang.org \
  --network-allow storage.googleapis.com --network-allow pypi.org \
  --network-allow files.pythonhosted.org --yes
prime --context "$CONTEXT" --plain sandbox run "$ID" --user root --timeout 30 \
  -- bash -c "$SCRIPT"
prime --context "$CONTEXT" --plain sandbox download "$ID" "$REMOTE_FILE" "$FRESH_PARTIAL"
prime --context "$CONTEXT" --plain sandbox delete "$ID" --yes
PRIME_TEAM_ID="$TEAM" prime --context "$CONTEXT" --plain images list \
  --output json --page 1 --num 250
```

Exec stdout is wrapped, so guest JSON is downloaded from fresh receipt files.
Uploads get guest-side hash checks. Copy validates size, SHA-256, safe members,
manifest coverage and scope before publishing. The SDK buffers whole files with
a per-phase 300-second HTTP timeout. The approved 100 Mbps floors are host
measurements, not proved sandbox-path rates. Forecast excess warns; deadlines
stay fixed.

Cleanup needs get-confirmed ID/name/unique label. Wrong billing refuses use but
cannot block cleanup of that owned ID. One delete intent permits one invocation.
A lost acknowledgement permits only read-only verification. `TERMINATED` or
`TIMEOUT` proves stopped. Alternatively require a narrow native HTTP404 and a
complete owned-team/unique-label list showing absence. `ERROR`, `PAUSED`, other
CLI failures and an empty list alone do not prove stopping.

Read-only image receipts before create and after deletion stay in private host
state/results. The driver pins `PRIME_TEAM_ID` only in its own process; image
list has no team flag. Saved CLI config does not change. New artifacts require
manual ownership/cleanup review, not automatic deletion or attribution to this
run. Failed mandatory post-delete receipts make the run incomplete. Image fees
remain covered only by the stated allowance, not a provider-proved all-fee cap.
