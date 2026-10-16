package gitops

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

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
	// status reads the HTTP status git reports: "returned error: 403", "HTTP 500".
	status      = regexp.MustCompile(`(?i)(?:returned error:|\bHTTP(?:/\d(?:\.\d)?)?(?: code)?)\s*(\d{3})\b`)
	unreachable = regexp.MustCompile(`(?i)could not resolve host|failed to connect|connection (refused|reset|timed out)|timed out|operation too slow|ssl_error_syscall|empty reply|unexpected disconnect|early eof|couldn't connect|network is unreachable|no route to host|send failure|recv failure|non-properly terminated|handshake\(\) failed: the tls connection`)
	badCert     = regexp.MustCompile(`(?i)ssl certificate problem|certificate verify failed|unknown ca|bad certificate|certificate (has expired|required)|tlsv1 alert|server (certificate )?verification failed|certificate signer not trusted`)
)

// Push sends the commit to refs/heads/<branch> of the relay, without force and
// without any hook, from the bare repository of the Project. The environment is
// built from nothing and the configuration travels in it, so that the token is
// in neither argv nor any file; redirects are not followed.
//
// The guard reads what the commit brings first, every time (forbidden): a push
// it does not let through is a BlockedError, and nothing is sent.
func (g *Git) Push(ctx context.Context, project, commit, branch string, a PushAuth, forbidden Forbidden) error {
	if !domain.ValidBranch(branch) {
		return &PushError{Class: PushLocal, Message: "the project, commit or branch is not valid"}
	}
	return g.PushRef(ctx, project, commit, "refs/heads/"+branch, a, forbidden)
}

// PushRef is Push to a full ref: a branch of CodeFabric, or the ref of an orphan.
func (g *Git) PushRef(ctx context.Context, project, commit, ref string, a PushAuth, forbidden Forbidden) error {
	branch, isBranch := strings.CutPrefix(ref, "refs/heads/")
	switch {
	case !isProject(project) || !domain.ValidCommitID(commit) || (isBranch && !domain.ValidBranch(branch)) || (!isBranch && !domain.ValidOrphanRef(ref)):
		return &PushError{Class: PushLocal, Message: "the project, commit or branch is not valid"}
	case a.URL == "" || a.Token == "" || a.Cert == "" || a.Key == "" || a.CA == "":
		return &PushError{Class: PushLocal, Message: "the credentials for the relay are incomplete"}
	}
	if err := g.guard(ctx, project, commit, forbidden); err != nil {
		return err
	}
	bare := g.o.Paths.WorkerRepoDir(project)
	// The keys of the TLS and of the proxy are those of this URL, which win over any
	// the repository may carry for it or in general; the rest are in the scope of the
	// command, above the configuration of the repository.
	scoped := func(key string) string { return "http." + a.URL + "." + key }
	config := [][2]string{
		{"core.hooksPath", "/dev/null"},
		{"core.askPass", ""},
		{"credential.helper", ""},
		{"http.followRedirects", "false"},
		{"http.lowSpeedLimit", "1000"},
		{"http.lowSpeedTime", "120"},
		{"http.sslVerify", "true"},
		{scoped("sslVerify"), "true"},
		{scoped("sslCert"), a.Cert},
		{scoped("sslKey"), a.Key},
		{scoped("sslCAInfo"), a.CA},
		{scoped("proxy"), ""},
		{scoped("extraHeader"), "Authorization: Bearer " + a.Token},
	}
	env := append(isolatedEnv(bare), "GIT_CONFIG_COUNT="+strconv.Itoa(len(config)))
	for i, kv := range config {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	res, err := g.run(ctx, cmd{
		dir: bare, env: env, timeout: g.pushTimeout(), secrets: []string{a.Token},
		args: []string{"push", "--porcelain", "--no-verify", "--no-progress", "--end-of-options", a.URL, commit + ":" + ref},
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
	case strings.Contains(text, "could not read Username") || strings.Contains(text, "terminal prompts disabled"):
		e.Class = PushPermanent // a 401 that git would have answered with a prompt
	case hasStatus(text):
		e.Class = classOfStatus(statusOf(text))
	case badCert.MatchString(text):
		e.Class = PushPermanent
	case unreachable.MatchString(text):
		e.Class = PushUnreachable
	default:
		e.Class = PushLocal
	}
	if e.Message == "" {
		e.Message = fmt.Sprintf("git push exited %d", res.exit)
	}
	return e
}

func hasStatus(text string) bool { return statusOf(text) != 0 }

func statusOf(text string) int {
	m := status.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// classOfStatus: a server that fails or is busy says nothing of the result; a 403
// is the relay forbidding the push (the token is new at each attempt); any other
// answer that is not a success will not change by itself.
func classOfStatus(code int) PushClass {
	switch {
	case code >= 500 || code == 429 || code == 408:
		return PushUnreachable
	case code == 403:
		return PushForbidden
	}
	return PushPermanent
}

func (g *Git) pushTimeout() time.Duration {
	if g.o.PushTimeout > 0 {
		return g.o.PushTimeout
	}
	return pushTimeout
}
