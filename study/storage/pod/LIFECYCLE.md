# One owned CPU pod lifecycle

This procedure prepares a future lead-operated run. It grants no spend approval.
Use the run-specific approval and reviewed commit before any account command.
These helpers require Prime CLI **0.17.0**. They do not import caller-map tooling
or change `run.py` or `test_driver.py`.

```text
Approve exact inputs -> prepare/hash inputs -> start host owner -> create once
  -> compare reported pins -> detached pod session -> bounded copy
  -> terminate --yes once -> complete active/history proof -> local verification
                        ^ independent host cutoff also calls termination
```

## 1. Approval and required inputs

The lead obtains a fresh offer receipt through the resource owner. The receipt
must name the CLI context/account, provider, on-demand CPU offer short ID, cloud
ID, region, CPU/RAM, GPU label, image, disk and **total** hourly price, including
storage and other charged components. Refresh availability and price immediately
before approval/use. A saved old quote is not a current offer. No interactive
selection, resource default, spot substitution or second create is allowed.

Prepare an execution JSON outside the checkout, in a private host directory.
All keys below are mandatory. Replace placeholders with reviewed, approved data.
The numeric example is the accuracy profile, not approval to execute it.

```json
{
  "cli_context": "REQUIRED_APPROVED_CONTEXT",
  "provider": "nebius",
  "offer_id": "REQUIRED_FRESH_SHORT_ID",
  "cloud_id": "REQUIRED_FRESH_CLOUD_ID",
  "gpu_label": "REQUIRED_EXACT_CLI_CPU_LABEL",
  "image": "REQUIRED_APPROVED_IMAGE",
  "disk_gb": 64,
  "vcpus": 8,
  "memory_gb": 32,
  "quoted_total_hourly": "REQUIRED_FRESH_TOTAL",
  "quote_checked_epoch": 0,
  "quote_reference": "REQUIRED_RECEIPT_REFERENCE",
  "harness_commit": "REQUIRED_REVIEWED_FULL_FORK_COMMIT",
  "max_seconds": 10800,
  "cutoff_seconds": 10680,
  "setup_seconds": 1800,
  "budget": "2.00",
  "hourly_ceiling": "0.50",
  "contingency_seconds": 3600,
  "minimum_memory_bytes": 30064771072,
  "minimum_free_bytes": 34359738368,
  "payload_sha256": "REQUIRED_64_HEX",
  "session_sha256": "REQUIRED_64_HEX",
  "environment_sha256": "REQUIRED_64_HEX"
}
```

`lifecycle.py` rejects missing inputs, invalid pins, nonpositive numeric limits,
a future/stale quote (older than 15 minutes), and over-ceiling arithmetic.
For this profile: 3 h × $0.50/h = $1.50. A further 1 h contingency makes $2.
That contingency is **not permission to extend the three-hour run**. A quote
above $0.50/h is refused even when its three-hour estimate is below $2.
`max_seconds` is the full paid interval. `cutoff_seconds` reserves two minutes
for deletion and verification. Start the timer before create, not after setup.

The CLI has no create `--max-price` and no create JSON response. Therefore this
is not an atomic price lock or an absolute billing guarantee during an API or
host outage. An approval must accept this limit. If that limit is unacceptable,
do not rent. The code checks the fresh quote before create and every reported
price afterward. A mismatch requests termination, not a higher ceiling.

## 2. Prepare a public-only payload before rental

Use a clean reviewed fork checkout. The lead supplies its full commit. Stage only
public source and approved public inputs. Never include `.git`, credentials,
private code, host environment dumps or the execution JSON in the pod payload.
Record the source commit and the selected source files' hashes. Include exactly
the reviewed session script and a complete `env.sh` with nonsecret explicit
settings. A later boundary must source that file again; it must not rely on a
previous SSH shell's exports.

`artifacts.py` packages plain tar, not a second compression layer. Each payload
file is SHA-256-bound in `evidence/file-manifest.json`. The external tar SHA-256
covers that manifest. There are at most 1,024 payload files, 4 GiB per file,
8 GiB total, and 16 MiB of allowed tar overhead. A payload directory must not
already contain `file-manifest.json`. Reject links, devices, traversal, duplicate
or noncanonical archive names. Keep bundles outside their source directory.

```bash
python3.12 -B study/storage/pod/artifacts.py   --source "$STAGED_PUBLIC_INPUTS" --bundle "$PRIVATE/inputs.tar"
# Put the resulting hash and the script/env hashes in the approved JSON.
# Validate its complete fields without invoking Prime or loading project code:
python3.12 -B -c 'import sys; sys.path.insert(0,"study/storage/pod"); from pathlib import Path; from lifecycle import configuration; configuration(Path(sys.argv[1]))' "$PRIVATE/execution.json"
```

## 3. One host owner, one creation

Use a host that stays awake and connected for the whole paid interval. Do not
run the owner inside the SSH connection to the rented pod. Keep the CLI
installation and account context unchanged until deletion is verified. Keep a managed process
handle and its full stdout/stderr. Do not daemonize or duplicate the owner.
The state directory must be new, absolute and private. Its files are local-only.

```bash
python3.12 -B study/storage/pod/lifecycle.py rent   --config "$PRIVATE/execution.json" --state "$PRIVATE/lease"
```

This command remains running. The lead starts it with the host's managed
background-process mechanism and retains its handle. Wait for `RENTED` and
`session.json`; do not treat a create timeout as permission to repeat create.

Create must start within the initial three-minute readiness window (or the
shorter setup allowance). After opening its logs, the helper checks cancellation
and the monotonic deadline again before spawning the CLI. An expired/cancelled
pre-mutation create records `not-created` and cancels its unused timer; it does
not create late or guess a resource. An uncertain CLI outcome is different and
retains the cutoff/reconciliation path.

The helper uses the explicit context, `--plain`, offer ID, cloud ID, unique name,
CPU, memory, disk, image and `--yes`. It supplies no stdin. CPU/no-team/provider
request-summary fields are checked against the pinned CLI output. The complete
paginated JSON list identifies exactly one unique owned name. The ID then stays
fixed. Unknown identity is an escalation, never permission to delete a guess.

The initial status and ACTIVE status compare every available requested field,
including total hourly price. CLI 0.17.0 omits offer/cloud IDs, spot status,
vCPU/RAM and boot-disk size. Save those omissions honestly. Echoed request fields
are not observed allocation proof. Require provider, image and price on ACTIVE,
then require `preflight.py` inside the pod before setup/inference. It records
CPU affinity, MemTotal, inherited cgroup memory limits and free disk. Exit 78
means immediate refusal and termination, not a different shape or smaller job.

## 4. Detached session and bounded copy

The reviewed session must be a complete noninteractive script. It must use
`set -euo pipefail`, close stdin, source `/srv/storage-pod/env.sh`, enforce setup
and analysis deadlines, and package receipts on failure. It must run the observed
shape preflight, not merely print capacity. Use `DEBIAN_FRONTEND=noninteractive`
and explicit package-manager yes flags. Do not use a pseudo-terminal.

```bash
python3.12 -B study/storage/pod/host_run.py   --state "$PRIVATE/lease" --destination "$PRIVATE/copied"   --ssh-key "$APPROVED_PRIVATE_SSH_KEY"   --session "$PRIVATE/session.sh" --environment "$PRIVATE/env.sh"   --payload "$PRIVATE/inputs.tar"
```

The helper checks clean local HEAD and the three approved transport hashes.
It uses BatchMode SSH/SCP and state-private known_hosts with first-use trust.
If first-use SSH trust is unacceptable, do not use this transport without a
separately verified host key. The remote command uses `nohup setsid`, redirects
logs, closes stdin, and waits for that one detached process. A lost SSH transport
or timeout does not kill or restart it. The host cutoff stays armed. A lead may
request early deletion, but must not repeat a session after uncertain SSH output.

The script must produce `receipts.tar` first, then `evidence.tar` only on success.
The small bundle uses an explicit JSON list of relative receipt paths, at most
1 MiB each and 32 MiB for the archive. Omitted diagnostics are listed. A small
bundle is **diagnostics only**, not a successful result. Keep the live log outside
the manifested directory; add a stable, explicitly partial 1 MiB tail on exit.

The full copy gets at most 420 seconds, fallback copy 60 seconds and early-log
copy 15 seconds, all shortened by the existing cutoff. Timed-out copy processes
are killed and reaped. The helper requests termination after a normal session
and copy attempt, including failure. Shape refusal skips bulky copying.

## 5. Stop once, prove deletion, then verify locally

Manual stop, signals and timer all feed the same locked owner:

```bash
python3.12 -B study/storage/pod/lifecycle.py stop --state "$PRIVATE/lease"
```

The timer callback itself invokes shutdown. It does not merely wake a potentially
blocked coordinator. The owner marks the identified lease attempted before its
single `prime ... pods terminate POD_ID --yes`. It executes deletion before
journal writes. Never retry an uncertain terminate. Read complete active/history
JSON, prove the ID absent from active and present once with `terminated_at`, and
record total cost, monotonic paid duration, stdout/stderr and exit status.
`done.json` with `state: deleted` proves observed deletion. `not-created` means
no create subprocess ran; it is not a deletion receipt. Absence is not success. A delayed or missing
history entry needs immediate operator escalation, not a second mutation.

Observe the owner's terminal result. A request to stop is not deletion proof.
A verified deletion with exceeded time/cost is still a failed attempt. On owner,
host or API failure, escalate to the resource owner with the exact ID/name and
all saved observations. Do not create a replacement pod.

After deletion, verify the external bundle hash, extract into a fresh directory,
and verify its exact manifested payload set:

```bash
cd "$PRIVATE/copied"
sha256sum --check evidence.tar.sha256
python3.12 -B "$REVIEWED_CHECKOUT/study/storage/pod/artifacts.py"   --extract evidence.tar --destination "$PRIVATE/verified"
python3.12 -B "$REVIEWED_CHECKOUT/study/storage/pod/artifacts.py"   --verify "$PRIVATE/verified/evidence/file-manifest.json"
```

For a small bundle, use its own tar/hash and a different fresh destination.
Never relabel partial evidence as full evidence. Final acceptance also needs the
run-specific input, runtime, command, hard-limit, OOM and scoring checks.

## Verification boundary

These are new helpers adapted from earlier lifecycle work, not an accepted paid
execution. Static checks do not prove concurrency, cloud deletion, fit or cost.
Before rental, the lead needs independent source review and fresh CI of positive,
refusal, timer/manual race, ambiguous-effect, lost-SSH, partial-copy and safe
archive paths on the final commit. No local study test or cloud command belongs
to runbook preparation.
