package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/procgroup"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

// guardTimeout bounds the reading of everything a publication would carry.
const guardTimeout = 10 * time.Minute

// Forbidden is what the publication of a Run must not carry: the values of its
// secrets. It is handed to one sealing or push and lives in memory only; it is
// never part of Intent, which is stored. Its zero value is a Run without
// secrets, which is published as in Phase 1, with nothing read.
type Forbidden struct {
	// Since is the commit the Run started from. What it reaches came from
	// origin and is not new; everything else the result reaches is read.
	Since  string
	Values []redact.Needle
}

func (f Forbidden) none() bool { return len(f.Values) == 0 }

// withheld stands for a text about a result whose values cannot be redacted.
const withheld = "[withheld: it could not be checked for the values of the secrets of the Run]"

// redaction matches the values of f in what git prints of the result, which
// quotes the paths the harness left: a name can hold a value. It is nil when f
// has no values.
func (f Forbidden) redaction() (*redact.Set, error) {
	if f.none() {
		return nil, nil //nolint:nilnil // no values, nothing to redact
	}
	return redact.NewEscapedSet(f.Values)
}

// Redact is s without the values of f: what of a text about the result may be
// logged, kept or reported. If the values cannot be redacted, all of it is
// withheld.
func (f Forbidden) Redact(s string) string {
	set, err := f.redaction()
	if err != nil {
		return withheld
	}
	return redacted(s, set)
}

// BlockedError is a publication the guard did not let through: a value of a
// secret is in it, or it could not be read whole. Its reason names refs,
// never values.
type BlockedError struct{ Reason string }

func (e *BlockedError) Error() string { return "the publication is blocked: " + e.Reason }

func blocked(format string, args ...any) *BlockedError {
	return &BlockedError{Reason: fmt.Sprintf(format, args...)}
}

// markerPrefix is how every redaction marker starts; see scanner.
const markerPrefix = "[REDACTE"

// guard reads, in a stream, every object that commit reaches and f.Since does
// not (the commits, trees and blobs of the sealing and of what the harness
// committed itself, whatever their size) and fails unless none holds a value
// of f. A match, an object it cannot read, a git that fails or the deadline
// are all a BlockedError: the guard is never skipped.
func (g *Git) guard(ctx context.Context, project, commit string, f Forbidden) error {
	if f.none() {
		return nil
	}
	if !domain.ValidCommitID(f.Since) || !domain.ValidCommitID(commit) {
		return blocked("the result cannot be checked for secrets: it has no source commit to compare with")
	}
	sc, err := newScanner(f.Values)
	if err != nil {
		return err
	}
	values, err := f.redaction()
	if err != nil {
		return blocked("the result cannot be checked for secrets: %v", err)
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, g.guardTimeout())
	defer cancel()
	sc.stop = cancel

	bare := g.o.Paths.WorkerRepoDir(project)
	// Replacements would let git show objects other than those that are pushed.
	env := append(isolatedEnv(bare), "GIT_NO_REPLACE_OBJECTS=1")
	oids, feed, err := os.Pipe()
	if err != nil {
		return blocked("the result cannot be checked for secrets: %v", err)
	}
	defer func() { _ = feed.Close() }()

	catErr, listErr := &boundedBuffer{max: stderrBytes}, &boundedBuffer{max: stderrBytes}
	cat := exec.Command(g.git, "cat-file", "--batch") //nolint:gosec,noctx // git is the located executable; ctx goes to procgroup
	cat.Dir, cat.Env, cat.Stdin = bare, env, oids
	catGroup, err := procgroup.Start(ctx, cat, procgroup.Options{Stdout: sc, Stderr: catErr, Drain: killDelay})
	_ = oids.Close()
	if err != nil {
		return blocked("the result cannot be checked for secrets: %v", err)
	}
	list := exec.Command(g.git, "rev-list", "--objects", "--no-object-names", commit, "--not", f.Since) //nolint:gosec,noctx // validated commit IDs; ctx goes to procgroup
	list.Dir, list.Env = bare, env
	listGroup, err := procgroup.Start(ctx, list, procgroup.Options{Stdout: feed, Stderr: listErr, Drain: killDelay})
	var listed error
	if err == nil {
		listed = listGroup.Wait()
	} else {
		listed = err
		cancel()
	}
	_ = feed.Close()
	read := catGroup.Wait()

	switch {
	case sc.hit != "":
		return blocked("the result contains the value of secret %q", sc.hit)
	case sc.err != nil:
		return blocked("the result cannot be checked for secrets: %v", sc.err)
	case parent.Err() != nil:
		return fmt.Errorf("gitops: checking the result for secrets: %w", parent.Err())
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return blocked("checking the result for secrets took too long")
	case listed != nil:
		return blocked("the result cannot be checked for secrets: listing its objects failed: %s", gitFailure(listed, listErr, values))
	case read != nil:
		return blocked("the result cannot be checked for secrets: reading its objects failed: %s", gitFailure(read, catErr, values))
	case !sc.done():
		return blocked("the result cannot be checked for secrets: its objects were not read whole")
	}
	return nil
}

func gitFailure(err error, stderr *boundedBuffer, values *redact.Set) string {
	if msg := clean(redacted(stderr.text(), values), nil); msg != "" {
		return msg
	}
	return err.Error()
}

func (g *Git) guardTimeout() time.Duration {
	if g.o.GuardTimeout > 0 {
		return g.o.GuardTimeout
	}
	return guardTimeout
}

// scanner reads the output of `cat-file --batch` as it comes, and runs each
// object through the redaction of each secret on its own. What the redaction
// lets out differs from what went in exactly where a value was (its marker
// stands in its place), which is how a match is seen, with the ref it is of.
// That holds unless a value could start like a marker: such a value is
// refused, since it could not be told from its marker.
type scanner struct {
	values []redact.Needle
	sets   []*redact.Set
	stop   func()

	header  []byte
	pending int64 // bytes of the object still to come; -1 between objects
	trailer bool  // the line break after an object is due
	objects []*detector

	hit string
	err error
}

func newScanner(values []redact.Needle) (*scanner, error) {
	sc := &scanner{values: values, pending: -1}
	for _, v := range values {
		if bytes.Contains(v.Value, []byte(markerPrefix)) {
			return nil, blocked("the result cannot be checked for secrets: the value of secret %q looks like a redaction marker", v.Ref)
		}
		set, err := redact.NewEscapedSet([]redact.Needle{v})
		if err != nil {
			return nil, blocked("the result cannot be checked for secrets: %v", err)
		}
		sc.sets = append(sc.sets, set)
	}
	return sc, nil
}

// done says the stream ended between two objects.
func (s *scanner) done() bool { return s.pending < 0 && !s.trailer && len(s.header) == 0 }

func (s *scanner) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && s.hit == "" && s.err == nil {
		switch {
		case s.trailer:
			if p[0] != '\n' {
				s.fail(errors.New("the output of git cat-file is not what it should be"))
				break
			}
			s.trailer, p = false, p[1:]
		case s.pending < 0:
			i := bytes.IndexByte(p, '\n')
			if i < 0 {
				s.header = append(s.header, p...)
				p = nil
				if len(s.header) > 512 {
					s.fail(errors.New("the output of git cat-file is not what it should be"))
				}
				break
			}
			s.header = append(s.header, p[:i]...)
			p = p[i+1:]
			s.begin(string(s.header))
			s.header = s.header[:0]
		default:
			k := min(int64(len(p)), s.pending)
			for _, d := range s.objects {
				d.feed(p[:k])
			}
			s.pending -= k
			p = p[k:]
			if s.pending == 0 {
				s.end()
			}
		}
	}
	if s.hit != "" || s.err != nil {
		return 0, errors.New("gitops: the guard stopped reading")
	}
	return n, nil
}

// begin reads the header of an object: "<oid> <type> <size>".
func (s *scanner) begin(header string) {
	fields := strings.Fields(header)
	if len(fields) != 3 {
		s.fail(errors.New("the output of git cat-file is not what it should be"))
		return
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		s.fail(errors.New("the output of git cat-file is not what it should be"))
		return
	}
	s.objects = s.objects[:0]
	for i, set := range s.sets {
		s.objects = append(s.objects, newDetector(s.values[i].Ref, set))
	}
	s.pending = size
	if size == 0 {
		s.end()
	}
}

func (s *scanner) end() {
	for _, d := range s.objects {
		if d.close() {
			s.hit = d.ref
			s.stop()
			break
		}
	}
	s.pending, s.trailer = -1, true
}

func (s *scanner) fail(err error) {
	s.err = err
	s.stop()
}

// detector runs one object through the redaction of one secret and compares
// what comes out with what went in.
type detector struct {
	ref  string
	w    *redact.Writer
	sink *compare
}

func newDetector(ref string, set *redact.Set) *detector {
	c := &compare{}
	return &detector{ref: ref, w: redact.NewWriter(c, set), sink: c}
}

func (d *detector) feed(p []byte) {
	if d.sink.changed {
		return
	}
	d.sink.in = append(d.sink.in, p...)
	_, _ = d.w.Write(p)
}

// close says whether the object held the value.
func (d *detector) close() bool {
	if !d.sink.changed {
		_ = d.w.Close()
	}
	return d.sink.changed || len(d.sink.in) != 0
}

// compare is the destination of a redaction: it checks each byte that comes
// out against the input still unmatched.
type compare struct {
	in      []byte
	changed bool
}

func (c *compare) Write(out []byte) (int, error) {
	if c.changed || len(out) > len(c.in) || !bytes.Equal(out, c.in[:len(out)]) {
		c.changed = true
		return len(out), nil
	}
	c.in = c.in[:copy(c.in, c.in[len(out):])]
	return len(out), nil
}
