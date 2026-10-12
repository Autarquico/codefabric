package relay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

// Endpoint is one of the three requests the relay serves. Nothing else of the
// git HTTP protocol (the dumb one included) is reachable.
type Endpoint int

// Endpoints.
const (
	InfoRefsUpload  Endpoint = iota // GET  info/refs?service=git-upload-pack
	InfoRefsReceive                 // GET  info/refs?service=git-receive-pack
	UploadPack                      // POST git-upload-pack
	ReceivePack                     // POST git-receive-pack
)

func (e Endpoint) writes() bool { return e == InfoRefsReceive || e == ReceivePack }

// Grant is what a request has been authorised to do, resolved by the server
// from its own state for this request alone. For a write, Branch is the one
// branch the push may update, or Ref the one full ref it may create (the ref of
// an orphan, CreateOnly), and ExpectedOID the one commit it may set it to.
type Grant struct {
	Project     domain.ID
	Worker      domain.ID
	Write       bool
	Branch      string
	Ref         string
	CreateOnly  bool
	ExpectedOID string
}

// allowedRef is the one ref a write may touch.
func (g Grant) allowedRef() string {
	if g.Ref != "" {
		return g.Ref
	}
	if g.Branch == "" {
		return ""
	}
	return "refs/heads/" + g.Branch
}

// Errors of Serve that are answered before anything is written. The caller
// maps them to a response.
var (
	ErrBusy     = errors.New("relay: too many requests in flight")
	ErrTooLarge = errors.New("relay: the request is larger than the relay accepts")
	ErrBadBody  = errors.New("relay: the request body cannot be read")
)

// DefaultIdleRead is how long a peer may stay silent while it sends a body. It
// is an infrastructure deadline of the connection, on real time.
const DefaultIdleRead = 60 * time.Second

// maxRequestsPerWorker bounds the requests of the relay one Worker has in
// flight, whatever they are.
const maxRequestsPerWorker = 2

// killDelay is how long the pipes of a killed child are waited for.
const killDelay = 3 * time.Second

var (
	protocolHeader = regexp.MustCompile(`^version=[012]$`)
	bodyTypes      = map[Endpoint]string{
		UploadPack:  "application/x-git-upload-pack-request",
		ReceivePack: "application/x-git-receive-pack-request",
	}
	// responseHeaders are the only headers of the CGI answer that reach the peer.
	responseHeaders = []string{"Content-Type", "Cache-Control", "Expires", "Pragma"}
)

// Serve runs `git http-backend` for one request that the caller has
// authorised, and streams its answer. It is a CGI adapter of our own rather
// than net/http/cgi: the body goes to a bounded temporary file first, so that
// the child sees a real CONTENT_LENGTH whatever the framing of the request;
// the child leads its own process group, with a deadline and a kill; its stderr
// is bounded and redacted; and its environment is built from nothing.
//
// The Authorization header of the request is never passed on: the environment
// of the hooks is the environment of the child.
func (r *Relay) Serve(w http.ResponseWriter, req *http.Request, ep Endpoint, g Grant) error {
	if ep.writes() && (!g.Write || g.allowedRef() == "" || g.ExpectedOID == "") {
		return errors.New("relay: a write was not granted")
	}
	// One deadline for the whole request, the upload included, on real time: it
	// bounds infrastructure. Nothing below may outlast it.
	deadline := time.Now().Add(r.cfg.Deadline)
	release, err := r.acquire(ep, g.Worker)
	if err != nil {
		return err
	}
	defer release()
	if _, err := r.Ensure(req.Context(), g.Project); err != nil {
		return err
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(deadline)
	body, size, err := r.spool(req, rc, ep, deadline)
	if err != nil {
		return err
	}
	// net/http reads in the background once the body is read: the deadline of the
	// last read of the body must not cancel a request that is only being answered.
	_ = rc.SetReadDeadline(deadline)
	if body != nil {
		defer body.Close()
	}
	env, err := r.cgiEnv(req, ep, g, size)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithDeadline(req.Context(), deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.git, "http-backend") //nolint:gosec // git is the located executable
	cmd.Dir, cmd.Env = r.cfg.Dir, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = killDelay
	if body != nil {
		cmd.Stdin = body
	}
	var stderr bytes.Buffer
	cmd.Stderr = &boundedWriter{w: &stderr, max: stderrBytes}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("relay: pipe of http-backend: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("relay: starting http-backend: %w", err)
	}
	waited := false
	wait := func() error {
		waited = true
		return cmd.Wait()
	}
	defer func() {
		if !waited {
			cancel()
			_ = wait()
		}
	}()

	br := bufio.NewReaderSize(stdout, 64<<10)
	header, err := textproto.NewReader(br).ReadMIMEHeader()
	if err != nil {
		cancel()
		_ = wait()
		r.logChild(ep, g, stderr.String(), err)
		return fmt.Errorf("relay: reading the answer of http-backend: %w", err)
	}
	status, err := cgiStatus(header)
	if err != nil {
		cancel()
		_ = wait()
		return err
	}
	for _, name := range responseHeaders {
		if v := header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
	w.WriteHeader(status)
	_, copyErr := io.Copy(flushWriter{w: w, rc: rc}, br)
	if copyErr != nil || ctx.Err() != nil {
		// The peer went away, or the deadline passed: the answer is cut and the
		// child is gone with its group. The connection must not look complete.
		cancel()
		_ = wait()
		r.logChild(ep, g, stderr.String(), errors.Join(copyErr, ctx.Err()))
		panic(http.ErrAbortHandler)
	}
	waitErr := wait()
	r.logChild(ep, g, stderr.String(), waitErr)
	return nil
}

// acquire takes the slots a request needs: one of the relay's, one of the
// few requests a Worker may have in flight, and for a push the single push it
// may have. It never waits.
func (r *Relay) acquire(ep Endpoint, worker domain.ID) (release func(), err error) {
	select {
	case r.slots <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	r.mu.Lock()
	_, pushBusy := r.pushing[worker]
	busy := r.inflight[worker] >= maxRequestsPerWorker || (ep == ReceivePack && pushBusy)
	if !busy {
		r.inflight[worker]++
		if ep == ReceivePack {
			r.pushing[worker] = struct{}{}
		}
	}
	r.mu.Unlock()
	if busy {
		<-r.slots
		return nil, ErrBusy
	}
	return func() {
		r.mu.Lock()
		if r.inflight[worker]--; r.inflight[worker] <= 0 {
			delete(r.inflight, worker)
		}
		if ep == ReceivePack {
			delete(r.pushing, worker)
		}
		r.mu.Unlock()
		<-r.slots
	}, nil
}

// spool copies the body of a POST, as net/http decoded it (chunked included),
// to a temporary file that is already deleted, bounded by the limit of its
// endpoint. It returns the file rewound and its size; a GET has no body.
func (r *Relay) spool(req *http.Request, rc *http.ResponseController, ep Endpoint, deadline time.Time) (*os.File, int64, error) {
	if req.Method != http.MethodPost {
		return nil, 0, nil
	}
	limit := int64(maxFetchRequestBytes)
	if ep == ReceivePack {
		limit = r.cfg.MaxPushBytes
	}
	f, err := os.CreateTemp(r.tmpDir(), "body-")
	if err != nil {
		return nil, 0, fmt.Errorf("relay: spooling a request: %w", err)
	}
	_ = os.Remove(f.Name()) // the open descriptor is all that is needed
	n, err := io.Copy(f, io.LimitReader(activityReader{r: req.Body, rc: rc, idle: r.cfg.IdleRead, deadline: deadline}, limit+1))
	switch {
	case err != nil:
		_ = f.Close()
		return nil, 0, fmt.Errorf("%w: %w", ErrBadBody, err)
	case n > limit:
		_ = f.Close()
		return nil, 0, ErrTooLarge
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("relay: rewinding a request: %w", err)
	}
	return f, n, nil
}

// cgiEnv is the whole environment of http-backend and, through it, of
// receive-pack and the hook. The Authorization header is not in it, and nothing
// that comes from the peer is, apart from the validated few below.
func (r *Relay) cgiEnv(req *http.Request, ep Endpoint, g Grant, size int64) ([]string, error) {
	repo := string(g.Project) + ".git"
	env := append(r.baseEnv(),
		"GIT_PROJECT_ROOT="+r.cfg.Dir,
		"GIT_HTTP_EXPORT_ALL=1",
		"REMOTE_USER="+string(g.Worker), // what turns receive-pack on for the request
		"REMOTE_ADDR=0.0.0.0",
		"REQUEST_METHOD="+req.Method,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0="+r.hooksDir(),
		"GIT_CONFIG_KEY_1=receive.maxInputSize", "GIT_CONFIG_VALUE_1="+strconv.FormatInt(r.cfg.MaxPushBytes, 10),
	)
	switch ep {
	case InfoRefsUpload, InfoRefsReceive:
		service := "git-upload-pack"
		if ep == InfoRefsReceive {
			service = "git-receive-pack"
		}
		env = append(env, "PATH_INFO=/"+repo+"/info/refs", "QUERY_STRING=service="+service)
	case UploadPack:
		env = append(env, "PATH_INFO=/"+repo+"/git-upload-pack", "QUERY_STRING=")
	case ReceivePack:
		env = append(env, "PATH_INFO=/"+repo+"/git-receive-pack", "QUERY_STRING=")
	}
	if want, post := bodyTypes[ep]; post {
		env = append(env, "CONTENT_LENGTH="+strconv.FormatInt(size, 10))
		if req.Header.Get("Content-Type") == want {
			env = append(env, "CONTENT_TYPE="+want)
		}
		switch enc := strings.ToLower(req.Header.Get("Content-Encoding")); {
		case enc == "" || enc == "identity":
		case ep == UploadPack && (enc == "gzip" || enc == "x-gzip"):
			env = append(env, "HTTP_CONTENT_ENCODING=gzip")
		default:
			return nil, fmt.Errorf("%w: content encoding %q", ErrBadBody, enc)
		}
	}
	if p := req.Header.Get("Git-Protocol"); protocolHeader.MatchString(p) {
		env = append(env, "HTTP_GIT_PROTOCOL="+p)
	}
	if ep.writes() {
		env = append(env, "CF_RELAY_ALLOWED_REF="+g.allowedRef(), "CF_RELAY_EXPECTED_OID="+g.ExpectedOID)
		if g.CreateOnly {
			env = append(env, envCreateOnly+"=1")
		}
	}
	return env, nil
}

// cgiStatus reads the Status line of a CGI answer; no line means 200.
func cgiStatus(h textproto.MIMEHeader) (int, error) {
	line := h.Get("Status")
	if line == "" {
		return http.StatusOK, nil
	}
	code, _, _ := strings.Cut(line, " ")
	n, err := strconv.Atoi(code)
	if err != nil || n < 100 || n > 599 {
		return 0, fmt.Errorf("relay: http-backend answered a status %q", line)
	}
	return n, nil
}

// logChild records what the child said on stderr, at debug level: bounded and
// redacted, and never sent to the peer.
func (r *Relay) logChild(ep Endpoint, g Grant, stderr string, err error) {
	if err == nil && stderr == "" {
		return
	}
	r.cfg.Log.Debug("git http-backend", "endpoint", int(ep), "project", g.Project, "worker", g.Worker,
		"err", err, "stderr", redact.Tail(redact.Text(strings.TrimSpace(stderr)), stderrBytes))
}

// activityReader asks for a deadline to the next byte before each read, so a
// peer that goes quiet does not hold a slot of the relay.
type activityReader struct {
	r        io.Reader
	rc       *http.ResponseController
	idle     time.Duration
	deadline time.Time
}

func (a activityReader) Read(p []byte) (int, error) {
	next := time.Now().Add(a.idle)
	if next.After(a.deadline) {
		next = a.deadline
	}
	_ = a.rc.SetReadDeadline(next)
	return a.r.Read(p)
}

// flushWriter sends each piece as it comes, which is what keeps the progress
// of a long pack visible to the client.
type flushWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		_ = f.rc.Flush()
	}
	return n, err
}
