#!/bin/sh
# Render the Homebrew formula from the signed checksums.txt of a vX.Y.Z release.
# The formula is a derived artifact: version and digests come only from checksums.txt, and its
# signature is verified first. TRUST is an allowed_signers file or the install.sh of the repo checkout
# at the tag commit, never the install.sh downloaded from the release (that would be circular). Suffixed versions are refused.
# The formula is rendered to a temporary file and printed only when complete: a failure never
# leaves partial output on stdout.
# Usage: brew-formula.sh CHECKSUMS TRUST [VERSION]    Env: CF_RELEASE_BASE_URL (tests and mirrors)
set -eu
LC_ALL=C
export LC_ALL

if [ $# -lt 2 ] || [ $# -gt 3 ]; then echo "usage: brew-formula.sh CHECKSUMS TRUST [VERSION]" >&2; exit 2; fi
checksums=$1
trust=$2
here=$(cd "$(dirname "$0")" && pwd)
template="$here/../homebrew/codefabric.rb.tmpl"
base_url=${CF_RELEASE_BASE_URL:-https://github.com/Autarquico/codefabric/releases/download}

fail() { echo "brew-formula: $*" >&2; exit 1; }

[ -f "$checksums" ] || fail "$checksums is missing"
"$here/verify-asset.sh" "$checksums" "$trust" >/dev/null || fail "checksums signature does not verify"

tag=$(sed -n 's/^version: //p;1q' "$checksums")
stable='^v[0-9]+\.[0-9]+\.[0-9]+$'
printf '%s\n' "$tag" | grep -Eq "$stable" || fail "not a plain vX.Y.Z version: '$tag'"
if [ $# -eq 3 ]; then
	printf '%s\n' "$3" | grep -Eq "$stable" || fail "not a plain vX.Y.Z version: '$3'"
	[ "$3" = "$tag" ] || fail "checksums are for $tag, not $3"
fi
version=${tag#v}

digest_of() {
	name="cf_${version}_$1.tar.gz"
	digest=$(awk -v n="$name" 'NR > 1 && $2 == n { print $1; c++ } END { exit c != 1 }' "$checksums") ||
		fail "$name is not listed exactly once"
	printf '%s\n' "$digest" | grep -Eq '^[0-9a-f]{64}$' || fail "$name has no sha256 digest"
	printf '%s\n' "$digest"
}

darwin_arm64=$(digest_of darwin_arm64)
linux_arm64=$(digest_of linux_arm64)
linux_amd64=$(digest_of linux_amd64)

out=$(mktemp)
trap 'rm -f "$out"' EXIT
sed -e "s|@BASE_URL@|$base_url|g" \
	-e "s|@VERSION@|$version|g" \
	-e "s|@SHA_DARWIN_ARM64@|$darwin_arm64|" \
	-e "s|@SHA_LINUX_ARM64@|$linux_arm64|" \
	-e "s|@SHA_LINUX_AMD64@|$linux_amd64|" \
	"$template" >"$out"
! grep -q '@[A-Z_0-9]*@' "$out" || fail "unrendered placeholder left in the formula"
cat "$out"
