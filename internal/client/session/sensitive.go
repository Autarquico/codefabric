package session

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Finding is a line of a session that looks like a secret. It names the place and
// the kind, never the value: the finding is shown to the user and logged.
type Finding struct {
	File string
	Line int
	Kind string
}

func (f Finding) String() string { return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.Kind) }

// SensitiveError is an export blocked because the session holds what looks like a
// secret: the session of a harness records what the model was shown, files and
// command output included. AllowSensitive goes on anyway.
type SensitiveError struct{ Findings []Finding }

func (e *SensitiveError) Error() string {
	var b strings.Builder
	b.WriteString("the session holds what looks like a secret, so it was not exported:")
	for i, f := range e.Findings {
		if i == maxShown {
			fmt.Fprintf(&b, "\n  ... and %d more", len(e.Findings)-maxShown)
			break
		}
		b.WriteString("\n  " + f.String())
	}
	b.WriteString("\nThe Controller and the Worker would keep a copy. Pass --allow-sensitive-session to send it anyway.")
	return b.String()
}

const maxShown = 10

// secretPatterns are blunt on purpose: a false positive costs a flag, a miss costs a
// credential. The transcript is JSON, so a quote may come escaped.
var secretPatterns = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"private key", regexp.MustCompile(`-----BEGIN (?:[A-Z]+ )*PRIVATE KEY-----`)},
	{"AWS access key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"GitHub token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,})\b`)},
	{"API key", regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}`)},
	{"Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"JSON web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{"Authorization header", regexp.MustCompile(`(?i)\bauthorization\\?"?\s*[:=]\s*\\?"?\s*(?:bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{8,}`)},
	{"bearer token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{20,}`)},
	{"credential assignment", regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|password|passwd|(?:access|auth|refresh)?[_-]?token|client[_-]?secret)\\?"?\s*[:=]\s*\\?"?[A-Za-z0-9/+_=.-]{20,}`)},
}

// Scan reports the lines of a transcript that look like secrets; file names where
// they are in the report.
func Scan(file string, content []byte) []Finding {
	var found []Finding
	for n, line := range bytes.Split(content, []byte("\n")) {
		for _, p := range secretPatterns {
			if p.re.Match(line) {
				found = append(found, Finding{File: file, Line: n + 1, Kind: p.kind})
				break
			}
		}
	}
	return found
}

// ScanArchive scans every file of a session archive (the tar.gz of a
// harness.SessionPorter).
func ScanArchive(archive []byte) ([]Finding, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("session: the archive is not readable: %w", err)
	}
	defer func() { _ = gz.Close() }()
	var found []Finding
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return found, nil
		}
		if err != nil {
			return nil, fmt.Errorf("session: the archive is not readable: %w", err)
		}
		if h.Typeflag != tar.TypeReg || h.Name == manifestName {
			continue
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("session: the archive is not readable: %w", err)
		}
		found = append(found, Scan(h.Name, content)...)
	}
}

const manifestName = "manifest.json"
