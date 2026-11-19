#!/usr/bin/env bash
set -euo pipefail
umask 077
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [[ -f "$SCRIPT_DIR/marionette-agent" ]]; then
  AGENT_DIR="$SCRIPT_DIR"
  AGENT_BIN="$AGENT_DIR/marionette-agent"
  AGENT_JAR="$AGENT_DIR/marionette-device.jar"
  AGENT_ENV="$AGENT_DIR/.env"
else
  AGENT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
  AGENT_BIN="$AGENT_DIR/build/marionette-agent"
  AGENT_JAR="$AGENT_DIR/build/marionette-device.jar"
  AGENT_ENV="$AGENT_DIR/.env.agent"
fi
# A service manager does not inherit the terminal's working directory or shell setup.
# Keep the default policy/AVD directory stable across manual and service startup.
cd "$AGENT_DIR"
if [[ -f "$AGENT_ENV" ]]; then
  set -a
  . "$AGENT_ENV"
  set +a
fi
ARGS=(--device-jar "$AGENT_JAR")
SDK_ROOT="${MARIONETTE_ANDROID_SDK:-${ANDROID_HOME:-${ANDROID_SDK_ROOT:-}}}"
if [[ -n "${MARIONETTE_ADB:-}" ]]; then
  ARGS+=(--adb "$MARIONETTE_ADB")
elif [[ -n "$SDK_ROOT" ]]; then
  ARGS+=(--adb "$SDK_ROOT/platform-tools/adb")
fi
if [[ -n "$SDK_ROOT" ]]; then ARGS+=(--android-sdk "$SDK_ROOT"); fi
if [[ -n "${MARIONETTE_STATE_DIR:-}" ]]; then ARGS+=(--state-dir "$MARIONETTE_STATE_DIR"); fi
if [[ -n "${MARIONETTE_MAX_EMULATORS:-}" ]]; then ARGS+=(--max-emulators "$MARIONETTE_MAX_EMULATORS"); fi
if [[ ! -x "$AGENT_BIN" || ! -f "$AGENT_JAR" ]]; then
  echo "agent binary or Android JAR is missing from $AGENT_DIR; build or extract the complete package first" >&2
  exit 1
fi
exec "$AGENT_BIN" "${ARGS[@]}" "$@"
