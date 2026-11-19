# shellcheck shell=sh
# shellcheck disable=SC2154,SC2034
# Scenarios of the cf -> cflk rename in the installer and the release set: updating the cf of a Code Flock beta,
# leaving any other cf alone, symlinks, reinstalling, the old CF_* variables, and old releases.

BETA_COMMIT=0123456789abcdef0123456789abcdef01234567

# seed_cf PATH JSON: an executable `cf` whose `version --json` prints JSON. Any other call prints "beta-cf".
seed_cf() {
	mkdir -p "$(dirname "$1")"
	cat >"$1" <<SH
#!/bin/sh
if [ "\$1" = version ] && [ "\$2" = --json ]; then printf '%s\n' '$2'; else echo beta-cf; fi
SH
	chmod 755 "$1"
}

# The output of `cf version --json` of the beta.6 binary: exactly these three keys.
beta_json() { printf '{"version":"%s","commit":"%s","date":"2026-11-10T10:00:00Z"}' "${1:-0.4.0-beta.6}" "$BETA_COMMIT"; }

installed_release() {
	good_release "$KEY_A"
	serve_dist
}

TestScenario_UpdateCfOfCodeFlock_betaBecomesTransition() {
	installed_release
	seed_cf "$T/bin/cf" "$(beta_json)"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ "$("$T/bin/cflk" version)" = "$BARE_VERSION" ] || fail "cflk was not installed"
	[ "$("$T/bin/cf")" = "transition-cf" ] || fail "cf was not replaced by the transition cf"
	assert_out_contains "replaced the cf of Code Flock 0.4.0-beta.6"
	assert_out_contains "runs cflk"
	[ "$(find "$T/bin" -mindepth 1 | wc -l | tr -d ' ')" -eq 2 ] || fail "unexpected files: $(ls -A "$T/bin")"
}

# The real cf of the beta.6, built from its tag with its own GoReleaser ldflags, updated by the new installer.
TestScenario_UpdateCfOfCodeFlock_realBeta6() {
	command -v go >/dev/null 2>&1 || fail "go is required for the real beta.6 test"
	git -C "$ROOT" rev-parse -q --verify "v0.4.0-beta.6^{commit}" >/dev/null 2>&1 || {
		[ -z "${CI:-}" ] || fail "tag v0.4.0-beta.6 not in this clone: CI must fetch it"
		echo "SKIP: tag v0.4.0-beta.6 not in this clone"
		return 0
	}
	mkdir -p "$T/beta6" "$T/bin"
	git -C "$ROOT" archive v0.4.0-beta.6 | tar -x -C "$T/beta6"
	pkg=github.com/Autarquico/codefabric/internal/buildinfo
	(cd "$T/beta6" && CGO_ENABLED=0 go build -trimpath \
		-ldflags "-s -w -X $pkg.version=0.4.0-beta.6 -X $pkg.commit=$BETA_COMMIT -X $pkg.date=2026-11-10T10:00:00Z" \
		-o "$T/bin/cf" ./cmd/cf) || fail "could not build the beta.6 cf"
	installed_release
	run_install "$KEY_A/install.sh"
	assert_rc 0
	assert_out_contains "replaced the cf of Code Flock 0.4.0-beta.6"
	[ "$("$T/bin/cf")" = "transition-cf" ] || fail "the real beta.6 cf was not replaced"
}

TestScenario_UpdateCfOfCodeFlock_oldestBetaToo() {
	installed_release
	seed_cf "$T/bin/cf" "$(beta_json 0.3.1)"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ "$("$T/bin/cf")" = "transition-cf" ] || fail "an older Code Flock cf was not replaced"
}

# --- A cf that is not (with certainty) Code Flock's is never touched ---------------

foreign_cf_is_left_alone() { # JSON
	installed_release
	seed_cf "$T/bin/cf" "$1"
	cp "$T/bin/cf" "$T/old-cf"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	cmp -s "$T/bin/cf" "$T/old-cf" || fail "the foreign cf was modified"
	[ "$("$T/bin/cflk" version)" = "$BARE_VERSION" ] || fail "cflk was not installed next to it"
	assert_out_contains "left $T/bin/cf alone"
}

TestScenario_CfAjeno_cloudFoundry() { foreign_cf_is_left_alone 'cf version 8.7.0'; }
TestScenario_CfAjeno_sameShapeOtherVersion() { foreign_cf_is_left_alone "$(beta_json 8.7.0)"; }
TestScenario_CfAjeno_sameShapeAfterTheBetas() { foreign_cf_is_left_alone "$(beta_json 0.4.0)"; }
TestScenario_CfAjeno_extraKey() {
	foreign_cf_is_left_alone '{"version":"0.4.0-beta.6","commit":"'$BETA_COMMIT'","date":"2026-11-10T10:00:00Z","extra":"x"}'
}
TestScenario_CfAjeno_missingKey() { foreign_cf_is_left_alone '{"version":"0.4.0-beta.6","commit":"'$BETA_COMMIT'"}'; }
TestScenario_CfAjeno_commitNotAHash() {
	foreign_cf_is_left_alone '{"version":"0.4.0-beta.6","commit":"not a hash","date":"2026-11-10T10:00:00Z"}'
}

TestScenario_CfAjeno_notRunnable() {
	installed_release
	mkdir -p "$T/bin"
	printf 'not an executable' >"$T/bin/cf"
	cp "$T/bin/cf" "$T/old-cf"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	cmp -s "$T/bin/cf" "$T/old-cf" || fail "the cf that does not run was modified"
	assert_out_contains "left $T/bin/cf alone"
}

# --- Symlinks -----------------------------------------------------------------------

TestScenario_SymlinkedCf_theTargetIsReplacedAndReported() {
	installed_release
	seed_cf "$T/real/cf" "$(beta_json)"
	mkdir -p "$T/bin"
	ln -s "$T/real/cf" "$T/bin/cf"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ -L "$T/bin/cf" ] || fail "the symlink was replaced by a file"
	[ "$("$T/real/cf")" = "transition-cf" ] || fail "the symlink target was not replaced"
	assert_out_contains "$T/bin/cf is a symlink to "
}

TestScenario_SymlinkedCf_relativeLinkIsResolved() {
	installed_release
	seed_cf "$T/real/cf" "$(beta_json)"
	mkdir -p "$T/bin"
	ln -s ../real/cf "$T/bin/cf"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ "$("$T/real/cf")" = "transition-cf" ] || fail "the relative symlink target was not replaced"
}

TestScenario_SymlinkedCf_toCflkIsNotReplaced() {
	installed_release
	mkdir -p "$T/bin"
	printf '#!/bin/sh\necho old\n' >"$T/bin/cflk"
	chmod 755 "$T/bin/cflk"
	ln -s cflk "$T/bin/cf"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ "$("$T/bin/cflk" version)" = "$BARE_VERSION" ] || fail "cflk is not the new one"
	assert_out_contains "already runs cflk"
	[ -L "$T/bin/cf" ] || fail "the cf link changed"
}

TestScenario_SymlinkedCf_danglingIsLeftAlone() {
	installed_release
	mkdir -p "$T/bin"
	ln -s "$T/nowhere" "$T/bin/cf"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ -L "$T/bin/cf" ] || fail "the dangling link changed"
	[ ! -e "$T/nowhere" ] || fail "something was created at the dangling target"
	assert_out_contains "does not resolve"
}

# --- Reinstalling ---------------------------------------------------------------------

TestScenario_Reinstall_overTheTransitionCfChangesNothing() {
	installed_release
	seed_cf "$T/bin/cf" "$(beta_json)"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	before=$(ls -i "$T/bin/cf")
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ "$(ls -i "$T/bin/cf")" = "$before" ] || fail "the transition cf was rewritten"
	[ "$("$T/bin/cf")" = "transition-cf" ] || fail "the transition cf is gone"
	assert_out_contains "already the transition cf"
}

# --- Order: cflk first, validated, and only then the cf ---------------------------------

TestScenario_Order_aBrokenCflkLeavesTheCfAlone() {
	make_dist "$T/dist"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cflk_${BARE_VERSION}_linux_amd64.tar.gz" ok 6.6.6
	finalize_dist "$T/dist" "$KEY_A"
	serve_dist
	seed_cf "$T/bin/cf" "$(beta_json)"
	cp "$T/bin/cf" "$T/old-cf"
	run_install "$KEY_A/install.sh"
	assert_rc 4
	assert_out_contains "does not report $VERSION"
	cmp -s "$T/bin/cf" "$T/old-cf" || fail "the cf was replaced although cflk did not validate"
	[ "$(ls -A "$T/bin")" = "cf" ] || fail "something was left in the install dir: $(ls -A "$T/bin")"
}

# --- Releases from before the rename --------------------------------------------------

TestScenario_OldRelease_isRefusedWithTheInstallerToUse() {
	good_release "$KEY_A"
	signed_checksums_body "$(printf '%064d  cf_%s_linux_amd64.tar.gz' 0 "$BARE_VERSION")"
	serve_dist
	seed_cf "$T/bin/cf" "$(beta_json)"
	cp "$T/bin/cf" "$T/old-cf"
	run_install "$KEY_A/install.sh"
	assert_rc 5
	assert_out_contains "predates the cflk rename"
	assert_out_contains "/$VERSION/install.sh"
	cmp -s "$T/bin/cf" "$T/old-cf" || fail "the cf was modified"
	[ "$(ls -A "$T/bin")" = "cf" ] || fail "something landed in the install dir"
}

# --- The CF_* variables keep working, with a warning --------------------------------------

TestScenario_VariableAntigua_installerReadsThemWithAWarning() {
	installed_release
	rc=0
	env PATH="$SHIM:$PATH" HOME="$T/home" CF_RELEASE_BASE_URL="$URL/$TEST_NAME" CF_INSTALL_DIR="$T/bin" CF_VERSION="$VERSION" \
		FAKE_UNAME_S=Linux FAKE_UNAME_M=x86_64 "$(command -v sh)" "$KEY_A/install.sh" >"$T/out" 2>&1 || rc=$?
	assert_rc 0
	[ "$("$T/bin/cflk" version)" = "$BARE_VERSION" ] || fail "the CF_* variables were not honoured"
	assert_out_contains "CF_VERSION is deprecated"
	assert_out_contains "CF_INSTALL_DIR is deprecated"
	assert_out_contains "CF_RELEASE_BASE_URL is deprecated"
}

TestScenario_VariableAntigua_cflkWinsWhenBothAreSet() {
	installed_release
	rc=0
	env PATH="$SHIM:$PATH" HOME="$T/home" CFLK_RELEASE_BASE_URL="$URL/$TEST_NAME" CFLK_INSTALL_DIR="$T/bin" CFLK_VERSION="$VERSION" \
		CF_INSTALL_DIR="$T/other" CF_VERSION=v9.9.9 FAKE_UNAME_S=Linux FAKE_UNAME_M=x86_64 \
		"$(command -v sh)" "$KEY_A/install.sh" >"$T/out" 2>&1 || rc=$?
	assert_rc 0
	[ -x "$T/bin/cflk" ] || fail "CFLK_INSTALL_DIR was not used"
	[ ! -e "$T/other" ] || fail "CF_INSTALL_DIR was used although CFLK_INSTALL_DIR is set"
	assert_out_contains "CF_INSTALL_DIR is ignored because CFLK_INSTALL_DIR is set"
	assert_out_contains "CF_VERSION is ignored because CFLK_VERSION is set"
}

# --- The release set: every tarball carries both binaries --------------------------------

TestScenario_ReleaseSet_checkRejectsATarballWithoutTheTransitionCf() {
	make_dist "$T/dist"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cflk_${BARE_VERSION}_darwin_arm64.tar.gz" nocf "$BARE_VERSION"
	finalize_dist "$T/dist" "$KEY_A"
	run_cmd sh "$RELEASE_DIR/check.sh" "$T/dist" "$KEY_A/install.sh"
	assert_rc 1
	assert_out_contains "cflk_${BARE_VERSION}_darwin_arm64.tar.gz does not contain cf"
}

TestScenario_ReleaseSet_fourTarballsWithBothBinariesPassCheck() {
	good_release "$KEY_A"
	run_cmd sh "$RELEASE_DIR/check.sh" "$T/dist" "$KEY_A/install.sh"
	assert_rc 0
	for target in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64; do
		tar -tzf "$T/dist/cflk_${BARE_VERSION}_$target.tar.gz" | grep -qx cflk || fail "$target lacks cflk"
		tar -tzf "$T/dist/cflk_${BARE_VERSION}_$target.tar.gz" | grep -qx cf || fail "$target lacks cf"
	done
}
