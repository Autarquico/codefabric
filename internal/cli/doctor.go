package cli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

// sshProbeTimeout bounds the connection of the probe (and the probe as a whole).
const sshProbeTimeout = 5 * time.Second

// doctorStore is what `cf doctor` reads of the Controller.
type doctorStore interface {
	Controller() string
	Status(ctx context.Context) (v1.Status, error)
	Workers(ctx context.Context, includeRemoved bool) ([]v1.Worker, error)
}

// sshProbe runs a non-interactive `true` on the target and says why it could
// not. Tests replace it; the binary uses the OpenSSH client on the PATH.
type sshProbe func(ctx context.Context, target domain.SSHTarget) error

func probeOf(opts Options) sshProbe {
	if opts.sshProbe != nil {
		return opts.sshProbe
	}
	return runSSHProbe
}

func openDoctorStore(opts Options) func(*cobra.Command) (doctorStore, error) {
	if opts.doctorStore != nil {
		return opts.doctorStore
	}
	return func(cmd *cobra.Command) (doctorStore, error) { return openClient(cmd) }
}

// DoctorReport is the result of `cf doctor --json`.
type DoctorReport struct {
	Controller DoctorController `json:"controller"`
	Workers    []DoctorWorker   `json:"workers"`
}

// DoctorController is the Controller as the doctor found it.
type DoctorController struct {
	Address string `json:"address"`
	Version string `json:"version"`
	OK      bool   `json:"ok"`
}

// DoctorWorker is one Worker as the doctor sees it. Warnings are what the
// client found from this machine (ssh), which the Worker cannot know.
type DoctorWorker struct {
	Name       string               `json:"name"`
	State      domain.WorkerState   `json:"state"`
	Message    string               `json:"message,omitempty"`
	Components []v1.ComponentHealth `json:"components"`
	Warnings   []string             `json:"warnings,omitempty"`
}

func newDoctorCmd(open func(*cobra.Command) (doctorStore, error), probe sshProbe) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the Controller, the health of each Worker and whether attach can reach them over ssh",
		Long: "Checks that the Controller answers, lists every Worker with the verdict of its own doctor per component,\n" +
			"and tries a non-interactive ssh to the target each Worker announces. An unreachable Controller fails\n" +
			"the command; a failing component or an ssh that does not work is shown as a warning only.\n" +
			"Run `cf worker doctor` on a Worker's machine for the detail of its local checks.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := open(cmd)
			if err != nil {
				return err
			}
			overrides, err := sshOverrides()
			if err != nil {
				return err
			}
			rep, err := diagnose(cmd.Context(), store, probe, overrides)
			if err != nil {
				return err
			}
			return emit(cmd, rep, func(out io.Writer) error { return printDoctor(out, rep) })
		},
	}
}

// sshOverrides is attach.ssh_targets of the client config: the targets that
// `cf attach` uses instead of the ones the Workers announce.
func sshOverrides() (map[string]string, error) {
	paths, err := config.ResolveFromEnv()
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadClient(paths)
	if err != nil {
		return nil, domain.NewError(domain.CodeInvalidRequest, err.Error(), nil)
	}
	return cfg.Attach.SSHTargets, nil
}

func diagnose(ctx context.Context, store doctorStore, probe sshProbe, overrides map[string]string) (DoctorReport, error) {
	st, err := store.Status(ctx)
	if err != nil {
		return DoctorReport{}, err
	}
	workers, err := store.Workers(ctx, false)
	if err != nil {
		return DoctorReport{}, err
	}
	rep := DoctorReport{
		Controller: DoctorController{Address: store.Controller(), Version: st.Version, OK: st.Database.OK},
		Workers:    make([]DoctorWorker, len(workers)),
	}
	var wg sync.WaitGroup
	for i, w := range workers {
		rep.Workers[i] = DoctorWorker{Name: w.Name, State: w.State, Message: w.Health.Message, Components: w.Health.Components}
		if rep.Workers[i].Components == nil {
			rep.Workers[i].Components = []v1.ComponentHealth{}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reason := attachProblem(ctx, cmp.Or(overrides[w.Name], w.SSHTarget), probe); reason != "" {
				rep.Workers[i].Warnings = []string{"attach no funcionará en " + w.Name + ": " + reason}
			}
		}()
	}
	wg.Wait()
	if !st.Database.OK {
		return rep, fmt.Errorf("the Controller's database is not healthy: %s", st.Database.Error)
	}
	return rep, nil
}

// attachProblem is why `cf attach` cannot work toward a Worker from here, or "".
func attachProblem(ctx context.Context, announced string, probe sshProbe) string {
	if announced == "" {
		return "el Worker no anuncia un SSHTarget"
	}
	target, err := domain.ParseSSHTarget(announced)
	if err != nil {
		return "el SSHTarget anunciado no es [usuario@]host[:puerto]"
	}
	if err := probe(ctx, target); err != nil {
		return err.Error()
	}
	return ""
}

func printDoctor(out io.Writer, rep DoctorReport) error {
	fmt.Fprintf(out, "Controller  ok  %s  %s\n", dash(rep.Controller.Address), dash(rep.Controller.Version))
	if len(rep.Workers) == 0 {
		_, err := fmt.Fprintln(out, "\nNo hay Workers.")
		return err
	}
	for _, w := range rep.Workers {
		fmt.Fprintf(out, "\nWorker %s  %s\n", printable(w.Name), w.State)
		if w.Message != "" {
			fmt.Fprintf(out, "  %s\n", printable(w.Message))
		}
		if len(w.Components) == 0 {
			fmt.Fprintln(out, "  (sin componentes: el daemon no informa de su salud por componente)")
		} else {
			t := newTable(out, "  COMPONENT\tSTATUS\tMESSAGE\tACTION")
			for _, c := range w.Components {
				fmt.Fprintf(t, "  %s\t%s\t%s\t%s\n", printable(c.Name), c.Status, dash(printable(c.Message)), dash(printable(c.Action)))
			}
			if err := t.Flush(); err != nil {
				return err
			}
		}
		for _, warning := range w.Warnings {
			fmt.Fprintf(out, "  aviso: %s\n", printable(warning))
		}
	}
	return nil
}

// runSSHProbe is the probe of the binary: `ssh -o BatchMode=yes
// -o ConnectTimeout=5 [-p port] -- target true`. BatchMode makes ssh fail
// instead of asking for a password or a passphrase.
func runSSHProbe(ctx context.Context, target domain.SSHTarget) error {
	ctx, cancel := context.WithTimeout(ctx, sshProbeTimeout+time.Second)
	defer cancel()
	// The probe only runs `true`, on a host a Worker chose: nothing of the
	// user's ssh config may be forwarded to it.
	args := []string{
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=" + strconv.Itoa(int(sshProbeTimeout/time.Second)),
		"-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no",
	}
	if target.Port != 0 {
		args = append(args, "-p", strconv.Itoa(target.Port))
	}
	args = append(args, "--", target.SSHArg(), "true")
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // the target was parsed by domain.ParseSSHTarget and follows "--"
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if reason := lastLine(stderr.String()); reason != "" {
			return errors.New(reason)
		}
		return fmt.Errorf("ssh terminó con código %d", exit.ExitCode())
	}
	return fmt.Errorf("no se pudo ejecutar ssh: %w", err)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
