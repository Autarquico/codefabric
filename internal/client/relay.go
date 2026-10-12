package client

import (
	"context"
	"net/http"
	"path/filepath"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
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
