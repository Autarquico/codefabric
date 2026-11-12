package gitops

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// RelaySource is where the source of a Run that continues a checkpoint comes
// from: the ref of the checkpoint in the git relay of the Controller, which
// holds commits that were never pushed to the origin. The token is the read
// token of the Run's lease; it goes to git in the environment, as in a push.
type RelaySource struct {
	// Ref is refs/cf/checkpoints/<checkpoint id>.
	Ref  string
	Auth PushAuth
}

func (r *RelaySource) check() error {
	switch {
	case !domain.ValidCheckpointRef(r.Ref):
		return errors.New("gitops: the source of this Run is not a checkpoint ref")
	case r.Auth.URL == "" || r.Auth.Token == "" || r.Auth.Cert == "" || r.Auth.Key == "" || r.Auth.CA == "":
		return errors.New("gitops: the credentials for the relay are incomplete")
	}
	return nil
}

// relayEnv is the environment of a git command against the relay: built from
// nothing, with the configuration in it, so that the token is in neither argv
// nor any file; redirects are not followed. The keys of the TLS and of the
// proxy are those of this URL, which win over any the repository may carry for
// it or in general; the rest are in the scope of the command, above the
// configuration of the repository.
func relayEnv(bare string, a PushAuth) []string {
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
	return env
}

// fetchRelay brings the commit of a checkpoint from its ref in the relay into
// the bare repository, and writes no ref there. Network failures are retried
// with a backoff.
func (g *Git) fetchRelay(ctx context.Context, bare string, s SourceSpec) error {
	a := s.Relay.Auth
	env := relayEnv(bare, a)
	var lastFailure string
	delay := time.Second
	for attempt := 1; attempt <= fetchAttempts; attempt++ {
		_, err := g.ok(ctx, cmd{
			dir: bare, env: env, timeout: fetchTimeout, secrets: []string{a.Token},
			args: []string{"fetch", "--quiet", "--no-tags", "--no-auto-maintenance", "--no-write-fetch-head", "--end-of-options", a.URL, s.Relay.Ref},
		})
		if err == nil {
			if g.hasCommit(ctx, bare, s.SourceCommit) {
				return nil
			}
			return fmt.Errorf("gitops: the checkpoint %s of the relay does not hold the commit %s", s.Relay.Ref, s.SourceCommit[:12])
		}
		lastFailure = clean(err.Error(), []string{a.Token})
		if ctx.Err() != nil {
			break
		}
		g.log.WarnContext(ctx, "fetching the checkpoint of a Run from the relay failed", "run", s.Run, "err", lastFailure)
		if attempt < fetchAttempts {
			g.sleep(ctx, delay)
			delay *= 2
		}
	}
	return fmt.Errorf("gitops: the checkpoint %s is not available from the relay: %s", s.Relay.Ref, lastFailure)
}
