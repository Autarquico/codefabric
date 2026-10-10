package input

import (
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

func TestRepo_AcceptsOnlyTheAllowlistAndNeverRepeatsTheURL(t *testing.T) {
	for _, repo := range []string{
		"git@github.com:org/repo.git", "ssh://git@github.com/org/repo.git", "https://github.com/org/repo.git",
		"file:///srv/repo", "/srv/repo",
	} {
		if err := Repo(repo); err != nil {
			t.Errorf("%q: %v", repo, err)
		}
	}
	for _, repo := range []string{ //nolint:gosec // test data: fake secrets
		"https://user:s3cret@github.com/org/repo.git", "user:s3cret@build_box:org/repo.git", "https://s3cret@github.com/org/repo.git",
		"https://github.com/org/repo.git?access_token=s3cret", "ssh://git@github.com/org/repo.git#s3cret",
		"git@host:repo?s3cret", "../s3cret", "s3cret", "-s3cret", "C:/s3cret", "host:-s3cret",
		"git@host:org/a@s3cret:c.git", strings.Repeat("a", maxURLLen+1),
	} {
		err := Repo(repo)
		if err == nil {
			t.Errorf("%q was accepted", repo)
		} else if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("the error repeats the URL: %v", err)
		}
	}
}

func TestRequirements_ReportsTheFirstInvalidListDeterministically(t *testing.T) {
	r := v1.Requirements{Capabilities: []string{"a b"}, Workers: v1.WorkerPreferences{Prefer: []string{""}, Avoid: []string{""}, Require: []string{""}}}
	for range 50 {
		err := Requirements(r)
		if err == nil || !strings.Contains(err.Error(), "capabilities entries") {
			t.Fatalf("err = %v, want the capabilities list first", err)
		}
	}
}
