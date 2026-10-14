package svcunit

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// systemd manages the service as a unit of the user's systemd (--user).
type systemd struct{ env Env }

func (m systemd) path(s Spec) string {
	return filepath.Join(m.env.Home, ".config", "systemd", "user", s.Name+".service")
}

func (systemd) render(s Spec) ([]byte, error) { return renderSystemd(s) }

func (systemd) installedEnv(file []byte) (map[string]string, error) { return systemdEnv(file), nil }

// unitState is what `systemctl show` says of a unit.
type unitState map[string]string

// running is a unit with a process, or about to have or lose one.
func (u unitState) running() bool {
	return !slices.Contains([]string{"inactive", "failed", ""}, u["ActiveState"])
}

func (u unitState) enabled() bool {
	return slices.Contains([]string{"enabled", "enabled-runtime"}, u["UnitFileState"])
}

func (m systemd) show(o *op, unit string) (unitState, error) {
	out, err := o.query("systemctl", "--user", "show",
		"-p", "LoadState", "-p", "UnitFileState", "-p", "ActiveState", "-p", "NeedDaemonReload",
		"-p", "FragmentPath", "-p", "KillMode", "-p", "DropInPaths", unit)
	if err != nil {
		return nil, fmt.Errorf("svcunit: asking systemd about %s: %w", unit, err)
	}
	u := unitState{}
	for line := range strings.Lines(out) {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			u[key] = value
		}
	}
	return u, nil
}

// checkEffective refuses a unit that systemd loads from another file, or
// whose effective KillMode (drop-ins included) would kill what the daemon
// started when it stops.
func (m systemd) checkEffective(u unitState, s Spec, path string) error {
	if fragment := u["FragmentPath"]; fragment != "" && !sameFile(fragment, path) {
		return fmt.Errorf("systemd loads %s.service from %s, not from %s: another unit file shadows it, "+
			"or the user's systemd looks for units elsewhere (XDG_CONFIG_HOME)", s.Name, fragment, path)
	}
	if s.KeepProcesses && u["KillMode"] != "process" {
		return fmt.Errorf("the effective KillMode of %s.service is %q, not process (drop-ins: %q): "+
			"stopping or restarting it would kill what it started; remove the override", s.Name, u["KillMode"], u["DropInPaths"])
	}
	return nil
}

func (m systemd) install(o *op, s Spec) error {
	o.res.Manager, o.res.Unit = "systemd", s.Name+".service"
	content, err := m.render(s)
	if err != nil {
		return err
	}
	undo, err := o.write(m.path(s), content)
	if err != nil {
		return err
	}
	return o.rollback(m.apply(o, s, undo != nil), undo, "systemctl", "--user", "daemon-reload")
}

// apply makes systemd run the unit file as it is: changed says it was just written.
func (m systemd) apply(o *op, s Spec, changed bool) error {
	unit, path := s.Name+".service", m.path(s)
	u, err := m.show(o, unit)
	if err != nil {
		return err
	}
	if changed || u["NeedDaemonReload"] == "yes" {
		if err := o.act("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		if u, err = m.show(o, unit); err != nil {
			return err
		}
	}
	if u["LoadState"] == "not-found" || u["FragmentPath"] == "" {
		return fmt.Errorf("systemd does not find %s at %s: the user's systemd looks for units elsewhere (XDG_CONFIG_HOME?)", unit, path)
	}
	if err := m.checkEffective(u, s, path); err != nil {
		return err
	}
	if !u.enabled() {
		if err := o.act("systemctl", "--user", "enable", "--no-reload", unit); err != nil {
			return err
		}
	}
	switch {
	case !u.running():
		err = o.act("systemctl", "--user", "start", unit)
	case changed:
		err = o.act("systemctl", "--user", "restart", unit)
	}
	if err != nil {
		return err
	}
	m.warnLinger(o)
	return nil
}

// warnLinger warns that, without lingering, systemd ends the user's services
// with the user's last session.
func (m systemd) warnLinger(o *op) {
	out, err := o.query("loginctl", "show-user", strconv.Itoa(m.env.UID), "-p", "Linger", "--value")
	switch {
	case err != nil:
		o.warn("could not tell whether lingering is on for this user (%v): without it, systemd stops the service, "+
			"and everything it started, when the user's last session ends", err)
	case strings.TrimSpace(out) == "no":
		o.warn("lingering is off for this user: systemd stops the service, and everything it started, when the user's " +
			"last session ends; `sudo loginctl enable-linger $USER` keeps it running")
	}
}

func (m systemd) uninstall(o *op, s Spec) error {
	unit, path := s.Name+".service", m.path(s)
	o.res.Manager, o.res.Unit = "systemd", unit
	u, err := m.show(o, unit)
	if err != nil {
		return err
	}
	exists, err := fileExists(path)
	if err != nil {
		return err
	}
	if !exists && u["LoadState"] == "not-found" {
		return nil
	}
	if fragment := u["FragmentPath"]; fragment != "" && !sameFile(fragment, path) {
		return fmt.Errorf("systemd loads %s from %s, which this command did not write: leaving it alone", unit, fragment)
	}
	if u.running() {
		if err := m.checkEffective(u, s, path); err != nil {
			return err
		}
		if err := o.act("systemctl", "--user", "stop", unit); err != nil {
			return err
		}
	}
	if u.enabled() {
		if err := o.act("systemctl", "--user", "disable", "--no-reload", unit); err != nil {
			return err
		}
	}
	if u["ActiveState"] == "failed" {
		if err := o.act("systemctl", "--user", "reset-failed", unit); err != nil {
			return err
		}
	}
	if _, err := o.remove(path); err != nil {
		return err
	}
	return o.act("systemctl", "--user", "daemon-reload")
}
