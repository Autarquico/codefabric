package migrate

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/Autarquico/codeflock/internal/support/svcunit"
)

// LaunchPlan is how a launchd service of the old name is moved by hand, for
// the one cflk cannot manage itself: a LaunchDaemon of the system, which only
// root loads, or a Controller agent, which cflk does not install on macOS. The
// commands are printed, with the ones that put things back, and nothing changes.
type LaunchPlan struct {
	Plist   string // the old property list
	Content []byte
	System  bool   // a LaunchDaemon: its commands need sudo
	Binary  string // the program the new service runs: cflk, as an absolute path
	// UserHome is where the user of an agent keeps their files, if its plist
	// does not say. It is never used for a LaunchDaemon, whose files are those
	// the property list says: the home of whoever asks is not the daemon's.
	UserHome string
	// LookupHome is the home directory of a user, for a LaunchDaemon that runs
	// as one; nil asks the system.
	LookupHome func(name string) (string, error)
}

var programPath = regexp.MustCompile(`(<key>ProgramArguments</key>\s*<array>\s*<string>)[^<]*(</string>)`)

// PlanBinary is the cflk a plan makes the service run. A LaunchDaemon runs as
// root, so its program must be one only root can replace (svcunit.SystemProgram);
// an agent runs as the user, who may own it.
func PlanBinary(binary string, system bool) (string, error) {
	program := svcunit.Program
	if system {
		program = svcunit.SystemProgram
	}
	path, _, err := program(binary)
	return path, err
}

// plannedPair is a directory that moves, and whether a plist that has none of
// it is fine: the logs of launchd, which a hand-written plist may not have.
type plannedPair struct {
	Pair
	optional bool
}

// Commands are the commands, as lines of a shell script. What moves is one
// block for the shell, `sh -eu`, so that a failure stops it where it is; the way
// back is another, commented out, so that pasting it all runs only the first.
func (p LaunchPlan) Commands() ([]string, error) {
	info, err := svcunit.ReadPlist(p.Content)
	if err != nil {
		return nil, err
	}
	home, err := p.homeOf(info)
	if err != nil {
		return nil, err
	}
	planned, err := p.pairs(info, home)
	if err != nil {
		return nil, err
	}
	oldLabel := strings.TrimSuffix(filepath.Base(p.Plist), ".plist")
	newLabel := strings.Replace(oldLabel, "com.codefabric.", "com.codeflock.", 1)
	for _, label := range []string{oldLabel, newLabel} {
		if err := checkName(label); err != nil {
			return nil, err
		}
	}
	newPlist := filepath.Join(filepath.Dir(p.Plist), newLabel+".plist")
	domain, shell, as := "gui/$(id -u)", "sh -eu", ""
	if p.System {
		domain, shell = "system", "sudo sh -eu"
		// The block runs as root; the directories are the user's.
		if info.UserName != "" {
			as = "sudo -u " + shellQuote(info.UserName) + " "
		}
	}
	target := func(label string) string { return domain + "/" + shellQuote(label) }
	pairs := make([]Pair, len(planned))
	for i, pr := range planned {
		pairs[i] = pr.Pair
	}
	plist := strings.TrimRight(string(p.rewritten(pairs)), "\n")
	inner := distinct("CFLK_PLIST", plist)
	tmpl := shellQuote(filepath.Join(filepath.Dir(newPlist), "."+newLabel+".XXXXXX"))

	oldKept, written := p.Plist+".cflk-old", newPlist+".cflk-written"
	absent := func(path string) string {
		return fmt.Sprintf("if [ -e %[1]s ] || [ -L %[1]s ]; then echo %[2]s >&2; exit 1; fi", shellQuote(path), shellQuote(path+" exists: stop here"))
	}

	var forward []string
	say := func(format string, a ...any) { forward = append(forward, fmt.Sprintf(format, a...)) }
	say("# 1. Check that nothing of the new service is there yet, before anything is stopped.")
	say("%s", absent(newPlist))
	say("%s", absent(written))
	say("%s", absent(oldKept))
	say("# 2. Stop the old service; its file stays.")
	say("launchctl bootout %s", target(oldLabel))
	say("# 3. Move the directories it uses.")
	for _, pr := range planned {
		forward = append(forward, move(as, pr.Old, pr.New, pr.optional)...)
	}
	say("# 4. Write the new property list; an existing file is never overwritten.")
	say("%s", absent(newPlist))
	say("tmp=$(mktemp %s)", tmpl)
	say(`trap 'rm -f "$tmp"' EXIT`)
	say(`cat >"$tmp" <<'%s'`, inner)
	forward = append(forward, plist, inner)
	if p.System {
		say(`chown root:wheel "$tmp"`)
	}
	say(`chmod 644 "$tmp"`)
	say(`mv -n "$tmp" %s`, shellQuote(newPlist))
	say(`if [ -e "$tmp" ]; then echo %s >&2; exit 1; fi`, shellQuote(newPlist+" appeared meanwhile: stop here"))
	say("# The way back removes the new property list only if this plan wrote it, which this file says.")
	say("touch %s", shellQuote(written))
	say("# 5. Load it.")
	say("launchctl bootstrap %s %s", domain, shellQuote(newPlist))
	say("# 6. Keep the old property list aside: the way back needs it.")
	say("mv %s %s", shellQuote(p.Plist), shellQuote(oldKept))
	say("# Once the new service runs, remove %s by hand.", oldKept)

	var back []string
	undo := func(format string, a ...any) { back = append(back, fmt.Sprintf(format, a...)) }
	undo("# Stop and remove the new service, only if this plan wrote it.")
	undo("if [ -e %s ]; then", shellQuote(written))
	undo("\tlaunchctl bootout %s || true", target(newLabel))
	undo("\trm -f %s %s", shellQuote(newPlist), shellQuote(written))
	undo("fi")
	for i := len(planned) - 1; i >= 0; i-- {
		back = append(back, move(as, planned[i].New, planned[i].Old, true)...)
	}
	undo("# Put the old property list back and load it.")
	undo("if [ -e %s ]; then", shellQuote(oldKept))
	undo("\t%s", absent(p.Plist))
	undo("\tmv %s %s", shellQuote(oldKept), shellQuote(p.Plist))
	undo("fi")
	undo("launchctl bootstrap %s %s", domain, shellQuote(p.Plist))

	text := plist + strings.Join(forward, "\n") + strings.Join(back, "\n")
	outer, undoDelim := distinct("CFLK_PLAN", text, inner), distinct("CFLK_BACK", text, inner)
	for undoDelim == outer {
		undoDelim += "_"
	}

	out := []string{
		"# Make sure no Run is live on this Worker first (`cflk worker drain <worker>`): the Runs do not follow the move.",
		"# Run this as one block: it stops at the first command that fails.",
		fmt.Sprintf("%s <<'%s'", shell, outer),
	}
	out = append(out, forward...)
	out = append(out, outer, "",
		"# To go back, if a step failed: take the `# ` off the start of every line below, and run them as one block.",
		"# Pasting all of this does not run them.")
	out = append(out, commented(fmt.Sprintf("%s <<'%s'", shell, undoDelim)))
	for _, line := range back {
		out = append(out, commented(line))
	}
	out = append(out, commented(undoDelim))
	return out, nil
}

// distinct is name, made longer until none of avoid has it: a heredoc ends at
// the first line that is its delimiter.
func distinct(name string, avoid ...string) string {
	for slices.ContainsFunc(avoid, func(a string) bool { return strings.Contains(a, name) }) {
		name += "_"
	}
	return name
}

// commented is every line of text behind `# `.
func commented(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = "# " + l
	}
	return strings.Join(lines, "\n")
}

// move is a move that never goes into a destination that exists: `mv` would put
// the directory inside it, and follow it if it is a link. The block stops there,
// and stops too if, after the move, the source is still there or the
// destination holds a directory of its name: it appeared between the check and
// the move, and the directory went inside it. An optional directory that is not
// there is not moved.
func move(as, from, to string, optional bool) []string {
	lines := []string{
		fmt.Sprintf("if [ -e %[1]s ] || [ -L %[1]s ]; then echo %[2]s >&2; exit 1; fi", shellQuote(to), shellQuote(to+" exists: stop here")),
		fmt.Sprintf("%smv %s %s", as, shellQuote(from), shellQuote(to)),
		fmt.Sprintf("if [ ! -d %[1]s ] || [ -e %[2]s ] || [ -e %[3]s ]; then echo %[4]s >&2; exit 1; fi",
			shellQuote(to), shellQuote(from), shellQuote(filepath.Join(to, filepath.Base(from))), shellQuote(to+" is not the directory that moved: stop here")),
	}
	if !optional {
		return lines
	}
	out := []string{fmt.Sprintf("if [ -e %s ]; then", shellQuote(from))}
	for _, l := range lines {
		out = append(out, "\t"+l)
	}
	return append(out, "fi")
}

// homeOf is where the user of the service keeps their files. For a
// LaunchDaemon only the property list says: its HOME, or the home of its
// UserName; for an agent, the user's own.
func (p LaunchPlan) homeOf(info svcunit.PlistInfo) (string, error) {
	switch {
	case info.Env["HOME"] != "":
		return info.Env["HOME"], nil
	case !p.System:
		return p.UserHome, nil
	case info.UserName == "":
		return "", nil
	}
	lookup := p.LookupHome
	if lookup == nil {
		lookup = func(name string) (string, error) {
			u, err := user.Lookup(name)
			if err != nil {
				return "", err
			}
			return u.HomeDir, nil
		}
	}
	home, err := lookup(info.UserName)
	if err == nil && !filepath.IsAbs(home) {
		err = errors.New("it is not an absolute path")
	}
	if err != nil {
		return "", fmt.Errorf("the home of %q, the user of the service, cannot be found, so there is no telling what to move: %w", info.UserName, err)
	}
	return home, nil
}

// pairs are the directories the service uses that the old name put where they
// are, and the logs of launchd, which sit under the home of the user.
func (p LaunchPlan) pairs(info svcunit.PlistInfo, home string) ([]plannedPair, error) {
	cfg, data := envDir(info.Env, "CONFIG_DIR", ""), envDir(info.Env, "DATA_DIR", "")
	if home == "" && (cfg == "" || data == "") {
		return nil, errors.New("the property list says neither the directories of the service nor HOME, so there is no telling what to move")
	}
	if cfg == "" {
		cfg = filepath.Join(home, ".config", "codefabric")
	}
	if data == "" {
		data = filepath.Join(home, ".local", "share", "codefabric")
	}
	var pairs []plannedPair
	for _, dir := range []string{cfg, data} {
		if renamed := Renamed(dir); renamed != dir {
			pairs = append(pairs, plannedPair{Pair: Pair{Old: dir, New: renamed}})
		}
	}
	if home != "" {
		logs := filepath.Join(home, "Library", "Logs")
		pairs = append(pairs, plannedPair{Pair: Pair{Old: filepath.Join(logs, "codefabric"), New: filepath.Join(logs, "codeflock")}, optional: true})
	}
	return pairs, nil
}

// rewritten is the new property list: the names, the variables and the places
// of the old one, in its own words.
func (p LaunchPlan) rewritten(pairs []Pair) []byte {
	renames := []string{"com.codefabric.", "com.codeflock."}
	info, _ := svcunit.ReadPlist(p.Content)
	for _, name := range []string{"CONFIG_DIR", "DATA_DIR"} {
		// One that has its CFLK_* already keeps it, and its CF_* is the one left behind: no key twice.
		if _, has := info.Env["CFLK_"+name]; !has {
			renames = append(renames, "<key>CF_"+name+"</key>", "<key>CFLK_"+name+"</key>")
		}
	}
	text := strings.NewReplacer(renames...).Replace(string(p.Content))
	for _, pr := range escaped(pairs) {
		text = strings.ReplaceAll(text, ">"+pr.Old+"<", ">"+pr.New+"<")
		text = strings.ReplaceAll(text, pr.Old+"/", pr.New+"/")
	}
	if p.Binary != "" {
		text = programPath.ReplaceAllString(text, "${1}"+strings.ReplaceAll(xmlEscaped(p.Binary), "$", "$$")+"${2}")
	}
	return []byte(text)
}

func escaped(pairs []Pair) []Pair {
	out := make([]Pair, len(pairs))
	for i, p := range pairs {
		out[i] = Pair{xmlEscaped(p.Old), xmlEscaped(p.New)}
	}
	return out
}

func xmlEscaped(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
