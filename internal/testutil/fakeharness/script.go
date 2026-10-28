package fakeharness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// MaxWait is the longest timeout a wait_file or read_line step may have.
const MaxWait = 5 * time.Minute

// Script is what the program does: its steps, in order.
type Script struct {
	Steps []Step `json:"steps"`
}

// Step is one thing the program does. Exactly one field is set.
type Step struct {
	Emit          *Line     `json:"emit,omitempty"`
	EmitEnv       *EmitEnv  `json:"emit_env,omitempty"`
	Record        *Recorder `json:"record,omitempty"`
	WaitFile      *WaitFile `json:"wait_file,omitempty"`
	ReadLine      *ReadLine `json:"read_line,omitempty"`
	IgnoreSignals bool      `json:"ignore_signals,omitempty"`
	// Exit ends the program with its code, 0 to 255.
	Exit *int `json:"exit,omitempty"`
}

// Line is one line of the JSONL the program emits and the Adapter parses.
// Type is a harness.EventKind; the field of its type is set.
type Line struct {
	Type      harness.EventKind `json:"type"`
	At        *time.Time        `json:"at,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	Model     string            `json:"model,omitempty"`
	// Usage is set on a usage line.
	Usage *Usage `json:"usage,omitempty"`
	// Result is set on a result line.
	Result *Result `json:"result,omitempty"`
	// Message is set on an error or input_required line.
	Message string `json:"message,omitempty"`
}

// Usage is the usage of a usage line. A field left out is unknown; one that
// is 0 is a known zero.
type Usage struct {
	Provider         string `json:"provider,omitempty"`
	Model            string `json:"model,omitempty"`
	InputTokens      *int64 `json:"input_tokens,omitempty"`
	OutputTokens     *int64 `json:"output_tokens,omitempty"`
	CacheReadTokens  *int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
	RequestCount     *int64 `json:"request_count,omitempty"`
	LatencyMS        *int64 `json:"latency_ms,omitempty"`
}

// Result is the result of a result line.
type Result struct {
	Failed  bool   `json:"failed,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// EmitEnv is an emit_env step: it writes the value of the variable Name to
// stdout, and then a newline. With SplitAt it writes the first SplitAt bytes
// (or all, if the value is shorter), waits PauseMS and writes the rest, so that
// a reader gets the value in two pieces: what a secret printed in two goes
// looks like to the redaction of the output.
type EmitEnv struct {
	Name    string `json:"name"`
	SplitAt int    `json:"split_at,omitempty"`
	PauseMS int64  `json:"pause_ms,omitempty"`
}

// Recorder is a record step: where to write the [Record] and which
// variables of the environment go in it.
type Recorder struct {
	// Path is where the Record is written, atomically; relative to the
	// working directory.
	Path string `json:"path"`
	// Env are the names of the variables to keep; a name that ends in '*'
	// keeps every variable with that prefix. Empty is [DefaultRecordEnv].
	Env []string `json:"env,omitempty"`
}

// DefaultRecordEnv is the environment a record step keeps when it names
// none: what tells how the program was launched, without the rest of the
// host.
var DefaultRecordEnv = []string{"FAKEHARNESS_*", "CF_*", "HOME", "TMUX", "TMUX_PANE"}

// WaitFile is a wait_file step.
type WaitFile struct {
	Path      string `json:"path"`
	TimeoutMS int64  `json:"timeout_ms"`
}

// ReadLine is a read_line step.
type ReadLine struct {
	TimeoutMS int64 `json:"timeout_ms"`
	// To is a file the line is written to, without its newline; empty
	// drops it.
	To string `json:"to,omitempty"`
}

// Record is what a record step writes.
type Record struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
	Dir  string            `json:"dir"`
	// ConfigDigest is the treedigest/v1 of FAKEHARNESS_CONFIG_DIR, with
	// credentials.json left out as the auth link; empty when it is unset.
	ConfigDigest string `json:"config_digest,omitempty"`
	// AuthLinked says that the auth in FAKEHARNESS_CONFIG_DIR was a link.
	AuthLinked bool `json:"auth_linked,omitempty"`
}

// ReadRecord reads the Record a record step wrote at path.
func ReadRecord(path string) (Record, error) {
	b, err := os.ReadFile(path) //nolint:gosec // a path the test chose
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, fmt.Errorf("fakeharness: record %s: %w", path, err)
	}
	return r, nil
}

// ParseScript reads a Script and checks every step.
func ParseScript(b []byte) (Script, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s Script
	if err := dec.Decode(&s); err != nil {
		return Script{}, fmt.Errorf("fakeharness: script: %w", err)
	}
	for i, st := range s.Steps {
		if err := st.check(); err != nil {
			return Script{}, fmt.Errorf("fakeharness: script: step %d: %w", i, err)
		}
	}
	return s, nil
}

func (s Step) check() error {
	set := 0
	for _, isSet := range []bool{s.Emit != nil, s.EmitEnv != nil, s.Record != nil, s.WaitFile != nil, s.ReadLine != nil, s.IgnoreSignals, s.Exit != nil} {
		if isSet {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("has %d actions, wants exactly one", set)
	}
	switch {
	case s.Emit != nil:
		return s.Emit.check()
	case s.EmitEnv != nil:
		return s.EmitEnv.check()
	case s.Record != nil:
		if s.Record.Path == "" {
			return errors.New("record: no path")
		}
	case s.WaitFile != nil:
		if s.WaitFile.Path == "" {
			return errors.New("wait_file: no path")
		}
		return checkTimeout("wait_file", s.WaitFile.TimeoutMS)
	case s.ReadLine != nil:
		return checkTimeout("read_line", s.ReadLine.TimeoutMS)
	case s.Exit != nil:
		if *s.Exit < 0 || *s.Exit > 255 {
			return fmt.Errorf("exit: code %d is not 0 to 255", *s.Exit)
		}
	}
	return nil
}

func (e EmitEnv) check() error {
	switch {
	case e.Name == "":
		return errors.New("emit_env: no name")
	case e.SplitAt < 0:
		return fmt.Errorf("emit_env: split_at %d is negative", e.SplitAt)
	case e.PauseMS < 0 || time.Duration(e.PauseMS)*time.Millisecond > MaxWait:
		return fmt.Errorf("emit_env: pause_ms %d is not in [0, %d]", e.PauseMS, MaxWait.Milliseconds())
	}
	return nil
}

func checkTimeout(step string, ms int64) error {
	if ms <= 0 || time.Duration(ms)*time.Millisecond > MaxWait {
		return fmt.Errorf("%s: timeout_ms %d is not in (0, %d]", step, ms, MaxWait.Milliseconds())
	}
	return nil
}

func (l Line) check() error {
	switch l.Type {
	case harness.EventUsage:
		if l.Usage == nil {
			return errors.New("emit: usage line without usage")
		}
	case harness.EventResult:
		if l.Result == nil {
			return errors.New("emit: result line without result")
		}
	case harness.EventSessionStarted, harness.EventInputRequired, harness.EventError:
	default:
		return fmt.Errorf("emit: unknown type %q", l.Type)
	}
	return nil
}

// event is the harness.Event of a line of output, and false for a line that
// is not one this program emits.
func event(b []byte) (harness.Event, bool) {
	var l Line
	if json.Unmarshal(b, &l) != nil || l.check() != nil {
		return harness.Event{}, false
	}
	ev := harness.Event{Kind: l.Type, SessionID: l.SessionID, Model: l.Model}
	if l.At != nil {
		ev.At = *l.At
	}
	switch l.Type {
	case harness.EventUsage:
		u := l.Usage
		model := u.Model
		if model == "" {
			model = l.Model
		}
		ev.Usage = &harness.UsageEvent{
			At: ev.At, Provider: u.Provider, Model: model,
			InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
			RequestCount: u.RequestCount, LatencyMS: u.LatencyMS,
		}
	case harness.EventResult:
		ev.Result = &harness.Result{Failed: l.Result.Failed, Summary: l.Result.Summary}
	case harness.EventError, harness.EventInputRequired:
		ev.Message = l.Message
	}
	return ev, true
}
