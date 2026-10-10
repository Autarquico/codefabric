package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

// oneJSONValue decodes stdout, which must hold exactly one JSON value.
func oneJSONValue(t *testing.T, stdout string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("stdout is not JSON: %q: %v", stdout, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("stdout holds more than one JSON value: %q", stdout)
	}
	return v
}

func isolatedMachine(t *testing.T) {
	t.Helper()
	t.Setenv("CF_CONFIG_DIR", t.TempDir())
	t.Setenv("CF_DATA_DIR", t.TempDir())
}

func TestApiSdkCliStatus_UsageErrorsAreInvalidRequestWithExit2(t *testing.T) {
	cases := map[string][]string{
		"unknown flag":         {"status", "--bogus"},
		"unknown flag on root": {"--bogus"},
		"too many arguments":   {"status", "extra"},
		"missing argument":     {"client", "remove"},
		"unknown command":      {"nope"},
		"unknown subcommand":   {"client", "nope"},
		"bad flag value":       {"version", "--json=maybe"},
	}
	for name, args := range cases {
		for _, mode := range []string{"human", "json before", "json after"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				isolatedMachine(t)
				full := args
				switch mode {
				case "json before":
					full = append([]string{"--json"}, args...)
				case "json after":
					full = append(append([]string{}, args...), "--json")
				}
				code, stdout, stderr := run(t, full...)
				if code != 2 {
					t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
				}
				if mode == "human" {
					if stdout != "" || !strings.Contains(stderr, "CF_INVALID_REQUEST") {
						t.Errorf("stdout %q, stderr %q", stdout, stderr)
					}
					return
				}
				if name == "bad flag value" {
					return // --json itself is the broken flag: no JSON mode to honour
				}
				env := oneJSONValue(t, stdout)["error"].(map[string]any)
				if env["code"] != string(domain.CodeInvalidRequest) {
					t.Errorf("envelope %v", env)
				}
				if stderr != "" {
					t.Errorf("stderr %q", stderr)
				}
			})
		}
	}
}

func TestApiSdkCliStatus_JSONCommandsWriteExactlyOneValueToStdout(t *testing.T) {
	isolatedMachine(t)
	for _, args := range [][]string{
		{"version", "--json"},
		{"--json", "config", "paths"},
		{"config", "show", "--json"},
	} {
		code, stdout, stderr := run(t, args...)
		if code != 0 || stderr != "" {
			t.Fatalf("cf %v: exit %d, stderr %q", args, code, stderr)
		}
		oneJSONValue(t, stdout)
	}
}

func TestApiSdkCliStatus_ConfigShowJSONSaysWhetherThisMachineIsConnected(t *testing.T) {
	isolatedMachine(t)
	_, stdout, _ := run(t, "config", "show", "--json")
	client := oneJSONValue(t, stdout)["client"].(map[string]any)
	if client["connected"] != false {
		t.Errorf("client %v", client)
	}
}

func TestApiSdkCliStatus_AFailureWithJSONIsTheEnvelopeOnStdout(t *testing.T) {
	isolatedMachine(t)
	code, stdout, stderr := run(t, "status", "--json")
	if code != 6 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	var env v1.ErrorEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != domain.CodeUnauthenticated || !strings.Contains(env.Error.Message, "cf connect") {
		t.Errorf("envelope %+v", env)
	}
}

func TestApiSdkCliStatus_AnUncodedFailureWithJSONIsUnclassified(t *testing.T) {
	isolatedMachine(t)
	code, stdout, _ := run(t, "connect", "--json") // no invitation on stdin
	env := oneJSONValue(t, stdout)["error"].(map[string]any)
	if code != 1 || env["code"] != string(domain.CodeInternal) {
		t.Errorf("exit %d, envelope %v", code, env)
	}
}

func TestApiSdkCliStatus_DebugShowsDetailsAndTheDefaultDoesNot(t *testing.T) {
	err := domain.NewError(domain.CodeInternal, "internal error", map[string]any{"request_id": "req-1", "cause": "x"})
	var plain, debug bytes.Buffer
	report(io.Discard, &plain, err, false, false)
	report(io.Discard, &debug, err, false, true)
	if got := strings.TrimSpace(plain.String()); got != "error: CF_INTERNAL: internal error (request req-1)" {
		t.Errorf("default: %q", got)
	}
	if !strings.Contains(debug.String(), "cause: x") || !strings.Contains(debug.String(), "request_id: req-1") {
		t.Errorf("debug: %q", debug.String())
	}
}
