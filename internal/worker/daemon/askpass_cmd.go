package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
)

// askpassRunDirEnv names the run directory whose origin delivery the askpass
// helper answers from. The script of the Worker does not carry it: git passes
// the environment of the fetch down to its helper.
const askpassRunDirEnv = "CF_ASKPASS_RUN_DIR"

// errNoAnswer is all the helper ever says: why it did not answer is not
// something git's output, which a Run may print, should explain.
var errNoAnswer = errors.New("git-askpass: no answer")

// newGitAskpassCmd is GIT_ASKPASS for the authenticated fetch of an origin:
// git runs it with its prompt as the only argument and reads the answer from
// stdout. It answers only the destination the delivery is authorized for.
func newGitAskpassCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "git-askpass <prompt>",
		Short:         "Answer the credential prompt of git for the origin of a Run (started by git, not by hand)",
		Hidden:        true,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return answerAskpass(cmd.OutOrStdout(), os.Getenv(askpassRunDirEnv), args[0])
		},
	}
}

// answerAskpass writes the answer to prompt to out, newline included, or
// nothing and errNoAnswer.
func answerAskpass(out io.Writer, runDir, prompt string) error {
	if runDir == "" || !filepath.IsAbs(runDir) {
		return errNoAnswer
	}
	grant, err := secretjit.AskpassGrant(runDir)
	if err != nil {
		return errNoAnswer
	}
	answer, ok := askpassAnswer(prompt, grant)
	if !ok {
		return errNoAnswer
	}
	if _, err := io.WriteString(out, answer+"\n"); err != nil {
		return errNoAnswer
	}
	return nil
}

// askpassAnswer is what git's prompt is answered with, if the grant covers
// it. The prompts are those of git with credential.useHttpPath=true:
//
//	Username for 'https://host[:port]/path':
//	Password for 'https://user@host[:port]/path':
//
// Only https, only the host:port and path of the grant (canonicalised: host
// in lower case, port 443 implicit, a trailing ".git" or "/" of the path not
// significant), and, for the password, only the user of the grant.
func askpassAnswer(prompt string, g secretjit.Grant) (string, bool) {
	p, ok := parsePrompt(prompt)
	if !ok {
		return "", false
	}
	want, ok := authorizedDestination(g.Origin)
	if !ok || p.destination != want {
		return "", false
	}
	var answer string
	switch p.kind {
	case askUsername:
		if p.user != "" {
			return "", false
		}
		answer = g.User
	case askPassword:
		if p.user != g.User {
			return "", false
		}
		answer = g.Value
	}
	if answer == "" || strings.ContainsAny(answer, "\r\n\x00") {
		return "", false
	}
	return answer, true
}

type askKind int

const (
	askUsername askKind = iota + 1
	askPassword
)

type parsedPrompt struct {
	kind        askKind
	user        string // the userinfo the prompt names, if any
	destination string // canonical host[:port]/path
}

// parsePrompt reads a prompt of git strictly: anything else is not one.
func parsePrompt(prompt string) (parsedPrompt, bool) {
	var p parsedPrompt
	var rest string
	switch {
	case strings.HasPrefix(prompt, "Username for '"):
		p.kind, rest = askUsername, strings.TrimPrefix(prompt, "Username for '")
	case strings.HasPrefix(prompt, "Password for '"):
		p.kind, rest = askPassword, strings.TrimPrefix(prompt, "Password for '")
	default:
		return p, false
	}
	target, ok := strings.CutSuffix(rest, "': ")
	if !ok || strings.ContainsFunc(target, unicode.IsControl) {
		return p, false
	}
	target, ok = strings.CutPrefix(target, "https://")
	if !ok {
		return p, false
	}
	authority, path, _ := strings.Cut(target, "/")
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		p.user, authority = authority[:i], authority[i+1:]
		if p.user == "" {
			return p, false
		}
	}
	if p.kind == askPassword && p.user == "" {
		return p, false
	}
	p.destination, ok = canonicalDestination(authority, path)
	return p, ok
}

// authorizedDestination is the canonical destination of the https URL of a
// grant: no userinfo, query or fragment.
func authorizedDestination(origin string) (string, bool) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.ForceQuery {
		return "", false
	}
	return canonicalDestination(u.Host, u.Path)
}

// canonicalDestination is "host[:port]/path": the host in lower case, the
// default port dropped, the path without the slashes around it or a ".git" at
// its end. A destination without a host or a path is none.
func canonicalDestination(authority, path string) (string, bool) {
	u, err := url.Parse("https://" + authority)
	if err != nil || u.Host != authority || u.Hostname() == "" || u.User != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && port != "443" {
		host += ":" + port
	}
	path = strings.Trim(path, "/")
	path = strings.Trim(strings.TrimSuffix(path, ".git"), "/")
	if path == "" {
		return "", false
	}
	return host + "/" + path, true
}

// AskpassScript is the content of the script git runs as GIT_ASKPASS: it
// execs argv, the helper, with the arguments of git. Each word is in single
// quotes, so a path with spaces or quotes works; there is no credential in it.
func AskpassScript(argv []string) (string, error) {
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		return "", errors.New("daemon: the askpass helper must be an absolute path")
	}
	words := make([]string, len(argv))
	for i, a := range argv {
		if strings.ContainsRune(a, 0) {
			return "", errors.New("daemon: the askpass helper has a NUL in its command")
		}
		words[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return "#!/bin/sh\nexec " + strings.Join(words, " ") + " \"$@\"\n", nil
}

// AskpassScriptPath is where the Worker keeps its GIT_ASKPASS script.
func AskpassScriptPath(paths config.Paths) string {
	return filepath.Join(paths.WorkerDataDir(), "bin", "git-askpass")
}

// EnsureAskpassScript writes the script for argv at AskpassScriptPath (mode
// 0700, absolute) and returns its path. A script that is already there with
// the same content and mode is left alone; otherwise it is replaced atomically,
// so a git that is running it never finds it half written.
func EnsureAskpassScript(paths config.Paths, argv []string) (string, error) {
	content, err := AskpassScript(argv)
	if err != nil {
		return "", err
	}
	path := AskpassScriptPath(paths)
	if !filepath.IsAbs(path) {
		return "", errors.New("daemon: the data directory of the Worker must be an absolute path")
	}
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, []byte(content)) { //nolint:gosec // the script of this Worker
		if info, err := os.Stat(path); err == nil && info.Mode().Perm() == 0o700 {
			return path, nil
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("daemon: making %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".git-askpass-*")
	if err != nil {
		return "", fmt.Errorf("daemon: writing the askpass script: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.WriteString(content)
	if err == nil {
		err = tmp.Chmod(0o700)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return "", fmt.Errorf("daemon: writing the askpass script: %w", err)
	}
	return path, nil
}
