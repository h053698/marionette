#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
if [[ -d "$ROOT/.tools/android-sdk/platform-tools" ]]; then
  export PATH="$ROOT/.tools/android-sdk/platform-tools:$PATH"
fi
COMPONENT="${1:-backend}"
if [[ $# -gt 0 ]]; then shift; fi
case "$COMPONENT" in
  backend|agent|web) ;;
  *) echo 'usage: scripts/run.sh {backend|agent|web} [flags...]' >&2; exit 2 ;;
esac
cd "$ROOT"
exec go run "./cmd/marionette-$COMPONENT" "$@"
