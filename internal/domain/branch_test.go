package domain

import "testing"

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
