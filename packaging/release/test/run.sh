#!/bin/sh
# shellcheck disable=SC1091,SC2034,SC2329
# Run the installer and release-script tests: every TestScenario_* function in the *_test.sh files next to this
# file. Prints go-test style lines (=== RUN, --- PASS, --- FAIL) and exits non-zero when any test fails.
# Usage: run.sh [PATTERN]      only the tests whose name contains PATTERN
set -eu

here=$(cd "$(dirname "$0")" && pwd)
RELEASE_DIR=$(cd "$here/.." && pwd)
ROOT=$(cd "$here/../../.." && pwd)
pattern=${1:-}

for tool in python3 ssh-keygen jq git curl tar; do
	command -v "$tool" >/dev/null 2>&1 || { echo "run.sh: required tool not found: $tool" >&2; exit 2; }
done

TMP=$(mktemp -d)
server_pid=
# shellcheck disable=SC2317 # invoked by the EXIT trap
cleanup() {
	[ -z "$server_pid" ] || kill "$server_pid" 2>/dev/null || true
	rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

WWW=$TMP/www
SHIM=$TMP/shim
mkdir -p "$WWW" "$SHIM" "$TMP/t"

# uname is the host boundary the platform tests fake; the installer only ever calls `uname -s` and `uname -m`.
cat >"$SHIM/uname" <<'EOF'
#!/bin/sh
case "$1" in
	-m) echo "${FAKE_UNAME_M:-x86_64}" ;;
	*) echo "${FAKE_UNAME_S:-Linux}" ;;
esac
EOF
chmod 755 "$SHIM/uname"

# Two release keys: KEY_A is the trusted one baked into its install.sh copy, KEY_B is an unrelated signer.
KEY_A=$TMP/keyA
KEY_B=$TMP/keyB
sh "$RELEASE_DIR/testkey.sh" "$KEY_A"
sh "$RELEASE_DIR/testkey.sh" "$KEY_B"

python3 -I "$here/server.py" "$WWW" "$TMP/port" 2>"$TMP/server.log" &
server_pid=$!
tries=0
while [ ! -s "$TMP/port" ]; do
	tries=$((tries + 1))
	[ "$tries" -le 100 ] || { echo "run.sh: the test server did not start" >&2; cat "$TMP/server.log" >&2; exit 2; }
	sleep 0.1
done
URL=http://127.0.0.1:$(cat "$TMP/port")

# shellcheck source=lib.sh
. "$here/lib.sh"
for f in "$here"/*_test.sh; do
	# shellcheck source=/dev/null
	. "$f"
done

names=$(cat "$here"/*_test.sh | sed -n 's/^\(TestScenario_[A-Za-z0-9_]*\)().*/\1/p')
status=0
ran=0
for name in $names; do
	case $name in *"$pattern"*) ;; *) continue ;; esac
	echo "=== RUN   $name"
	ran=$((ran + 1))
	TEST_NAME=$name
	T=$TMP/t/$name
	mkdir -p "$T"
	rc=0
	( set -eu; "$name" ) >"$TMP/log" 2>&1 || rc=$?
	if [ "$rc" -eq 0 ]; then
		echo "--- PASS: $name"
	else
		status=1
		echo "--- FAIL: $name"
		sed 's/^/    /' "$TMP/log"
	fi
done

[ "$ran" -gt 0 ] || { echo "run.sh: no test matches '$pattern'" >&2; exit 2; }
if [ "$status" -eq 0 ]; then echo "PASS ($ran tests)"; else echo "FAIL"; fi
exit "$status"
