#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
bash "${CHAIN_DIR:-$ROOT/chain}/nodes/127.0.0.1/stop_all.sh"
