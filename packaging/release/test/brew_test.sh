# shellcheck shell=sh
# shellcheck disable=SC2154,SC2034
# Scenario "Fórmula derivada": brew-formula.sh over a finalized dist, trust root = KEY_A's install.sh.

BREW_BASE=https://example.invalid/releases/download

# brew_formula [VERSION]: sets rc, $T/out (stdout) and $T/err (stderr).
brew_formula() {
	rc=0
	CF_RELEASE_BASE_URL=$BREW_BASE "$RELEASE_DIR/brew-formula.sh" "$T/dist/checksums.txt" "$KEY_A/install.sh" "$@" \
		>"$T/out" 2>"$T/err" || rc=$?
}

# brew-formula.sh and its template are R4's and still read the pre-rename cf_ tarballs: until R4, this file
# renames the cflk_ fixture to them. Delete this function with R4 and go back to good_release.
legacy_release() { # KEYDIR
	good_release "$1"
	for f in "$T"/dist/cflk_*.tar.gz; do mv "$f" "$T/dist/cf_${f##*/cflk_}"; done
	sed 's/  cflk_/  cf_/' "$T/dist/checksums.txt" >"$T/dist/checksums.new"
	mv "$T/dist/checksums.new" "$T/dist/checksums.txt"
	sign_checksums "$1"
}

assert_no_stdout() { [ ! -s "$T/out" ] || fail "stdout is not empty: $(cat "$T/out")"; }

assert_brew_rejects() { # error-substring
	assert_rc 1
	assert_no_stdout
	grep -q -- "$1" "$T/err" || { sed 's/^/    | /' "$T/err"; fail "stderr lacks: $1"; }
}

TestScenario_FormulaDerivada_valid() {
	legacy_release "$KEY_A"
	brew_formula "$VERSION"
	assert_rc 0
	for target in darwin_arm64 darwin_amd64 linux_arm64 linux_amd64; do
		name="cf_${BARE_VERSION}_$target.tar.gz"
		grep -qF "url \"$BREW_BASE/$VERSION/$name\"" "$T/out" || fail "no url for $target"
		grep -qF "sha256 \"$(hash_of "$T/dist/$name")\"" "$T/out" || fail "no sha256 for $target"
	done
	grep -q '^class Codefabric < Formula' "$T/out" || fail "not the codefabric formula"
	grep -q 'bin.install "cf"' "$T/out" || fail "does not install cf"
	grep -q '@[A-Z_0-9]*@' "$T/out" && fail "unrendered placeholder"
	if command -v ruby >/dev/null 2>&1; then
		cp "$T/out" "$T/codefabric.rb"
		ruby -c "$T/codefabric.rb" >/dev/null || fail "ruby -c rejects the formula"
	else
		echo "SKIP: ruby not available"
	fi
}

TestScenario_FormulaDerivada_validWithoutExplicitVersion() {
	legacy_release "$KEY_A"
	brew_formula
	assert_rc 0
	grep -qF "$VERSION/cf_${BARE_VERSION}_linux_amd64.tar.gz" "$T/out" || fail "version not taken from the header"
}

TestScenario_FormulaDerivada_invalidSignature() {
	legacy_release "$KEY_A"
	sign_checksums "$KEY_B"
	brew_formula "$VERSION"
	assert_brew_rejects "signature does not verify"
}

TestScenario_FormulaDerivada_tamperedChecksums() {
	legacy_release "$KEY_A"
	printf '# tampered\n' >>"$T/dist/checksums.txt"
	brew_formula "$VERSION"
	assert_brew_rejects "signature does not verify"
}

TestScenario_FormulaDerivada_suffixedVersion() {
	legacy_release "$KEY_A"
	sed '1s/.*/version: v1.2.3-rc.1/' "$T/dist/checksums.txt" >"$T/new" && mv "$T/new" "$T/dist/checksums.txt"
	sign_checksums "$KEY_A"
	brew_formula
	assert_brew_rejects "not a plain vX.Y.Z"
}

TestScenario_FormulaDerivada_suffixedRequestedVersion() {
	legacy_release "$KEY_A"
	brew_formula v1.2.3-rc.1
	assert_brew_rejects "not a plain vX.Y.Z"
}

TestScenario_FormulaDerivada_missingPlatform() {
	legacy_release "$KEY_A"
	grep -v linux_arm64 "$T/dist/checksums.txt" >"$T/new" && mv "$T/new" "$T/dist/checksums.txt"
	sign_checksums "$KEY_A"
	brew_formula "$VERSION"
	assert_brew_rejects "cf_${BARE_VERSION}_linux_arm64.tar.gz is not listed exactly once"
}

TestScenario_FormulaDerivada_duplicatedPlatform() {
	legacy_release "$KEY_A"
	grep linux_amd64 "$T/dist/checksums.txt" >"$T/dup"
	cat "$T/dup" >>"$T/dist/checksums.txt"
	sign_checksums "$KEY_A"
	brew_formula "$VERSION"
	assert_brew_rejects "cf_${BARE_VERSION}_linux_amd64.tar.gz is not listed exactly once"
}

TestScenario_FormulaDerivada_invalidDigest() {
	legacy_release "$KEY_A"
	sed 's/^[0-9a-f]*  \(cf_.*darwin_arm64\)/zz  \1/' "$T/dist/checksums.txt" >"$T/new" && mv "$T/new" "$T/dist/checksums.txt"
	sign_checksums "$KEY_A"
	brew_formula "$VERSION"
	assert_brew_rejects "has no sha256 digest"
}

TestScenario_FormulaDerivada_headerVersionMismatch() {
	legacy_release "$KEY_A"
	brew_formula v1.2.4
	assert_brew_rejects "checksums are for $VERSION, not v1.2.4"
}
