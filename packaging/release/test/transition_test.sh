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

# seed_transition_cf PATH: a transition cf of some other release: it answers the probe, and its bytes are not the tarball's.
seed_transition_cf() {
	mkdir -p "$(dirname "$1")"
	cat >"$1" <<'SH'
#!/bin/sh
# transition cf of another release
if [ -n "$CF_TRANSITION_PROBE" ]; then echo code-flock-cf-transition; else echo old-transition-cf; fi
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

# A cf that does not answer is not Code Flock's: it is left alone, and the installer does not wait for it.
TestScenario_CfAjeno_doesNotAnswerInTime() {
	installed_release
	mkdir -p "$T/bin"
	printf '#!/bin/sh\nexec sleep 60\n' >"$T/bin/cf"
	chmod 755 "$T/bin/cf"
	cp "$T/bin/cf" "$T/old-cf"
	started=$(date +%s)
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ $(($(date +%s) - started)) -lt 30 ] || fail "the installer waited for the cf"
	cmp -s "$T/bin/cf" "$T/old-cf" || fail "the cf that does not answer was modified"
	assert_out_contains "left $T/bin/cf alone"
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

# --- A cf in another directory of the PATH: said, never touched -------------------------

cf_in_another_dir() { # JSON
	installed_release
	seed_cf "$T/other/cf" "$1"
	cp "$T/other/cf" "$T/old-cf"
	INST_PATH="$T/other:$SHIM:$PATH" run_install "$KEY_A/install.sh"
	assert_rc 0
	cmp -s "$T/other/cf" "$T/old-cf" || fail "the cf in the other directory was modified"
	[ "$(ls -A "$T/bin")" = "cflk" ] || fail "unexpected files in the install dir: $(ls -A "$T/bin")"
}

# It is said as a fact, and never run: whatever the cf is, it says nothing of which cflk runs or of what the cf is.
TestScenario_CfEnOtroDirectorio_seDiceYNoSeToca() {
	cf_in_another_dir "$(beta_json)"
	assert_out_contains "another cf is first on PATH at $T/other/cf; it was left alone. The Code Flock command is cflk"
	! grep -q "CFLK_INSTALL_DIR=" "$T/out" || { dump; fail "recommended another install dir"; }
}

TestScenario_CfEnOtroDirectorio_noSeEjecuta() {
	installed_release
	mkdir -p "$T/other"
	printf '#!/bin/sh\n: >"%s/ran"\n' "$T" >"$T/other/cf"
	chmod 755 "$T/other/cf"
	INST_PATH="$T/other:$SHIM:$PATH" run_install "$KEY_A/install.sh"
	assert_rc 0
	[ ! -e "$T/ran" ] || fail "the installer ran the cf of the PATH"
}

# SEC-1: a trailing ':' in PATH is the current directory; a cf there is not run, and cflk is installed all the same.
TestScenario_CfEnOtroDirectorio_pathConEntradaVaciaNoEjecutaElCfDelDirectorioActual() {
	installed_release
	mkdir -p "$T/repo"
	printf '#!/bin/sh\n: >"%s/pwned"\n' "$T" >"$T/repo/cf"
	chmod 755 "$T/repo/cf"
	cd "$T/repo" || exit 1
	INST_PATH="$SHIM:$PATH:" run_install "$KEY_A/install.sh"
	assert_rc 0
	[ ! -e "$T/pwned" ] || fail "the cf of the current directory was run"
	[ -x "$T/bin/cflk" ] || fail "cflk was not installed"
}

TestScenario_CfEnOtroDirectorio_noDiceNadaSiEsElDelDirectorioDeInstalacion() {
	installed_release
	seed_cf "$T/bin/cf" "$(beta_json)"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	! grep -q "another cf is first" "$T/out" || { dump; fail "reported a cf that was replaced"; }
}

# CODE-7: the cf that was just replaced, reached through a link, is not "another cf" even when the directory
# the link pointed to is first on the PATH: the cf there is the one the link was replaced by, or it is a cf that was left alone.
TestScenario_SymlinkedCf_theLinkJustReplacedIsNotReportedAsAnother() {
	installed_release
	seed_cf "$T/real/cf" "$(beta_json)"
	mkdir -p "$T/bin"
	ln -s "$T/real/cf" "$T/bin/cf"
	INST_PATH="$T/bin:$SHIM:$PATH" run_install "$KEY_A/install.sh"
	assert_rc 0
	assert_out_contains "replaced the cf link at $T/bin/cf"
	! grep -q "another cf is first" "$T/out" || { dump; fail "reported the replaced cf as another one"; }
}

# --- Symlinks -----------------------------------------------------------------------

# A link to another directory is replaced itself; the cf it pointed to is never touched.
TestScenario_SymlinkedCf_theLinkIsReplacedAndTheTargetIsLeftAlone() {
	installed_release
	seed_cf "$T/real/cf" "$(beta_json)"
	cp "$T/real/cf" "$T/old-cf"
	mkdir -p "$T/bin"
	ln -s "$T/real/cf" "$T/bin/cf"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ ! -L "$T/bin/cf" ] || fail "the link was not replaced"
	[ "$("$T/bin/cf")" = "transition-cf" ] || fail "the link was not replaced by the transition cf"
	cmp -s "$T/real/cf" "$T/old-cf" || fail "the cf the link pointed to was modified"
	assert_out_contains "$T/bin/cf is a symlink to .*/real/cf"
	assert_out_contains "replaced the cf link at $T/bin/cf"
	assert_out_contains ".*/real/cf was left alone; "
}

# A link inside the install directory is not "another directory": its target is replaced and the link stays.
TestScenario_SymlinkedCf_aLinkInsideTheInstallDirKeepsItsShape() {
	installed_release
	seed_cf "$T/bin/cf.real" "$(beta_json)"
	ln -s cf.real "$T/bin/cf"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ -L "$T/bin/cf" ] || fail "the symlink was replaced by a file"
	[ "$("$T/bin/cf.real")" = "transition-cf" ] || fail "the target in the install dir was not replaced"
	assert_out_contains "replaced the cf of Code Flock 0.4.0-beta.6 at .*/bin/cf.real"
}

# The first line only states the fact; what happened is said by the line that follows.
TestScenario_SymlinkedCf_toAForeignCfSaysOnlyThatItWasLeftAlone() {
	installed_release
	seed_cf "$T/real/cf" 'cf version 8.7.0'
	cp "$T/real/cf" "$T/old-cf"
	mkdir -p "$T/bin"
	ln -s "$T/real/cf" "$T/bin/cf"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	cmp -s "$T/real/cf" "$T/old-cf" || fail "the foreign cf was modified"
	assert_out_contains "$T/bin/cf is a symlink to .*/real/cf"
	assert_out_contains "left .*/real/cf alone"
	! grep -q "replaced" "$T/out" || { dump; fail "the output says something was replaced"; }
}

# The link target is found as the kernel does: a ../ after a symlinked directory is relative to the real directory.
TestScenario_SymlinkedCf_relativeTargetBehindASymlinkedDirectory() {
	installed_release
	seed_cf "$T/x/z/cf" "$(beta_json)"
	mkdir -p "$T/x/y"
	ln -s "$T/x/y" "$T/bin"
	ln -s ../z/cf "$T/x/y/cf"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ "$("$T/x/y/cf")" = "transition-cf" ] || fail "the link was not replaced by the transition cf"
	[ "$("$T/x/z/cf")" != "transition-cf" ] || fail "the target the kernel resolves to was replaced"
	assert_out_contains "$T/bin/cf is a symlink to $(cd -P "$T/x/z" && pwd -P)/cf"
}

TestScenario_SymlinkedCf_relativeLinkIsResolved() {
	installed_release
	seed_cf "$T/real/cf" "$(beta_json)"
	mkdir -p "$T/bin"
	ln -s ../real/cf "$T/bin/cf"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ "$("$T/bin/cf")" = "transition-cf" ] || fail "the relative link was not replaced"
	[ ! -L "$T/bin/cf" ] || fail "the link was kept"
	[ "$("$T/real/cf")" != "transition-cf" ] || fail "the cf outside the install dir was replaced"
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

# The transition cf of another release is recognised by what it answers, not by its bytes.
TestScenario_Reinstall_overTheTransitionCfOfAnotherReleaseUpdatesItQuietly() {
	installed_release
	seed_transition_cf "$T/bin/cf"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ "$("$T/bin/cf")" = "transition-cf" ] || fail "the old transition cf was not updated"
	assert_out_contains "updated the transition cf at $T/bin/cf"
	! grep -q "not the cf\|left" "$T/out" || { dump; fail "alarming output for our own transition cf"; }
}

# --- Order: cflk first, validated, and only then the cf ---------------------------------

# 1.2.3 is not 1.2.3-beta.1: the version is compared whole.
TestScenario_Order_aCflkOfALongerVersionIsNotTheOneRequested() {
	make_dist "$T/dist"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cflk_${BARE_VERSION}_linux_amd64.tar.gz" ok "$BARE_VERSION-beta.1"
	finalize_dist "$T/dist" "$KEY_A"
	serve_dist
	run_install "$KEY_A/install.sh"
	assert_rc 4
	assert_out_contains "does not report $VERSION"
	[ -z "$(ls -A "$T/bin")" ] || fail "something was installed: $(ls -A "$T/bin")"
}

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
	assert_out_contains "curl -fsSL .*/$VERSION/install.sh | CF_VERSION=$VERSION CF_INSTALL_DIR='$T/bin' sh"
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

TestScenario_ReleaseSet_checkRejectsATarballWithAnUnsafeMemberPath() {
	make_dist "$T/dist"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cflk_${BARE_VERSION}_linux_amd64.tar.gz" dotdot "$BARE_VERSION"
	finalize_dist "$T/dist" "$KEY_A"
	run_cmd sh "$RELEASE_DIR/check.sh" "$T/dist" "$KEY_A/install.sh"
	assert_rc 1
	assert_out_contains "cflk_${BARE_VERSION}_linux_amd64.tar.gz has an unsafe member path"
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

# --- A cf that is not trustworthy is neither run nor read ---------------------------------

# the probe would leave $T/ran behind
seed_probing_cf() { # PATH
	mkdir -p "$(dirname "$1")"
	printf '#!/bin/sh\n: >"%s/ran"\nprintf "%%s\\n" '"'"'%s'"'"'\n' "$T" "$(beta_json)" >"$1"
	chmod 755 "$1"
}

# The installer's primary group is not private (macOS: staff), so its write bit opens the cf too.
TestScenario_CfNoFiable_writableByThePrimaryGroupIsNotRun() {
	installed_release
	seed_probing_cf "$T/bin/cf"
	chmod 775 "$T/bin/cf"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ ! -e "$T/ran" ] || fail "a cf writable by the primary group was run"
}

TestScenario_CfNoFiable_writableByAnotherGroupIsNotRun() {
	other_group=$(id -G | tr ' ' '\n' | grep -vx "$(id -g)" | head -n 1)
	[ -n "$other_group" ] || { echo "SKIP: the user belongs to no second group"; return 0; }
	installed_release
	seed_probing_cf "$T/bin/cf"
	chgrp "$other_group" "$T/bin/cf"
	chmod 775 "$T/bin/cf"
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ ! -e "$T/ran" ] || fail "a cf that others can write was run"
	assert_out_contains "left $T/bin/cf alone: it, or a directory above it, is not owned by you or root, or can be written by others"
	[ "$("$T/bin/cflk" version)" = "$BARE_VERSION" ] || fail "cflk was not installed"
}

TestScenario_CfNoFiable_inADirectoryOthersCanWriteIsNotRun() {
	installed_release
	seed_probing_cf "$T/other/cf"
	chmod 777 "$T/other"
	mkdir -p "$T/bin"
	ln -s "$T/other/cf" "$T/bin/cf"
	run_install "$KEY_A/install.sh"
	chmod 755 "$T/other"
	assert_rc 0
	[ ! -e "$T/ran" ] || fail "a cf in a directory others can write was run"
	[ -L "$T/bin/cf" ] || fail "the link changed"
	assert_out_contains "left .*/other/cf alone: it, or a directory above it, is not owned by you or root"
}

# A directory above the cf that others can write lets them swap the cf between the check and the run.
TestScenario_CfNoFiable_underAGrandparentOthersCanWriteIsNotRun() {
	installed_release
	seed_probing_cf "$T/open/d/cf"
	chmod 777 "$T/open"
	mkdir -p "$T/bin"
	ln -s "$T/open/d/cf" "$T/bin/cf"
	run_install "$KEY_A/install.sh"
	chmod 755 "$T/open"
	assert_rc 0
	[ ! -e "$T/ran" ] || fail "a cf under a directory others can write was run"
	assert_out_contains "left .*/open/d/cf alone: it, or a directory above it, is not owned by you or root"
}

# A sticky directory such as /tmp: others can add to it, not rename what is ours.
TestScenario_CfNoFiable_underAStickyGrandparentIsRun() {
	installed_release
	seed_probing_cf "$T/sticky/d/cf"
	chmod 1777 "$T/sticky"
	mkdir -p "$T/bin"
	ln -s "$T/sticky/d/cf" "$T/bin/cf"
	run_install "$KEY_A/install.sh"
	chmod 755 "$T/sticky"
	assert_rc 0
	[ -e "$T/ran" ] || fail "a cf under a sticky directory was not probed"
}

# No metadata (the path vanished between the checks) is not closed.
TestScenario_CfNoFiable_aMissingPathIsNotClosed() {
	sed '/^main "\$@"$/d' "$RELEASE_DIR/../../install.sh" >"$T/install-lib.sh"
	# shellcheck source=/dev/null
	if (. "$T/install-lib.sh" && closed_to_others "$T/missing"); then fail "a missing path counted as closed"; fi
}

# A name with a newline could forge a second `ls` line; it is never closed.
TestScenario_CfNoFiable_aNameWithANewlineIsNotClosed() {
	sed '/^main "\$@"$/d' "$RELEASE_DIR/../../install.sh" >"$T/install-lib.sh"
	forged="$T/x
drwxr-xr-x 1 0 0 0 Jan 1 00:00 y"
	mkdir -p "$forged"
	# shellcheck source=/dev/null
	if (. "$T/install-lib.sh" && closed_to_others "$forged"); then fail "a name with a newline counted as closed"; fi
}

# macOS: the `deny delete` ACL of every home keeps it closed; an `allow write` entry opens it.
TestScenario_CfNoFiable_macOSACLs() {
	[ "$(uname -s)" = Darwin ] || { echo "SKIP: macOS ACLs"; return 0; }
	sed '/^main "\$@"$/d' "$RELEASE_DIR/../../install.sh" >"$T/install-lib.sh"
	mkdir -p "$T/acl"
	chmod +a "everyone deny delete" "$T/acl"
	# shellcheck source=/dev/null
	(. "$T/install-lib.sh" && closed_to_others "$T/acl") || fail "a deny-delete ACL counted as open"
	for right in write delete; do
		chmod -N "$T/acl"
		chmod +a "everyone allow $right" "$T/acl"
		# shellcheck source=/dev/null
		if (. "$T/install-lib.sh" && closed_to_others "$T/acl"); then fail "an allow-$right ACL counted as closed"; fi
	done
	chmod -N "$T/acl"
}

# A FIFO would block cmp and the probe's open: it is not a regular file, so it is left alone, and quickly.
TestScenario_CfNoFiable_aFifoIsLeftAloneWithoutWaiting() {
	installed_release
	mkdir -p "$T/bin"
	mkfifo "$T/bin/cf"
	started=$(date +%s)
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ $(($(date +%s) - started)) -lt 4 ] || fail "the installer waited for the FIFO"
	[ -p "$T/bin/cf" ] || fail "the FIFO changed"
	assert_out_contains "left $T/bin/cf alone: it is not a regular file"
}

TestScenario_CfNoFiable_aLinkToAFifoIsLeftAlone() {
	installed_release
	mkdir -p "$T/bin" "$T/other"
	mkfifo "$T/other/cf"
	ln -s "$T/other/cf" "$T/bin/cf"
	started=$(date +%s)
	run_install "$KEY_A/install.sh"
	assert_rc 0
	[ $(($(date +%s) - started)) -lt 4 ] || fail "the installer waited for the FIFO"
	[ -L "$T/bin/cf" ] || fail "the link changed"
	assert_out_contains "not a regular file"
}

# --- CDPATH does not redirect a relative install directory -----------------------------

TestScenario_Cdpath_aRelativeInstallDirIsNotRedirected() {
	installed_release
	mkdir -p "$T/work/bin" "$T/decoy/bin"
	seed_cf "$T/work/bin/cf" "$(beta_json)"
	cd "$T/work" || exit 1
	rc=0
	env PATH="$SHIM:$PATH" HOME="$T/home" CDPATH="$T/decoy" \
		CFLK_RELEASE_BASE_URL="$URL/$TEST_NAME" CFLK_INSTALL_DIR=bin CFLK_VERSION="$VERSION" \
		FAKE_UNAME_S=Linux FAKE_UNAME_M=x86_64 "$(command -v sh)" "$KEY_A/install.sh" >"$T/out" 2>&1 || rc=$?
	assert_rc 0
	[ "$("$T/work/bin/cf")" = "transition-cf" ] || fail "the beta cf was not replaced"
	[ -z "$(ls -A "$T/decoy/bin")" ] || fail "the CDPATH directory was used"
}

# --- The release check enforces the installer's rule on every tarball ---------------------

TestScenario_ReleaseSet_checkRejectsADuplicateCf() {
	make_dist "$T/dist"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cflk_${BARE_VERSION}_linux_arm64.tar.gz" dupcf "$BARE_VERSION"
	finalize_dist "$T/dist" "$KEY_A"
	run_cmd sh "$RELEASE_DIR/check.sh" "$T/dist" "$KEY_A/install.sh"
	assert_rc 1
	assert_out_contains "cflk_${BARE_VERSION}_linux_arm64.tar.gz contains cf more than once"
}

TestScenario_ReleaseSet_checkRejectsASymlinkedCflk() {
	make_dist "$T/dist"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cflk_${BARE_VERSION}_darwin_amd64.tar.gz" symlink "$BARE_VERSION"
	finalize_dist "$T/dist" "$KEY_A"
	run_cmd sh "$RELEASE_DIR/check.sh" "$T/dist" "$KEY_A/install.sh"
	assert_rc 1
	assert_out_contains "cflk in cflk_${BARE_VERSION}_darwin_amd64.tar.gz is not a regular file"
}
