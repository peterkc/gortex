# Full-source CHA on an approved CPU pod

This is the evidence path for FR-010 and AC-011. The `seam-callgraph` workflow
runs fixture tests only. It does **not** produce either requirement's evidence.
A completed full-source report is still subject to the lead's acceptance review.

**Do not create or operate a pod until the plan amendment is approved.**
The lead, not this worker, owns the pod, its deadline, copy-out and deletion.
The approved ceiling is one hour and $2 total, including setup and deletion.
Use only the approved Nebius on-demand CPU offer: 16 vCPU and 64 GB RAM.
No GPU, persistent disk, team sharing, larger instance or automatic retry.

```text
approve + pin -> rent + start clock -> root/cgroup preflight -> build
  -> bounded full CHA -> bounded filter -> cleanup -> hash + copy + verify
  -> delete pod + verify deletion
any failure -> preserve partial evidence -> copy if time permits -> delete
```

## Before renting

The lead records the exact offer, Ubuntu 24.04 image identifier, CPU/RAM, disk,
region, on-demand price, total hourly price including storage, and pod ID in its
local execution record. Recheck the live quote. Refuse a total price above $2
for the maximum one-hour allocation, or any different configuration.
The Ubuntu image must expose a writable cgroup-v2 hierarchy with the memory
controller, `memory.peak` and `cgroup.kill`. A root login alone is not proof.
Do not bypass a container's missing delegation or change the host configuration.

Supply `HARNESS_COMMIT`: the **full 40-character reviewed commit** on
`peterkc/gortex` containing this runbook and its scripts. The lead obtains it from
its reviewed checkout before renting. Do not use a moving branch name, the old
round-1 head, or the upstream source commit as the harness revision.
The final commit does not exist while this runbook is being authored; this is a
required input, not a default that can silently select unreviewed code.

Pins:

- Source commit: `7096db70df5ab7d83e20a28d9ccfd0994812acf9`.
- Source tree: `829d2461eb61b73069b4206a137346fc338b02c6`.
- Go: `go1.27.3.linux-amd64.tar.gz` from `https://go.dev/dl/`.
- Published Go SHA-256:
  `65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40`.
- x/tools: `v0.52.0`, resolved in the source module, with `-mod=readonly`.
- Algorithm and scope: CHA, tests included, empty tags/profiles, full `./...`.

The fixed `pod` profile uses a **52 GiB hard cap** and `GOMEMLIMIT=46GiB`.
It refuses MemTotal below 56 GiB. This leaves at least 4 GiB outside the payload
and also fits a nominal 64 decimal GB offer. Both analysis and filter use this
cap. Swap is disabled for each payload; `memory.oom.group=1` is required.
The soft limit is not an RSS or whole-process-tree limit.

Each payload file is limited to 4 GiB by Bash `ulimit -f` (1,024-byte blocks).
The checked stdout capture fails rather than accepting truncated edges.
Preflight requires at least 10 GiB free for raw output, packaging and headroom.
It records `df`, MemTotal, nproc, swaps and cgroup membership. Source/cache/build
space is additional; retain enough disk in the selected offer without exceeding
its approved total price. Stop on any disk refusal or write failure.

Time allocation, measured from the start of the paid allocation:

- Setup and build: finish by minute 10, or stop before analysis.
- Analysis: 27 minutes, including package inventory and cold tool compilation.
- Filter: 6 minutes. Both deadlines come from the fixed profile.
- Metadata checks and native shutdown overhead: reserve 3 minutes.
  Each metadata command has a 15-second timeout.
- Cleanup, compression and packing: at most 2 minutes.
- Copy-out and verification: at most 10 minutes.
- Delete and verify deletion: reserve the final 2 minutes.

The lead uses one host-side deadline owner. At minute 58, it stops copying and
starts deletion even if evidence is incomplete. A pod shutdown or broken SSH
connection does not stop billing. Do not wait for a worker message to delete it.
If setup misses its allocation, do not borrow the copy/deletion reserve.

## Fresh Ubuntu pod: setup and refusal checks

Run as root through the lead's approved SSH connection. Keep credentials and
private SSH keys on the lead's host, outside this directory and the bundle.
Set the paid start epoch to the recorded creation time, not the login time.
The following variables are mandatory operator inputs:

```bash
export HARNESS_COMMIT='<full-reviewed-harness-commit>'
export POD_START_EPOCH='<paid-allocation-start-unix-seconds>'
```

Then run:

```bash
set -euo pipefail
export LC_ALL=C
case "$HARNESS_COMMIT" in (*[!0-9a-f]*|'') exit 2;; esac
test "${#HARNESS_COMMIT}" -eq 40
case "$POD_START_EPOCH" in (*[!0-9]*|'') exit 2;; esac
test "$(id -u)" -eq 0
test "$(uname -m)" = x86_64
test "$(stat -fc %T /sys/fs/cgroup)" = cgroup2fs
test -w /sys/fs/cgroup/cgroup.subtree_control
grep -qw memory /sys/fs/cgroup/cgroup.controllers
apt-get update
apt-get install -y ca-certificates curl git python3.12 build-essential gzip
mkdir /srv/gx-seam
cd /srv/gx-seam
mkdir evidence cache
curl --fail --location --output go.tar.gz \
  https://go.dev/dl/go1.27.3.linux-amd64.tar.gz
printf '%s  go.tar.gz\n' \
  65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40 \
  | sha256sum --check -
tar -xzf go.tar.gz
export PATH="/srv/gx-seam/go/bin:$PATH"
export GOTOOLCHAIN=local GOWORK=off GOFLAGS='-mod=readonly -p=2'
export GOMAXPROCS=2 GOMEMLIMIT=46GiB CGO_ENABLED=1 GOOS=linux GOARCH=amd64
export CGO_CFLAGS='-O2 -g -fno-strict-aliasing'
export CGO_CXXFLAGS='-O2 -g -fno-strict-aliasing'
export PYTHONDONTWRITEBYTECODE=1
export GOCACHE=/srv/gx-seam/cache/build GOMODCACHE=/srv/gx-seam/cache/mod
export SOURCE_COMMIT=7096db70df5ab7d83e20a28d9ccfd0994812acf9
export SOURCE_TREE=829d2461eb61b73069b4206a137346fc338b02c6
mkdir harness source
git -C harness init
git -C harness fetch --depth=1 https://github.com/peterkc/gortex.git "$HARNESS_COMMIT"
git -C harness checkout --detach FETCH_HEAD
test "$(git -C harness rev-parse HEAD)" = "$HARNESS_COMMIT"
git -C source init
git -C source fetch --depth=1 https://github.com/zzet/gortex.git "$SOURCE_COMMIT"
git -C source checkout --detach FETCH_HEAD
test "$(git -C source rev-parse HEAD)" = "$SOURCE_COMMIT"
test "$(git -C source rev-parse HEAD^{tree})" = "$SOURCE_TREE"
grep -Fx 'go 1.27.3' source/go.mod
grep -Fx $'\tgolang.org/x/tools v0.52.0' source/go.mod
printf 'harness=%s\nsource=%s\ntree=%s\npaid_start=%s\n' \
  "$HARNESS_COMMIT" "$SOURCE_COMMIT" "$SOURCE_TREE" "$POD_START_EPOCH" \
  > evidence/pins.txt
sha256sum go.tar.gz > evidence/go-download.sha256
python3.12 -B harness/study/storage/seam/run_limited.py preflight \
  --profile pod --output /srv/gx-seam/evidence
cd harness/study/storage/harness
go build -trimpath -p=1 -o /srv/gx-seam/evidence/limited-command ./cmd/limited-command
cd /srv/gx-seam
sha256sum evidence/limited-command > evidence/launcher.sha256
```

The `preflight` command refuses insufficient memory/disk or a different soft
limit. Do not edit its profile to pass. The native launcher reads back all kernel
limits and refuses missing delegation, `memory.peak` or writable `cgroup.kill`.
A fresh pod lacking those features is unsuitable; stop and delete it.

Create a private delegated root. Leave this SSH shell outside it so it can copy
and clean up after a contained failure. The pipeline owner runs in a sibling
`owner` group; only its payload enters the hard-limited launcher scope.

```bash
export RUN_ID=pod ATTEMPT=1
export GXSEAM_CGROUP_ROOT=/sys/fs/cgroup/gxseam-pod-1
printf '+memory\n' > /sys/fs/cgroup/cgroup.subtree_control
mkdir "$GXSEAM_CGROUP_ROOT"
printf '+memory\n' > "$GXSEAM_CGROUP_ROOT/cgroup.subtree_control"
mkdir "$GXSEAM_CGROUP_ROOT/owner"
stat -c '%d %i' "$GXSEAM_CGROUP_ROOT" > evidence/cgroup-root.identity
elapsed=$(( $(date +%s) - POD_START_EPOCH ))
test "$elapsed" -ge 0
test "$elapsed" -le 600
```

## Run once, with the exact full-source command

The driver invokes this argv from `/srv/gx-seam/source` through the real launcher.
`FORMAT` is the checked-in 11-field TSV template printed by the filter.
No alternate package pattern, algorithm, tests flag or sample is permitted.

```text
go run golang.org/x/tools/cmd/callgraph -algo=cha -test=true -tags= \
  -cpuprofile= -memprofile= "-format=$FORMAT" ./...
```

Inspect the literal format before starting if needed:

```bash
python3.12 -B harness/study/storage/seam/filter_callgraph.py --print-format
```

Run the checked-in driver once. Its child records the actual argv, kernel values
and profile. The filter compares that argv with the native bound receipt and
with the required options. It rejects a changed commit/tree/schema or toolchain.
The pipeline verifies the source Git checkout is clean and x/tools resolves to
`v0.52.0` in-module with `-mod=readonly` before launching analysis.

```bash
set +e
bash --noprofile --norc -c '
  echo $$ > "$GXSEAM_CGROUP_ROOT/owner/cgroup.procs"
  exec python3.12 -B /srv/gx-seam/harness/study/storage/seam/run_pipeline.py \
    --profile pod --source /srv/gx-seam/source \
    --artifact /srv/gx-seam/evidence \
    --launcher /srv/gx-seam/evidence/limited-command \
    --root "$GXSEAM_CGROUP_ROOT" --go /srv/gx-seam/go/bin/go \
    --commit "$SOURCE_COMMIT" --tree "$SOURCE_TREE"
' > evidence/pipeline.log 2>&1
status=$?
set -e
printf 'pipeline_exit=%s\n' "$status" > evidence/pipeline-exit.txt
```

Any nonzero exit means **no completed map**. OOM means stop, no retry and no cap
raise. A deadline, pin mismatch, disk/file limit, missing symbol, missing cleanup,
or invalid receipt also stops analysis. Do not narrow `./...` or drop tests.
Keep the native failure receipts and partial edges. Never rename partial evidence
as completed or accept a report from a different profile.

## Cleanup, pack and copy out

Run these even after failure, while the SSH shell remains outside the group.
The launcher removes its own scope; parent cleanup only removes empty,
inode-matched directories. If cleanup refuses, preserve that error. Do not force
remove a replaced or populated scope. Pod deletion is still required.

```bash
set +e
python3.12 -B harness/study/storage/seam/run_limited.py cleanup \
  --root "$GXSEAM_CGROUP_ROOT" --identity evidence/cgroup-root.identity \
  --run-id "$RUN_ID" --attempt "$ATTEMPT" \
  > evidence/cleanup.log 2>&1
cleanup_status=$?
set -e
printf 'cleanup_exit=%s\n' "$cleanup_status" > evidence/cleanup-exit.txt
timeout 120 bash --noprofile --norc -c '
  set -euo pipefail
  if test -f evidence/edges.tsv; then gzip -n evidence/edges.tsv; fi
  if test -f evidence/edges.tsv.partial; then gzip -n evidence/edges.tsv.partial; fi
  python3.12 -B harness/study/storage/seam/package_artifacts.py \
    --source /srv/gx-seam/evidence --bundle /srv/gx-seam/seam-evidence.tar.gz
'
```

A nonzero cleanup status is a recorded refusal, not success. Do not force it.
If compression or packing fails, copy the small receipts and logs first if time
permits. Reserve the deletion deadline even if copy-out fails.

The tarball contains full or labelled partial raw edges, package inventory,
report/summary, streamed diagnostics, native analysis/filter receipts and logs,
actual kernel snapshots, binding/policy scripts, capacity/df facts, launcher and
its hash, source/tool/harness pins, exit status and cleanup output.
`file-manifest.json` records SHA-256 for **every payload file**. The manifest
cannot hash itself; the external bundle SHA-256 covers it and the whole archive.

On the lead host, use the SSH address/port from the approved pod record.
Set `COPY_DIRECTORY` to an absolute, empty local destination. Do not put credentials in commands or the bundle.
Use the managed `scp` process with an eight-minute timeout, leaving two minutes
within the copy allocation for verification. Substitute the connection values:

```bash
python3 -c '
import subprocess, sys
port, target, destination = sys.argv[1:]
prefix = target + ":/srv/gx-seam/seam-evidence.tar.gz"
subprocess.run(["scp", "-P", port, prefix, prefix + ".sha256", destination],
               timeout=480, check=True)
' "$POD_SSH_PORT" "$POD_SSH_TARGET" "$COPY_DIRECTORY/"
```

From the copied bundle directory, check the bundle SHA before extracting:

```bash
shasum -a 256 --check seam-evidence.tar.gz.sha256
tar -xzf seam-evidence.tar.gz
cd "$REVIEWED_HARNESS_CHECKOUT"
python3 study/storage/seam/package_artifacts.py \
  --verify "$COPY_DIRECTORY/evidence/file-manifest.json"
```

Set `REVIEWED_HARNESS_CHECKOUT` to the same reviewed checkout used to select
`HARNESS_COMMIT`. Extract only into the empty copy destination. The verifier rejects changed or unlisted payload files.
A completed result needs
both native receipts completed/removed, zero OOM kills and zero exits, correct
52 GiB/swap-off kernel facts, correct pins/options, and a completed filter receipt.
Record measured analysis/filter peaks, MemTotal, package and edge counts. Verify
raw SHA from decompressed edges against the report's raw SHA. Do not call the
fixture CI artifact full-source evidence.

## Delete and verify

After verified copy-out, **delete the exact pod ID immediately** through the lead's
already authenticated Prime CLI. Also delete on any failure, or at the minute-58
cutoff even if evidence could not be copied. Do not delete unrelated pods.

```bash
prime pods terminate "$POD_ID"
prime pods status "$POD_ID"
```

Confirm deletion at the CLI prompt. Verify the pod is terminated, not ACTIVE or
STOPPED-but-billable; the documented API moves deleted pods to history. Record the
terminal status and elapsed paid time. If status no longer lists a deleted ID,
verify that exact ID in Prime's pod history. A not-found error alone is not proof.
Do not repeat an ambiguous termination; inspect history first. If deletion fails
or remains unclear, stop other work and escalate to the user/provider before the
one-hour ceiling. Do not assume SSH loss, analysis failure or CLI timeout deleted it.

## Operator references

- Go release checksums: <https://go.dev/dl/?mode=json&include=all>.
- Prime pod SSH/status/termination:
  <https://docs.primeintellect.ai/cli-reference/provision-gpu>.
- Prime deletion and history semantics:
  <https://docs.primeintellect.ai/api-reference/managing-pods>.

These references document operations, not authorization to perform them.
