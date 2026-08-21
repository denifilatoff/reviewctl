#!/bin/sh
set -eu

root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT

case $(uname -s) in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) echo "unsupported OS" >&2; exit 1 ;;
esac
case $(uname -m) in
x86_64|amd64) arch=amd64 ;;
arm64|aarch64) arch=arm64 ;;
*) echo "unsupported architecture" >&2; exit 1 ;;
esac

version=v9.8.7
asset="reviewctl-$os-$arch"
release_dir="$root/releases/download/$version"
install_dir="$root/bin"
mkdir -p "$release_dir"
printf '#!/bin/sh\nprintf fixture\n' > "$release_dir/$asset"
chmod +x "$release_dir/$asset"

checksum() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

printf '%s  %s\n' "$(checksum "$release_dir/$asset")" "$asset" > "$release_dir/SHA256SUMS"
REVIEWCTL_INSTALL_VERSION="$version" REVIEWCTL_INSTALL_BASE_URL="file://$root/releases" \
	REVIEWCTL_INSTALL_DIR="$install_dir" sh scripts/install.sh
test "$("$install_dir/reviewctl")" = fixture
before=$(checksum "$install_dir/reviewctl")

printf '%064d  %s\n' 0 "$asset" > "$release_dir/SHA256SUMS"
if REVIEWCTL_INSTALL_VERSION="$version" REVIEWCTL_INSTALL_BASE_URL="file://$root/releases" \
	REVIEWCTL_INSTALL_DIR="$install_dir" sh scripts/install.sh; then
	echo "checksum mismatch unexpectedly installed" >&2
	exit 1
fi
test "$(checksum "$install_dir/reviewctl")" = "$before"
