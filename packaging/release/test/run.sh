#!/bin/sh
# Run the installer and release-script tests: every *_test.sh next to this file. Exits 0 when there are none.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
status=0
for t in "$here"/*_test.sh; do
	[ -e "$t" ] || continue
	sh "$t" || status=1
done
exit "$status"
