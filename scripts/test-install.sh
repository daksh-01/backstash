#!/bin/sh
# Self-test for install.sh: runs it against a local fake release (file:// URLs, no network).
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fails=0
fail() { printf 'FAIL: %s\n' "$*" >&2; fails=$((fails + 1)); }

sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }

rel=$work/rel
mkdir -p "$rel"
printf '#!/bin/sh\necho "backstash vtest"\n' >"$rel/backstash_linux_amd64"
chmod +x "$rel/backstash_linux_amd64"
(cd "$rel" && sha backstash_linux_amd64 >checksums.txt)

run() { # run <install dir> [VAR=value ...]; prints combined output, returns install.sh's status
  dir=$1; shift
  env BACKSTASH_OS=linux BACKSTASH_ARCH=x86_64 BACKSTASH_BASE_URL="file://$rel" BACKSTASH_INSTALL_DIR="$dir" "$@" \
    sh "$here/install.sh" 2>&1
}

# 1. happy path
out=$(run "$work/bin1") || fail "happy path exited non-zero: $out"
[ "$("$work/bin1/backstash" version 2>&1)" = "backstash vtest" ] || fail "installed binary does not run"
[ -x "$work/bin1/backstash" ] || fail "binary is not executable"

# 2. arm64 name is mapped (asset missing from the fake release, so it must say so, not crash)
out=$(run "$work/bin2" BACKSTASH_ARCH=aarch64) && fail "arm64 should fail: asset absent" || true
case $out in *"download failed"*backstash_linux_arm64*) ;; *) fail "arm64 not mapped: $out" ;; esac

# 3. tampered binary must not install
printf 'evil' >>"$rel/backstash_linux_amd64"
out=$(run "$work/bin3") && fail "tampered binary installed" || true
case $out in *"checksum mismatch"*) ;; *) fail "no mismatch message: $out" ;; esac
[ ! -e "$work/bin3/backstash" ] || fail "tampered binary was left on disk"

# 4. asset missing from checksums.txt
printf '' >"$rel/checksums.txt"
out=$(run "$work/bin4") && fail "unlisted asset installed" || true
case $out in *"not listed in checksums.txt"*) ;; *) fail "no unlisted message: $out" ;; esac

# 5. unsupported OS and CPU
out=$(run "$work/bin5" BACKSTASH_OS=darwin) && fail "darwin should fail" || true
case $out in *"no prebuilt binary for darwin"*) ;; *) fail "no darwin message: $out" ;; esac
out=$(run "$work/bin6" BACKSTASH_ARCH=riscv64) && fail "riscv64 should fail" || true
case $out in *"unsupported CPU"*) ;; *) fail "no cpu message: $out" ;; esac

[ "$fails" -eq 0 ] && echo "install.sh: all checks passed"
exit "$fails"
