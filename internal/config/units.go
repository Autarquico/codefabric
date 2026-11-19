package config

import (
	"path/filepath"
	"strings"
)

// Unit is a service of the old name, CodeFabric, that is installed.
type Unit struct {
	Name   string // as the manager knows it
	Kind   string // "worker" or "controller": the command that migrates it
	Path   string // its unit file or property list
	System bool   // a LaunchDaemon of the system, which a user cannot manage
}

// OldUnits are the services installed under the names before the rename,
// running or not, with the suffixes the tests use: the unit files of systemd
// --user on Linux, the LaunchAgents of the user and the LaunchDaemons of the
// system on macOS. They are found by their files, which are what survives a
// stopped unit.
func OldUnits(home, goos string) []Unit {
	var patterns []string
	switch goos {
	case "linux":
		dir := filepath.Join(home, ".config", "systemd", "user")
		patterns = []string{filepath.Join(dir, "cf-worker*.service"), filepath.Join(dir, "cf-controller*.service")}
	case "darwin":
		for _, dir := range []string{filepath.Join(home, "Library", "LaunchAgents"), launchDaemonsDir} {
			patterns = append(patterns, filepath.Join(dir, "com.codefabric.worker*.plist"), filepath.Join(dir, "com.codefabric.controller*.plist"))
		}
	}
	var units []Unit
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			name := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(m), ".service"), ".plist")
			kind := "worker"
			if strings.Contains(name, "controller") {
				kind = "controller"
			}
			units = append(units, Unit{Name: name, Kind: kind, Path: m, System: filepath.Dir(m) == launchDaemonsDir})
		}
	}
	return units
}

const launchDaemonsDir = "/Library/LaunchDaemons"
