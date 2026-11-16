//go:build integration && (linux || darwin)

package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The backend of the live view of the TUI: the screen of a Run as a read-only
// stream through the real Controller and the real relay of the Worker, with real
// tmux. There is no TUI here.

// screenRead is what a goroutine got from the stream.
type screenRead struct {
	screen []byte
	err    error
}

// watchScreen reads the stream in the background: a test waits on what it says.
func watchScreen(s *client.ScreenStream) <-chan screenRead {
	out := make(chan screenRead, 64)
	go func() {
		defer close(out)
		for {
			b, err := s.Next()
			out <- screenRead{b, err}
			if err != nil {
				return
			}
		}
	}()
	return out
}

// awaitScreen reads until a screen has text in it, and fails after within.
func awaitScreen(t *testing.T, in <-chan screenRead, text string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case r, open := <-in:
			if !open || r.err != nil {
				t.Fatalf("waiting for %q: the stream ended: %v", text, r.err)
			}
			if strings.Contains(string(r.screen), text) {
				return
			}
		case <-deadline:
			t.Fatalf("no screen with %q in %s", text, within)
		}
	}
}

func openScreen(t *testing.T, c *client.Client, run string, hz int) *client.ScreenStream {
	t.Helper()
	s, err := c.OpenScreen(t.Context(), run, "", hz)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func typeInto(t *testing.T, o *observeBox, session, text string) {
	t.Helper()
	if _, err := o.workerTmux("send-keys", "-t", "="+session+":", text, "Enter"); err != nil {
		t.Fatal(err)
	}
}

// TestScenario_LiveAttach_PantallaEnDirecto: what the Run writes is in a screen
// in under a second at 4 Hz, with its colours; an idle Run sends nothing more;
// and when the Run ends the stream ends with CF_RUN_NOT_ATTACHABLE.
func TestScenario_LiveAttach_PantallaEnDirecto(t *testing.T) {
	o := newObserveBox(t, chatScript)
	c, err := client.Open(o.client.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	s := openScreen(t, c, o.run.ID, 4)
	in := watchScreen(s)
	awaitScreen(t, in, "hello-from-run", 5*time.Second)

	// Idle: the screen did not change, so nothing is sent.
	select {
	case r := <-in:
		t.Fatalf("a screen that did not change was sent: %q, %v", r.screen, r.err)
	case <-time.After(1200 * time.Millisecond):
	}

	began := time.Now()
	typeInto(t, o, o.session, "marker-one")
	awaitScreen(t, in, "marker-one", time.Second)
	if took := time.Since(began); took > time.Second {
		t.Errorf("the text took %s to show", took)
	}

	// The Run ends: the stream says so.
	o.finishChat()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case r := <-in:
			if r.err == nil {
				continue
			}
			var coded *domain.Error
			if !errors.As(r.err, &coded) || coded.Code != domain.CodeRunNotAttachable {
				t.Fatalf("the stream ended with %v, want CF_RUN_NOT_ATTACHABLE", r.err)
			}
			return
		case <-deadline:
			t.Fatal("the stream did not end with the Run")
		}
	}
}

// TestScenario_LiveAttach_VariosPantallasAlaVez: three Runs, three streams at
// once, each shows its own Run (the backend of the grid).
func TestScenario_LiveAttach_VariosPantallasAlaVez(t *testing.T) {
	o := newObserveBox(t, chatScript)
	runs := []v1.Run{o.run}
	for _, name := range []string{"second", "third"} {
		created := ok[v1.CreateTaskResponse](t, "do", chatScript, "--harness", "command", "-i", "--name", name)
		o.waitRunning(created.Run.ID)
		runs = append(runs, *created.Run)
	}
	use(t, o.client)
	c, err := client.Open(o.client.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	var feeds []<-chan screenRead
	for _, r := range runs {
		feeds = append(feeds, watchScreen(openScreen(t, c, r.ID, 4)))
	}
	for i, r := range runs {
		awaitScreen(t, feeds[i], "hello-from-run", 10*time.Second)
		typeInto(t, o, domain.SessionName(r.ID, 1), "only-in-"+r.ID[len(r.ID)-6:])
	}
	for i, r := range runs {
		mark := "only-in-" + r.ID[len(r.ID)-6:]
		awaitScreen(t, feeds[i], mark, 2*time.Second)
	}
	for _, r := range runs {
		o.release(r.ID)
		typeInto(t, o, domain.SessionName(r.ID, 1), "quit")
	}
}

// TestScenario_LiveAttach_PantallaNoEsParaUnWorker: a Worker identity cannot open
// the screen of a Run, as it cannot open any attach.
func TestScenario_LiveAttach_PantallaNoEsParaUnWorker(t *testing.T) {
	o := newObserveBox(t, chatScript)
	w, err := client.Open(o.box.WorkerCredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.OpenScreen(t.Context(), o.run.ID, "", 2)
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Code != domain.CodePolicyDenied {
		t.Fatalf("a Worker opening a screen: %v, want CF_POLICY_DENIED", err)
	}
	o.finishChat()
}
