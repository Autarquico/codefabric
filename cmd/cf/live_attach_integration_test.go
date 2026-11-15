//go:build integration && (linux || darwin)

package main

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/domain"
)

// TestScenario_LiveAttach_SinRelayEnElWorker: a Worker that does not announce
// attach-relay:v1 offers ssh and nothing else. Asking for the relay is refused
// with CF_RUN_NOT_ATTACHABLE (relay_unsupported) before anything is run, and
// ssh is not a silent fallback; with --ssh the session opens as it always did.
func TestScenario_LiveAttach_SinRelayEnElWorker(t *testing.T) {
	o := newObserveBox(t, holdScript)
	use(t, o.client)

	// cf ps says how the Run can be attached.
	listed := ok[v1.PsResponse](t, "ps")
	if len(listed.Runs) != 1 || !listed.Runs[0].Attachable || !slices.Equal(listed.Runs[0].Transports, []string{v1.AttachTransportSSH}) {
		t.Fatalf("ps = %+v", listed)
	}
	if code, out, _ := cf(t, "", "ps"); code != 0 || !strings.Contains(out, "TRANSPORT") {
		t.Fatalf("ps table: exit %d\n%s", code, out)
	}

	// The relay is refused, and ssh is not started in its place.
	t.Setenv("PATH", o.fakeBin+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_SSH_LOG", o.sshLog)
	term := &commandTerminal{}
	_, err := cli.PrepareAttach(t.Context(), term, cli.AttachParams{Ref: "auth-race", Transport: cli.TransportRelay})
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Code != domain.CodeRunNotAttachable || coded.Details["reason"] != v1.NotAttachableRelayUnsupported ||
		!strings.Contains(coded.Message, "--ssh") {
		t.Fatalf("relay without the capability: %v", err)
	}
	if term.calls.Load() != 0 || readFile(o.sshLog) != "" {
		t.Fatalf("the terminal was reached %d times, ssh log %q", term.calls.Load(), readFile(o.sshLog))
	}

	// With --ssh the session opens as today.
	terminal := o.attach(nil, "--ssh")
	eventually(t, "the attach client to be on the session of the Worker", func() bool { return o.clients() == 1 })
	if !strings.Contains(readFile(o.sshLog), "tester@fake-worker") {
		t.Fatalf("ssh was not run:\n%s", readFile(o.sshLog))
	}
	terminal.kill()
	eventually(t, "the client to leave the session", func() bool { return o.clients() == 0 })
	if _, err := o.workerTmux("send-keys", "-t", "="+o.session+":", "go on", "Enter"); err != nil { // the harness waits for a line
		t.Fatal(err)
	}
	o.release(o.run.ID)
	o.waitPublished(o.run.ID)
}
