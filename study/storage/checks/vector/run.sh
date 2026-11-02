#!/usr/bin/env bash
set -euo pipefail
if [[ "$#" -lt 2 || "$1" != /* ]]; then
  echo "Usage: run.sh /absolute/path/vector-check -data DIR -fixture-pin SHA -out NEWDIR -launcher BIN -cgroup-root ROOT -approval JSON -approval-pin SHA -wall-time DURATION -runner-bytes N -checker-bytes N -engine-bytes N -workers N -k N -candidates N" >&2
  exit 2
fi
binary="$1"
shift
exec "$binary" run "$@"
