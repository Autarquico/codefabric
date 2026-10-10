package domain

import (
	"regexp"
	"strings"
)

// RepoFormHint says which repository forms are accepted. It never repeats a URL.
const RepoFormHint = "an https/ssh/git URL, a scp-like user@host:path or an absolute path, with no password, query or fragment"

// The accepted repository forms are a strict allowlist (spec B5): anything
// else is refused, so the Controller never has to recognise a credential. Each
// grammar is anchored. The path of a URL or a scp-like address has no ':', and
// an http(s) URL has no userinfo.
const (
	host  = `(?:[A-Za-z0-9._-]+|\[[0-9A-Fa-f:.]+\])`
	port  = `(?::[0-9]{1,5})?`
	user  = `(?:[A-Za-z0-9._-]+@)?`
	rpath = `[A-Za-z0-9._~/+@-]+`
)

var (
	httpForm  = regexp.MustCompile(`^https?://` + host + port + `/` + rpath + `$`)
	sshForm   = regexp.MustCompile(`^ssh://` + user + host + port + `/` + rpath + `$`)
	gitForm   = regexp.MustCompile(`^git://` + host + port + `/` + rpath + `$`)
	scpForm   = regexp.MustCompile(`^` + user + `(` + host + `):(` + rpath + `)$`)
	localForm = regexp.MustCompile(`^/[^\s\p{Cc}\p{Z}]*$`)
	fileForm  = regexp.MustCompile(`^file:///[^?#\s\p{Cc}\p{Z}]*$`)
)

// ValidRepoURL tells whether repo has one of the accepted forms. A scp-like
// path does not start with '-' (git would read an option) or '//' (that is a
// URL with a scheme the allowlist does not know), and its host has two
// characters at least, so that a drive letter such as C:/x is not a host.
func ValidRepoURL(repo string) bool {
	switch {
	case httpForm.MatchString(repo), sshForm.MatchString(repo), gitForm.MatchString(repo),
		localForm.MatchString(repo), fileForm.MatchString(repo):
		return true
	}
	m := scpForm.FindStringSubmatch(repo)
	return m != nil && len(m[1]) >= 2 && !strings.HasPrefix(m[2], "-") && !strings.HasPrefix(m[2], "//")
}
