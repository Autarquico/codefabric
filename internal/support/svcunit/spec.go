// Package svcunit installs a daemon of CodeFabric as a service of the user's
// own service manager: a systemd --user unit on Linux, a launchd agent on
// macOS. It knows nothing of what the daemon does; the caller says what runs.
package svcunit

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// KV is a variable of the environment of a service.
type KV struct{ Key, Value string }

// Spec is what a service runs and how.
type Spec struct {
	Name        string // of the systemd unit, without ".service"
	Label       string // of the launchd agent
	Description string
	// Installer is the command that writes the file; its header names it.
	Installer string
	Argv      []string // the absolute path of the program first
	Env       []KV     // in the order they are written
	// KeepProcesses lets what the daemon starts outlive a stop or a restart of
	// the service: KillMode=process for systemd, AbandonProcessGroup for launchd.
	KeepProcesses bool
}

// ErrUnsupported is the error of an operating system with no manager here.
var ErrUnsupported = errors.New("svcunit: no service manager is supported on this operating system")

var (
	validName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	validKey  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// validateNames checks what Uninstall needs: the names a file is found by.
func (s Spec) validateNames() error {
	if !validName.MatchString(s.Name) || !validName.MatchString(s.Label) {
		return fmt.Errorf("svcunit: %q and %q must be names of letters, digits, '.', '_' and '-'", s.Name, s.Label)
	}
	return nil
}

// validate refuses what neither file format could say safely: a line break in
// a value would start a directive of its own.
func (s Spec) validate() error {
	if err := s.validateNames(); err != nil {
		return err
	}
	if strings.Contains(s.Installer, "--") {
		return fmt.Errorf("svcunit: the installer %q goes in an XML comment, which cannot hold \"--\"", s.Installer)
	}
	if len(s.Argv) == 0 || !filepath.IsAbs(s.Argv[0]) {
		return fmt.Errorf("svcunit: the program must be an absolute path, got %q", s.Argv)
	}
	texts := append([]string{s.Description, s.Installer}, s.Argv...)
	for _, kv := range s.Env {
		if !validKey.MatchString(kv.Key) {
			return fmt.Errorf("svcunit: %q is not the name of an environment variable", kv.Key)
		}
		texts = append(texts, kv.Value)
	}
	for _, text := range texts {
		if err := printable(text); err != nil {
			return err
		}
	}
	return nil
}

func printable(text string) error {
	for _, r := range text {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("svcunit: %q has a control character, which a service file cannot hold", text)
		}
	}
	return nil
}
