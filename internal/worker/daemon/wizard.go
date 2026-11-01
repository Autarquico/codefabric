package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/envutil"
	"github.com/Autarquico/codefabric/internal/support/prompt"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
	"github.com/Autarquico/codefabric/internal/worker/setup"
	"github.com/Autarquico/codefabric/internal/worker/sysinfo"
)

// What Progress.Step means: the last step of the wizard that finished. The
// questions that precede the join are not steps: the join needs their answers,
// and after it the invitation is spent, so a resume starts after it.
const (
	stepJoined    = iota + 1 // joined; the setup is pending
	stepHarnesses            // harnesses chosen and installed
	stepWorkflow             // initial Workflow chosen and ordered
)

const (
	defaultApplyAttempts = 20
	applyPollEvery       = 500 * time.Millisecond
	resumeHint           = "cf worker join --resume"
)

// workflowAPI is the part of *client.Client the initial Workflow needs.
type workflowAPI interface {
	ListWorkflows(ctx context.Context) ([]v1.WorkflowSummary, error)
	ApplyWorkflow(ctx context.Context, req v1.ApplyWorkflowRequest) (v1.ApplyWorkflowResponse, error)
	WorkflowApply(ctx context.Context, id string) (v1.WorkflowApply, error)
}

// wizardEnv is what the wizard asks of the machine; tests replace it.
type wizardEnv struct {
	Paths    config.Paths
	Hostname string
	Adapters []harness.Adapter
	// NewHost is called each time the harnesses are detected: after an install
	// the PATH of a host built before it may not have the new binary.
	NewHost func() (harness.Host, error)
	Sysinfo func(dir string) (sysinfo.Info, error)
	Join    func(ctx context.Context, o JoinOptions) (client.Connected, error)
	// Doctor runs the checks of the machine against diskMinFree, for the
	// harnesses the person chose, without the "setup" component: the wizard is
	// what clears it. A harness left out is not the wizard's to judge.
	Doctor func(ctx context.Context, diskMinFree string, harnesses []string) ([]v1.ComponentHealth, error)
	// Workflows returns the Controller client that lists and orders Workflows,
	// or nil if this machine has no client credentials.
	Workflows     func() (workflowAPI, error)
	ApplyAttempts int
	Sleep         func(time.Duration)
}

// JoinChoices are the answers the flags already gave: the wizard does not ask them.
type JoinChoices struct {
	Invitation  string
	Name        string
	DiskMinFree string
	Settings    v1.WorkerSettings
	Force       bool
	// Yes accepts every installation without asking.
	Yes bool
}

func defaultWizardEnv(paths config.Paths, controller string) wizardEnv {
	hostname, _ := os.Hostname()
	return wizardEnv{
		Paths: paths, Hostname: hostname,
		Adapters: []harness.Adapter{claude.Adapter{}, codex.Adapter{}},
		NewHost: func() (harness.Host, error) {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			return localhost.New(localhost.Options{Home: home, Env: envutil.WithoutTmux(os.Environ())})
		},
		Sysinfo: sysinfo.Collect,
		Join:    Join,
		Doctor: func(ctx context.Context, minFree string, chosen []string) ([]v1.ComponentHealth, error) {
			adapters := []harness.Adapter{command.Adapter{}}
			for _, a := range []harness.Adapter{codex.Adapter{}, claude.Adapter{}} {
				if slices.Contains(chosen, a.ID()) {
					adapters = append(adapters, a)
				}
			}
			return RunDoctor(ctx, DoctorOptions{Paths: paths, DiskMinFree: minFree, Controller: controller, Doctor: doctor.Options{Adapters: adapters}})
		},
		Workflows: func() (workflowAPI, error) {
			exists, err := client.CredentialsExist(paths.CredentialsDir())
			if err != nil || !exists {
				return nil, err
			}
			var opts []client.Option
			if controller != "" {
				opts = append(opts, client.WithController(controller))
			}
			return client.Open(paths.CredentialsDir(), opts...)
		},
		ApplyAttempts: defaultApplyAttempts, Sleep: time.Sleep,
	}
}

type wizard struct {
	env     wizardEnv
	p       *prompt.Prompter
	out     io.Writer
	choices JoinChoices
	pr      setup.Progress
}

// runWizard walks the steps of §9.1. With resume it continues from the step
// saved in the Worker config; otherwise it starts with the invitation.
func runWizard(ctx context.Context, env wizardEnv, p *prompt.Prompter, choices JoinChoices, resume bool) error {
	w := &wizard{env: env, p: p, out: p.Out(), choices: choices}
	pr, pending, err := setup.Load(env.Paths)
	if err != nil {
		return err
	}
	switch {
	case resume && !pending:
		return domain.NewError(domain.CodeInvalidRequest, "no hay un join sin terminar que reanudar", nil)
	case resume:
		w.pr = pr
		w.say("Reanudando el join de %s desde el paso %d.", pr.Name, pr.Step)
	case pending && !choices.Force:
		return domain.NewError(domain.CodeInvalidRequest,
			"hay un join sin terminar en este equipo: continúalo con `"+resumeHint+"` o empieza de cero con `cf worker join --force`", nil)
	}
	if err := w.steps(ctx); err != nil {
		if errors.Is(err, io.EOF) {
			w.say("\nEntrada cerrada: el progreso queda guardado. Continúa con `%s`.", resumeHint)
		}
		return err
	}
	return nil
}

func (w *wizard) say(format string, args ...any) { _, _ = fmt.Fprintf(w.out, format+"\n", args...) }

func (w *wizard) steps(ctx context.Context) error {
	if w.pr.Step < stepJoined {
		if err := w.join(ctx); err != nil {
			return err
		}
	}
	stages := []struct {
		after int
		run   func(context.Context) error
	}{
		{stepHarnesses, w.harnesses},
		{stepWorkflow, w.initialWorkflow},
	}
	for _, s := range stages {
		if w.pr.Step >= s.after {
			continue
		}
		if err := s.run(ctx); err != nil {
			return err
		}
		if err := w.save(s.after); err != nil {
			return err
		}
	}
	// The logins are asked on every run, resumed or not: skipping one is not
	// finishing it.
	if err := w.logins(ctx); err != nil {
		return err
	}
	return w.finish(ctx)
}

func (w *wizard) save(step int) error {
	w.pr.Step = step
	return setup.SaveProgress(w.env.Paths, w.pr)
}

// join asks what the Controller needs to hear at the join (the name and the
// resources cannot change after it), redeems the invitation and starts the setup.
func (w *wizard) join(ctx context.Context) error {
	info, err := w.env.Sysinfo(w.env.Paths.WorkerDataDir())
	if err != nil {
		w.say("No se pudieron detectar los recursos de este equipo (%v); se aportan todos.", err)
	} else {
		w.say("Este equipo: %s/%s, %d CPU, %s de memoria, %s de disco.", info.OS, info.Arch, info.CPU, sizeText(info.MemoryMB), sizeText(info.DiskMB))
	}
	c := w.choices
	invitation := c.Invitation
	if invitation == "" {
		if invitation, err = w.p.Ask("Invitación de Worker", ""); err != nil {
			return err
		}
		if invitation == "" {
			return domain.NewError(domain.CodeInvalidRequest, "falta la invitación de Worker", nil)
		}
	}
	name := c.Name
	if name == "" {
		if name, err = w.p.Ask("Nombre del Worker", workerName(w.env.Hostname)); err != nil {
			return err
		}
	}
	settings := c.Settings
	if settings.CPUMax == 0 && info.CPU > 0 {
		cpu, err := askValid(w.p, w.out, "CPUs que aportas", strconv.Itoa(info.CPU), parseCPUs)
		if err != nil {
			return err
		}
		if cpu != info.CPU {
			settings.CPUMax = cpu
		}
	}
	if settings.MemoryMaxMB == 0 && info.MemoryMB > 0 {
		def := sizeText(info.MemoryMB)
		mb, err := askValid(w.p, w.out, "Memoria que aportas (p. ej. 16GB)", def, parseMemory)
		if err != nil {
			return err
		}
		if mb != info.MemoryMB {
			settings.MemoryMaxMB = mb
		}
	}
	minFree := c.DiskMinFree
	if minFree == "" {
		minFree, err = askValid(w.p, w.out, "Disco libre mínimo (p. ej. 20% o 50GiB)", doctor.DefaultMinFree,
			func(text string) (string, error) { _, err := doctor.ParseThreshold(text); return text, err })
		if err != nil {
			return err
		}
	}

	if _, err := w.env.Join(ctx, JoinOptions{Paths: w.env.Paths, Invitation: invitation, Name: name, Settings: settings, Force: c.Force}); err != nil {
		return err
	}
	// One write: the Worker is pending from the moment its config exists.
	w.pr = setup.Progress{Step: stepJoined, Name: name, DiskMinFree: minFree}
	if err := doctor.SaveSettings(w.env.Paths, doctor.Settings{Name: name, DiskMinFree: minFree, SetupPending: true, Wizard: &w.pr}); err != nil {
		return err
	}
	w.say("Unido como Worker %s. No recibirá Runs hasta terminar este asistente.", name)
	return nil
}

// harnesses detects what is installed, asks which to offer and offers to install the missing ones.
func (w *wizard) harnesses(ctx context.Context) error {
	chosen := []string{}
	for _, a := range w.env.Adapters {
		det, err := w.detect(ctx, a)
		if err != nil {
			w.say("%s: no se pudo detectar: %v", a.ID(), err)
		}
		if det.Installed {
			w.say("%s: instalado en %s", a.ID(), det.Path)
		} else {
			w.say("%s: no instalado", a.ID())
		}
		offer, err := w.p.Confirm("¿Ofrecer "+a.ID()+" en este Worker?", det.Installed)
		if err != nil {
			return err
		}
		if !offer {
			continue
		}
		if !det.Installed {
			installed, err := w.install(ctx, a)
			if err != nil {
				return err
			}
			if !installed {
				continue
			}
		}
		chosen = append(chosen, a.ID())
	}
	w.pr.Harnesses = chosen
	return nil
}

func (w *wizard) detect(ctx context.Context, a harness.Adapter) (harness.Detection, error) {
	host, err := w.env.NewHost()
	if err != nil {
		return harness.Detection{}, err
	}
	return a.Detect(ctx, host)
}

// install shows the plan of a missing harness, asks and runs it. It reports
// whether the harness is left to be offered: a declined install drops it. A
// failed one keeps it, and the doctor fails until it is installed by hand.
func (w *wizard) install(ctx context.Context, a harness.Adapter) (bool, error) {
	id := a.ID()
	if !a.Capabilities().Install {
		w.say("%s no se puede instalar desde aquí: instálalo a mano y vuelve a ejecutar `%s`.", id, resumeHint)
		return true, nil
	}
	host, err := w.env.NewHost()
	if err != nil {
		return false, err
	}
	plan, err := a.Install(ctx, host, harness.InstallOptions{DryRun: true})
	if err != nil {
		w.say("No se puede instalar %s automáticamente: %s", id, doctor.Clean(err.Error()))
		return true, nil
	}
	w.say("Para instalar %s se ejecutaría:", id)
	for _, step := range plan.Steps {
		w.say("  - %s: %s", step.Summary, quoteArgv(step.Argv))
	}
	if !w.choices.Yes {
		accepted, err := w.p.Confirm("¿Instalar "+id+" ahora?", false)
		if err != nil {
			return false, err
		}
		if !accepted {
			w.say("%s no se instala: no se ofrecerá en este Worker.", id)
			return false, nil
		}
	}
	// The plan the person saw is the one that runs: it is not worked out again.
	if err := harness.RunInstallPlan(ctx, host, plan, w.out); err != nil {
		w.say("La instalación de %s falló: %s. Instálalo a mano y ejecuta `%s`.", id, doctor.Clean(err.Error()), resumeHint)
		return true, nil
	}
	if det, err := w.detect(ctx, a); err != nil || !det.Installed {
		w.say("%s se instaló pero no se encuentra en el PATH: abre una shell nueva y ejecuta `%s`.", id, resumeHint)
		return true, nil
	}
	w.say("%s instalado.", id)
	return true, nil
}

// quoteArgv writes argv as a shell would need it, so that what is shown is
// what runs: an element with spaces or metacharacters goes in single quotes.
func quoteArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = arg
		if arg == "" || strings.ContainsAny(arg, " \t\n'\"$|&;<>()*?[]{}!\\`#~") {
			quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
		}
	}
	return strings.Join(quoted, " ")
}

// initialWorkflow offers the Workflows of the Controller and orders the chosen one.
func (w *wizard) initialWorkflow(ctx context.Context) error {
	api, err := w.env.Workflows()
	if err != nil || api == nil {
		w.say("Workflow inicial omitido: este equipo no tiene credenciales de cliente. Usa `cf workflow apply` desde un cliente.")
		return nil
	}
	list, err := api.ListWorkflows(ctx)
	if err != nil || len(list) == 0 {
		w.say("Workflow inicial omitido: no hay Workflows que aplicar (%s).", doctor.Clean(orText(err, "la lista está vacía")))
		return nil
	}
	for i, wf := range list {
		w.say("  %d) %s (versión %d)", i+1, doctor.Clean(wf.Name), wf.Head)
	}
	answer, err := w.p.Ask("Workflow inicial (número o nombre; Enter para omitir)", "")
	if err != nil || answer == "" {
		return err
	}
	name, found := pickWorkflow(list, answer)
	if !found {
		w.say("«%s» no es un Workflow de la lista: se omite.", doctor.Clean(answer))
		return nil
	}
	w.pr.InitialWorkflow = name
	for _, id := range w.pr.Harnesses {
		w.apply(ctx, api, name, id)
	}
	return nil
}

// apply orders the Workflow for one harness and waits briefly: the Worker only
// carries the order out once its daemon runs.
func (w *wizard) apply(ctx context.Context, api workflowAPI, workflow, harnessID string) {
	res, err := api.ApplyWorkflow(ctx, v1.ApplyWorkflowRequest{Worker: w.pr.Name, Harness: harnessID, Workflow: workflow})
	switch {
	case err != nil:
		w.say("No se pudo pedir %s para %s: %s", workflow, harnessID, doctor.Clean(err.Error()))
		return
	case res.Apply == nil:
		w.say("%s en %s: nada que aplicar.", workflow, harnessID)
		return
	}
	id := res.Apply.ID
	for range w.env.ApplyAttempts {
		order, err := api.WorkflowApply(ctx, id)
		if err != nil {
			break
		}
		switch order.State {
		case v1.ApplyStateApplied:
			w.say("%s aplicado a %s.", workflow, harnessID)
			return
		case v1.ApplyStateFailed, v1.ApplyStateSuperseded:
			w.say("%s en %s: %s %s", workflow, harnessID, order.State, doctor.Clean(order.Error))
			return
		}
		w.env.Sleep(applyPollEvery)
	}
	w.say("%s para %s queda pedido: se aplicará cuando arranque `cf worker run`.", workflow, harnessID)
}

// logins asks for the local login of each harness that lacks it, until it is there or the person skips it.
func (w *wizard) logins(ctx context.Context) error {
	for {
		components, err := w.env.Doctor(ctx, w.pr.DiskMinFree, w.pr.Harnesses)
		if err != nil {
			return err
		}
		missing := w.missingLogins(components)
		if len(missing) == 0 {
			return nil
		}
		for _, c := range missing {
			w.say("%s: %s. Acción: %s", c.Name, c.Message, c.Action)
		}
		answer, err := w.p.Ask("Pulsa Enter para volver a comprobar, o escribe «o» para omitir", "")
		if err != nil {
			return err
		}
		if strings.EqualFold(answer, "o") {
			return nil
		}
	}
}

func (w *wizard) missingLogins(components []v1.ComponentHealth) []v1.ComponentHealth {
	var out []v1.ComponentHealth
	for _, c := range components {
		for _, id := range w.pr.Harnesses {
			if c.Name == "auth:"+id && c.Status == v1.ComponentFail {
				out = append(out, c)
			}
		}
	}
	return out
}

// missingHarnesses fails each chosen harness that is not installed: the doctor
// of the Worker says nothing of a harness it does not find, but the person
// chose to offer it.
func (w *wizard) missingHarnesses(ctx context.Context) []v1.ComponentHealth {
	var out []v1.ComponentHealth
	for _, a := range w.env.Adapters {
		if !slices.Contains(w.pr.Harnesses, a.ID()) {
			continue
		}
		if det, err := w.detect(ctx, a); err != nil || !det.Installed {
			out = append(out, v1.ComponentHealth{
				Name: "harness:" + a.ID(), Status: v1.ComponentFail, Message: "no está instalado",
				Action: "instálalo a mano y ejecuta `" + resumeHint + "`",
			})
		}
	}
	return out
}

// finish runs the doctor: green clears the pending setup, anything else keeps it.
func (w *wizard) finish(ctx context.Context) error {
	components, err := w.env.Doctor(ctx, w.pr.DiskMinFree, w.pr.Harnesses)
	if err != nil {
		return err
	}
	components = append(components, w.missingHarnesses(ctx)...)
	if err := writeDoctorTable(w.out, components); err != nil {
		return err
	}
	if failing := doctor.Failing(components); failing > 0 {
		w.say("\nQuedan %d componente(s) en fallo: corrígelos y continúa con `%s`. Mientras tanto el Worker no recibe Runs.", failing, resumeHint)
		return domain.NewError(domain.CodeInternal, fmt.Sprintf("join sin terminar: %d componente(s) fallan", failing), nil)
	}
	settings, err := doctor.LoadSettings(w.env.Paths)
	if err != nil {
		return err
	}
	settings.Name, settings.DiskMinFree = w.pr.Name, w.pr.DiskMinFree
	settings.Harnesses = append([]string{}, w.pr.Harnesses...) // non-nil: no harness is a choice too
	if err := doctor.SaveSettings(w.env.Paths, settings); err != nil {
		return err
	}
	if err := setup.MarkDone(w.env.Paths); err != nil {
		return err
	}
	w.say("\nListo: el Worker %s está preparado. Arráncalo con `cf worker run` (o `cf worker install-service`).", w.pr.Name)
	return nil
}

// askValid asks until the answer parses.
func askValid[T any](p *prompt.Prompter, out io.Writer, question, def string, parse func(string) (T, error)) (T, error) {
	for {
		answer, err := p.Ask(question, def)
		if err != nil {
			var zero T
			return zero, err
		}
		v, err := parse(answer)
		if err == nil {
			return v, nil
		}
		_, _ = fmt.Fprintf(out, "%v\n", err)
	}
}

// isTerminalReader says whether r is the terminal: a file with a line discipline.
func isTerminalReader(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && isTerminal(f)
}

func parseMemory(text string) (int, error) {
	mb, err := parseSizeMB("", text)
	if err != nil {
		return 0, errors.New("indica un tamaño como 512MB, 16GB o 2TB")
	}
	return mb, nil
}

func parseCPUs(text string) (int, error) {
	n, err := strconv.Atoi(text)
	if err != nil || n <= 0 {
		return 0, errors.New("indica un número de CPUs mayor que 0")
	}
	return n, nil
}

// sizeText writes megabytes as 16GB when they are whole gigabytes.
func sizeText(mb int) string {
	if mb >= 1<<10 && mb%(1<<10) == 0 {
		return strconv.Itoa(mb>>10) + "GB"
	}
	return strconv.Itoa(mb) + "MB"
}

func pickWorkflow(list []v1.WorkflowSummary, answer string) (string, bool) {
	if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(list) {
		return list[n-1].Name, true
	}
	for _, wf := range list {
		if wf.Name == answer {
			return wf.Name, true
		}
	}
	return "", false
}

func orText(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}
