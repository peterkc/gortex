#!/usr/bin/env bash
set -euo pipefail
# Required config names every data path, memory/wall limit and fresh output.
# Builds, downloads and source setup happen before this CI-only entrypoint.
if [[ $# -ne 2 || "$1" != "--config" || -z "$2" ]]; then
  echo 'usage: run.sh --config ABSOLUTE_CONFIG.json' >&2
  exit 2
fi
exec python3 -B "$(dirname "$0")/measure.py" --config "$2"
