# shellcheck shell=sh
# shellcheck disable=SC2154
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
	assert_out_contains "signature verification failed"
}

TestScenario_BadSignatureOrChecksum_signedByOtherKey() {
	make_dist "$T/dist"
	CF_SIGNING_KEY_FILE="$KEY_B/key" CF_INSTALL_SH="$KEY_A/install.sh" sh "$RELEASE_DIR/finalize.sh" "$T/dist" "$VERSION" >/dev/null
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "signature verification failed"
}

TestScenario_BadSignatureOrChecksum_archiveShaMismatch() {
	good_release "$KEY_A"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cf_${BARE_VERSION}_linux_amd64.tar.gz" ok 6.6.6
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "checksum mismatch"
}

TestScenario_BadSignatureOrChecksum_missingSignature() {
	good_release "$KEY_A"
	rm "$T/dist/checksums.txt.sig"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
}

TestScenario_BadSignatureOrChecksum_revokedKey() {
	good_release "$KEY_A"
	awk -v key="$(cut -d' ' -f1,2 "$KEY_A/key.pub")" '{ print } $0 == "# BEGIN revoked_keys" { print key }' \
		"$KEY_A/install.sh" >"$T/install-revoked.sh"
	grep -qF "$(cut -d' ' -f2 "$KEY_A/key.pub")" "$T/install-revoked.sh" || fail "the fixture did not revoke the key"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$T/install-revoked.sh"
	assert_out_contains "signature verification failed"
}

TestScenario_BadSignatureOrChecksum_wrongPrincipal() {
	good_release "$KEY_A"
	sed 's/^release@codefabric namespaces=/other@codefabric namespaces=/' "$KEY_A/install.sh" >"$T/install-principal.sh"
	cmp -s "$T/install-principal.sh" "$KEY_A/install.sh" && fail "the fixture did not change the principal"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$T/install-principal.sh"
	assert_out_contains "signature verification failed"
}

TestScenario_BadSignatureOrChecksum_wrongNamespace() {
	good_release "$KEY_A"
	rm "$T/dist/checksums.txt.sig"
	ssh-keygen -q -Y sign -f "$KEY_A/key" -n some-other-namespace "$T/dist/checksums.txt"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "signature verification failed"
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
	assert_out_contains "another version"
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
	assert_out_contains "signature verification failed"
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
	assert_out_contains "exactly once"
}

TestScenario_Hardening_duplicateEntryWithDifferentHash() {
	good_release "$KEY_A"
	name=cf_${BARE_VERSION}_linux_amd64.tar.gz
	good=$(hash_of "$T/dist/$name")
	signed_checksums_body "$(printf '%064d  %s' 0 "$name")" "$good  $name"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
}

TestScenario_Hardening_malformedLine() {
	good_release "$KEY_A"
	name=cf_${BARE_VERSION}_linux_amd64.tar.gz
	signed_checksums_body "$(hash_of "$T/dist/$name") $name" # one space instead of two
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "malformed"
}

TestScenario_Hardening_entryForPlatformMissing() {
	good_release "$KEY_A"
	signed_checksums_body "$(sed -n '3p' "$T/dist/checksums.txt")"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
}

# A tarball whose checksum is signed is still not trusted to write outside the work dir or to ship a non-cf.
tarball_variant_is_rejected() { # mode
	make_dist "$T/dist"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cf_${BARE_VERSION}_linux_amd64.tar.gz" "$1" "$BARE_VERSION"
	finalize_dist "$T/dist" "$KEY_A"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
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

TestScenario_Hardening_unexpectedArguments() {
	run_install "$KEY_A/install.sh" --force
	assert_rc 2
	assert_no_requests
}

TestScenario_Hardening_downloadFailureNotPublished() {
	seed_old_cf
	INST_VERSION=v8.8.8 assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "download failed"
}

TestScenario_Hardening_downloadFailureServerDown() {
	seed_old_cf
	INST_BASE=http://127.0.0.1:1 assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "download failed"
}

TestScenario_Hardening_downloadFailureTarballMissing() {
	good_release "$KEY_A"
	rm "$T/dist/cf_${BARE_VERSION}_linux_amd64.tar.gz"
	serve_dist
	seed_old_cf
	assert_rejected_untouched "$KEY_A/install.sh"
	assert_out_contains "download failed"
}
