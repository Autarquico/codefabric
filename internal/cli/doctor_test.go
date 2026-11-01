package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

type fakeDoctor struct {
	status  v1.Status
	err     error
	workers []v1.Worker
}

func (f *fakeDoctor) Controller() string                        { return "ctl.lan:7443" }
func (f *fakeDoctor) Status(context.Context) (v1.Status, error) { return f.status, f.err }
func (f *fakeDoctor) Workers(context.Context, bool) ([]v1.Worker, error) {
	return f.workers, nil
}

func runDoctor(t *testing.T, f *fakeDoctor, probe sshProbe, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	opts := Options{doctorStore: func(*cobra.Command) (doctorStore, error) { return f, nil }, sshProbe: probe}
	var out, errOut bytes.Buffer
	code = RunContextWith(context.Background(), opts, append([]string{"doctor"}, args...), strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func healthyController() v1.Status {
	return v1.Status{Version: "v0.9.0", Database: v1.DatabaseStatus{OK: true}}
}

func TestDoctor_WorkersWithComponentsAndSSH(t *testing.T) {
	f := &fakeDoctor{status: healthyController(), workers: []v1.Worker{
		{Name: "alpha", State: domain.WorkerOnline, SSHTarget: "me@alpha.lan", Health: v1.Health{Status: v1.HealthOK, Components: []v1.ComponentHealth{
			{Name: "disk", Status: v1.ComponentOK, Message: "40% libre"},
		}}},
		{Name: "beta", State: domain.WorkerDegraded, SSHTarget: "beta.lan:2222", Health: v1.Health{Status: v1.HealthDegraded, Components: []v1.ComponentHealth{
			{Name: "auth:claude-code", Status: v1.ComponentFail, Message: "sin login", Action: "claude login en beta"},
		}}},
		{Name: "gamma", State: domain.WorkerOnline},
	}}
	var probed []string
	probe := func(_ context.Context, target domain.SSHTarget) error {
		probed = append(probed, target.String())
		if target.Host == "beta.lan" {
			return errors.New("Permission denied (publickey)")
		}
		return nil
	}
	code, out, errOut := runDoctor(t, f, probe)
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{
		"Controller  ok  ctl.lan:7443  v0.9.0",
		"Worker alpha  online", "40% libre",
		"Worker beta  degraded", "auth:claude-code", "claude login en beta",
		"aviso: attach no funcionará en beta: Permission denied (publickey)",
		"Worker gamma  online", "sin componentes",
		"aviso: attach no funcionará en gamma: el Worker no anuncia un SSHTarget",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "en alpha") {
		t.Errorf("alpha reachable but warned:\n%s", out)
	}
	if len(probed) != 2 {
		t.Errorf("probed %v: the Worker without a target is not probed", probed)
	}
}

func TestDoctor_JSON(t *testing.T) {
	f := &fakeDoctor{status: healthyController(), workers: []v1.Worker{{Name: "gamma", State: domain.WorkerOnline}}}
	code, out, _ := runDoctor(t, f, func(context.Context, domain.SSHTarget) error { return nil }, "--json")
	var rep DoctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 0 {
		t.Fatalf("exit %d, %v: %s", code, err, out)
	}
	if !rep.Controller.OK || rep.Controller.Version != "v0.9.0" || len(rep.Workers) != 1 ||
		rep.Workers[0].Components == nil || len(rep.Workers[0].Warnings) != 1 {
		t.Errorf("report = %+v", rep)
	}
}

func TestDoctor_AControllerThatDoesNotAnswerFails(t *testing.T) {
	f := &fakeDoctor{err: domain.NewError(domain.CodeInternal, "connection refused", nil)}
	code, out, errOut := runDoctor(t, f, func(context.Context, domain.SSHTarget) error {
		t.Error("probed without a Controller")
		return nil
	})
	if code == 0 || out != "" || !strings.Contains(errOut, "connection refused") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestDoctor_AFailingWorkerOnlyWarns(t *testing.T) {
	f := &fakeDoctor{status: healthyController(), workers: []v1.Worker{
		{Name: "beta", State: domain.WorkerDegraded, SSHTarget: "beta.lan", Health: v1.Health{Components: []v1.ComponentHealth{{Name: "disk", Status: v1.ComponentFail}}}},
	}}
	if code, _, _ := runDoctor(t, f, func(context.Context, domain.SSHTarget) error { return errors.New("timeout") }); code != 0 {
		t.Errorf("exit %d: warnings do not fail", code)
	}
}

func TestDoctor_ProbesTheSSHTargetOverrideOfAttach(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CF_CONFIG_DIR", dir)
	t.Setenv("CF_DATA_DIR", t.TempDir())
	paths, err := config.ResolveFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.ClientConfigFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ClientConfigFile(), []byte(`{"attach":{"ssh_targets":{"alpha":"root@10.0.0.5:2222","gamma":"gamma.vpn"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeDoctor{status: healthyController(), workers: []v1.Worker{
		{Name: "alpha", State: domain.WorkerOnline, SSHTarget: "me@alpha.lan"},
		{Name: "beta", State: domain.WorkerOnline, SSHTarget: "beta.lan"},
		{Name: "gamma", State: domain.WorkerOnline}, // announces nothing; the override is its target
	}}
	var mu sync.Mutex
	var probed []string
	probe := func(_ context.Context, target domain.SSHTarget) error {
		mu.Lock()
		defer mu.Unlock()
		probed = append(probed, target.Host)
		return nil
	}
	if code, out, errOut := runDoctor(t, f, probe); code != 0 || strings.Contains(out, "warning") {
		t.Fatalf("exit %d, stderr %q:\n%s", code, errOut, out)
	}
	slices.Sort(probed)
	if want := []string{"10.0.0.5", "beta.lan", "gamma.vpn"}; !slices.Equal(probed, want) {
		t.Errorf("probed %v, want %v", probed, want)
	}
}
