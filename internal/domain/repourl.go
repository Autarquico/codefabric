package domain

import (
	"regexp"
	"strings"
)

var (
	schemeURL = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*)://`)
	scpURL    = regexp.MustCompile(`^(?:[^@/:]+@)?[^/:]+:`)
)

// RepoURLProblem says why a repository URL must not be stored, or "" if it may.
// The Controller never keeps a secret, and git finds credentials in the user's
// own configuration, so a URL with a scheme must carry no password (in any
// scheme), no user at all over http(s), where the name is often a token, and no
// query or fragment, where tokens also travel. A user without password over ssh,
// like git@, is no secret. The reason never repeats the URL.
func RepoURLProblem(repo string) string {
	m := schemeURL.FindStringSubmatch(repo)
	if m == nil {
		// scp-like [userinfo@]host:path: a ':' in the userinfo is a password
		// (git@host:path has none; an '@' in the path is no userinfo).
		if user, _, _, ok := SplitScp(repo); ok && strings.Contains(user, ":") {
			return "a password"
		}
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

// RepoIsRelative tells a relative local path: it names a different repository
// on every machine, and an origin like it never infers a Project.
func RepoIsRelative(repo string) bool {
	return !schemeURL.MatchString(repo) && !scpURL.MatchString(repo) && !strings.HasPrefix(repo, "/")
}

// scpForm is [userinfo@]host:path. The userinfo is greedy, so it ends at the
// last '@' that is followed by a host and a ':'; an '@' in the path is not one.
var scpForm = regexp.MustCompile(`^(?:(.*)@)?([A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\]):(.*)$`)

// SplitScp splits a scp-like remote into its userinfo, host and path.
func SplitScp(s string) (userinfo, host, path string, ok bool) {
	if schemeURL.MatchString(s) {
		return "", "", "", false
	}
	m := scpForm.FindStringSubmatch(s)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}
