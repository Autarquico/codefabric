package daemon

import (
	"github.com/Autarquico/codefabric/internal/worker/attachrelay"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
)

// shellOptions is what the relay needs to open a shell. The capability that
// announces it is added by attachRelay only while allow_shell is on; the relay
// reads the setting again at each request, so withdrawing it takes effect at
// once. A config that cannot be read is a no.
func (d *daemon) shellOptions() attachrelay.ShellOptions {
	return attachrelay.ShellOptions{
		Allow: func() bool {
			s, err := doctor.LoadSettings(d.paths)
			return err == nil && s.AllowShell
		},
		Worktree: d.paths.WorkerWorktreeDir,
	}
}
