#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SDK="${ANDROID_SDK_ROOT:-${ANDROID_HOME:-$ROOT/.tools/android-sdk}}"
JDK="${JAVA_HOME:-$ROOT/.tools/jdk}"
ANDROID_JAR="$SDK/platforms/android-34/android.jar"
D8_JAR="$SDK/build-tools/34.0.0/lib/d8.jar"
for required in "$JDK/bin/javac" "$ANDROID_JAR" "$D8_JAR"; do
  if [[ ! -f "$required" ]]; then
    echo "Missing: $required. Run make tools, or set JAVA_HOME and ANDROID_SDK_ROOT." >&2
    exit 1
  fi
done
mkdir -p "$ROOT/build"
WORK="$(mktemp -d "$ROOT/build/device-build.XXXXXX")"
trap 'rm -rf -- "$WORK"' EXIT
mkdir -p "$WORK/classes" "$WORK/dex"
find "$ROOT/device/src" -name '*.java' -print | sort > "$WORK/sources.txt"
if [[ ! -s "$WORK/sources.txt" ]]; then
  echo 'No Android sources found.' >&2
  exit 1
fi
"$JDK/bin/javac" -encoding UTF-8 -source 8 -target 8 -bootclasspath "$ANDROID_JAR" \
  -d "$WORK/classes" @"$WORK/sources.txt"
"$JDK/bin/jar" cf "$WORK/device-classes.jar" -C "$WORK/classes" .
"$JDK/bin/java" -cp "$D8_JAR" com.android.tools.r8.D8 \
  --min-api 30 --lib "$ANDROID_JAR" --output "$WORK/dex" "$WORK/device-classes.jar"
"$JDK/bin/jar" cf "$WORK/marionette-device.jar" -C "$WORK/dex" classes.dex
mv "$WORK/marionette-device.jar" "$ROOT/build/marionette-device.jar"
echo "Built $ROOT/build/marionette-device.jar"
