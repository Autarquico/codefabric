package gitops

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Autarquico/codefabric/internal/support/redact"
)

// OriginAuth makes the fetches of a source authenticated: git asks the
// askpass script for the credential of the origin, and answers only with what
// the delivery in RunDir allows (cf worker git-askpass). It carries no
// credential: the script and the directory are paths, and the needles are only
// what git's output is redacted of.
type OriginAuth struct {
	// Askpass is the absolute path of the GIT_ASKPASS script of the Worker.
	Askpass string
	// RunDir is the absolute directory of the Run, with the delivery askpass reads.
	RunDir string
	// Needles are the values of the Run, redacted from everything git prints
	// and from the errors and logs made of it.
	Needles []redact.Needle
}

// check is an error unless a fetch of repo may use a (nil is always fine):
// the repository is an https URL without userinfo, and the paths are absolute.
func (a *OriginAuth) check(repo string) error {
	if a == nil {
		return nil
	}
	u, err := url.Parse(repo)
	switch {
	case err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil:
		return errors.New("gitops: a credential for origin is only sent to an https repository without userinfo")
	case !filepath.IsAbs(a.Askpass) || !filepath.IsAbs(a.RunDir):
		return errors.New("gitops: the askpass script and the run directory must be absolute paths")
	}
	return nil
}

// originFetch is how one fetch from origin runs git.
type originFetch struct {
	env     []string
	values  *redact.Set // nil if there is nothing to redact
	cleanup func()      // removes what the fetch made; never nil
}

// proxyEnv is the only proxy configuration an authenticated fetch honours:
// that of the daemon's environment.
var proxyEnv = []string{"HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"}

// refSnapshot is the refs of bare as "<oid> <refname>" lines, for scratchRepo.
func (g *Git) refSnapshot(ctx context.Context, bare string) (string, error) {
	out, err := g.ok(ctx, cmd{dir: bare, env: isolatedEnv(bare), timeout: quickTimeout, args: []string{"for-each-ref", "--format=%(objectname) %(refname)"}})
	if err != nil {
		return "", fmt.Errorf("gitops: reading the refs of the repository for a fetch: %w", err)
	}
	return out, nil
}

// originFetch is the environment of a fetch from origin. Without auth it is
// that of the user, with their git configuration and credentials. With auth
// it is built from nothing, as the relay's is: no system or global
// configuration (so no helper, rewrite, header, filter or TLS setting of the
// machine), https only, verified TLS, no redirect, no helper, the askpass
// script as the only source of credentials, and a proxy only from the
// environment of the daemon. refs is the snapshot the scratch negotiates with (see scratchRepo). The repository git runs in is not the bare one
// but a scratch one made for this fetch (see scratchRepo), so that the
// configuration of the shared bare repository, which a harness of another Run
// can write, takes no part in it.
func (g *Git) originFetch(bare string, a *OriginAuth, refs string) (originFetch, error) {
	if a == nil {
		return originFetch{env: userEnv(), cleanup: func() {}}, nil
	}
	f := originFetch{cleanup: func() {}}
	if len(a.Needles) > 0 {
		set, err := redact.NewEscapedSet(a.Needles)
		if err != nil {
			return f, fmt.Errorf("gitops: the output of git cannot be redacted of the credential: %w", err)
		}
		f.values = set
	}
	config := [][2]string{
		{"core.hooksPath", "/dev/null"},
		{"credential.helper", ""},
		{"credential.useHttpPath", "true"},
		{"http.followRedirects", "false"},
		{"http.sslVerify", "true"},
		{"gc.auto", "0"},
		{"maintenance.auto", "false"},
	}
	if g.trustCA != "" { // tests only
		config = append(config, [2]string{"http.sslCAInfo", g.trustCA})
	}
	scratch, err := scratchRepo(a.RunDir, refs)
	if err != nil {
		return f, err
	}
	f.cleanup = func() { _ = os.RemoveAll(scratch) }
	env := append(isolatedEnv(bare), "GIT_DIR="+scratch, "GIT_OBJECT_DIRECTORY="+filepath.Join(bare, "objects"),
		"GIT_ASKPASS="+a.Askpass, "CF_ASKPASS_RUN_DIR="+a.RunDir, "GIT_CONFIG_COUNT="+strconv.Itoa(len(config)))
	for i, kv := range config {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	for _, k := range proxyEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	f.env = env
	return f, nil
}

// scratchRepo makes the repository an authenticated fetch runs in: its own
// minimal config and its own, empty refs. Git reads the configuration of a
// repository from its own config file, and a URL-scoped key there
// (http.<url>.sslVerify, sslCAInfo, proxy, extraHeader, curloptResolve, an
// include, an insteadOf) beats the command-line setting for the same URL. The
// config of bare is shared by all the Runs of the Project, so it is not read at
// all. Only the objects go to bare's store; no ref, reflog or worktree of bare
// is reachable from here, and the scratch turns off every automatic
// maintenance (gc, pack-refs, prune), which would otherwise run against the
// shared store with a partial view of what is reachable. The refs of bare are
// the daemon's to write, with update-ref in bare, under the project lock.
//
// The scratch does start with a copy of the refs of bare (refs, the output of
// for-each-ref, taken under the project lock) as its packed-refs. It is a
// plain file, never a link, and only for negotiation: git offers those commits
// as "have", so a fetch brings the delta and not the whole history again. The
// fetch has no destination ref, so nothing is written back.
func scratchRepo(runDir, refs string) (string, error) {
	dir, err := os.MkdirTemp(runDir, "fetch-")
	if err != nil {
		return "", fmt.Errorf("gitops: making the repository of a fetch: %w", err)
	}
	files := map[string]string{
		"HEAD":        "ref: refs/heads/main\n",
		"packed-refs": packedRefs(refs),
		"config":      "[core]\n\trepositoryformatversion = 0\n\tbare = true\n[gc]\n\tauto = 0\n[maintenance]\n\tauto = false\n",
	}
	for name, content := range files {
		if err = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			break
		}
	}
	if err == nil {
		err = os.Mkdir(filepath.Join(dir, "refs"), 0o700)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("gitops: making the repository of a fetch: %w", err)
	}
	return dir, nil
}

var refLine = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64}) (refs/[^\x00-\x20\x7f]+)$`)

// packedRefs is the packed-refs file of the lines "<oid> <refname>" that
// for-each-ref printed (already sorted); a line that is not exactly that is
// left out. Without the peeled trait, git peels tags itself.
func packedRefs(refs string) string {
	var b strings.Builder
	b.WriteString("# pack-refs with: sorted\n")
	for _, line := range strings.Split(refs, "\n") {
		if refLine.MatchString(line) {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}
