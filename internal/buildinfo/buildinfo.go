// Package buildinfo exposes the version metadata stamped into the cf binary.
package buildinfo

import "runtime/debug"

// Set at link time via -ldflags "-X github.com/Autarquico/codefabric/internal/buildinfo.version=...".
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// Info identifies the running binary.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// Get returns the stamped build info, falling back to VCS data recorded by the Go toolchain.
func Get() Info {
	info := Info{Version: version, Commit: commit, Date: date}
	if info.Commit != "" {
		return info
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Commit = s.Value
			case "vcs.time":
				info.Date = s.Value
			}
		}
	}
	return info
}
