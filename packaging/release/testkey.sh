#!/bin/sh
# Generate a throwaway release key in DIR and render its public half into DIR/install.sh, a test copy of
# the repo's install.sh (CI and tests only). DIR/key is the private key for CF_SIGNING_KEY_FILE.
# Usage: testkey.sh DIR [OPTIONS]    e.g. 'valid-before="20200101"' to mint an expired key.
set -eu

[ $# -ge 1 ] || { echo "usage: testkey.sh DIR [OPTIONS]" >&2; exit 2; }
here=$(cd "$(dirname "$0")" && pwd)
src=${CF_INSTALL_SH:-$here/../../install.sh}
dir=$1
mkdir -p "$dir"
rm -f "$dir/key" "$dir/key.pub"
ssh-keygen -q -t ed25519 -N '' -C release@codefabric -f "$dir/key"
line=$(printf 'release@codefabric %snamespaces="codefabric-release" %s' "${2:+$2,}" "$(cut -d' ' -f1,2 "$dir/key.pub")")
printf '%s\n' "$line" >"$dir/allowed_signers"
awk -v line="$line" '
	$0 == "# END allowed_signers" { skipping = 0 }
	!skipping { print }
	$0 == "# BEGIN allowed_signers" { print line; skipping = 1 }
' "$src" >"$dir/install.sh"
chmod 755 "$dir/install.sh"
if grep -q 'ssh-ed25519 PLACEHOLDER' "$dir/install.sh"; then echo "testkey: placeholder survived rendering" >&2; exit 1; fi
