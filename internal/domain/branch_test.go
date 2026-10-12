package domain

import (
	"strings"
	"testing"
)

func TestBranchOf(t *testing.T) {
	task, run := ShortID{Kind: KindTask, N: 7}, ShortID{Kind: KindRun, N: 42}
	for name, want := range map[string]string{
		"auth-race":  "cf/auth-race/run-42",
		"a.b_c":      "cf/a.b_c/run-42",
		"foo.lock":   "cf/task-7/run-42",
		"bar.lock.x": "cf/bar.lock.x/run-42",
	} {
		got := BranchOf(name, task, run)
		if got != want {
			t.Errorf("BranchOf(%q) = %q, want %q", name, got, want)
		}
		if !ValidBranch(got) {
			t.Errorf("%q is not a valid branch", got)
		}
	}
}

func TestValidBranchRefusesWhatGitRefuses(t *testing.T) {
	for _, bad := range []string{
		"", "main", "cf/x", "cf/x/y/z", "cf/x.lock/run-1", "cf/../run-1", "cf/a b/run-1", "cf/a~b/run-1",
		"cf/.a/run-1", "cf/a./run-1", "cf/a@{b/run-1", "cf//run-1", "refs/heads/cf/a/run-1", "cf/a:b/run-1",
	} {
		if ValidBranch(bad) {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestRunBranchPrefersTheStoredOne(t *testing.T) {
	task, run := ShortID{Kind: KindTask, N: 7}, ShortID{Kind: KindRun, N: 42}
	if got := RunBranch("cf/x/run-1", "auth", task, run); got != "cf/x/run-1" {
		t.Errorf("got %q", got)
	}
	if got := RunBranch("", "auth", task, run); got != "cf/auth/run-42" {
		t.Errorf("got %q", got)
	}
}

func TestValidSourceRefIsARefNotARefspec(t *testing.T) {
	for _, ok := range []string{"main", "feature/x", "v1.0.0", "release-2026_10", "refs/heads/main"} {
		if !ValidSourceRef(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "-x", "+main", "a:b", "main:other", "a*", "a^", "a~1", "a?", "a[b", `a\b`, "a@{1}", "a..b", "a b", "a\nb", "a\u200bb", strings.Repeat("a", 256)} {
		if ValidSourceRef(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidCommitIDAndPrintable(t *testing.T) {
	if !ValidCommitID(strings.Repeat("a", 40)) || !ValidCommitID(strings.Repeat("0", 64)) || ValidCommitID(strings.Repeat("A", 40)) || ValidCommitID("abc") {
		t.Error("ValidCommitID")
	}
	if !Printable("ñ ok") || Printable("a\nb") || Printable("a\u200bb") || Printable("\xff") {
		t.Error("Printable")
	}
}
