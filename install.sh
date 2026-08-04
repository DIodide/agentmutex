#!/bin/sh
# agentmutex installer — downloads the prebuilt binary from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/DIodide/agentmutex/main/install.sh | sh
#
# Environment:
#   AGENTMUTEX_VERSION      version to install, e.g. "v0.2.0" (default: latest)
#   AGENTMUTEX_INSTALL_DIR  target directory (default: /usr/local/bin if
#                           writable, else ~/.local/bin)
#
# Linux and macOS (amd64/arm64). On Windows, download the .zip from
# https://github.com/DIodide/agentmutex/releases or use `go install`.
set -eu

err() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

main() {
  REPO="DIodide/agentmutex"

  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  case "$os" in
    linux | darwin) ;;
    *) err "unsupported OS '$os' — download from https://github.com/$REPO/releases" ;;
  esac

  arch="$(uname -m)"
  case "$arch" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) err "unsupported architecture '$arch' — download from https://github.com/$REPO/releases" ;;
  esac

  version="${AGENTMUTEX_VERSION:-}"
  if [ -z "$version" ]; then
    # Resolve "latest" from the release redirect — no API token, no rate limits.
    version="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" | sed 's#.*/##')"
    [ -n "$version" ] && [ "$version" != "latest" ] || err "could not determine the latest version"
  fi
  case "$version" in v*) ;; *) version="v$version" ;; esac
  bare="${version#v}" # asset filenames drop the leading v

  asset="agentmutex_${bare}_${os}_${arch}.tar.gz"
  base="https://github.com/$REPO/releases/download/$version"

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT INT TERM HUP

  printf 'Downloading %s (%s)...\n' "$asset" "$version" >&2
  curl -fsSL -o "$tmp/$asset" "$base/$asset" || err "download failed: $base/$asset"
  curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || err "download failed: checksums.txt"

  # Exact-field match: never treat the filename as a regex.
  expected="$(awk -v f="$asset" '$2 == f { print $1; exit }' "$tmp/checksums.txt")"
  [ -n "$expected" ] || err "no checksum entry for $asset"
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$tmp/$asset" | awk '{print $1}')"
  else
    actual="$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')"
  fi
  [ "$expected" = "$actual" ] || err "checksum mismatch for $asset"

  tar -xzf "$tmp/$asset" -C "$tmp" agentmutex

  dir="${AGENTMUTEX_INSTALL_DIR:-}"
  if [ -z "$dir" ]; then
    if [ -w /usr/local/bin ]; then
      dir=/usr/local/bin
    elif [ -n "${HOME:-}" ]; then
      dir="$HOME/.local/bin"
    else
      err "/usr/local/bin is not writable and HOME is unset — set AGENTMUTEX_INSTALL_DIR"
    fi
  fi
  mkdir -p "$dir"
  install -m 0755 "$tmp/agentmutex" "$dir/agentmutex"

  printf 'Installed %s to %s/agentmutex\n' "$("$dir/agentmutex" version)" "$dir" >&2
  case ":$PATH:" in
    *":$dir:"*) ;;
    *) printf 'Note: %s is not on your PATH.\n' "$dir" >&2 ;;
  esac
}

# Everything is inside main() so a partially downloaded `curl | sh` stream
# defines functions but executes nothing — truncation becomes a syntax error,
# never a half-run install reporting success.
main "$@"
