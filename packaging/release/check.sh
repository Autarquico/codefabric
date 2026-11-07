#!/bin/sh
# Assert a finalized release directory is complete and verifiable.
# Usage: check.sh DIST INSTALL_SH
#   INSTALL_SH  the install.sh at the release commit: it is the trust root, and DIST/install.sh must be
#               byte-identical to it. checksums.txt must list exactly the three tarballs, which must exist
#               and match, and both signatures must verify.
set -eu

[ $# -eq 2 ] || { echo "usage: check.sh DIST INSTALL_SH" >&2; exit 2; }
dist=$1
trust=$2
here=$(cd "$(dirname "$0")" && pwd)

fail() { echo "release-check: $*" >&2; exit 1; }

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

if [ ! -f "$trust" ] || ! grep -q '^# BEGIN allowed_signers$' "$trust"; then fail "$trust is not an install.sh with a trust root"; fi
[ -f "$dist/checksums.txt" ] || fail "checksums.txt is missing"
[ -f "$dist/install.sh" ] || fail "install.sh is missing"
cmp -s "$dist/install.sh" "$trust" || fail "install.sh in $dist differs from the one at the release commit"
for f in checksums.txt install.sh; do
	"$here/verify-asset.sh" "$dist/$f" "$trust" >/dev/null || fail "$f signature does not verify"
done

version=$(sed -n 's/^version: //p;1q' "$dist/checksums.txt")
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || fail "checksums.txt has no valid version header"

expected=""
for target in darwin_arm64 linux_amd64 linux_arm64; do
	expected="$expected cf_${version#v}_$target.tar.gz"
done
listed=$(sed 1d "$dist/checksums.txt" | awk '{ print $2 }' | LC_ALL=C sort | tr '\n' ' ')
want="${expected# } "
[ "$listed" = "$want" ] || fail "checksums.txt must list exactly: $want(got: $listed)"
if sed 1d "$dist/checksums.txt" | grep -Evq '^[0-9a-f]{64}  [^ ]+$'; then fail "checksums.txt has a malformed line"; fi

for name in $expected; do
	[ -f "$dist/$name" ] || fail "$name is missing"
	sum=$(awk -v n="$name" '$2 == n { print $1 }' "$dist/checksums.txt")
	[ "$sum" = "$(sha256_of "$dist/$name")" ] || fail "$name does not match its checksum"
	tar -tzf "$dist/$name" | grep -qx 'cf' || fail "$name does not contain cf"
done
[ "$(find "$dist" -maxdepth 1 -name 'cf_*.tar.gz' | wc -l | tr -d ' ')" -eq 3 ] || fail "dist holds tarballs beyond the three listed"
echo "release-check: $dist ok ($version)"
