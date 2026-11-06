#!/bin/sh
# Verify a release as published: the inventory is exactly the seven assets, then the downloaded set
# passes check.sh against INSTALL_SH (signatures, sha256 of each tarball, install.sh identical).
# Usage: verify-release.sh TAG INSTALL_SH [REPO]      (REPO defaults to Autarquico/codefabric; needs gh)
set -eu

[ $# -ge 2 ] && [ $# -le 3 ] || { echo "usage: verify-release.sh TAG INSTALL_SH [REPO]" >&2; exit 2; }
tag=$1
trust=$2
repo=${3:-Autarquico/codefabric}
here=$(cd "$(dirname "$0")" && pwd)
version=${tag#v}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

want=$(printf '%s\n' \
	"cf_${version}_darwin_arm64.tar.gz" "cf_${version}_linux_amd64.tar.gz" "cf_${version}_linux_arm64.tar.gz" \
	checksums.txt checksums.txt.sig install.sh install.sh.sig | LC_ALL=C sort)
got=$(gh release view "$tag" --repo "$repo" --json assets --jq '.assets[].name' | LC_ALL=C sort)
[ "$got" = "$want" ] || { printf 'verify-release: unexpected asset inventory for %s\nwant:\n%s\ngot:\n%s\n' "$tag" "$want" "$got" >&2; exit 1; }

gh release download "$tag" --repo "$repo" --dir "$work"
"$here/check.sh" "$work" "$trust"
[ "$(sed -n 1p "$work/checksums.txt")" = "version: $tag" ] || { echo "verify-release: checksums.txt is for another version" >&2; exit 1; }
echo "verify-release: $tag ok"
