#!/bin/sh
# Install Refraktal from a GitHub release. Fetch this script from the
# release you want (it is attached to every release, and at the tag in the
# repository), for example:
#
#   curl -fsSL --proto '=https' https://github.com/refraktal/refraktal/releases/download/v0.1.0/install.sh | REFRAKTAL_VERSION=v0.1.0 sh
#
# (Without REFRAKTAL_VERSION it installs the latest release, which need not
# be the one the script came from.)
#
# It downloads the archive for this machine (macOS or Linux, arm64 or amd64)
# and the release's SHA256SUMS over HTTPS only, refuses to install unless the
# checksum matches, and puts `refraktal` in ~/.local/bin (no sudo). Settings:
#
#   REFRAKTAL_VERSION=v0.1.0         a release tag (default: the latest release; never a prerelease)
#   REFRAKTAL_INSTALL_DIR=/some/bin  where to install (default: ~/.local/bin)
#   REFRAKTAL_DOWNLOAD_BASE=URL      https base where release assets live (for tests)
#   REFRAKTAL_RELEASES=URL           https base of the releases pages (for tests)
set -eu

repo="https://github.com/refraktal/refraktal"
base="${REFRAKTAL_DOWNLOAD_BASE:-$repo/releases/download}"
releases="${REFRAKTAL_RELEASES:-$repo/releases}"
dir="${REFRAKTAL_INSTALL_DIR:-$HOME/.local/bin}"

fail() { echo "refraktal install: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "needs $1"; }
need curl
need tar
need mktemp

# HTTPS only, redirects included: never a downgrade to http or file.
case "$base" in https://*) ;; *) fail "the download base must be an https:// URL (got $base)" ;; esac
case "$releases" in https://*) ;; *) fail "the releases URL must be an https:// URL (got $releases)" ;; esac
get() { curl -fsSL --proto '=https' --proto-redir '=https' "$@"; }

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
  # GitHub's latest release is never a prerelease or a draft; refuse one anyway.
  url=$(get -I -o /dev/null -w '%{url_effective}' "$releases/latest") || fail "cannot reach $releases"
  version=${url##*/}
  case "$version" in
    *-*) fail "the latest release resolved to a prerelease ($version): set REFRAKTAL_VERSION to install one on purpose" ;;
  esac
fi
echo "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$' ||
  fail "no release found (got \"$version\"); set REFRAKTAL_VERSION to a tag such as v0.1.0"

archive="refraktal_${version#v}_${os}_${arch}.tar.gz"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/refraktal-install.XXXXXX")
stage=""
cleanup() { rm -rf "$tmp"; [ -z "$stage" ] || rm -f "$stage"; }
trap cleanup EXIT INT TERM

echo "refraktal install: $version for $os/$arch"
get -o "$tmp/$archive" "$base/$version/$archive" || fail "cannot download $archive"
get -o "$tmp/SHA256SUMS" "$base/$version/SHA256SUMS" || fail "cannot download SHA256SUMS"

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
# A fresh staging file in the target folder (never a fixed name, which a
# planted symlink could redirect), then an atomic rename: a running
# refraktal keeps its old file.
stage=$(mktemp "$dir/.refraktal.XXXXXX") || fail "cannot write to $dir"
cat "$tmp/refraktal" > "$stage"
chmod 0755 "$stage"
mv -f "$stage" "$dir/refraktal"
stage=""
echo "refraktal install: installed $dir/refraktal"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "refraktal install: add $dir to your PATH" ;;
esac
echo "refraktal install: next, run: refraktal doctor"
