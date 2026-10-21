#!/bin/sh
# Install backstash from a GitHub release: detect the CPU, download the binary and checksums.txt,
# verify the checksum, and install it.
#
#   curl -fsSL https://raw.githubusercontent.com/daksh-01/backstash/main/install.sh | sh
#
# Options (environment variables):
#   BACKSTASH_VERSION      release tag such as v0.1.0 (default: latest)
#   BACKSTASH_INSTALL_DIR  where to put the binary (default: /usr/local/bin if writable, else ~/.local/bin)
#   BACKSTASH_REPO         owner/name to download from (default: daksh-01/backstash)
set -eu

REPO=${BACKSTASH_REPO:-daksh-01/backstash}
VERSION=${BACKSTASH_VERSION:-latest}

say() { printf '%s\n' "$*" >&2; }
die() { say "install.sh: $*"; exit 1; }

os=${BACKSTASH_OS:-$(uname -s | tr '[:upper:]' '[:lower:]')}
case $os in
  linux) ;;
  *) die "no prebuilt binary for $os; build from source: git clone https://github.com/$REPO && cd backstash && make build" ;;
esac
arch=${BACKSTASH_ARCH:-$(uname -m)}
case $arch in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported CPU: $arch (releases are amd64 and arm64)" ;;
esac
asset=backstash_${os}_${arch}

# BACKSTASH_BASE_URL exists for the self-test only (scripts/test-install.sh).
if [ -n "${BACKSTASH_BASE_URL:-}" ]; then
  base=$BACKSTASH_BASE_URL
elif [ "$VERSION" = latest ]; then
  base=https://github.com/$REPO/releases/latest/download
else
  base=https://github.com/$REPO/releases/download/$VERSION
fi

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO "$2" "$1"; }
else
  die "need curl or wget"
fi

if command -v sha256sum >/dev/null 2>&1; then
  sha() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "need sha256sum or shasum"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading $asset ($VERSION)"
fetch "$base/$asset" "$tmp/$asset" || die "download failed: $base/$asset"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "download failed: $base/checksums.txt"

want=$(awk -v f="$asset" '{ n = $2; sub(/^\*/, "", n) } n == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || die "$asset is not listed in checksums.txt"
[ "$(sha "$tmp/$asset")" = "$want" ] || die "checksum mismatch for $asset; refusing to install"

# The checksum only proves the download is intact. With an authenticated gh, also prove it was built
# by this repo's release workflow.
if [ -z "${BACKSTASH_BASE_URL:-}" ] && command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
  gh attestation verify "$tmp/$asset" --repo "$REPO" >/dev/null 2>&1 || die "build provenance verification failed"
  say "Verified build provenance"
fi

dir=${BACKSTASH_INSTALL_DIR:-}
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir=$HOME/.local/bin; fi
fi
mkdir -p "$dir"
# Copy then rename so a running backup never sees a half-written binary.
cp "$tmp/$asset" "$dir/.backstash.$$"
chmod 0755 "$dir/.backstash.$$"
mv -f "$dir/.backstash.$$" "$dir/backstash"

say "Installed: $("$dir/backstash" version) -> $dir/backstash"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "Note: $dir is not on your PATH. Add it, or run $dir/backstash directly." ;;
esac
