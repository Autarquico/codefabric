package input

import (
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

func TestRepo_RefusesPasswordsAndTokensInTheURL(t *testing.T) {
	for _, repo := range []string{
		"git@github.com:org/repo.git", "ssh://git@github.com/org/repo.git", "https://github.com/org/repo.git",
		"file:///srv/repo", "/srv/repo", "git@host:repo?x",
	} {
		if err := Repo(repo); err != nil {
			t.Errorf("%q: %v", repo, err)
		}
	}
	for _, repo := range []string{
		"https://user:s3cret@github.com/org/repo.git", "https://:ghp_token@github.com/org/repo.git",
		"ssh://git:s3cret@github.com/org/repo.git", "https://user:p@ss@github.com/org/repo.git",
		"https://ghp_token@github.com/org/repo.git", "HTTP://ghp_token@github.com/org/repo.git",
		"https://github.com/org/repo.git?access_token=ghp_token", "ssh://git@github.com/org/repo.git#ghp_token",
		"file:///srv/repo?ghp_token", "https://github.com?ghp_token",
	} {
		err := Repo(repo)
		if err == nil {
			t.Errorf("%q was accepted", repo)
		} else if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "ghp_token") {
			t.Errorf("the error repeats the secret: %v", err)
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
