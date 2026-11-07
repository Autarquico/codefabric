# shellcheck shell=sh
# shellcheck disable=SC2154
# Release gate scenarios: require-checks.sh over check-runs JSON fixtures, tag-in-main.sh over a temp repo.

# A check-run as the GitHub API reports it. cr NAME STATUS CONCLUSION-JSON [APP]
cr() { printf '{"name":"%s","status":"%s","conclusion":%s,"app":{"slug":"%s"}}' "$1" "$2" "$3" "${4:-github-actions}"; }
passed() { cr "$1" completed '"success"'; }
# A page of the check-runs API: page RUN [RUN...]
page() {
	body=$1
	shift
	for run in "$@"; do body="$body,$run"; done
	printf '{"total_count":2,"check_runs":[%s]}\n' "$body"
}

require_fixture() {
	printf '# required for release\nlint\n\nunit (ubuntu-latest)\n' >"$T/required.txt"
}

gate() { # checks-file
	run_cmd sh "$RELEASE_DIR/require-checks.sh" "$T/required.txt" "$1"
}

TestScenario_ReleaseGates_allSuccessPasses() {
	require_fixture
	page "$(passed lint)" "$(passed 'unit (ubuntu-latest)')" "$(passed unrelated)" >"$T/checks.json"
	gate "$T/checks.json"
	assert_rc 0
}

TestScenario_ReleaseGates_realRequiredListParsesAndPasses() {
	runs=""
	while IFS= read -r name; do
		case $name in '' | '#'*) continue ;; esac
		runs="$runs,$(passed "$name")"
	done <"$RELEASE_DIR/required-checks.txt"
	printf '{"check_runs":[%s]}\n' "${runs#,}" >"$T/checks.json"
	run_cmd sh "$RELEASE_DIR/require-checks.sh" "$RELEASE_DIR/required-checks.txt" "$T/checks.json"
	assert_rc 0
}

TestScenario_ReleaseGates_missingCheckFails() {
	require_fixture
	page "$(passed lint)" >"$T/checks.json"
	gate "$T/checks.json"
	assert_rc 1
	assert_out_contains "unit (ubuntu-latest): missing"
}

TestScenario_ReleaseGates_pendingCheckFails() {
	require_fixture
	page "$(passed lint)" "$(cr 'unit (ubuntu-latest)' in_progress null)" >"$T/checks.json"
	gate "$T/checks.json"
	assert_rc 1
	assert_out_contains "not finished"
}

TestScenario_ReleaseGates_failedCheckFails() {
	require_fixture
	page "$(passed lint)" "$(cr 'unit (ubuntu-latest)' completed '"failure"')" >"$T/checks.json"
	gate "$T/checks.json"
	assert_rc 1
	assert_out_contains "failure"
}

TestScenario_ReleaseGates_skippedCheckFails() {
	require_fixture
	page "$(passed lint)" "$(cr 'unit (ubuntu-latest)' completed '"skipped"')" >"$T/checks.json"
	gate "$T/checks.json"
	assert_rc 1
}

TestScenario_ReleaseGates_failedCheckOnSecondPageFails() {
	require_fixture
	page "$(passed lint)" "$(passed 'unit (ubuntu-latest)')" >"$T/checks.json"
	page "$(cr lint completed '"failure"')" >>"$T/checks.json"
	gate "$T/checks.json"
	assert_rc 1
}

TestScenario_ReleaseGates_checkOnlyOnSecondPagePasses() {
	require_fixture
	page "$(passed lint)" >"$T/checks.json"
	page "$(passed 'unit (ubuntu-latest)')" >>"$T/checks.json"
	gate "$T/checks.json"
	assert_rc 0
}

TestScenario_ReleaseGates_checkFromOtherAppFails() {
	require_fixture
	page "$(passed lint)" "$(cr 'unit (ubuntu-latest)' completed '"success"' some-third-party)" >"$T/checks.json"
	gate "$T/checks.json"
	assert_rc 1
}

TestScenario_ReleaseGates_malformedJsonFails() {
	require_fixture
	printf '{"check_runs": [ {"name": "lint"' >"$T/checks.json"
	gate "$T/checks.json"
	assert_rc_nonzero
}

TestScenario_ReleaseGates_emptyOutputFails() {
	require_fixture
	: >"$T/checks.json"
	gate "$T/checks.json"
	assert_rc_nonzero
}

TestScenario_ReleaseGates_apiErrorBodyFails() {
	require_fixture
	printf '{"message":"Bad credentials","documentation_url":"https://docs.github.com"}\n' >"$T/checks.json"
	gate "$T/checks.json"
	assert_rc_nonzero
}

TestScenario_ReleaseGates_emptyRequiredListFails() {
	printf '# nothing\n\n' >"$T/required.txt"
	page "$(passed lint)" >"$T/checks.json"
	gate "$T/checks.json"
	assert_rc_nonzero
}

TestScenario_ReleaseGates_missingInputFails() {
	require_fixture
	gate "$T/does-not-exist.json"
	assert_rc_nonzero
}

# --- tag-in-main.sh --------------------------------------------------------------

git_in() { git -C "$T/repo" -c user.name=test -c user.email=test@example.com -c commit.gpgsign=false "$@"; }

make_repo() {
	git init -q "$T/repo"
	git_in checkout -q -b main
	git_in commit -q --allow-empty -m first
	first=$(git_in rev-parse HEAD)
	git_in checkout -q -b feature
	git_in commit -q --allow-empty -m on-feature-only
	feature=$(git_in rev-parse HEAD)
	git_in checkout -q main
}

in_repo() { # command...
	( cd "$T/repo" && "$@" )
}

TestScenario_ReleaseGates_tagInMainPasses() {
	make_repo
	run_cmd in_repo sh "$RELEASE_DIR/tag-in-main.sh" "$first" main
	assert_rc 0
}

TestScenario_ReleaseGates_tagAtMainTipPasses() {
	make_repo
	run_cmd in_repo sh "$RELEASE_DIR/tag-in-main.sh" main main
	assert_rc 0
}

TestScenario_ReleaseGates_commitNotInMainFails() {
	make_repo
	run_cmd in_repo sh "$RELEASE_DIR/tag-in-main.sh" "$feature" main
	assert_rc 1
	assert_out_contains "is not in main"
}

TestScenario_ReleaseGates_unresolvableRefFails() {
	make_repo
	run_cmd in_repo sh "$RELEASE_DIR/tag-in-main.sh" "$first" origin/main
	assert_rc_nonzero
}

TestScenario_ReleaseGates_unknownCommitFails() {
	make_repo
	run_cmd in_repo sh "$RELEASE_DIR/tag-in-main.sh" 0123456789abcdef0123456789abcdef01234567 main
	assert_rc_nonzero
}
