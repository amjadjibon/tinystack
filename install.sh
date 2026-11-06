#!/bin/sh
# Installs or upgrades the latest TinyStack release binary (Linux and macOS).
#
#   curl -fsSL https://raw.githubusercontent.com/amjadjibon/tinystack/main/install.sh | sh
#
# TINYSTACK_VERSION pins a release (e.g. 0.0.1); TINYSTACK_INSTALL_DIR overrides /usr/local/bin.
set -eu

repo=amjadjibon/tinystack
dir=${TINYSTACK_INSTALL_DIR:-/usr/local/bin}
target="$dir/tinystack"

fail() {
	echo "install.sh: $*" >&2
	exit 1
}

command -v curl >/dev/null || fail "curl is required"
command -v tar >/dev/null || fail "tar is required"

case $(uname -s) in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) fail "unsupported OS $(uname -s); download a Windows build from https://github.com/$repo/releases" ;;
esac

case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail "unsupported architecture $(uname -m)" ;;
esac

if [ -n "${TINYSTACK_VERSION:-}" ]; then
	version=${TINYSTACK_VERSION#v}
else
	# The latest-release page redirects to .../releases/tag/vX.Y.Z; this avoids the rate-limited API.
	url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest") ||
		fail "could not resolve the latest release"
	version=${url##*/v}
	case $version in
	"" | */*) fail "no published release found at $url" ;;
	esac
fi

if [ -x "$target" ]; then
	current=$("$target" --version 2>/dev/null | awk '{print $3}') || current=
	if [ "$current" = "$version" ]; then
		echo "tinystack $version is already installed at $target"
		exit 0
	fi

	echo "Upgrading tinystack ${current:-unknown} to $version"
else
	echo "Installing tinystack $version"
fi

name="tinystack_${version}_${os}_${arch}"
base="https://github.com/$repo/releases/download/v$version"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

curl -fsSL -o "$tmp/$name.tar.gz" "$base/$name.tar.gz" || fail "download failed: $base/$name.tar.gz"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || fail "download failed: $base/checksums.txt"

want=$(awk -v f="$name.tar.gz" '$2 == f {print $1}' "$tmp/checksums.txt")
[ -n "$want" ] || fail "$name.tar.gz is not listed in checksums.txt"
if command -v sha256sum >/dev/null; then
	got=$(sha256sum "$tmp/$name.tar.gz" | awk '{print $1}')
else
	got=$(shasum -a 256 "$tmp/$name.tar.gz" | awk '{print $1}')
fi

[ "$got" = "$want" ] || fail "checksum mismatch for $name.tar.gz"

tar -xzf "$tmp/$name.tar.gz" -C "$tmp"

sudo=
if [ ! -d "$dir" ] && ! mkdir -p "$dir" 2>/dev/null || [ ! -w "$dir" ]; then
	command -v sudo >/dev/null || fail "$dir is not writable; rerun as root or set TINYSTACK_INSTALL_DIR"
	sudo=sudo
	echo "Writing to $dir needs sudo"
fi

# install replaces the file instead of writing into it, so a running tinystack keeps its old binary.
$sudo mkdir -p "$dir"
$sudo install -m 0755 "$tmp/$name/tinystack" "$target"

echo "Installed $("$target" --version) at $target"
