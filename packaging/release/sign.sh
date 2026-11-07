#!/bin/sh
# Sign a file with the release key: ssh-keygen -Y sign, namespace codefabric-release. Writes <file>.sig.
# Env: CF_SIGNING_KEY_FILE  path of the private key (never printed).
set -eu

[ $# -eq 1 ] || { echo "usage: sign.sh FILE" >&2; exit 2; }
: "${CF_SIGNING_KEY_FILE:?CF_SIGNING_KEY_FILE is required}"

rm -f "$1.sig"
ssh-keygen -q -Y sign -f "$CF_SIGNING_KEY_FILE" -n codefabric-release "$1"
