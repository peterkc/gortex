#!/usr/bin/env bash
# D14 only. Exact offer, profile and files require lead/user approval first.
set -euo pipefail
cd /srv/storage-pod
. ./env.sh
mkdir evidence
CGROOT=
CAPACITY_GROUP=
IDENTITY=
W=/srv/storage-pod/input/evidence

remove_scope() {
  test "$(stat -c '%d:%i' "$CGROOT")" = "$IDENTITY" || return 1
  echo 1 > "$1/cgroup.kill" || return 1
  python3.12 -B - "$1" <<'PY'
import select
import sys
import time
from pathlib import Path
path = Path(sys.argv[1])
with (path / "cgroup.events").open() as stream:
    poller = select.poll()
    poller.register(stream, select.POLLPRI | select.POLLERR)
    deadline = time.monotonic() + 30
    while True:
        stream.seek(0)
        events = dict(line.split() for line in stream)
        if events["populated"] == "0":
            break
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("owned cgroup did not become empty")
        poller.poll(max(1, int(remaining * 1000)))
path.rmdir()
PY
}

finish() {
  result=$?
  trap - EXIT
  set +e
  if test -n "$CGROOT"; then
    if test "$(stat -c '%d:%i' "$CGROOT")" = "$IDENTITY"; then
      for group in "$CGROOT"/*; do
        test "$group" = "$CGROOT/owner" && continue
        test -d "$group" || continue
        name=${group##*/}
        cat "$group/memory.peak" > "evidence/$name-memory-peak.txt"
        cat "$group/memory.events" > "evidence/$name-memory-events.txt"
        remove_scope "$group" || result=1
      done
      cat "$CGROOT/owner/memory.peak" > evidence/controller-peak-before-pack.txt
      cat "$CGROOT/owner/memory.events" > evidence/controller-events-before-pack.txt
    else
      echo 'Owned cgroup identity changed; cleanup refused' >&2
      result=1
    fi
  fi
  if test -d "$W/results"; then cp -a "$W/results" evidence/ || result=1; fi
  if test -d "$W/corpus"; then cp -a "$W/corpus" evidence/ || result=1; fi
  if test -f "$W/file-manifest.json"; then
    cp "$W/file-manifest.json" evidence/input-file-manifest.json || result=1
  fi
  if test -f "$W/model-limit.json"; then
    cp "$W/model-limit.json" evidence/model-limit.json || result=1
  fi
  if test -f "$W/runtime/receipt.json"; then
    cp "$W/runtime/receipt.json" evidence/runtime-receipt.json || result=1
  fi
  printf 'session_exit=%s\n' "$result" > evidence/session-exit.txt
  tail -c 1048576 session.log > evidence/session-log-tail.partial.txt
  python3.12 -B - <<'PY'
import json
from pathlib import Path
root = Path("evidence")
paths = ["session-exit.txt", "session-log-tail.partial.txt", "host-before.json",
         "host-after.json", "pins.txt", "go-env.json", "tool-hashes.txt",
         "controller-peak-before-pack.txt", "controller-events-before-pack.txt",
         "gcc-version.txt", "outer-cgroup-limits.txt", "bootstrap-memory-peak.txt",
         "bootstrap-memory-events.txt", "runtime-receipt.json",
         "model-limit.json"]
paths += [str(p.relative_to(root)) for p in
          sorted(root.glob("results/*-limits/launch/receipt.json"))]
Path("small-list.json").write_text(json.dumps(paths))
PY
  timeout --kill-after=5 30 python3.12 -B artifacts.py \
    --source evidence --bundle receipts.tar --small-list small-list.json
  small=$?
  if test "$small" -ne 0; then result=1; fi
  if test "$result" -eq 0; then
    timeout --kill-after=5 240 python3.12 -B artifacts.py \
      --source evidence --bundle evidence.tar || result=1
  fi
  # The bounded controller exits normally. Its private empty directories live
  # only until the host owner verifies deletion of the entire pod.
  exit "$result"
}
trap finish EXIT

test "$(id -u)" -eq 0
test "$(uname -m)" = x86_64
. /etc/os-release
test "$ID" = ubuntu && test "$VERSION_ID" = 24.04
command -v python3.12
python3.12 -B - <<'PY'
import json
import os
from decimal import Decimal
value = json.load(open("session-inputs.json"))
if (value["harness_commit"] != os.environ["HARNESS_COMMIT"]
        or value["provider"] != "nebius"
        or value["vcpus"] != 8 or value["memory_gb"] != 32
        or value["minimum_memory_bytes"] < 28 * 1024 ** 3
        or value["minimum_free_bytes"] < 32 * 1024 ** 3
        or value["max_seconds"] > 10800 or value["setup_seconds"] > 1800
        or value["cutoff_epoch"] - value["started_epoch"] > 10680
        or Decimal(str(value["budget"])) > 2
        or Decimal(str(value["hourly_ceiling"])) > Decimal("0.50")):
    raise SystemExit(78)
PY
python3.12 -B preflight.py --inputs session-inputs.json \
  --directory . --receipt evidence/host-before.json || exit 78
# The uniquely created root is private to this session; no shared-root cleanup.
CAPACITY_GROUP=$(python3.12 -c 'import json; print(json.load(open("evidence/host-before.json"))["capacity_cgroup"])')
CGROOT="$CAPACITY_GROUP/storage-accuracy-$$"
echo +memory > "$CAPACITY_GROUP/cgroup.subtree_control"
mkdir "$CGROOT"
IDENTITY=$(stat -c '%d:%i' "$CGROOT")
echo 27917287424 > "$CGROOT/memory.max"
echo 0 > "$CGROOT/memory.swap.max"
echo +memory > "$CGROOT/cgroup.subtree_control"
mkdir "$CGROOT/owner" "$CGROOT/bootstrap"
echo 536870912 > "$CGROOT/owner/memory.max"
echo 0 > "$CGROOT/owner/memory.swap.max"
echo 1 > "$CGROOT/owner/memory.oom.group"
echo 4294967296 > "$CGROOT/bootstrap/memory.max"
echo 0 > "$CGROOT/bootstrap/memory.swap.max"
echo 1 > "$CGROOT/bootstrap/memory.oom.group"
echo $$ > "$CGROOT/owner/cgroup.procs"
for group in "$CGROOT" "$CGROOT/owner" "$CGROOT/bootstrap"; do
  printf '%s\n' "$group"
  for field in memory.max memory.swap.max memory.oom.group; do
    printf '%s=' "$field"
    cat "$group/$field"
  done
done > evidence/outer-cgroup-limits.txt
export CGROOT W CAPACITY_GROUP

setup() {
  set -euo pipefail
  cd /srv/storage-pod
  . ./env.sh
  echo $$ > "$CGROOT/bootstrap/cgroup.procs"
  sha256sum --check inputs.tar.sha256
  python3.12 -B artifacts.py --extract inputs.tar --destination input
  python3.12 -B artifacts.py --verify "$W/file-manifest.json"
  python3.12 -B preflight.py --inputs session-inputs.json \
    --directory . --capacity-cgroup "$CAPACITY_GROUP" \
    --receipt evidence/host-after.json || exit 78
  apt-get update
  apt-get install -y ca-certificates python3.12-venv build-essential
  cd "$W"
  printf '%s  go.tar.gz\n' \
    65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40 \
    | sha256sum --check -
  printf '%s  uv.tar.gz\n' \
    f68d291c75783122dad58ab77def0e13f3233f358731db5d6d7e8715603b928f \
    | sha256sum --check -
  tar -xzf go.tar.gz
  tar -xzf uv.tar.gz
  export PATH="$W/go/bin:$W/uv-x86_64-unknown-linux-gnu:$PATH"
  test "$(go version)" = 'go version go1.27.3 linux/amd64'
  uv --version
  mkdir bin
  ( cd harness/study/storage/harness
    go build -trimpath -p=1 -o "$W/bin/limited-command" ./cmd/limited-command )
  ( cd harness/study/storage/onnx
    go build -trimpath -p=1 -o "$W/bin/onnxprobe" . )
  export UV_PROJECT_ENVIRONMENT="$W/bridge-env"
  uv sync --frozen --project harness/study/storage/pod-cost \
    --python /usr/bin/python3.12
  export UV_PROJECT_ENVIRONMENT="$W/score-env"
  uv sync --frozen --project harness/study/storage/neon-eval \
    --python /usr/bin/python3.12
  go env -json > /srv/storage-pod/evidence/go-env.json
  gcc --version > /srv/storage-pod/evidence/gcc-version.txt
  test "$(gcc -dumpmachine)" = x86_64-linux-gnu
  python3.12 -B - <<'PY'
import json
value = json.load(open("/srv/storage-pod/evidence/go-env.json"))
expected = {"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "1",
            "CGO_CFLAGS": "-O2 -g -fno-strict-aliasing",
            "CGO_CXXFLAGS": "-O2 -g -fno-strict-aliasing",
            "GOFLAGS": "-mod=readonly -p=2", "GOWORK": "off",
            "GOTOOLCHAIN": "local"}
if any(value[key] != expected[key] for key in expected):
    raise SystemExit("Go environment differs from pinned recipe")
PY
  sha256sum bin/* runtime/libonnxruntime.so.1.31.0 go.tar.gz uv.tar.gz \
    > /srv/storage-pod/evidence/tool-hashes.txt
}
export -f setup
started=$(python3.12 -c 'import json; print(int(json.load(open("session-inputs.json"))["started_epoch"]))')
remaining=$(( started + 1800 - $(date +%s) ))
test "$remaining" -gt 0
timeout --kill-after=5 "$remaining" bash --noprofile --norc -c setup
# Bootstrap's descendants must be gone before any measured stage starts.
cat "$CGROOT/bootstrap/memory.peak" > evidence/bootstrap-memory-peak.txt
cat "$CGROOT/bootstrap/memory.events" > evidence/bootstrap-memory-events.txt
remove_scope "$CGROOT/bootstrap"
export PATH="$W/go/bin:$W/uv-x86_64-unknown-linux-gnu:$PATH"
export LD_LIBRARY_PATH="$W/runtime"
printf 'harness=%s\nfreeze=%s\npaid_start=%s\n' \
  "$HARNESS_COMMIT" dd6bddf4e92f1357c2d0f49bbd602c26d30c44d08b19d83aeb8dec3de4f3b401   "$started" > evidence/pins.txt
deadline=$(date -u -d "@$(( started + 9600 ))" +%Y-%m-%dT%H:%M:%SZ)
python3.12 -B "$W/harness/study/storage/pod/accuracy.py" \
  --root "$W" --source "$HARNESS_COMMIT" --deadline "$deadline" \
  --cgroup-root "$CGROOT"
