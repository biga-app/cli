#!/bin/sh
# Installs the biga CLI from its GitHub releases, on macOS, Linux, or FreeBSD.
#
#   curl -fsSL https://raw.githubusercontent.com/biga-app/cli/main/install.sh | sh
#
# The download is checked against the release's checksums.txt, and when cosign
# is installed, checksums.txt is checked against its Sigstore signature.
#
# Environment:
#   BIGA_VERSION      version to install, e.g. 1.0.0 (default: the latest)
#   BIGA_INSTALL_DIR  directory to install into (default: ~/.local/bin)
#   BIGA_RELEASE_URL  base URL serving a release's files, for testing
set -eu

repo="biga-app/cli"

say() { printf '%s\n' "$*" >&2; }
fail() {
	say "biga install: $*"
	exit 1
}
has() { command -v "$1" >/dev/null 2>&1; }

# download URL FILE
download() {
	if has curl; then
		curl -fsSL --retry 3 -o "$2" "$1"
	elif has wget; then
		wget -q -O "$2" "$1"
	else
		fail "curl or wget is required"
	fi
}

latest_version() {
	if has curl; then
		# The latest-release page redirects to the tag, without the API's
		# rate limits.
		url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest")
		echo "${url##*/}"
	else
		wget -q -O - "https://api.github.com/repos/$repo/releases/latest" |
			sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p'
	fi
}

sha256() {
	if has sha256sum; then
		sha256sum "$1" | cut -d ' ' -f 1
	elif has shasum; then
		shasum -a 256 "$1" | cut -d ' ' -f 1
	elif has sha256; then
		sha256 -q "$1"
	else
		fail "sha256sum, shasum, or sha256 is required to verify the download"
	fi
}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
linux | darwin | freebsd) ;;
*) fail "unsupported OS: $os (on Windows, use install.ps1)" ;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
armv7* | armv8l) arch=armv7 ;;
*) fail "unsupported CPU: $(uname -m)" ;;
esac
# macOS gets one binary for Apple silicon and Intel.
[ "$os" = darwin ] && arch=universal

version=${BIGA_VERSION:-}
if [ -z "$version" ]; then
	version=$(latest_version)
	[ -n "$version" ] || fail "could not find the latest release"
fi
version=${version#v}
tag="v$version"

base=${BIGA_RELEASE_URL:-https://github.com/$repo/releases/download/$tag}
archive="biga_${version}_${os}_${arch}.tar.gz"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM

say "Downloading biga $version for $os/$arch..."
download "$base/$archive" "$tmp/$archive" || fail "could not download $base/$archive"
download "$base/checksums.txt" "$tmp/checksums.txt" || fail "could not download checksums.txt"

expected=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$expected" ] || fail "$archive is not listed in checksums.txt"
[ "$(sha256 "$tmp/$archive")" = "$expected" ] || fail "checksum mismatch for $archive; refusing to install"

if has cosign && [ -z "${BIGA_RELEASE_URL:-}" ]; then
	download "$base/checksums.txt.sigstore.json" "$tmp/checksums.txt.sigstore.json" ||
		fail "could not download the release signature"
	cosign verify-blob \
		--certificate-identity "https://github.com/$repo/.github/workflows/release.yml@refs/tags/$tag" \
		--certificate-oidc-issuer https://token.actions.githubusercontent.com \
		--bundle "$tmp/checksums.txt.sigstore.json" "$tmp/checksums.txt" >/dev/null 2>&1 ||
		fail "the release signature does not verify; refusing to install"
	say "Verified the release signature."
fi

tar -xzf "$tmp/$archive" -C "$tmp" biga

dir=${BIGA_INSTALL_DIR:-$HOME/.local/bin}
mkdir -p "$dir"
# Replace by rename, which works even while an old biga is running.
cp "$tmp/biga" "$dir/.biga.$$"
chmod 0755 "$dir/.biga.$$"
mv -f "$dir/.biga.$$" "$dir/biga"

say "Installed biga $version to $dir/biga"
case ":$PATH:" in
*":$dir:"*) ;;
*) say "Add it to your PATH:  export PATH=\"$dir:\$PATH\"" ;;
esac
