package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/cli/actions"
)

// The colon mode of the palette runs the CLI command it is given: the same
// requests reach the Controller and the exit code is the same as typing it.
func TestScenario_PaletteSearchNotify_ModoDosPuntos(t *testing.T) {
	host, _, _ := initController(t)
	use(t, host)
	fake := newFakeController(t, host)
	opts := cli.Options{Terminal: &spyTerminal{}}
	runner := actions.Runner{Options: opts}

	viaLine := func(text string) int {
		line, failure := actions.ParseLine(text)
		if failure != nil {
			t.Fatalf("%q was not accepted: %v", text, failure)
		}
		if res := runner.RunLine(t.Context(), line, fake.addr); res.Failure != nil {
			return res.Failure.ExitCode
		}
		return 0
	}
	viaCLI := func(args ...string) int {
		var out, errOut bytes.Buffer
		return cli.RunContextWith(t.Context(), opts, append(args, "--json", "--controller="+fake.addr), strings.NewReader(""), &out, &errOut)
	}

	for _, c := range []struct {
		line string
		cli  []string
	}{
		{"task cancel x", []string{"task", "cancel", "x"}},
		{"task cancel x --project backend", []string{"task", "cancel", "x", "--project=backend"}},
		{`worker drain "my worker"`, []string{"worker", "drain", "my worker"}},
		{"task cancel", []string{"task", "cancel"}}, // a usage error is the CLI's too
	} {
		gotExit := viaLine(c.line)
		gotSent := fake.take()
		wantExit := viaCLI(c.cli...)
		wantSent := fake.take()
		if gotExit != wantExit || !slices.Equal(gotSent, wantSent) {
			t.Errorf(":%s exit %d sent %v; cf %s exit %d sent %v", c.line, gotExit, gotSent, strings.Join(c.cli, " "), wantExit, wantSent)
		}
		if c.line != "task cancel" && len(gotSent) == 0 {
			t.Errorf(":%s never reached the Controller", c.line)
		}
	}
}

// Each line runs in a fresh command tree: a flag given to one is not remembered by the next.
func TestPalette_ColonLinesShareNoFlagState(t *testing.T) {
	host, _, _ := initController(t)
	use(t, host)
	fake := newFakeController(t, host)
	runner := actions.Runner{Options: cli.Options{Terminal: &spyTerminal{}}}

	run := func(text string) []recordedRequest {
		line, failure := actions.ParseLine(text)
		if failure != nil {
			t.Fatal(failure)
		}
		if res := runner.RunLine(t.Context(), line, fake.addr); res.Failure != nil {
			t.Fatalf("%q: %v", text, res.Failure)
		}
		return fake.take()
	}
	withFlag := run("task cancel x --project backend")
	plain := run("task cancel y")
	alone := run("task cancel y")

	if len(withFlag) == 0 || len(plain) == 0 {
		t.Fatalf("a line never reached the Controller: %v, %v", withFlag, plain)
	}
	if !strings.Contains(withFlag[0].URI+withFlag[0].Body, "backend") {
		t.Fatalf("the flag never reached the request: %v", withFlag)
	}
	if strings.Contains(plain[0].URI+plain[0].Body, "backend") {
		t.Errorf("--project leaked into the next line: %v", plain)
	}
	if !slices.Equal(plain, alone) {
		t.Errorf("the same line sent %v then %v", plain, alone)
	}
}

func TestPalette_ColonAttachTakesOnlyItsOwnFlags(t *testing.T) {
	runner := actions.Runner{Options: cli.Options{Terminal: &spyTerminal{}}}
	for _, text := range []string{"attach", "attach r1 r2", "attach r1 --json", "attach r1 --debug"} {
		line, failure := actions.ParseLine(text)
		if failure != nil {
			t.Fatalf("%q: %v", text, failure)
		}
		if _, failure := runner.AttachLine(t.Context(), line, ""); failure == nil || failure.ExitCode != 2 {
			t.Errorf("%q: failure = %v, want the usage exit 2", text, failure)
		}
	}
}

func TestPalette_ColonRejectsCommandsTheRegistryExcludes(t *testing.T) {
	for _, text := range []string{
		"controller run", "controller init", "setup", "install-service", "worker run", "worker join x",
		"connect", "client remove x", "task list", "status", "doctor", "do fix it", "bogus", "",
	} {
		if _, failure := actions.ParseLine(text); failure == nil {
			t.Errorf("%q was accepted", text)
		} else if failure.ExitCode != 2 {
			t.Errorf("%q: exit %d, want the usage exit 2", text, failure.ExitCode)
		}
	}
	line, failure := actions.ParseLine("attach r1 --read-only")
	if failure != nil || !line.Action.Terminal {
		t.Errorf("attach = %+v, %v: want the terminal action", line, failure)
	}
}
