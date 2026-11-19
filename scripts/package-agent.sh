#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
AGENT_OS="${GOOS:-$(go env GOOS)}"
AGENT_ARCH="${GOARCH:-$(go env GOARCH)}"
case "$AGENT_OS" in
  darwin|linux) ;;
  *) echo 'agent packages currently support macOS and Linux' >&2; exit 2 ;;
esac
if command -v sha256sum >/dev/null 2>&1; then
  HASH=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
  HASH=(shasum -a 256)
else
  echo 'agent packaging requires sha256sum or shasum' >&2
  exit 2
fi
PACKAGE="marionette-agent-$AGENT_OS-$AGENT_ARCH"
# A new staging directory avoids overwriting a previously unpacked installation.
STAGING="$(mktemp -d "$ROOT/build/agent-package.XXXXXX")"
trap 'rm -rf -- "$STAGING"' EXIT
mkdir "$STAGING/$PACKAGE"
CGO_ENABLED=0 GOOS="$AGENT_OS" GOARCH="$AGENT_ARCH" go build -o "$STAGING/$PACKAGE/marionette-agent" ./cmd/marionette-agent
cp build/marionette-device.jar "$STAGING/$PACKAGE/"
cp .env.agent.example "$STAGING/$PACKAGE/.env.example"
cp scripts/start-agent.sh "$STAGING/$PACKAGE/start-agent.sh"
cp scripts/render-launchd.py "$STAGING/$PACKAGE/render-launchd.py"
cp docs/AGENT_INSTALL.md "$STAGING/$PACKAGE/README.md"
cp docs/AGENT_INSTALL.md docs/UBUNTU_AGENT.md PROTOCOL.md "$STAGING/$PACKAGE/"
# These documents live one level deeper in the source tree than in the package.
sed 's|(../deploy/|(deploy/|g' docs/AUTOMATION.md > "$STAGING/$PACKAGE/AUTOMATION.md"
sed 's|(../PROTOCOL.md)|(PROTOCOL.md)|g' docs/API_MCP.md > "$STAGING/$PACKAGE/API_MCP.md"
mkdir "$STAGING/$PACKAGE/deploy"
cp deploy/kafka-local.properties "$STAGING/$PACKAGE/deploy/"
if [[ "$AGENT_OS" == linux ]]; then
  cp deploy/marionette-agent.service "$STAGING/$PACKAGE/"
  install -m 755 scripts/setup-ubuntu-agent.sh "$STAGING/$PACKAGE/setup-ubuntu-agent.sh"
fi
chmod 755 "$STAGING/$PACKAGE/start-agent.sh"
COPYFILE_DISABLE=1 tar -czf "build/$PACKAGE.tar.gz" -C "$STAGING" "$PACKAGE"
# Record basenames so the manifest can be checked from the distribution directory.
(
  cd "$ROOT/build"
  "${HASH[@]}" marionette-agent-"$AGENT_OS"-*.tar.gz > "$STAGING/SHA256SUMS"
)
mv "$STAGING/SHA256SUMS" "build/marionette-agent-$AGENT_OS-SHA256SUMS"
echo "build/$PACKAGE.tar.gz"
