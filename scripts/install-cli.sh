#!/bin/sh
# Install the latest macOS CLI, or set MARIONETTE_VERSION=cli-v0.1.1.
set -eu

fail() { printf 'marionette: %s\n' "$*" >&2; exit 1; }

main() {
    [ "$(uname -s)" = Darwin ] || fail 'This installer supports macOS. Use install.ps1 on Windows.'
    case "$(uname -m)" in
        arm64) arch=arm64 ;;
        x86_64) arch=amd64 ;;
        *) fail 'Unsupported Mac architecture.' ;;
    esac
    for tool in curl unzip shasum; do
        command -v "$tool" >/dev/null 2>&1 || fail "Required command not found: $tool"
    done

    releases=https://github.com/h053698/marionette/releases
    tag=${MARIONETTE_VERSION:-}
    if [ -z "$tag" ]; then
        release_url=$(curl --fail --silent --show-error --location --proto '=https' \
            --output /dev/null --write-out '%{url_effective}' "$releases/latest")
        tag=${release_url##*/}
    fi
    case "$tag" in v*) tag=cli-$tag ;; esac
    case "$tag" in cli-v[0-9]*) ;; *) fail "Invalid release version: $tag" ;; esac
    case "$tag" in *[!A-Za-z0-9._-]*) fail "Invalid release version: $tag" ;; esac

    tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/marionette-install.XXXXXX")
    staged=
    trap 'rm -rf "$tmpdir"; if [ -n "$staged" ]; then rm -f "$staged"; fi' EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    archive=marionette-cli-darwin-$arch.zip
    base=$releases/download/$tag
    printf 'Downloading Marionette %s for macOS %s...\n' "$tag" "$arch"
    curl --fail --silent --show-error --location --proto '=https' --output "$tmpdir/$archive" "$base/$archive"
    curl --fail --silent --show-error --location --proto '=https' --output "$tmpdir/SHA256SUMS" "$base/SHA256SUMS"
    expected=$(awk -v name="$archive" '$2 == name {print $1}' "$tmpdir/SHA256SUMS")
    actual=$(shasum -a 256 "$tmpdir/$archive" | awk '{print $1}')
    [ "$expected" = "$actual" ] || fail 'SHA-256 verification failed; installation was not changed.'
    unzip -q "$tmpdir/$archive" -d "$tmpdir/unpacked"

    install_dir=${MARIONETTE_INSTALL_DIR:-"$HOME/.local/bin"}
    mkdir -p "$install_dir"
    install_dir=$(cd "$install_dir" && pwd -P)
    staged=$(mktemp "$install_dir/.marionette.XXXXXX")
    cp "$tmpdir/unpacked/${archive%.zip}/marionette" "$staged"
    chmod 755 "$staged"
    "$staged" --version
    mv -f "$staged" "$install_dir/marionette"
    staged=

    # A piped installer cannot change its parent shell's environment.
    escaped_dir=$(printf '%s' "$install_dir" | sed "s/'/'\\\\''/g")
    path_line="export PATH='$escaped_dir':\"\$PATH\""
    profile=${MARIONETTE_PROFILE:-}
    login_shell=${SHELL:-/bin/zsh}
    if [ -z "$profile" ]; then
        case "${login_shell##*/}" in
            zsh|'') profile=$HOME/.zshrc ;;
            bash) profile=$HOME/.bash_profile ;;
        esac
    fi
    if [ -n "$profile" ]; then
        if [ ! -f "$profile" ] || ! grep -Fqx "$path_line" "$profile"; then
            printf '\n# Marionette CLI\n%s\n' "$path_line" >> "$profile"
        fi
        printf 'PATH registered in %s.\n' "$profile"
    fi
    printf 'Installed: %s/marionette\nOpen a new terminal, or run:\n  %s\n' "$install_dir" "$path_line"
}

main "$@"
