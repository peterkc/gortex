#!/usr/bin/env bash
set -euo pipefail

if (( $# < 2 )); then
  echo "usage: fetch.sh ABSOLUTE_PYTHON --release-tag TAG --launcher BIN ..." >&2
  exit 2
fi
python="$1"
shift
if [[ "$python" != /* || ! -x "$python" ]]; then
  echo "an explicit absolute Python 3.12+ executable is required" >&2
  exit 2
fi
here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
exec "$python" -B "$here/fetch.py" run "$@"
