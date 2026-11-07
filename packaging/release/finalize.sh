#!/bin/sh
# Turn the GoReleaser tarballs into the signed release set.
# Writes checksums.txt (line 1 "version: vX.Y.Z", then "<sha256>  <tarball>" for exactly the three
# tarballs), copies install.sh next to it, and signs both.
# Usage: finalize.sh DIST VERSION
# Env:   CF_SIGNING_KEY_FILE, CF_INSTALL_SH (the install.sh to ship; default the repo's)
set -eu
LC_ALL=C
export LC_ALL

[ $# -eq 2 ] || { echo "usage: finalize.sh DIST VERSION" >&2; exit 2; }
dist=$1
version=$2
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
install_sh=${CF_INSTALL_SH:-$root/install.sh}

printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || { echo "invalid version: $version" >&2; exit 2; }

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

targets='linux_amd64 linux_arm64 darwin_arm64'
for target in $targets; do
	[ -f "$dist/cf_${version#v}_$target.tar.gz" ] || { echo "missing tarball for $target in $dist" >&2; exit 1; }
done
[ "$(find "$dist" -maxdepth 1 -name 'cf_*.tar.gz' | wc -l | tr -d ' ')" -eq 3 ] || { echo "dist must hold exactly the three tarballs" >&2; exit 1; }

{
	printf 'version: %s\n' "$version"
	for target in $targets; do
		name="cf_${version#v}_$target.tar.gz"
		printf '%s  %s\n' "$(sha256_of "$dist/$name")" "$name"
	done
} >"$dist/checksums.txt"
cp "$install_sh" "$dist/install.sh"
"$here/sign.sh" "$dist/checksums.txt"
"$here/sign.sh" "$dist/install.sh"
echo "finalized $version in $dist"
