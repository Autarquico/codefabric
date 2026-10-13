package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestCommandsFromOtherPackagesJoinTheGroupOfTheSameName(t *testing.T) {
	daemon := &cobra.Command{Use: "worker"}
	daemon.AddCommand(&cobra.Command{Use: "run", Short: "the daemon's", RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := cmd.OutOrStdout().Write([]byte("running\n"))
		return err
	}})
	var out, errOut bytes.Buffer
	if code := Run([]string{"worker", "run"}, strings.NewReader(""), &out, &errOut, daemon, &cobra.Command{Use: "extra"}); code != 0 || out.String() != "running\n" {
		t.Fatalf("worker run: exit %d: %q %q", code, out.String(), errOut.String())
	}

	root := newRootCmd(Options{})
	addCommands(root, []*cobra.Command{daemon, {Use: "extra"}})
	var names []string
	for _, sub := range commandNamed(t, root, "worker").Commands() {
		names = append(names, sub.Name())
	}
	for _, want := range []string{"list", "show", "drain", "undrain", "remove", "run"} {
		if !strings.Contains(strings.Join(names, " "), want) {
			t.Errorf("`cf worker` lacks %q: %v", want, names)
		}
	}
	commandNamed(t, root, "extra")
}

func commandNamed(t *testing.T, root *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("no command %q", name)
	return nil
}

func TestWorkerCommandsNeedCredentialsAndTheirArguments(t *testing.T) {
	isolatedMachine(t)
	for _, args := range [][]string{{"worker", "list"}, {"worker", "show", "box"}, {"worker", "drain", "box"}, {"worker", "undrain", "box"}, {"worker", "remove", "box"}} {
		code, out, _ := run(t, append(args, "--json")...)
		if env := errorOf(t, out); code != 6 || env["code"] != string(domain.CodeUnauthenticated) {
			t.Errorf("%v: exit %d, %v", args, code, env)
		}
	}
	for _, args := range [][]string{{"worker", "show"}, {"worker", "drain"}, {"worker", "remove", "a", "b"}, {"worker", "list", "extra"}} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want a usage error", args, code)
		}
	}
}

func TestStateOfNamesTheCauseOfADegradedWorker(t *testing.T) {
	for _, tt := range []struct {
		w    v1.Worker
		want string
	}{
		{v1.Worker{State: domain.WorkerOnline, Compatible: true}, "online"},
		{v1.Worker{State: domain.WorkerDegraded, Compatible: false}, "degraded(version)"},
		{v1.Worker{State: domain.WorkerDegraded, Compatible: true}, "degraded"},
	} {
		if got := stateOf(tt.w); got != tt.want {
			t.Errorf("stateOf(%+v) = %q, want %q", tt.w, got, tt.want)
		}
	}
}
