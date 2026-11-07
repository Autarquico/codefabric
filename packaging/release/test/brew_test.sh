# shellcheck shell=sh
# shellcheck disable=SC2154,SC2034
# Scenario "Fórmula derivada": brew-formula.sh over a finalized dist, trust root = KEY_A's install.sh.

BREW_BASE=https://example.invalid/releases/download

# brew_formula [VERSION]: sets rc, $T/out (stdout) and $T/err (stderr).
brew_formula() {
	rc=0
	CF_RELEASE_BASE_URL=$BREW_BASE sh "$RELEASE_DIR/brew-formula.sh" "$T/dist/checksums.txt" "$KEY_A/install.sh" "$@" \
		>"$T/out" 2>"$T/err" || rc=$?
}

assert_no_stdout() { [ ! -s "$T/out" ] || fail "stdout is not empty: $(cat "$T/out")"; }

assert_brew_rejects() { # error-substring
	assert_rc 1
	assert_no_stdout
	grep -q -- "$1" "$T/err" || { sed 's/^/    | /' "$T/err"; fail "stderr lacks: $1"; }
}

# resign_with_checksums: $T/dist/checksums.txt is rewritten by the caller; sign it again with KEY_A.
resign_with_checksums() { sign_checksums "$KEY_A"; }

TestScenario_FormulaDerivada_valid() {
	good_release "$KEY_A"
	brew_formula "$VERSION"
	assert_rc 0
	for target in darwin_arm64 linux_arm64 linux_amd64; do
		name="cf_${BARE_VERSION}_$target.tar.gz"
		grep -qF "url \"$BREW_BASE/$VERSION/$name\"" "$T/out" || fail "no url for $target"
		grep -qF "sha256 \"$(hash_of "$T/dist/$name")\"" "$T/out" || fail "no sha256 for $target"
	done
	grep -q '^class Codefabric < Formula' "$T/out" || fail "not the codefabric formula"
	grep -q 'bin.install "cf"' "$T/out" || fail "does not install cf"
	grep -q '@[A-Z_]*@' "$T/out" && fail "unrendered placeholder"
	if command -v ruby >/dev/null 2>&1; then
		cp "$T/out" "$T/codefabric.rb"
		ruby -c "$T/codefabric.rb" >/dev/null || fail "ruby -c rejects the formula"
	else
		echo "SKIP: ruby not available"
	fi
}

TestScenario_FormulaDerivada_validWithoutExplicitVersion() {
	good_release "$KEY_A"
	brew_formula
	assert_rc 0
	grep -qF "$VERSION/cf_${BARE_VERSION}_linux_amd64.tar.gz" "$T/out" || fail "version not taken from the header"
}

TestScenario_FormulaDerivada_invalidSignature() {
	good_release "$KEY_A"
	sign_checksums "$KEY_B"
	brew_formula "$VERSION"
	assert_brew_rejects "signature does not verify"
}

TestScenario_FormulaDerivada_tamperedChecksums() {
	good_release "$KEY_A"
	printf '# tampered\n' >>"$T/dist/checksums.txt"
	brew_formula "$VERSION"
	assert_brew_rejects "signature does not verify"
}

TestScenario_FormulaDerivada_suffixedVersion() {
	good_release "$KEY_A"
	sed '1s/.*/version: v1.2.3-rc.1/' "$T/dist/checksums.txt" >"$T/new" && mv "$T/new" "$T/dist/checksums.txt"
	resign_with_checksums
	brew_formula
	assert_brew_rejects "not a plain vX.Y.Z"
}

TestScenario_FormulaDerivada_suffixedRequestedVersion() {
	good_release "$KEY_A"
	brew_formula v1.2.3-rc.1
	assert_brew_rejects "not a plain vX.Y.Z"
}

TestScenario_FormulaDerivada_missingPlatform() {
	good_release "$KEY_A"
	grep -v linux_arm64 "$T/dist/checksums.txt" >"$T/new" && mv "$T/new" "$T/dist/checksums.txt"
	resign_with_checksums
	brew_formula "$VERSION"
	assert_brew_rejects "cf_${BARE_VERSION}_linux_arm64.tar.gz is not listed exactly once"
}

TestScenario_FormulaDerivada_duplicatedPlatform() {
	good_release "$KEY_A"
	grep linux_amd64 "$T/dist/checksums.txt" >"$T/dup"
	cat "$T/dup" >>"$T/dist/checksums.txt"
	resign_with_checksums
	brew_formula "$VERSION"
	assert_brew_rejects "cf_${BARE_VERSION}_linux_amd64.tar.gz is not listed exactly once"
}

TestScenario_FormulaDerivada_invalidDigest() {
	good_release "$KEY_A"
	sed 's/^[0-9a-f]*  \(cf_.*darwin_arm64\)/zz  \1/' "$T/dist/checksums.txt" >"$T/new" && mv "$T/new" "$T/dist/checksums.txt"
	resign_with_checksums
	brew_formula "$VERSION"
	assert_brew_rejects "has no sha256 digest"
}

TestScenario_FormulaDerivada_headerVersionMismatch() {
	good_release "$KEY_A"
	brew_formula v1.2.4
	assert_brew_rejects "checksums are for $VERSION, not v1.2.4"
}
