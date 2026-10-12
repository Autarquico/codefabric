package gitops

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Autarquico/codefabric/internal/domain"
)

// PushClass says what a failed push means for the attempts allowed (Appendix B5).
type PushClass int

// Classes of a failed push.
const (
	// PushUnreachable: no connection, DNS, timeout, a server error or a busy
	// relay. It says nothing about the result and does not count.
	PushUnreachable PushClass = iota
	// PushForbidden: the relay answered 403. The token is asked for again before
	// every attempt, so what the relay now forbids is the push itself.
	PushForbidden
	// PushRefused: the relay (its hook or git) refused the update of the ref.
	PushRefused
	// PushPermanent: a certificate that does not verify, or 401.
	PushPermanent
	// PushLocal: git failed on this machine.
	PushLocal
)

// PushError is a push that did not happen. Its message has been redacted and
// bounded; the token is never in it.
type PushError struct {
	Class   PushClass
	Message string
}

func (e *PushError) Error() string { return "gitops: push failed: " + e.Message }

// Counts says whether the failure counts as an attempt.
func (e *PushError) Counts() bool { return e.Class != PushUnreachable }

// PushAuth is how a push presents itself to the relay: the URL of the Project's
// repository, the PEM files of the Worker's certificate and the CA it pins, and
// the token of the lease.
type PushAuth struct {
	URL   string
	Token string
	Cert  string
	Key   string
	CA    string
}

var (
	unreachable = regexp.MustCompile(`(?i)could not resolve host|failed to connect|connection (refused|reset|timed out)|timed out|operation too slow|ssl_error_syscall|empty reply|unexpected disconnect|http (status )?code|returned error: (5\d\d|429)|rpc failed|early eof|couldn't connect|network is unreachable|no route to host|send failure|recv failure`)
	badCert     = regexp.MustCompile(`(?i)ssl certificate problem|certificate verify failed|unknown ca|bad certificate|certificate (has expired|required)|tlsv1 alert`)
)

// Push sends the commit to refs/heads/<branch> of the relay, without force and
// without any hook, from the bare repository of the Project. The environment is
// built from nothing and the configuration travels in it, so that the token is
// in neither argv nor any file; redirects are not followed.
func (g *Git) Push(ctx context.Context, project, commit, branch string, a PushAuth) error {
	switch {
	case !projectID.MatchString(project) || !fullCommit.MatchString(commit) || !domain.ValidBranch(branch):
		return &PushError{Class: PushLocal, Message: "the project, commit or branch is not valid"}
	case a.URL == "" || a.Token == "" || a.Cert == "" || a.Key == "" || a.CA == "":
		return &PushError{Class: PushLocal, Message: "the credentials for the relay are incomplete"}
	}
	bare := g.o.Paths.WorkerRepoDir(project)
	env := append(isolatedEnv(bare),
		"GIT_CONFIG_COUNT=8",
		"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null",
		"GIT_CONFIG_KEY_1=http.sslCert", "GIT_CONFIG_VALUE_1="+a.Cert,
		"GIT_CONFIG_KEY_2=http.sslKey", "GIT_CONFIG_VALUE_2="+a.Key,
		"GIT_CONFIG_KEY_3=http.sslCAInfo", "GIT_CONFIG_VALUE_3="+a.CA,
		"GIT_CONFIG_KEY_4=http."+a.URL+".extraHeader", "GIT_CONFIG_VALUE_4=Authorization: Bearer "+a.Token,
		"GIT_CONFIG_KEY_5=http.followRedirects", "GIT_CONFIG_VALUE_5=false",
		"GIT_CONFIG_KEY_6=http.lowSpeedLimit", "GIT_CONFIG_VALUE_6=1000",
		"GIT_CONFIG_KEY_7=http.lowSpeedTime", "GIT_CONFIG_VALUE_7=120",
		"GIT_CONFIG_PARAMETERS=", // nothing inherited reaches it
	)
	res, err := g.run(ctx, cmd{
		dir: bare, env: env, timeout: pushTimeout, secrets: []string{a.Token},
		args: []string{"push", "--porcelain", "--no-verify", "--no-progress", "--end-of-options", a.URL, commit + ":refs/heads/" + branch},
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return &PushError{Class: PushUnreachable, Message: "the push took too long"}
		}
		return &PushError{Class: PushLocal, Message: clean(err.Error(), []string{a.Token})}
	}
	if res.exit == 0 {
		return nil
	}
	return classify(res, a.Token)
}

func classify(res result, token string) *PushError {
	text := clean(res.stdout+"\n"+res.stderr, []string{token})
	e := &PushError{Message: text}
	switch {
	case strings.Contains(text, "[remote rejected]") || strings.Contains(text, "[rejected]") || strings.Contains(text, "hook declined"):
		e.Class = PushRefused
	case strings.Contains(text, "error: 403") || strings.Contains(text, "returned error: 403"):
		e.Class = PushForbidden
	case strings.Contains(text, "returned error: 401") || strings.Contains(text, "could not read Username") || strings.Contains(text, "terminal prompts disabled"):
		e.Class = PushPermanent
	case unreachable.MatchString(text):
		e.Class = PushUnreachable
	case badCert.MatchString(text):
		e.Class = PushPermanent
	default:
		e.Class = PushLocal
	}
	if e.Message == "" {
		e.Message = fmt.Sprintf("git push exited %d", res.exit)
	}
	return e
}
