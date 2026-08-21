#!/bin/sh
set -eu

if [ "$#" -ne 0 ]; then
	echo "usage: curl -fsSL https://raw.githubusercontent.com/denifilatoff/reviewctl/main/scripts/install.sh | sh" >&2
	exit 2
fi

case $(uname -s) in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac
case $(uname -m) in
x86_64|amd64) arch=amd64 ;;
arm64|aarch64) arch=arm64 ;;
*) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

version=${REVIEWCTL_INSTALL_VERSION:-latest}
base_url=${REVIEWCTL_INSTALL_BASE_URL:-https://github.com/denifilatoff/reviewctl/releases}
install_dir=${REVIEWCTL_INSTALL_DIR:-"$HOME/.local/bin"}
asset="reviewctl-$os-$arch"
case $version in
latest) release_url="$base_url/latest/download" ;;
*) release_url="$base_url/download/$version" ;;
esac

mkdir -p "$install_dir"
temporary_dir=$(mktemp -d "$install_dir/.reviewctl.XXXXXX")
trap 'rm -rf "$temporary_dir"' EXIT
curl -fsSL "$release_url/$asset" -o "$temporary_dir/$asset"
curl -fsSL "$release_url/SHA256SUMS" -o "$temporary_dir/SHA256SUMS"

expected=$(awk -v asset="$asset" '$2 == asset || $2 == "*" asset { print $1; exit }' "$temporary_dir/SHA256SUMS")
if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$temporary_dir/$asset" | awk '{print $1}')
else
	actual=$(shasum -a 256 "$temporary_dir/$asset" | awk '{print $1}')
fi
if [ -z "$expected" ] || [ "$actual" != "$expected" ]; then
	echo "checksum verification failed for $asset" >&2
	exit 1
fi

chmod 755 "$temporary_dir/$asset"
mv "$temporary_dir/$asset" "$install_dir/reviewctl"
echo "installed $install_dir/reviewctl"
if ! command -v reviewctl >/dev/null 2>&1; then
	echo "add $install_dir to PATH"
fi
