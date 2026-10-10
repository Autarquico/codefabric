package domain_test

import (
	"testing"

	"github.com/Autarquico/codefabric/internal/domain"
)

func TestValidRepoURL_AcceptsTheAllowlist(t *testing.T) {
	for _, repo := range []string{
		"git@github.com:org/repo.git", "github.com:org/repo", "git@host:a@b.git", "git@[::1]:org/repo",
		"ssh://git@host:2222/srv/repo", "ssh://host/srv/repo", "https://github.com/org/repo.git",
		"http://h/r", "https://github.com:8443/org/repo", "https://[::1]:8443/r", "git://h/r", "/srv/repo",
		"/srv/a:b@host:repo", "file:///srv/repo", "git@build_box:org/repo",
	} {
		if !domain.ValidRepoURL(repo) {
			t.Errorf("%q refused", repo)
		}
	}
}

func TestValidRepoURL_RefusesEverythingElse(t *testing.T) {
	for _, repo := range []string{ //nolint:gosec // test data: fake secrets
		"user:SECRET@build_box:org/repo.git", "user:pw@host:path",
		"https://user@h/r", "https://u:p@h/r", "https://h/r?x", "https://h/r#x", "ssh://u:p@h/r",
		"https://h", "https://h/", "HTTPS://h/r", "ftp://h/r", "git://u@h/r", "ssh://h:port/r",
		"file://../repo", "file://repo", "file:///srv/r?x", "file:///srv/r#x", "../repo", "./repo", "repo", "",
		"C:/x", "C:\\x", "host:-oProxyCommand=x", "/srv/a b", "/srv/a\nb", "/srv/a\u00a0b",
		"git@host:org/a@b:c.git", // a ':' in the path is a supported form we do not keep
		"host:", "host://x", "git@host:/srv/r?",
	} {
		if domain.ValidRepoURL(repo) {
			t.Errorf("%q accepted", repo)
		}
	}
}
