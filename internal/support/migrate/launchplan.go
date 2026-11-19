package migrate

import (
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Autarquico/codeflock/internal/support/svcunit"
)

// LaunchPlan is how a launchd service of the old name is moved by hand, for
// the one cflk cannot manage itself: a LaunchDaemon of the system, which only
// root loads, or a Controller agent, which cflk does not install on macOS. The
// commands are printed, with the ones that put things back, and nothing changes.
type LaunchPlan struct {
	Plist    string // the old property list
	Content  []byte
	System   bool   // a LaunchDaemon: its commands need sudo
	Binary   string // the program the new service runs; empty keeps the old one
	UserHome string // where the user of the service keeps their files, if the plist does not say
}

var programPath = regexp.MustCompile(`(<key>ProgramArguments</key>\s*<array>\s*<string>)[^<]*(</string>)`)

// Commands are the commands, as lines of a shell script.
func (p LaunchPlan) Commands() ([]string, error) {
	info, err := svcunit.ReadPlist(p.Content)
	if err != nil {
		return nil, err
	}
	home := info.Env["HOME"]
	if home == "" {
		home = p.UserHome
	}
	pairs, err := p.pairs(info, home)
	if err != nil {
		return nil, err
	}
	oldLabel := strings.TrimSuffix(filepath.Base(p.Plist), ".plist")
	newLabel := strings.Replace(oldLabel, "com.codefabric.", "com.codeflock.", 1)
	newPlist := filepath.Join(filepath.Dir(p.Plist), newLabel+".plist")
	domain, sudo, as := "gui/$(id -u)", "", ""
	if p.System {
		domain, sudo = "system", "sudo "
		if info.UserName != "" {
			as = "sudo -u " + info.UserName + " "
		}
	}
	// The directories are the user's, whoever runs the commands.
	if as == "" {
		as = sudo
	}

	var out []string
	say := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }
	say("# Make sure no Run is live on this Worker first (`cflk worker drain <worker>`): the Runs do not follow the move.")
	say("# 1. Stop the old service; its file stays.")
	say("%slaunchctl bootout %s/%s", sudo, domain, oldLabel)
	say("# 2. Move the directories it uses.")
	for _, pair := range pairs {
		say("%smv %s %s", as, shellQuote(pair.Old), shellQuote(pair.New))
	}
	say("# 3. Write the new property list.")
	say("%stee %s >/dev/null <<'CFLK_PLIST'", sudo, shellQuote(newPlist))
	out = append(out, strings.TrimRight(string(p.rewritten(pairs)), "\n"), "CFLK_PLIST")
	if p.System {
		say("sudo chown root:wheel %s", shellQuote(newPlist))
		say("sudo chmod 644 %s", shellQuote(newPlist))
	}
	say("# 4. Load it.")
	say("%slaunchctl bootstrap %s %s", sudo, domain, shellQuote(newPlist))
	say("# 5. Once it runs, remove the old property list.")
	say("%srm %s", sudo, shellQuote(p.Plist))
	say("")
	say("# To go back, if a step failed:")
	say("%slaunchctl bootout %s/%s", sudo, domain, newLabel)
	say("%srm -f %s", sudo, shellQuote(newPlist))
	for i := len(pairs) - 1; i >= 0; i-- {
		say("%smv %s %s", as, shellQuote(pairs[i].New), shellQuote(pairs[i].Old))
	}
	say("%slaunchctl bootstrap %s %s", sudo, domain, shellQuote(p.Plist))
	return out, nil
}

// pairs are the directories the service uses that the old name put where they
// are, and the logs of launchd, which sit under the home of the user.
func (p LaunchPlan) pairs(info svcunit.PlistInfo, home string) ([]Pair, error) {
	cfg, data := info.Env["CF_CONFIG_DIR"], info.Env["CF_DATA_DIR"]
	if home == "" && (cfg == "" || data == "") {
		return nil, errors.New("the property list says neither the directories of the service nor HOME, so there is no telling what to move")
	}
	if cfg == "" {
		cfg = filepath.Join(home, ".config", "codefabric")
	}
	if data == "" {
		data = filepath.Join(home, ".local", "share", "codefabric")
	}
	var pairs []Pair
	for _, dir := range []string{cfg, data} {
		if renamed := Renamed(dir); renamed != dir {
			pairs = append(pairs, Pair{Old: dir, New: renamed})
		}
	}
	if home != "" {
		logs := filepath.Join(home, "Library", "Logs")
		pairs = append(pairs, Pair{Old: filepath.Join(logs, "codefabric"), New: filepath.Join(logs, "codeflock")})
	}
	return pairs, nil
}

// rewritten is the new property list: the names, the variables and the places
// of the old one, in its own words.
func (p LaunchPlan) rewritten(pairs []Pair) []byte {
	text := strings.NewReplacer(
		"com.codefabric.", "com.codeflock.",
		"<key>CF_CONFIG_DIR</key>", "<key>CFLK_CONFIG_DIR</key>",
		"<key>CF_DATA_DIR</key>", "<key>CFLK_DATA_DIR</key>",
	).Replace(string(p.Content))
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
