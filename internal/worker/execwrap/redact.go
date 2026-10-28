package execwrap

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

// CaptureAckFile is where, in a run directory, the capture filter of an
// interactive Run acknowledges that it redacts (Spec.CaptureAck of the
// Runtime); the gate opens only then, and the wrapper checks it again.
const CaptureAckFile = "capture.ack"

// CaptureDisabledNote is what the capture filter writes to the log in place of
// the output when it cannot redact it.
const CaptureDisabledNote = "[cf: output capture disabled: secrets could not be loaded]\n"

// LoadSecrets is every value the output of the Run must be redacted of: the
// retention of its delivery (secretjit.Load), not the harness's access to it.
type LoadSecrets func() ([]redact.Needle, error)

// redaction is the matcher of the secrets load returns, with the escaped forms
// of each (redact.NewEscapedSet). It fails rather than leave one out: no
// loader, a value it cannot load, or one too short or too many to redact.
func redaction(load LoadSecrets) (*redact.Set, error) {
	if load == nil {
		return nil, errors.New("this Worker keeps no secrets")
	}
	needles, err := load()
	if err != nil {
		return nil, err
	}
	return redact.NewEscapedSet(needles)
}

// PipeOptions parameterise RedactPipe.
type PipeOptions struct {
	RunDir    string
	Retention LoadSecrets
	In        io.Reader // the output of the terminal
	Out       io.Writer // the log
}

// RedactPipe is the capture filter of an interactive Run with secrets (`cf
// worker redact-pipe`): it loads the secrets of the Run once, acknowledges
// in CaptureAckFile, and copies the output of the terminal to the log with
// every secret redacted, until the terminal closes.
//
// It fails closed. If it cannot redact it writes CaptureDisabledNote to the
// log, acknowledges a refusal, which keeps the gate shut and the harness from
// running, and discards the output so that the terminal never blocks.
func RedactPipe(o PipeOptions) error {
	ack := filepath.Join(o.RunDir, CaptureAckFile)
	set, err := redaction(o.Retention)
	if err == nil {
		err = cfruntime.AckCapture(ack, nil)
	} else {
		err = fmt.Errorf("%s: the output cannot be redacted: %w", domain.CodeSecretUnavailable, err)
		err = errors.Join(err, cfruntime.AckCapture(ack, err))
	}
	if err != nil {
		_, _ = io.WriteString(o.Out, CaptureDisabledNote)
		_, _ = io.Copy(io.Discard, o.In)
		return err
	}
	w := redact.NewWriter(o.Out, set)
	_, err = io.Copy(w, o.In)
	return errors.Join(err, w.Close())
}
