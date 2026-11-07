# shellcheck shell=sh
# shellcheck disable=SC2154
# Helpers shared by the *_test.sh files. Globals come from run.sh: ROOT RELEASE_DIR TMP WWW URL SHIM KEY_A KEY_B
# and, per test, TEST_NAME and T (the test's private scratch dir).
VERSION=v1.2.3
BARE_VERSION=1.2.3

fail() { echo "FAIL: $*"; exit 1; }

# Contents of $T/out (the last run_install / run_cmd output), for failure messages.
dump() { sed 's/^/    | /' "$T/out" 2>/dev/null || true; }

assert_rc() { [ "$rc" -eq "$1" ] || { dump; fail "exit code $rc, want $1"; }; }
assert_rc_nonzero() { [ "$rc" -ne 0 ] || { dump; fail "exit code 0, want non-zero"; }; }
assert_out_contains() { grep -q -- "$1" "$T/out" || { dump; fail "output lacks: $1"; }; }

requests() { grep -c "\"GET /$TEST_NAME/" "$TMP/server.log" || true; }
assert_no_requests() { [ "$(requests)" -eq 0 ] || fail "the server saw $(requests) request(s); want none"; }

# --- release fixture -------------------------------------------------------

# make_dist DIR [MODE]: the three platform tarballs, each holding a fake cf that prints the bare version.
make_dist() {
	mkdir -p "$1"
	for target in linux_amd64 linux_arm64 darwin_arm64; do
		python3 -I "$RELEASE_DIR/test/mktar.py" "$1/cf_${BARE_VERSION}_$target.tar.gz" "${2:-ok}" "$BARE_VERSION"
	done
}

# finalize_dist DIR KEYDIR: checksums.txt, install.sh (KEYDIR's test copy) and both signatures, as the release does.
finalize_dist() {
	CF_SIGNING_KEY_FILE="$2/key" CF_INSTALL_SH="$2/install.sh" sh "$RELEASE_DIR/finalize.sh" "$1" "$VERSION" >/dev/null
}

# good_release KEYDIR: a finalized dist in $T/dist, signed by KEYDIR.
good_release() {
	make_dist "$T/dist"
	finalize_dist "$T/dist" "$1"
}

# serve_dist: publish $T/dist where the server finds it for this test: $URL/$TEST_NAME/$VERSION/.
serve_dist() {
	mkdir -p "$WWW/$TEST_NAME/$VERSION"
	cp "$T/dist"/* "$WWW/$TEST_NAME/$VERSION/"
}

# sign_checksums KEYDIR: replace the signature of $T/dist/checksums.txt with a valid one from KEYDIR.
sign_checksums() { CF_SIGNING_KEY_FILE="$1/key" sh "$RELEASE_DIR/sign.sh" "$T/dist/checksums.txt"; }

# hash_of FILE: sha256 hex, whichever tool the platform has.
hash_of() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

# --- running the installer -------------------------------------------------

seed_old_cf() {
	mkdir -p "$T/bin"
	printf '#!/bin/sh\necho old\n' >"$T/bin/cf"
	chmod 755 "$T/bin/cf"
	cp "$T/bin/cf" "$T/old-cf"
}

# The previous binary is intact and nothing else landed in the install directory.
assert_install_dir_untouched() {
	cmp -s "$T/bin/cf" "$T/old-cf" || fail "the previous cf was modified"
	[ "$(ls -A "$T/bin")" = "cf" ] || fail "the install dir holds more than cf: $(ls -A "$T/bin")"
}

# run_install INSTALL_SH [args...]; knobs: INST_VERSION INST_BASE INST_PATH FAKE_UNAME_S FAKE_UNAME_M.
# Sets rc and writes the combined output to $T/out. The fake uname defaults to Linux x86_64 so the tests do not
# depend on the host. CF_VERSION=latest needs publish_latest (server.py redirects <base>/latest).
run_install() {
	script=$1
	shift
	rc=0
	mkdir -p "$T/home"
	env PATH="${INST_PATH:-$SHIM:$PATH}" HOME="$T/home" \
		CF_RELEASE_BASE_URL="${INST_BASE:-$URL/$TEST_NAME}" CF_INSTALL_DIR="$T/bin" CF_VERSION="${INST_VERSION-$VERSION}" \
		FAKE_UNAME_S="${FAKE_UNAME_S:-Linux}" FAKE_UNAME_M="${FAKE_UNAME_M:-x86_64}" \
		"$(command -v sh)" "$script" "$@" >"$T/out" 2>&1 || rc=$?
}

# run_cmd CMD [args...]: sets rc and $T/out for any command.
run_cmd() {
	rc=0
	"$@" >"$T/out" 2>&1 || rc=$?
}

# The install must fail with exit 5 and leave the previous binary alone.
assert_rejected_untouched() { # install_sh
	run_install "$1"
	assert_rc 5
	assert_install_dir_untouched
}
