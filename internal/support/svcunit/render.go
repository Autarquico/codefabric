package svcunit

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/xml"
	"fmt"
	"strings"
	"text/template"
)

//go:embed templates/*.tmpl
var templateFiles embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"specifiers": specifiers,
	"execArg":    execArg,
	"envArg":     envArg,
	"xml":        xmlText,
}).ParseFS(templateFiles, "templates/*.tmpl"))

// renderSystemd is the unit file of s.
func renderSystemd(s Spec) ([]byte, error) {
	return render("systemd.service.tmpl", s)
}

// renderLaunchd is the property list of the agent of s, which logs to log.
func renderLaunchd(s Spec, log string) ([]byte, error) {
	if err := printable(log); err != nil {
		return nil, err
	}
	return render("launchd.plist.tmpl", struct {
		Spec Spec
		Log  string
	}{s, log})
}

func render(name string, data any) ([]byte, error) {
	var out bytes.Buffer
	if err := templates.ExecuteTemplate(&out, name, data); err != nil {
		return nil, fmt.Errorf("svcunit: rendering %s: %w", name, err)
	}
	return out.Bytes(), nil
}

// specifiers escapes the '%' that systemd would read as a specifier.
func specifiers(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// envArg is a whole Environment= assignment, quoted: systemd unescapes '\' and
// '"' and expands specifiers in it, but not variables.
func envArg(key, value string) string {
	return `"` + specifiers(cEscape(key+"="+value)) + `"`
}

// execArg is an argument of ExecStart=, quoted: there systemd expands
// variables too, so '$' is doubled.
func execArg(arg string) string {
	return `"` + strings.ReplaceAll(specifiers(cEscape(arg)), "$", "$$") + `"`
}

func cEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

func xmlText(s string) (string, error) {
	var out strings.Builder
	if err := xml.EscapeText(&out, []byte(s)); err != nil {
		return "", err
	}
	return out.String(), nil
}

// systemdEnv reads the Environment= lines of a unit this package wrote.
func systemdEnv(unit []byte) map[string]string {
	env := map[string]string{}
	lines := bufio.NewScanner(bytes.NewReader(unit))
	for lines.Scan() {
		quoted, found := strings.CutPrefix(lines.Text(), "Environment=")
		if !found || len(quoted) < 2 || quoted[0] != '"' || quoted[len(quoted)-1] != '"' {
			continue
		}
		unquoted := strings.NewReplacer(`\\`, `\`, `\"`, `"`, "%%", "%").Replace(quoted[1 : len(quoted)-1])
		if key, value, ok := strings.Cut(unquoted, "="); ok {
			env[key] = value
		}
	}
	return env
}

// plistNode is an element of a property list, kept in document order.
type plistNode struct {
	XMLName  xml.Name
	Text     string      `xml:",chardata"`
	Children []plistNode `xml:",any"`
}

// launchdEnv reads the EnvironmentVariables of a property list.
func launchdEnv(plist []byte) (map[string]string, error) {
	var doc struct {
		Dict plistNode `xml:"dict"`
	}
	if err := xml.Unmarshal(plist, &doc); err != nil {
		return nil, fmt.Errorf("svcunit: reading the property list: %w", err)
	}
	env := map[string]string{}
	top := doc.Dict.Children
	for i := 0; i+1 < len(top); i++ {
		if top[i].XMLName.Local != "key" || top[i].Text != "EnvironmentVariables" || top[i+1].XMLName.Local != "dict" {
			continue
		}
		vars := top[i+1].Children
		for j := 0; j+1 < len(vars); j += 2 {
			env[vars[j].Text] = vars[j+1].Text
		}
	}
	return env, nil
}
