package domain

import (
	"regexp"
	"strings"
)

var schemeURL = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*)://`)

// RepoURLProblem says why a repository URL must not be stored, or "" if it may.
// The Controller never keeps a secret, and git finds credentials in the user's
// own configuration, so a URL with a scheme must carry no password (in any
// scheme), no user at all over http(s), where the name is often a token, and no
// query or fragment, where tokens also travel. A user without password over ssh,
// like git@, is no secret. The reason never repeats the URL.
func RepoURLProblem(repo string) string {
	m := schemeURL.FindStringSubmatch(repo)
	if m == nil {
		return ""
	}
	if strings.ContainsAny(repo, "?#") {
		return "a query or fragment"
	}
	authority, _, _ := strings.Cut(repo[len(m[0]):], "/")
	i := strings.LastIndex(authority, "@")
	if i < 0 {
		return ""
	}
	switch {
	case strings.Contains(authority[:i], ":"):
		return "a password"
	case strings.EqualFold(m[1], "http") || strings.EqualFold(m[1], "https"):
		return "a user name or token"
	}
	return ""
}
