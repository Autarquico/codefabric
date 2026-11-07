# shellcheck shell=sh
# shellcheck disable=SC2154
# Published-set scenarios: check.sh over a finalized dist, verify-release.sh over a fake `gh`.

check_dist() { run_cmd sh "$RELEASE_DIR/check.sh" "$T/dist" "$KEY_A/install.sh"; }

TestScenario_PublishedSet_completeSetPasses() {
	good_release "$KEY_A"
	check_dist
	assert_rc 0
	assert_out_contains "ok ($VERSION)"
}

TestScenario_PublishedSet_extraTarballFails() {
	good_release "$KEY_A"
	cp "$T/dist/cf_${BARE_VERSION}_linux_amd64.tar.gz" "$T/dist/cf_${BARE_VERSION}_linux_riscv64.tar.gz"
	check_dist
	assert_rc_nonzero
}

TestScenario_PublishedSet_missingTarballFails() {
	good_release "$KEY_A"
	rm "$T/dist/cf_${BARE_VERSION}_darwin_arm64.tar.gz"
	check_dist
	assert_rc_nonzero
}

TestScenario_PublishedSet_modifiedInstallShFails() {
	good_release "$KEY_A"
	printf '# tampered\n' >>"$T/dist/install.sh"
	check_dist
	assert_rc_nonzero
}

TestScenario_PublishedSet_installShNotTheCommitsFails() {
	good_release "$KEY_A"
	run_cmd sh "$RELEASE_DIR/check.sh" "$T/dist" "$KEY_B/install.sh"
	assert_rc_nonzero
	assert_out_contains "differs"
}

TestScenario_PublishedSet_tarballModifiedFails() {
	good_release "$KEY_A"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cf_${BARE_VERSION}_linux_arm64.tar.gz" ok 6.6.6
	check_dist
	assert_rc_nonzero
	assert_out_contains "does not match"
}

TestScenario_PublishedSet_checksumsSignedByOtherKeyFails() {
	good_release "$KEY_A"
	sign_checksums "$KEY_B"
	check_dist
	assert_rc_nonzero
	assert_out_contains "signature does not verify"
}

TestScenario_PublishedSet_checksumsMissingEntryFails() {
	good_release "$KEY_A"
	sed '$d' "$T/dist/checksums.txt" >"$T/dist/checksums.new"
	mv "$T/dist/checksums.new" "$T/dist/checksums.txt"
	sign_checksums "$KEY_A"
	check_dist
	assert_rc_nonzero
}

# verify-release.sh only talks to `gh`: a fake gh serves the dist directory as the release's assets.
fake_gh() { # assets-dir
	mkdir -p "$T/ghbin"
	cat >"$T/ghbin/gh" <<'EOF'
#!/bin/sh
case "$1 $2" in
	"release view") ls "$FAKE_GH_ASSETS" ;;
	"release download")
		while [ $# -gt 0 ]; do
			[ "$1" != --dir ] || dir=$2
			shift
		done
		cp "$FAKE_GH_ASSETS"/* "$dir"/ ;;
	*) exit 1 ;;
esac
EOF
	chmod 755 "$T/ghbin/gh"
	FAKE_GH_ASSETS=$1
	export FAKE_GH_ASSETS
}

verify_release() { # tag
	run_cmd env PATH="$T/ghbin:$PATH" sh "$RELEASE_DIR/verify-release.sh" "$1" "$KEY_A/install.sh" example/repo
}

TestScenario_PublishedSet_verifyReleasePassesOnPublishedSet() {
	good_release "$KEY_A"
	fake_gh "$T/dist"
	verify_release "$VERSION"
	assert_rc 0
	assert_out_contains "$VERSION ok"
}

TestScenario_PublishedSet_verifyReleaseRejectsExtraAsset() {
	good_release "$KEY_A"
	printf 'x' >"$T/dist/notes.txt"
	fake_gh "$T/dist"
	verify_release "$VERSION"
	assert_rc 1
	assert_out_contains "unexpected asset inventory"
}

TestScenario_PublishedSet_verifyReleaseRejectsMissingAsset() {
	good_release "$KEY_A"
	rm "$T/dist/install.sh.sig"
	fake_gh "$T/dist"
	verify_release "$VERSION"
	assert_rc 1
}

TestScenario_PublishedSet_verifyReleaseRejectsTamperedTarball() {
	good_release "$KEY_A"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cf_${BARE_VERSION}_linux_amd64.tar.gz" ok 6.6.6
	fake_gh "$T/dist"
	verify_release "$VERSION"
	assert_rc_nonzero
}

TestScenario_PublishedSet_verifyReleaseRejectsOtherTag() {
	good_release "$KEY_A"
	fake_gh "$T/dist"
	verify_release v1.2.4
	assert_rc_nonzero
}
