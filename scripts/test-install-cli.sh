#!/bin/sh
# Exercise installation, upgrades and checksum failures with real release ZIPs.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
fixtures=$(cd "${1:-$root/build/cli-release}" && pwd)
sandbox=$(mktemp -d "${TMPDIR:-/tmp}/marionette-install-test.XXXXXX")
trap 'rm -rf "$sandbox"' EXIT
mkdir -p "$sandbox/mock-bin" "$sandbox/zsh"
cat > "$sandbox/mock-bin/curl" <<'SH'
#!/bin/sh
set -eu
output=
url=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output) output=$2; shift 2 ;;
        --write-out|--proto) shift 2 ;;
        --*) shift ;;
        *) url=$1; shift ;;
    esac
done
case "$url" in
    */releases/latest) printf 'https://github.com/h053698/marionette/releases/tag/cli-v0.0.0-test' ;;
    */download/cli-v0.0.0-test/*)
        cp "$FIXTURES/${url##*/}" "$output"
        if [ "${TAMPER:-}" = 1 ] && [ "${url##*/}" != SHA256SUMS ]; then printf 'corrupted' >> "$output"; fi
        ;;
    *) printf 'Unexpected URL: %s\n' "$url" >&2; exit 1 ;;
esac
SH
chmod +x "$sandbox/mock-bin/curl"
export FIXTURES="$fixtures"
export MARIONETTE_INSTALL_DIR="$sandbox/path with ' quote/bin"
export MARIONETTE_PROFILE="$sandbox/zsh/.zshrc"
export PATH="$sandbox/mock-bin:$PATH"
export SHELL=/bin/zsh
unset MARIONETTE_VERSION

sh "$root/scripts/install-cli.sh"
binary=$MARIONETTE_INSTALL_DIR/marionette
[ -x "$binary" ]
version=$("$binary" --version)
# A new interactive zsh must find the installed command through its saved PATH.
ZDOTDIR="$sandbox/zsh" zsh -ic 'marionette --version' | grep -Fqx "$version"
original=$(shasum -a 256 "$binary")
profile=$(cat "$MARIONETTE_PROFILE")
sh "$root/scripts/install-cli.sh"
[ "$(cat "$MARIONETTE_PROFILE")" = "$profile" ]
[ "$(shasum -a 256 "$binary")" = "$original" ]

if TAMPER=1 sh "$root/scripts/install-cli.sh" > "$sandbox/failure.log" 2>&1; then
    printf 'Installer accepted a corrupted archive.\n' >&2
    exit 1
fi
grep -Fq 'SHA-256 verification failed' "$sandbox/failure.log"
[ "$(cat "$MARIONETTE_PROFILE")" = "$profile" ]
[ "$(shasum -a 256 "$binary")" = "$original" ]

export SHELL=/bin/bash
export MARIONETTE_PROFILE="$sandbox/bash_profile"
sh "$root/scripts/install-cli.sh"
bash -c '. "$MARIONETTE_PROFILE"; marionette --version' | grep -Fqx "$version"
printf 'macOS installer: install, startup PATH, upgrade and checksum failure passed.\n'
