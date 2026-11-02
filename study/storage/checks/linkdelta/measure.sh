#!/usr/bin/env bash
set -euo pipefail
if [[ $# -ne 3 ]]; then
  printf 'Usage: %s /absolute/linkdelta /absolute/operator-config.json CONFIG_SHA256\n' "$0" >&2
  exit 2
fi
if [[ "$1" != /* || "$2" != /* || ! -x "$1" ]]; then
  printf 'Absolute executable and config paths are required.\n' >&2
  exit 2
fi
exec "$1" measure -config "$2" -config-pin "$3"
