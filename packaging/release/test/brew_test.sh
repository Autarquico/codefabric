# shellcheck shell=sh
# shellcheck disable=SC2154,SC2034
# Scenario "Fórmula derivada": brew-formula.sh over a finalized dist, trust root = KEY_A's install.sh.

BREW_BASE=https://example.invalid/releases/download

# brew_formula [VERSION]: sets rc, $T/out (stdout) and $T/err (stderr).
brew_formula() {
	rc=0
	CFLK_RELEASE_BASE_URL=$BREW_BASE "$RELEASE_DIR/brew-formula.sh" "$T/dist/checksums.txt" "$KEY_A/install.sh" "$@" \
		>"$T/out" 2>"$T/err" || rc=$?
}

assert_no_stdout() { [ ! -s "$T/out" ] || fail "stdout is not empty: $(cat "$T/out")"; }

assert_brew_rejects() { # error-substring
	assert_rc 1
	assert_no_stdout
	grep -q -- "$1" "$T/err" || { sed 's/^/    | /' "$T/err"; fail "stderr lacks: $1"; }
}

TestScenario_FormulaDerivada_valid() {
	good_release "$KEY_A"
	brew_formula "$VERSION"
	assert_rc 0
	for target in darwin_arm64 darwin_amd64 linux_arm64 linux_amd64; do
		name="cflk_${BARE_VERSION}_$target.tar.gz"
		grep -qF "url \"$BREW_BASE/$VERSION/$name\"" "$T/out" || fail "no url for $target"
		grep -qF "sha256 \"$(hash_of "$T/dist/$name")\"" "$T/out" || fail "no sha256 for $target"
	done
	grep -q '^class Codeflock < Formula' "$T/out" || fail "not the codeflock formula"
	grep -q '^    bin.install "cflk"$' "$T/out" || fail "does not install cflk"
	[ "$(grep -c 'bin.install' "$T/out")" = 1 ] || fail "installs more than cflk"
	grep -q 'bin.install "cf"$' "$T/out" && fail "installs the transition cf, which clashes with cf-cli"
	grep -qF 'homepage "https://github.com/Autarquico/codeflock"' "$T/out" || fail "wrong homepage"
	grep -q '@[A-Z_0-9]*@' "$T/out" && fail "unrendered placeholder"
	if command -v ruby >/dev/null 2>&1; then
		cp "$T/out" "$T/codeflock.rb"
		ruby -c "$T/codeflock.rb" >/dev/null || fail "ruby -c rejects the formula"
	else
		echo "SKIP: ruby not available"
	fi
}

TestScenario_FormulaDerivada_validWithoutExplicitVersion() {
	good_release "$KEY_A"
	brew_formula
	assert_rc 0
	grep -qF "$VERSION/cflk_${BARE_VERSION}_linux_amd64.tar.gz" "$T/out" || fail "version not taken from the header"
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
	sign_checksums "$KEY_A"
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
	sign_checksums "$KEY_A"
	brew_formula "$VERSION"
	assert_brew_rejects "cflk_${BARE_VERSION}_linux_arm64.tar.gz is not listed exactly once"
}

TestScenario_FormulaDerivada_duplicatedPlatform() {
	good_release "$KEY_A"
	grep linux_amd64 "$T/dist/checksums.txt" >"$T/dup"
	cat "$T/dup" >>"$T/dist/checksums.txt"
	sign_checksums "$KEY_A"
	brew_formula "$VERSION"
	assert_brew_rejects "cflk_${BARE_VERSION}_linux_amd64.tar.gz is not listed exactly once"
}

TestScenario_FormulaDerivada_invalidDigest() {
	good_release "$KEY_A"
	sed 's/^[0-9a-f]*  \(cflk_.*darwin_arm64\)/zz  \1/' "$T/dist/checksums.txt" >"$T/new" && mv "$T/new" "$T/dist/checksums.txt"
	sign_checksums "$KEY_A"
	brew_formula "$VERSION"
	assert_brew_rejects "has no sha256 digest"
}

TestScenario_FormulaDerivada_headerVersionMismatch() {
	good_release "$KEY_A"
	brew_formula v1.2.4
	assert_brew_rejects "checksums are for $VERSION, not v1.2.4"
}

TestScenario_FormulaDerivada_legacyTarballsRefused() {
	good_release "$KEY_A"
	for f in "$T"/dist/cflk_*.tar.gz; do mv "$f" "$T/dist/cf_${f##*/cflk_}"; done
	sed 's/  cflk_/  cf_/' "$T/dist/checksums.txt" >"$T/dist/checksums.new"
	mv "$T/dist/checksums.new" "$T/dist/checksums.txt"
	sign_checksums "$KEY_A"
	brew_formula "$VERSION"
	assert_brew_rejects "cflk_${BARE_VERSION}_darwin_arm64.tar.gz is not listed exactly once"
}

# Scenario "Homebrew": brew upgrade of the installed codefabric lands on codeflock via the tap's rename map.
TestScenario_FormulaDerivada_codefabricRenamedToCodeflock() {
	map="$RELEASE_DIR/../homebrew/formula_renames.json"
	[ "$(tr -d ' \n' <"$map")" = '{"codefabric":"codeflock"}' ] || fail "formula_renames.json is not codefabric -> codeflock"
	[ ! -e "$RELEASE_DIR/../homebrew/codefabric.rb.tmpl" ] || fail "the old template is still there"
}
