package gitops

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/Autarquico/codefabric/internal/support/redact"
)

// LinesWithSecrets reads r line by line and returns the numbers (from 1) of the
// lines that hold a value of f, in the forms the guard looks for (escaped
// ones included). It is the guard for a file that is not an object of git, such
// as the transcript of a harness: it names lines, never values. A value that
// looks like a redaction marker cannot be told from one, so it is a
// BlockedError, as it is for the guard of a commit.
func LinesWithSecrets(r io.Reader, f Forbidden) ([]int, error) {
	if f.none() {
		return nil, nil
	}
	sets := make([]*redact.Set, 0, len(f.Values))
	for _, v := range f.Values {
		if bytes.Contains(v.Value, []byte(markerPrefix)) {
			return nil, blocked("the file cannot be checked for secrets: the value of secret %q looks like a redaction marker", v.Ref)
		}
		set, err := redact.NewEscapedSet([]redact.Needle{v})
		if err != nil {
			return nil, blocked("the file cannot be checked for secrets: %v", err)
		}
		sets = append(sets, set)
	}
	var lines []int
	br := bufio.NewReader(r)
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && holdsAny(line, sets) {
			lines = append(lines, n)
		}
		if errors.Is(err, io.EOF) {
			return lines, nil
		}
		if err != nil {
			return nil, fmt.Errorf("gitops: reading a file to check it for secrets: %w", err)
		}
	}
}

func holdsAny(line []byte, sets []*redact.Set) bool {
	for _, set := range sets {
		w := redact.NewWriter(io.Discard, set)
		_, _ = w.Write(line)
		_ = w.Close()
		if w.Matched() {
			return true
		}
	}
	return false
}
