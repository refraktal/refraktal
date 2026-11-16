#!/bin/sh
# Install Refraktal from its GitHub release, served at https://refraktal.dev/install:
#
#   curl -fsSL https://refraktal.dev/install | sh
#
# It downloads the archive for this machine (macOS or Linux, arm64 or amd64)
# and the release's SHA256SUMS, refuses to install unless the checksum
# matches, and puts `refraktal` in ~/.local/bin (no sudo). Settings:
#
#   REFRAKTAL_VERSION=v0.1.0         a release tag (default: the latest release)
#   REFRAKTAL_INSTALL_DIR=/some/bin  where to install (default: ~/.local/bin)
#   REFRAKTAL_DOWNLOAD_BASE=URL      where releases live (default: GitHub; for tests)
set -eu

repo="https://github.com/refraktal/refraktal"
base="${REFRAKTAL_DOWNLOAD_BASE:-$repo/releases/download}"
dir="${REFRAKTAL_INSTALL_DIR:-$HOME/.local/bin}"

fail() { echo "refraktal install: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "needs $1"; }
need curl
need tar

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "unsupported system $(uname -s): macOS and Linux only" ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) fail "unsupported architecture $(uname -m): arm64 and amd64 only" ;;
esac

version="${REFRAKTAL_VERSION:-}"
if [ -z "$version" ]; then
  # The latest release's tag, from GitHub's redirect: no API, no rate limit.
  url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$repo/releases/latest") || fail "cannot reach $repo"
  version=${url##*/}
fi
case "$version" in
  v[0-9]*) ;;
  *) fail "no release found (got \"$version\"); set REFRAKTAL_VERSION" ;;
esac

archive="refraktal_${version#v}_${os}_${arch}.tar.gz"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/refraktal-install.XXXXXX")
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "refraktal install: $version for $os/$arch"
curl -fsSL -o "$tmp/$archive" "$base/$version/$archive" || fail "cannot download $archive"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/$version/SHA256SUMS" || fail "cannot download SHA256SUMS"

want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/SHA256SUMS")
[ -n "$want" ] || fail "$archive is not listed in SHA256SUMS"
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$archive" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  got=$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')
else
  fail "needs sha256sum or shasum to verify the download"
fi
[ "$got" = "$want" ] || fail "checksum mismatch for $archive: refusing to install"

tar -xzf "$tmp/$archive" -C "$tmp" refraktal || fail "cannot unpack $archive"
mkdir -p "$dir"
# Replace atomically: a running refraktal keeps its old file.
cp "$tmp/refraktal" "$dir/.refraktal.new"
chmod 0755 "$dir/.refraktal.new"
mv -f "$dir/.refraktal.new" "$dir/refraktal"
echo "refraktal install: installed $dir/refraktal"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "refraktal install: add $dir to your PATH" ;;
esac
echo "refraktal install: next, run: refraktal doctor"
