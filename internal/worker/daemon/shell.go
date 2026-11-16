package daemon

import (
	"slices"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/attachrelay"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
)

// allowShell reads allow_shell from the config now. A config that cannot be
// read is a no.
func (d *daemon) allowShell() bool {
	s, err := doctor.LoadSettings(d.paths)
	return err == nil && s.AllowShell
}

// shellOptions is what the relay needs to open a shell. The relay reads
// allow_shell again at each request, so withdrawing it takes effect at once.
func (d *daemon) shellOptions() attachrelay.ShellOptions {
	return attachrelay.ShellOptions{Allow: d.allowShell, Worktree: d.paths.WorkerWorktreeDir}
}

// announcedCapabilities are the capabilities of this daemon for a heartbeat:
// those it computed at start, plus the shell while the relay runs and allow_shell
// is on. The shell follows the setting live, with no restart and no effect on Runs.
func (d *daemon) announcedCapabilities() []string {
	if !d.relayRuns || !d.allowShell() {
		return d.capabilities
	}
	return slices.Sorted(slices.Values(append(slices.Clone(d.capabilities), domain.AttachShellCapability)))
}
