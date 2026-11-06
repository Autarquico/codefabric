# shellcheck shell=sh
# shellcheck disable=SC2154,SC2034
# Installer scenarios. Each function is one test; its name is the spec scenario, its body the Then.

TestScenario_VerifiedInstall() {
	good_release "$KEY_A"
	serve_dist
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ -x "$T/bin/cf" ] || fail "cf is not installed and executable in CF_INSTALL_DIR"
	[ "$("$T/bin/cf" version)" = "$BARE_VERSION" ] || fail "cf version printed '$("$T/bin/cf" version)', want $BARE_VERSION"
	[ "$(ls -A "$T/bin")" = "cf" ] || fail "the install dir holds more than cf: $(ls -A "$T/bin")"
	assert_out_contains "installed $T/bin/cf ($VERSION)"
}

TestScenario_VerifiedInstall_replacesPreviousBinary() {
	good_release "$KEY_A"
	serve_dist
	seed_old_cf
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ "$("$T/bin/cf" version)" = "$BARE_VERSION" ] || fail "the previous cf was not replaced"
}

TestScenario_VerifiedInstall_hintsWhenDirNotOnPath() {
	good_release "$KEY_A"
	serve_dist
	run_install "$KEY_A/install.sh"
	assert_rc 0
	assert_out_contains "is not on your PATH"
}

TestScenario_VerifiedInstall_armLinux() {
	good_release "$KEY_A"
	serve_dist
	FAKE_UNAME_M=aarch64 run_install "$KEY_A/install.sh"
	assert_rc 0
	[ "$("$T/bin/cf" version)" = "$BARE_VERSION" ] || fail "cf version is wrong on linux/arm64"
}

# --- Bad signature or checksum ------------------------------------------------

TestScenario_BadSignatureOrChecksum_tamperedChecksums() {
	good_release "$KEY_A"
	sed '2s/^./z/' "$T/dist/checksums.txt" >"$T/dist/checksums.new"
	mv "$T/dist/checksums.new" "$T/dist/checksums.txt"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "signature verification failed: checksums.txt"
}

TestScenario_BadSignatureOrChecksum_signedByOtherKey() {
	make_dist "$T/dist"
	CF_SIGNING_KEY_FILE="$KEY_B/key" CF_INSTALL_SH="$KEY_A/install.sh" sh "$RELEASE_DIR/finalize.sh" "$T/dist" "$VERSION" >/dev/null
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "signature verification failed: checksums.txt"
}

TestScenario_BadSignatureOrChecksum_archiveShaMismatch() {
	good_release "$KEY_A"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cf_${BARE_VERSION}_linux_amd64.tar.gz" ok 6.6.6
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "checksum mismatch for cf_${BARE_VERSION}_linux_amd64.tar.gz"
}

TestScenario_BadSignatureOrChecksum_missingSignature() {
	good_release "$KEY_A"
	rm "$T/dist/checksums.txt.sig"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "download failed: .*checksums.txt.sig"
}

TestScenario_BadSignatureOrChecksum_revokedKey() {
	good_release "$KEY_A"
	awk -v key="$(cut -d' ' -f1,2 "$KEY_A/key.pub")" '{ print } $0 == "# BEGIN revoked_keys" { print key }' \
		"$KEY_A/install.sh" >"$T/install-revoked.sh"
	grep -qF "$(cut -d' ' -f2 "$KEY_A/key.pub")" "$T/install-revoked.sh" || fail "the fixture did not revoke the key"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$T/install-revoked.sh"
	assert_out_contains "signature verification failed: checksums.txt"
}

TestScenario_BadSignatureOrChecksum_wrongPrincipal() {
	good_release "$KEY_A"
	sed 's/^release@codefabric namespaces=/other@codefabric namespaces=/' "$KEY_A/install.sh" >"$T/install-principal.sh"
	cmp -s "$T/install-principal.sh" "$KEY_A/install.sh" && fail "the fixture did not change the principal"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$T/install-principal.sh"
	assert_out_contains "signature verification failed: checksums.txt"
}

TestScenario_BadSignatureOrChecksum_wrongNamespace() {
	good_release "$KEY_A"
	rm "$T/dist/checksums.txt.sig"
	ssh-keygen -q -Y sign -f "$KEY_A/key" -n some-other-namespace "$T/dist/checksums.txt"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "signature verification failed: checksums.txt"
}

# --- Header for another version -----------------------------------------------

TestScenario_HeaderOtherVersion() {
	good_release "$KEY_A"
	sed '1s/.*/version: v9.9.9/' "$T/dist/checksums.txt" >"$T/dist/checksums.new"
	mv "$T/dist/checksums.new" "$T/dist/checksums.txt"
	sign_checksums "$KEY_A"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "checksums.txt is signed for .v9.9.9., not $VERSION"
}

# --- Unsupported platform ------------------------------------------------------

TestScenario_UnsupportedPlatform_darwinIntel() {
	FAKE_UNAME_S=Darwin FAKE_UNAME_M=x86_64 run_install "$KEY_A/install.sh"
	assert_rc 3
	assert_out_contains "darwin/amd64"
	assert_no_requests
}

TestScenario_UnsupportedPlatform_freebsd() {
	FAKE_UNAME_S=FreeBSD FAKE_UNAME_M=amd64 run_install "$KEY_A/install.sh"
	assert_rc 3
	assert_out_contains "unsupported OS"
	assert_no_requests
}

TestScenario_UnsupportedPlatform_unknownArch() {
	FAKE_UNAME_M=riscv64 run_install "$KEY_A/install.sh"
	assert_rc 3
	assert_no_requests
}

# --- Placeholder ---------------------------------------------------------------

TestScenario_Placeholder() {
	grep -q 'ssh-ed25519 PLACEHOLDER' "$ROOT/install.sh" || fail "the repo install.sh no longer carries the placeholder; this test needs updating"
	good_release "$KEY_A"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$ROOT/install.sh"
	assert_out_contains "signature verification failed: checksums.txt"
}

# --- Installer hardening -------------------------------------------------------

# Replace the signed checksums with a body of our choosing, signed by the trusted key.
signed_checksums_body() { # lines...
	{
		printf 'version: %s\n' "$VERSION"
		printf '%s\n' "$@"
	} >"$T/dist/checksums.txt"
	sign_checksums "$KEY_A"
}

TestScenario_Hardening_duplicateChecksumEntry() {
	good_release "$KEY_A"
	line=$(sed -n '2p' "$T/dist/checksums.txt")
	signed_checksums_body "$line" "$line"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "cf_${BARE_VERSION}_linux_amd64.tar.gz is not listed exactly once"
}

TestScenario_Hardening_duplicateEntryWithDifferentHash() {
	good_release "$KEY_A"
	name=cf_${BARE_VERSION}_linux_amd64.tar.gz
	good=$(hash_of "$T/dist/$name")
	signed_checksums_body "$(printf '%064d  %s' 0 "$name")" "$good  $name"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "$name is not listed exactly once"
}

TestScenario_Hardening_malformedLine() {
	good_release "$KEY_A"
	name=cf_${BARE_VERSION}_linux_amd64.tar.gz
	signed_checksums_body "$(hash_of "$T/dist/$name") $name" # one space instead of two
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "checksums.txt contains a malformed line"
}

TestScenario_Hardening_entryForPlatformMissing() {
	good_release "$KEY_A"
	signed_checksums_body "$(sed -n '3p' "$T/dist/checksums.txt")"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "cf_${BARE_VERSION}_linux_amd64.tar.gz is not listed exactly once"
}

# A tarball whose checksum is signed is still not trusted to write outside the work dir or to ship a non-cf.
tarball_variant_is_rejected() { # mode
	make_dist "$T/dist"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cf_${BARE_VERSION}_linux_amd64.tar.gz" "$1" "$BARE_VERSION"
	finalize_dist "$T/dist" "$KEY_A"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "cf_${BARE_VERSION}_linux_amd64.tar.gz"
	[ ! -e "$T/evil" ] && [ ! -e "$T/dist/evil" ] || fail "a tar member escaped the work directory"
}

TestScenario_Hardening_tarMemberWithDotDot() { tarball_variant_is_rejected dotdot; }
TestScenario_Hardening_tarMemberDotDotCf() { tarball_variant_is_rejected dotdot_cf; }
TestScenario_Hardening_tarMemberNotCf() { tarball_variant_is_rejected notcf; }
TestScenario_Hardening_tarCfIsSymlink() { tarball_variant_is_rejected symlink; }

TestScenario_Hardening_missingPrerequisite() {
	good_release "$KEY_A"
	serve_dist
	seed_old_cf
	mkdir -p "$T/minpath"
	cp "$SHIM/uname" "$T/minpath/uname"
	for tool in curl tar awk sed grep mktemp cp chmod mv rm mkdir cat sha256sum shasum; do
		path=$(command -v "$tool" 2>/dev/null) || continue
		ln -s "$path" "$T/minpath/$tool"
	done
	[ ! -e "$T/minpath/ssh-keygen" ] || fail "the restricted PATH must not offer ssh-keygen"
	INST_PATH="$T/minpath" run_install "$KEY_A/install.sh"
	assert_rc 4
	assert_out_contains "ssh-keygen"
	assert_install_dir_untouched
	assert_no_requests
}

TestScenario_Hardening_invalidVersion() {
	for bad in 1.2.3 v1.2 'v1.2.3;id' latest-ish; do
		INST_VERSION=$bad run_install "$KEY_A/install.sh"
		assert_rc 2
		assert_out_contains "invalid CF_VERSION"
	done
	assert_no_requests
}

TestScenario_Hardening_multiLineVersion() {
	for bad in "$(printf 'v1.2.3\nv9.9.9')" "$(printf 'v1.2.3\ngarbage')" "$(printf 'garbage\nv1.2.3')"; do
		INST_VERSION=$bad run_install "$KEY_A/install.sh"
		assert_rc 2
		assert_out_contains "invalid CF_VERSION"
	done
	assert_no_requests
}

TestScenario_Hardening_unexpectedArguments() {
	run_install "$KEY_A/install.sh" --force
	assert_rc 2
	assert_no_requests
}

TestScenario_Hardening_downloadFailureNotPublished() {
	seed_old_cf
	INST_VERSION=v8.8.8 assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "download failed: .*v8.8.8/checksums.txt"
}

TestScenario_Hardening_downloadFailureServerDown() {
	seed_old_cf
	INST_BASE=http://127.0.0.1:1 assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "download failed: http://127.0.0.1:1/"
}

TestScenario_Hardening_downloadFailureTarballMissing() {
	good_release "$KEY_A"
	rm "$T/dist/cf_${BARE_VERSION}_linux_amd64.tar.gz"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "download failed: .*cf_${BARE_VERSION}_linux_amd64.tar.gz"
}

# --- Default path: CF_VERSION unset resolves the latest release ------------------

# publish_latest TARGET: releases/latest answers 302 to TARGET, which serves a page, as github.com does.
publish_latest() {
	mkdir -p "$WWW/$TEST_NAME/releases/tag"
	printf '%s\n' "$1" >"$WWW/$TEST_NAME/latest.redirect"
	: >"$WWW/$TEST_NAME/releases/tag/${1##*/}"
}

TestScenario_LatestVersion_installsWhatLatestRedirectsTo() {
	good_release "$KEY_A"
	serve_dist
	publish_latest "/$TEST_NAME/releases/tag/$VERSION"
	INST_VERSION=latest run_install "$KEY_A/install.sh"
	assert_rc 0
	assert_out_contains "installing $VERSION"
	[ "$("$T/bin/cf" version)" = "$BARE_VERSION" ] || fail "cf version is wrong after installing latest"
}

TestScenario_LatestVersion_unsetMeansLatest() {
	good_release "$KEY_A"
	serve_dist
	publish_latest "/$TEST_NAME/releases/tag/$VERSION"
	rc=0
	env -u CF_VERSION PATH="$SHIM:$PATH" HOME="$T/home" CF_RELEASE_BASE_URL="$URL/$TEST_NAME" CF_INSTALL_DIR="$T/bin" \
		"$(command -v sh)" "$KEY_A/install.sh" >"$T/out" 2>&1 || rc=$?
	assert_rc 0
	assert_out_contains "installing $VERSION"
}

TestScenario_LatestVersion_malformedRedirectTarget() {
	seed_old_cf
	publish_latest "/$TEST_NAME/releases/tag/not-a-version"
	INST_VERSION=latest assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "could not resolve the latest release"
}

TestScenario_LatestVersion_noLatestRelease() {
	seed_old_cf
	INST_VERSION=latest assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "download failed: .*/latest"
}

# --- Regression guards for the portable-awk fix ----------------------------------

# mawk (the Debian/Ubuntu default awk) has no regex intervals; the installer must not depend on them.
TestScenario_PortableAwk_installsWithMawk() {
	if ! command -v mawk >/dev/null 2>&1; then
		echo "SKIP: mawk not installed (brew install mawk | apt install mawk); the mawk guard did not run"
		return 0
	fi
	good_release "$KEY_A"
	serve_dist
	mkdir -p "$T/mawkbin"
	ln -s "$(command -v mawk)" "$T/mawkbin/awk"
	INST_PATH="$T/mawkbin:$SHIM:$PATH" run_install "$KEY_A/install.sh"
	assert_rc 0
	[ "$("$T/bin/cf" version)" = "$BARE_VERSION" ] || fail "cf was not installed with mawk as awk"
}

TestScenario_PortableAwk_noRegexIntervalInAwk() {
	hits=$(grep -nE 'awk.*[^$]\{[0-9]+(,[0-9]*)?\}' "$ROOT/install.sh" "$RELEASE_DIR"/*.sh || true)
	[ -z "$hits" ] || fail "regex interval inside awk (unsupported by mawk): $hits"
}
