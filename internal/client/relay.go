package client

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

// RelayToken asks for a token for the git relay under the lease epoch the
// Worker holds the Run with. It is for clients opened with a Worker's
// credentials. The token is for git's HTTP header and for nothing else: it is
// not to be written to a file, a log or a command line.
func (c *Client) RelayToken(ctx context.Context, run string, leaseEpoch uint64) (v1.RelayTokenResponse, error) {
	var out v1.RelayTokenResponse
	err := c.do(ctx, http.MethodPost, refPath("worker/runs", run, "relay-token"), v1.RelayTokenRequest{LeaseEpoch: leaseEpoch}, &out)
	return out, err
}

// OrphanToken asks for a token that may only create the ref of an orphan the
// Worker holds, at the commit it sealed. Like RelayToken, it is for git's HTTP
// header alone.
func (c *Client) OrphanToken(ctx context.Context, run string, leaseEpoch uint64, commit string) (v1.RelayTokenResponse, error) {
	var out v1.RelayTokenResponse
	err := c.do(ctx, http.MethodPost, refPath("worker/runs", run, "orphan-token"), v1.OrphanTokenRequest{LeaseEpoch: leaseEpoch, ResultCommit: commit}, &out)
	return out, err
}

// PublishRun asks for another try at publishing the result of a Run whose
// publication failed (`cf run publish`).
func (c *Client) PublishRun(ctx context.Context, ref string) (v1.Run, error) {
	var out v1.Run
	err := c.do(ctx, http.MethodPost, refPath("runs", ref, "publish"), v1.ActionRequest{}, &out)
	return out, err
}

// RelayURL is the URL of a path of the Controller, such as the Path of a
// RelayTokenResponse, as git has to dial it.
func (c *Client) RelayURL(path string) string { return c.base + path }

// TLSFiles are the PEM files of the credentials a git process presents and
// trusts when it talks to the relay: the certificate and key of this machine
// and the CA pinned at connect time.
type TLSFiles struct{ Cert, Key, CA string }

// TLSFiles locates them. It is empty for a client with no credentials directory.
func (c *Client) TLSFiles() TLSFiles {
	if c.dir == "" {
		return TLSFiles{}
	}
	return TLSFiles{Cert: filepath.Join(c.dir, certFile), Key: filepath.Join(c.dir, keyFile), CA: filepath.Join(c.dir, caFile)}
}

// ClientRelayToken asks for a token for the git relay of a Project (a
// reference): it reads the branches of Tasks and the checkpoints, and creates
// snapshots. Like the token of a Worker it is for git's HTTP header alone.
func (c *Client) ClientRelayToken(ctx context.Context, project string) (v1.RelayTokenResponse, error) {
	var out v1.RelayTokenResponse
	err := c.do(ctx, http.MethodPost, "/v1/relay-token", v1.ClientRelayTokenRequest{Project: project}, &out)
	return out, err
}

// FetchRelayRef brings a ref of the relay of a Project into the repository at
// dir and returns the commit it points to. The ref must be the branch of a
// Task (refs/heads/cf/...) or a checkpoint; nothing else is read. No ref of
// the repository is written: the caller puts the commit where it belongs.
func (c *Client) FetchRelayRef(ctx context.Context, dir, project, ref string) (string, error) {
	if !readableRelayRef(ref) {
		return "", domain.NewError(domain.CodeInvalidRequest, "only the branch of a Task and the checkpoints are read from the relay", nil)
	}
	tok, err := c.ClientRelayToken(ctx, project)
	if err != nil {
		return "", err
	}
	if err := c.relayGit(ctx, dir, tok, "fetch", "--quiet", "--no-tags", "--no-auto-maintenance", "--end-of-options", c.RelayURL(tok.Path), ref); err != nil {
		return "", err
	}
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "--quiet", "FETCH_HEAD^{commit}").Output() //nolint:gosec // fixed arguments
	if err != nil {
		return "", fmt.Errorf("reading what was fetched from the relay: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// PushSnapshot sends the snapshot commit, which must exist in the repository
// at dir, to refs/cf/snapshots/<commit> of the relay of a Project. The ref is
// only created: sending the same snapshot twice changes nothing.
func (c *Client) PushSnapshot(ctx context.Context, dir, project, commit string) error {
	if !domain.ValidCommitID(commit) {
		return domain.NewError(domain.CodeInvalidRequest, "a snapshot is a full commit ID", nil)
	}
	tok, err := c.ClientRelayToken(ctx, project)
	if err != nil {
		return err
	}
	return c.relayGit(ctx, dir, tok, "push", "--quiet", "--no-verify", "--end-of-options", c.RelayURL(tok.Path), commit+":"+domain.SnapshotRef(commit))
}

func readableRelayRef(ref string) bool {
	if branch, ok := strings.CutPrefix(ref, "refs/heads/cf/"); ok {
		return domain.ValidBranch("cf/" + branch)
	}
	return domain.ValidCheckpointRef(ref)
}

// relayGit runs git against the relay with this client's certificate, the CA
// it pinned and the token in the header, all by environment: the token is
// neither in argv nor in any file, the user's hooks and credential helpers do
// not run, and redirects are not followed.
func (c *Client) relayGit(ctx context.Context, dir string, tok v1.RelayTokenResponse, args ...string) error {
	files, url := c.TLSFiles(), c.RelayURL(tok.Path)
	scoped := func(key string) string { return "http." + url + "." + key }
	config := [][2]string{
		{"core.hooksPath", os.DevNull},
		{"core.askPass", ""},
		{"credential.helper", ""},
		{"http.followRedirects", "false"},
		{"http.sslVerify", "true"},
		{scoped("sslCert"), files.Cert},
		{scoped("sslKey"), files.Key},
		{scoped("sslCAInfo"), files.CA},
		{scoped("proxy"), ""},
		{scoped("extraHeader"), "Authorization: Bearer " + tok.Token},
	}
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_CONFIG_COUNT="+strconv.Itoa(len(config)))
	for i, kv := range config {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git subcommands; the user's values follow "--end-of-options"
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.ReplaceAll(strings.TrimSpace(string(out)), tok.Token, "[redacted]")
		return fmt.Errorf("git %s against the relay: %s", args[0], redact.Text(msg))
	}
	return nil
}
