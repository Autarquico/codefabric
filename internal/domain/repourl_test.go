package domain_test

import (
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/domain"
)

func TestRepoURLProblem(t *testing.T) {
	for _, ok := range []string{
		"git@github.com:org/repo.git", "ssh://git@github.com/org/repo.git", "https://github.com/org/repo.git",
		"file:///srv/repo", "/srv/repo", "host:repo?x", "ssh://host:2222/org/repo",
	} {
		if p := domain.RepoURLProblem(ok); p != "" {
			t.Errorf("%q refused: %s", ok, p)
		}
	}
	for repo, want := range map[string]string{ //nolint:gosec // test data: fake secrets
		"https://user:s3cret@h/r":         "a password",
		"https://:s3cret@h/r":             "a password",
		"ssh://git:s3cret@h/r":            "a password",
		"https://user:p@ss@h/r":           "a password",
		"https://s3cret@h/r":              "a user name or token",
		"HTTP://s3cret@h/r":               "a user name or token",
		"https://h/r?access_token=s3cret": "a query or fragment",
		"ssh://git@h/r#s3cret":            "a query or fragment",
		"file:///srv/r?s3cret":            "a query or fragment",
		"https://h?s3cret=1":              "a query or fragment",
	} {
		got := domain.RepoURLProblem(repo)
		if got != want {
			t.Errorf("%q: problem %q, want %q", repo, got, want)
		}
		if strings.Contains(got, "s3cret") {
			t.Errorf("the reason repeats the secret: %q", got)
		}
	}
}

func TestRepoIsRelative(t *testing.T) {
	for repo, want := range map[string]bool{
		"../repo": true, "repo": true, "./srv/repo": true,
		"/srv/repo": false, "git@host:repo": false, "host:repo": false, "https://h/r": false, "file:///srv/r": false,
	} {
		if got := domain.RepoIsRelative(repo); got != want {
			t.Errorf("RepoIsRelative(%q) = %v, want %v", repo, got, want)
		}
	}
}

func TestRepoURLProblem_ScpWithAPassword(t *testing.T) {
	for _, repo := range []string{"user:s3cret@host:org/repo.git", "u:s3cret@host:/srv/repo"} {
		if got := domain.RepoURLProblem(repo); got != "a password" {
			t.Errorf("%q: problem %q", repo, got)
		}
	}
	for _, repo := range []string{"git@github.com:org/repo.git", "host:org/repo", "git@host:org/a@b.git"} {
		if got := domain.RepoURLProblem(repo); got != "" {
			t.Errorf("%q refused: %s", repo, got)
		}
	}
}
