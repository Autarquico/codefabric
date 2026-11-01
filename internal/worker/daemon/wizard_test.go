package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/support/prompt"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
	"github.com/Autarquico/codefabric/internal/worker/setup"
	"github.com/Autarquico/codefabric/internal/worker/sysinfo"
)

// fakeHarness is a harness that is installed once its Install runs.
type fakeHarness struct {
	harness.Adapter
	id         string
	installed  bool
	installErr error
	installs   int
}

func (f *fakeHarness) ID() string { return f.id }
func (f *fakeHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Install: true}
}

func (f *fakeHarness) Detect(context.Context, harness.Host) (harness.Detection, error) {
	return harness.Detection{Installed: f.installed, Path: "/usr/bin/" + f.id}, nil
}

func (f *fakeHarness) Install(_ context.Context, _ harness.Host, o harness.InstallOptions) (harness.InstallPlan, error) {
	plan := harness.InstallPlan{Steps: []harness.InstallStep{{Summary: "Instala " + f.id, Argv: []string{"get", f.id}}}}
	if !o.DryRun {
		return plan, errors.New("the wizard must run the plan it showed, not install again")
	}
	return plan, nil
}

// installHost runs the steps of the fake plans (`get <id>`): it installs the
// harness of that id. Anything else goes to the real host.
type installHost struct {
	harness.Host
	harnesses []*fakeHarness
}

func (h installHost) Run(ctx context.Context, c harness.Cmd) (harness.CmdResult, error) {
	if len(c.Argv) == 2 && c.Argv[0] == "get" {
		for _, f := range h.harnesses {
			if f.id != c.Argv[1] {
				continue
			}
			f.installs++
			if f.installErr != nil {
				return harness.CmdResult{ExitCode: 1, Stderr: []byte(f.installErr.Error())}, f.installErr
			}
			f.installed = true
			return harness.CmdResult{}, nil
		}
	}
	return h.Host.Run(ctx, c)
}

type fakeWorkflows struct {
	list    []v1.WorkflowSummary
	ordered []v1.ApplyWorkflowRequest
	state   string
}

func (f *fakeWorkflows) ListWorkflows(context.Context) ([]v1.WorkflowSummary, error) {
	return f.list, nil
}

func (f *fakeWorkflows) ApplyWorkflow(_ context.Context, r v1.ApplyWorkflowRequest) (v1.ApplyWorkflowResponse, error) {
	f.ordered = append(f.ordered, r)
	return v1.ApplyWorkflowResponse{Apply: &v1.WorkflowApply{ID: "ap1"}}, nil
}

func (f *fakeWorkflows) WorkflowApply(context.Context, string) (v1.WorkflowApply, error) {
	return v1.WorkflowApply{State: f.state}, nil
}

// session is a wizard over a scripted input.
type session struct {
	env      wizardEnv
	claude   *fakeHarness
	codex    *fakeHarness
	joins    int
	loggedIn bool // the "auth:claude" check passes
	// loginAfterChecks, if > 0, makes the check pass from that many checks on.
	loginAfterChecks int
	checks           int
	workflow         *fakeWorkflows
	failDisk         bool
}

func newSession(t *testing.T) *session {
	t.Helper()
	home := t.TempDir()
	s := &session{
		claude: &fakeHarness{id: "claude", installed: true}, codex: &fakeHarness{id: "codex"}, loggedIn: true,
		workflow: &fakeWorkflows{state: v1.ApplyStateApplied},
	}
	paths := config.Paths{ConfigDir: t.TempDir(), DataDir: t.TempDir()}
	s.env = wizardEnv{
		Paths: paths, Hostname: "Mac-Book.local",
		Adapters: []harness.Adapter{s.claude, s.codex},
		NewHost: func() (harness.Host, error) {
			h, err := localhost.New(localhost.Options{Home: home, Env: os.Environ()})
			return installHost{Host: h, harnesses: []*fakeHarness{s.claude, s.codex}}, err
		},
		Sysinfo: func(string) (sysinfo.Info, error) {
			return sysinfo.Info{OS: "linux", Arch: "amd64", CPU: 8, MemoryMB: 16 << 10, DiskMB: 500 << 10}, nil
		},
		Join: func(context.Context, JoinOptions) (client.Connected, error) {
			s.joins++
			return client.Connected{}, nil
		},
		Doctor: func(context.Context, string, []string) ([]v1.ComponentHealth, error) {
			out := []v1.ComponentHealth{{Name: "git", Status: v1.ComponentOK}}
			s.checks++
			if s.claude.installed {
				st := v1.ComponentOK
				if !s.loggedIn && (s.loginAfterChecks == 0 || s.checks < s.loginAfterChecks) {
					st = v1.ComponentFail
				}
				out = append(out, v1.ComponentHealth{Name: "auth:claude", Status: st, Message: "sin login", Action: "ejecuta claude y haz login"})
			}
			if s.failDisk {
				out = append(out, v1.ComponentHealth{Name: "disk", Status: v1.ComponentFail, Message: "disco lleno"})
			}
			return out, nil
		},
		Workflows:     func() (workflowAPI, error) { return s.workflow, nil },
		ApplyAttempts: 2, Sleep: func(time.Duration) {},
	}
	return s
}

func (s *session) run(t *testing.T, input string, choices JoinChoices, resume bool) (string, error) {
	t.Helper()
	var out strings.Builder
	err := runWizard(t.Context(), s.env, prompt.New(strings.NewReader(input), &out), choices, resume)
	return out.String(), err
}

func (s *session) settings(t *testing.T) (doctor.Settings, bool) {
	t.Helper()
	st, err := doctor.LoadSettings(s.env.Paths)
	if err != nil {
		t.Fatal(err)
	}
	return st, st.SetupPending
}

// answers, in order: invitation, name, cpus, memory, disk, offer claude, offer codex.
const defaults = "cf://invite\n\n\n\n\n\n\n"

func TestWizard_HappyPath(t *testing.T) {
	s := newSession(t)
	s.codex.installed = false
	out, err := s.run(t, "cf://invite\n\n4\n8GB\n10%\n\ns\ns\n1\n", JoinChoices{}, false)
	if err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	st, pending := s.settings(t)
	if pending || s.joins != 1 {
		t.Errorf("pending=%v joins=%d\n%s", pending, s.joins, out)
	}
	if st.Name != "mac-book" || st.DiskMinFree != "10%" {
		t.Errorf("settings = %+v", st)
	}
	if s.codex.installs != 1 || !s.codex.installed {
		t.Errorf("codex installs = %d", s.codex.installs)
	}
	for _, want := range []string{"get codex", "Listo: el Worker mac-book"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(s.workflow.ordered) != 0 {
		t.Errorf("an empty list ordered %+v", s.workflow.ordered)
	}
}

func TestWizard_OrdersTheInitialWorkflowOnTheChosenHarnesses(t *testing.T) {
	s := newSession(t)
	s.workflow.list = []v1.WorkflowSummary{{Name: "default", Head: 2}}
	out, err := s.run(t, defaults+"1\n", JoinChoices{}, false)
	if err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	if len(s.workflow.ordered) != 1 || s.workflow.ordered[0].Worker != "mac-book" || s.workflow.ordered[0].Harness != "claude" || s.workflow.ordered[0].Workflow != "default" {
		t.Errorf("orders = %+v", s.workflow.ordered)
	}
	if !strings.Contains(out, "default aplicado a claude") {
		t.Errorf("output:\n%s", out)
	}
}

func TestWizard_DecliningTheInstallDropsTheHarnessFromTheOfferedSet(t *testing.T) {
	s := newSession(t)
	out, err := s.run(t, "cf://invite\n\n\n\n\n\ns\n\n", JoinChoices{}, false) // offer codex, decline its install
	if err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	if s.codex.installs != 0 {
		t.Errorf("installed without consent")
	}
	if !strings.Contains(out, "no se ofrecerá en este Worker") {
		t.Errorf("output:\n%s", out)
	}
	st, pending := s.settings(t)
	if pending || !slices.Equal(st.Harnesses, []string{"claude"}) {
		t.Errorf("pending=%v harnesses=%v:\n%s", pending, st.Harnesses, out)
	}
}

func TestWizard_YesInstallsWithoutAsking(t *testing.T) {
	s := newSession(t)
	out, err := s.run(t, "cf://invite\n\n\n\n\n\ns\n", JoinChoices{Yes: true}, false)
	if err != nil || s.codex.installs != 1 {
		t.Fatalf("err=%v installs=%d\n%s", err, s.codex.installs, out)
	}
}

func TestWizard_AFailedInstallFailsTheDoctorAndKeepsTheSetupPending(t *testing.T) {
	s := newSession(t)
	s.codex.installErr = errors.New("npm: sin red")
	out, err := s.run(t, "cf://invite\n\n\n\n\n\ns\ns\n", JoinChoices{}, false)
	if err == nil || !strings.Contains(out, "La instalación de codex falló") {
		t.Fatalf("a chosen harness that is not installed must fail the doctor: err=%v\n%s", err, out)
	}
	if _, pending := s.settings(t); !pending || !strings.Contains(out, "harness:codex") {
		t.Errorf("pending=%v:\n%s", pending, out)
	}
}

func TestWizard_MissingLoginWaitsForTheUserAndRechecks(t *testing.T) {
	s := newSession(t)
	s.loggedIn, s.loginAfterChecks = false, 2 // the login step finds it missing; the user logs in; Enter checks again
	out, err := s.run(t, defaults+"\n", JoinChoices{}, false)
	if err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	if _, pending := s.settings(t); pending {
		t.Errorf("setup still pending:\n%s", out)
	}
	if !strings.Contains(out, "ejecuta claude y haz login") {
		t.Errorf("the action is not shown:\n%s", out)
	}
}

func TestWizard_SkippingTheLoginKeepsTheSetupPending(t *testing.T) {
	s := newSession(t)
	s.loggedIn = false
	out, err := s.run(t, defaults+"o\n", JoinChoices{}, false)
	if err == nil {
		t.Fatalf("a failing doctor must not finish:\n%s", out)
	}
	if _, pending := s.settings(t); !pending {
		t.Error("the setup was cleared with a failing doctor")
	}
	if !strings.Contains(out, resumeHint) {
		t.Errorf("no way to resume:\n%s", out)
	}
}

func TestWizard_ResumeAsksAgainForASkippedLogin(t *testing.T) {
	s := newSession(t)
	s.loggedIn = false
	if _, err := s.run(t, defaults+"o\n", JoinChoices{}, false); err == nil {
		t.Fatal("a failing doctor must not finish")
	}
	s.loggedIn = true
	out, err := s.run(t, "", JoinChoices{}, true)
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if _, pending := s.settings(t); pending {
		t.Errorf("setup still pending:\n%s", out)
	}
}

func TestWizard_AbortedMidWayResumesWithoutJoiningAgain(t *testing.T) {
	s := newSession(t)
	// The input ends at the first harness question: joined, nothing else.
	out, err := s.run(t, "cf://invite\nw1\n\n\n\n", JoinChoices{}, false)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v\n%s", err, out)
	}
	pr, pending, _ := setup.Load(s.env.Paths)
	if !pending || pr.Step != stepJoined || pr.Name != "w1" {
		t.Fatalf("progress = %+v pending=%v", pr, pending)
	}
	out, err = s.run(t, "\n\n\n", JoinChoices{}, true) // offer claude, offer codex: defaults; no Workflow list
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if s.joins != 1 {
		t.Errorf("joined %d times", s.joins)
	}
	if st, pending := s.settings(t); pending || st.Name != "w1" {
		t.Errorf("settings = %+v pending=%v\n%s", st, pending, out)
	}
}

func TestWizard_ResumeNeedsAPendingJoin(t *testing.T) {
	s := newSession(t)
	if _, err := s.run(t, "", JoinChoices{}, true); err == nil {
		t.Error("resume with nothing pending succeeded")
	}
	if _, err := s.run(t, "cf://invite\nw1\n\n\n\n", JoinChoices{}, false); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if _, err := s.run(t, defaults, JoinChoices{}, false); err == nil || !strings.Contains(err.Error(), "--resume") {
		t.Errorf("a new join over a pending one: %v", err)
	}
}

func TestWizard_FlagsAnswerTheirQuestions(t *testing.T) {
	s := newSession(t)
	choices := JoinChoices{Invitation: "cf://x", Name: "box", DiskMinFree: "5%", Settings: v1.WorkerSettings{CPUMax: 2, MemoryMaxMB: 1024}}
	out, err := s.run(t, "\n\n\n", choices, false) // only the harness questions
	if err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	if st, _ := s.settings(t); st.Name != "box" || st.DiskMinFree != "5%" {
		t.Errorf("settings = %+v", st)
	}
}

func TestWizard_AnInvalidAnswerIsAskedAgain(t *testing.T) {
	s := newSession(t)
	out, err := s.run(t, "cf://invite\n\nmuchas\n4\nlots\n\nabc\n10%\n\n\n\n", JoinChoices{}, false)
	if err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	if strings.Count(out, "indica un número de CPUs") != 1 || !strings.Contains(out, "indica un tamaño") {
		t.Errorf("output:\n%s", out)
	}
}

func TestUseWizard(t *testing.T) {
	tty := wizardContext{Terminal: true}
	for name, tc := range map[string]struct {
		resume bool
		c      wizardContext
		want   bool
	}{
		"terminal asks":           {c: tty, want: true},
		"no terminal":             {c: wizardContext{}, want: false},
		"non-interactive":         {c: wizardContext{Terminal: true, NonInteractive: true}, want: false},
		"json":                    {c: wizardContext{Terminal: true, JSON: true}, want: false},
		"every flag given":        {c: wizardContext{Terminal: true, Complete: true}, want: false},
		"resume without terminal": {resume: true, c: wizardContext{}, want: true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := useWizard(tc.resume, tc.c); got != tc.want {
				t.Errorf("got %v", got)
			}
		})
	}
}
