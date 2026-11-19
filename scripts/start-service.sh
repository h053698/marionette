#!/usr/bin/env bash
set -euo pipefail
umask 077
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMPONENT="${1:-}"
case "$COMPONENT" in
  backend|web|runner) ;;
  *) echo 'usage: scripts/start-service.sh {backend|web|runner} [flags...]' >&2; exit 2 ;;
esac
shift
cd "$ROOT"
ENV_FILE="$ROOT/.env.$COMPONENT"
if [[ ! -f "$ENV_FILE" ]]; then
  echo "missing $ENV_FILE; copy deploy/$COMPONENT.env.example and configure it first" >&2
  exit 1
fi
set -a
. "$ENV_FILE"
set +a
if [[ ! -x "$ROOT/build/marionette-$COMPONENT" ]]; then
  echo "missing executable: $ROOT/build/marionette-$COMPONENT" >&2
  exit 1
fi
ARGS=()
case "$COMPONENT" in
  backend)
    STATE_DIR="${MARIONETTE_STATE_DIR:-$ROOT/.marionette}"
    mkdir -p "$STATE_DIR"
    ARGS=(--listen "${MARIONETTE_LISTEN:-127.0.0.1:8080}"
      --token-file "$STATE_DIR/tokens.json" --agent-file "$STATE_DIR/agents.json"
      --job-file "$STATE_DIR/jobs.json" --user-file "$STATE_DIR/users.json")
    ;;
  web)
    ARGS=(--listen "${MARIONETTE_LISTEN:-127.0.0.1:3000}" --web-dir "$ROOT/web")
    ;;
  runner) exec "$ROOT/build/marionette-runner" "$@" ;;
esac
exec "$ROOT/build/marionette-$COMPONENT" "${ARGS[@]}" "$@"
