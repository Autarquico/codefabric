package svcunit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// launchd manages the service as an agent of the user's launchd. An agent is
// loaded into the domain of the user's graphical session (gui/<uid>) or, on a
// Mac with nobody logged in, into the user's background domain (user/<uid>).
type launchd struct{ env Env }

func (m launchd) path(s Spec) string {
	return filepath.Join(m.env.Home, "Library", "LaunchAgents", s.Label+".plist")
}

// log is where the agent writes: launchd has no journal of its own.
func (m launchd) log(s Spec) string {
	return filepath.Join(m.env.Home, "Library", "Logs", "codefabric", s.Label+".log")
}

func (m launchd) render(s Spec) ([]byte, error) { return renderLaunchd(s, m.log(s)) }

func (launchd) installedEnv(file []byte) (map[string]string, error) { return launchdEnv(file) }

func (m launchd) gui() string  { return "gui/" + strconv.Itoa(m.env.UID) }
func (m launchd) user() string { return "user/" + strconv.Itoa(m.env.UID) }

// job is what `launchctl print` says of a loaded agent.
type job struct {
	domain string
	print  string
}

// field is a property of the agent itself, not of a nested block.
func (j job) field(name string) (string, bool) {
	for line := range strings.Lines(j.print) {
		if value, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "\t"+name+" = "); ok {
			return value, true
		}
	}
	return "", false
}

func (j job) running() bool {
	state, _ := j.field("state")
	return state != "not running"
}

// The codes `launchctl print` exits with when the agent is not there: the
// domain does not exist (no graphical session) or has no such service.
const (
	launchctlNoDomain  = 112
	launchctlNoService = 113
)

// notLoaded says whether err is launchctl saying the agent is not loaded, as
// opposed to failing to tell.
func notLoaded(err error) bool {
	var ce *CommandError
	return errors.As(err, &ce) && (ce.Code == launchctlNoDomain || ce.Code == launchctlNoService)
}

// find looks for the agent in both domains: it is operated where it is loaded.
func (m launchd) find(o *op, s Spec) ([]job, error) {
	var loaded []job
	for _, domain := range []string{m.gui(), m.user()} {
		out, err := o.query("launchctl", "print", domain+"/"+s.Label)
		switch {
		case err == nil:
			loaded = append(loaded, job{domain: domain, print: out})
		case !notLoaded(err):
			return nil, err
		}
	}
	if len(loaded) > 1 {
		return nil, fmt.Errorf("%s is loaded in both %s and %s: remove one with `launchctl bootout <domain>/%s`",
			s.Label, loaded[0].domain, loaded[1].domain, s.Label)
	}
	return loaded, nil
}

// checkLoaded refuses an agent loaded from a file this package did not write,
// or one that would kill what the daemon started when it stops.
func (m launchd) checkLoaded(o *op, j job, s Spec) error {
	if from, _ := j.field("path"); !sameFile(from, m.path(s)) {
		return fmt.Errorf("%s/%s is loaded from %s, not from %s: leaving it alone", j.domain, s.Label, from, m.path(s))
	}
	if !s.KeepProcesses {
		return nil
	}
	properties, ok := j.field("properties")
	switch {
	case !ok:
		o.warn("could not read whether %s/%s abandons its process group: if it does not, stopping it kills what it started", j.domain, s.Label)
	case !strings.Contains(properties, "abandon process group"):
		return fmt.Errorf("%s/%s does not abandon its process group: stopping it would kill what it started", j.domain, s.Label)
	}
	return nil
}

func (m launchd) disabled(o *op, domain string, s Spec) (bool, error) {
	out, err := o.query("launchctl", "print-disabled", domain)
	if err != nil {
		return false, err
	}
	for line := range strings.Lines(out) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), strconv.Quote(s.Label)+" => "); ok {
			return value == "disabled" || value == "true", nil
		}
	}
	return false, nil
}

// freshDomain is where a new agent goes: the graphical session if there is one.
func (m launchd) freshDomain(o *op) string {
	if _, err := o.query("launchctl", "print", m.gui()); err == nil {
		return m.gui()
	}
	o.warn("there is no graphical session for this user: the agent goes into %s, and launchd does not load it again after a restart until the user logs in", m.user())
	return m.user()
}

func (m launchd) install(o *op, s Spec) error {
	path := m.path(s)
	o.res.Manager = "launchd"
	content, err := m.render(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.log(s)), 0o700); err != nil {
		return fmt.Errorf("svcunit: %w", err)
	}
	changed, err := o.write(path, content)
	if err != nil {
		return err
	}
	loaded, err := m.find(o, s)
	if err != nil {
		return err
	}
	domain := ""
	if len(loaded) == 1 {
		domain = loaded[0].domain
	} else {
		domain = m.freshDomain(o)
	}
	o.res.Unit = domain + "/" + s.Label
	// Refused before anything is enabled: a refused install changes nothing.
	if len(loaded) == 1 {
		if err := m.checkLoaded(o, loaded[0], s); err != nil {
			return err
		}
	}
	if off, err := m.disabled(o, domain, s); err != nil {
		return err
	} else if off {
		if err := o.act("launchctl", "enable", o.res.Unit); err != nil {
			return err
		}
	}
	if len(loaded) == 0 {
		return o.act("launchctl", "bootstrap", domain, path)
	}
	switch {
	case changed:
		if err := o.act("launchctl", "bootout", o.res.Unit); err != nil {
			return err
		}
		return o.act("launchctl", "bootstrap", domain, path)
	case !loaded[0].running():
		return o.act("launchctl", "kickstart", o.res.Unit)
	}
	return nil
}

func (m launchd) uninstall(o *op, s Spec) error {
	path := m.path(s)
	o.res.Manager, o.res.Unit = "launchd", s.Label
	loaded, err := m.find(o, s)
	if err != nil {
		return err
	}
	if len(loaded) == 1 {
		o.res.Unit = loaded[0].domain + "/" + s.Label
		if err := m.checkLoaded(o, loaded[0], s); err != nil {
			return err
		}
		if err := o.act("launchctl", "bootout", o.res.Unit); err != nil {
			return err
		}
	}
	_, err = o.remove(path)
	return err
}
