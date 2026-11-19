package cli

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codeflock/internal/buildinfo"
	"github.com/Autarquico/codeflock/internal/domain"
)

//go:embed skills/handto/SKILL.md
var handtoSkillTemplate []byte

//go:embed skills/handto-codex/SKILL.md
var handtoCodexTemplate []byte

//go:embed skills/workers/SKILL.md
var workersSkillTemplate []byte

//go:embed skills/workers-codex/SKILL.md
var workersCodexTemplate []byte

// skillKind is one skill of one harness: what is written, where that harness reads
// it, and the command that manages it.
type skillKind struct {
	// name is the skill and its directory (handto, workers).
	name string
	// command is the `cf` command that installs it (claude, codex); label how a person
	// calls the harness.
	command, label string
	template       []byte
	// skillsDir is where the harness reads skills, under the base of a scope.
	skillsDir string
}

var (
	claudeSkillsDir = filepath.Join(".claude", "skills")
	codexSkillsDir  = filepath.Join(".agents", "skills")

	claudeHandto  = skillKind{name: "handto", command: "claude", label: "Claude Code", template: handtoSkillTemplate, skillsDir: claudeSkillsDir}
	claudeWorkers = skillKind{name: "workers", command: "claude", label: "Claude Code", template: workersSkillTemplate, skillsDir: claudeSkillsDir}
	codexHandto   = skillKind{name: "handto", command: "codex", label: "Codex", template: handtoCodexTemplate, skillsDir: codexSkillsDir}
	codexWorkers  = skillKind{name: "workers", command: "codex", label: "Codex", template: workersCodexTemplate, skillsDir: codexSkillsDir}

	claudeSkills = []skillKind{claudeHandto, claudeWorkers}
	codexSkills  = []skillKind{codexHandto, codexWorkers}
)

func (k skillKind) installCommand() string { return "cf " + k.command + " install" }

const skillVersionVar = "{{cf-version}}"

var (
	skillVersionRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
	skillMarkerRE  = regexp.MustCompile(`^<!-- cf-version: (\S+) -->\r?(\n|$)`)
)

// Skill states, as `cf claude status` and `cf doctor` report them.
const (
	skillMissing  = "missing"
	skillCurrent  = "current"  // written by this cf, byte for byte
	skillOutdated = "outdated" // written by cf, of another version
	skillModified = "modified" // written by this cf and edited since
	skillForeign  = "foreign"  // a skill of that name that cf did not write
)

// SkillStatus is a skill under a scope, as `cf claude` reports it.
type SkillStatus struct {
	Name    string `json:"name"`
	Scope   string `json:"scope"`
	Path    string `json:"path"`
	State   string `json:"state"`
	Action  string `json:"action,omitempty"`
	Version string `json:"version,omitempty"` // of the cf that wrote it, when it carries one
	DryRun  bool   `json:"dry_run,omitempty"`
	Reason  string `json:"reason,omitempty"` // why it was skipped
}

// skillMarkerVersion is the cf version in the marker, which sits on the line
// after the frontmatter. "" when the file does not have it there.
func skillMarkerVersion(file []byte) string {
	rest, ok := bytes.CutPrefix(file, []byte("---\n"))
	if !ok {
		return ""
	}
	_, afterFrontmatter, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		return ""
	}
	if m := skillMarkerRE.FindSubmatch(afterFrontmatter); m != nil {
		return string(m[1])
	}
	return ""
}

// renderSkill is the SKILL.md that the cf of this version writes.
func renderSkill(k skillKind, version string) ([]byte, error) {
	if !skillVersionRE.MatchString(version) {
		return nil, fmt.Errorf("version %q cannot be written into the skill", version)
	}
	return bytes.ReplaceAll(k.template, []byte(skillVersionVar), []byte(version)), nil
}

func skillPath(k skillKind, base string) string {
	return filepath.Join(base, k.skillsDir, k.name, "SKILL.md")
}

// skillStatus says how the skill stands under base for the cf of this version.
func skillStatus(k skillKind, scope, base, version string) (SkillStatus, error) {
	want, err := renderSkill(k, version)
	if err != nil {
		return SkillStatus{}, err
	}
	return classifySkill(k, scope, base, version, want)
}

func classifySkill(k skillKind, scope, base, version string, want []byte) (SkillStatus, error) {
	s := SkillStatus{Name: k.name, Scope: scope, Path: skillPath(k, base)}
	have, err := readRegular(s.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.State = skillMissing
	case err != nil:
		return SkillStatus{}, err
	case bytes.Equal(have, want):
		s.State, s.Version = skillCurrent, version
	default:
		s.Version = skillMarkerVersion(have)
		switch s.Version {
		case "":
			s.State = skillForeign
		case version:
			s.State = skillModified
		default:
			s.State = skillOutdated
		}
	}
	return s, nil
}

// installSkill writes the skill. It does nothing when it is current, updates
// one written by another cf and refuses one that cf did not write unless force:
// then the status comes back as skipped together with the error.
func installSkill(k skillKind, scope, base, version string, force, dryRun bool) (SkillStatus, error) {
	want, err := renderSkill(k, version)
	if err != nil {
		return SkillStatus{}, err
	}
	s, err := classifySkill(k, scope, base, version, want)
	if err != nil {
		return SkillStatus{}, err
	}
	s.DryRun = dryRun
	switch s.State {
	case skillCurrent:
		s.Action = "unchanged"
		return s, nil
	case skillForeign:
		if !force {
			s.Action = "skipped"
			s.Reason = fmt.Sprintf("%s is a skill named %s that cf did not write; use --force to replace it", s.Path, k.name)
			return s, domain.NewError(domain.CodeConfirmationRequired, s.Reason, nil)
		}
		s.Action = "replaced"
	case skillOutdated:
		s.Action = "updated"
	case skillModified:
		s.Action = "restored"
	default:
		s.Action = "created"
	}
	if dryRun {
		return s, nil
	}
	if _, err := writeSkill(s.Path, want); err != nil {
		return SkillStatus{}, err
	}
	s.State, s.Version = skillCurrent, version
	return s, nil
}

// uninstallSkill removes the skill that cf wrote. One that cf did not write stays.
func uninstallSkill(k skillKind, scope, base string) (SkillStatus, error) {
	s := SkillStatus{Name: k.name, Scope: scope, Path: skillPath(k, base)}
	have, err := readRegular(s.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.State, s.Action = skillMissing, "absent"
	case err != nil:
		return SkillStatus{}, err
	case skillMarkerVersion(have) == "":
		s.State, s.Action = skillForeign, "kept"
	default:
		if _, err := removeSkill(s.Path); err != nil {
			return SkillStatus{}, err
		}
		s.State, s.Action = skillMissing, "removed"
	}
	return s, nil
}

// InstallUserSkills is what `cf setup` offers a client: the skills of Claude Code under
// the home of the user. A skill that cf did not write is left alone. Every skill is tried;
// the statuses say what happened to each and the error is the first one that occurred.
func InstallUserSkills() ([]SkillStatus, error) {
	base, err := scopeBase("user")
	if err != nil {
		return nil, err
	}
	return installEach(claudeSkills, "user", base, false, false)
}

// installEach installs every skill even when one fails, and returns the statuses
// of the ones that have one together with the first error.
func installEach(skills []skillKind, scope, base string, force, dryRun bool) ([]SkillStatus, error) {
	var first error
	results := make([]SkillStatus, 0, len(skills))
	for _, k := range skills {
		s, err := installSkill(k, scope, base, buildinfo.Get().Version, force, dryRun)
		if s.Name != "" {
			results = append(results, s)
		}
		if err != nil && first == nil {
			first = err
		}
	}
	return results, first
}

// userSkill is the state of the skill under the home, or nil when it
// cannot be told (no home directory, an unreadable file).
func userSkill(k skillKind) *SkillStatus {
	base, err := scopeBase("user")
	if err != nil {
		return nil
	}
	s, err := skillStatus(k, "user", base, buildinfo.Get().Version)
	if err != nil {
		return nil
	}
	return &s
}

func newClaudeCmd() *cobra.Command { return newSkillsCmd(claudeSkills, true) }
func newCodexCmd() *cobra.Command  { return newSkillsCmd(codexSkills, false) }

// newSkillsCmd is `cf claude` or `cf codex`: install, uninstall and status of the
// skills of that harness, which share its command, label and directory.
// withStatusLine adds what only Claude Code has: its status line.
func newSkillsCmd(skills []skillKind, withStatusLine bool) *cobra.Command {
	h := skills[0]
	cmd := &cobra.Command{Use: h.command, Short: "Install into " + h.label + " what cf needs there: the " + skillNames(skills) + " skills"}
	cmd.AddCommand(newSkillInstallCmd(skills, withStatusLine), newSkillUninstallCmd(skills), newSkillStatusCmd(skills))
	if withStatusLine {
		cmd.AddCommand(newClaudeStatusLineCmd())
	}
	return cmd
}

// skillNames reads "handto and workers", "a, b and c".
func skillNames(skills []skillKind) string {
	names := make([]string, len(skills))
	for i, k := range skills {
		names[i] = k.name
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func bindScope(cmd *cobra.Command, scope *string) {
	cmd.Flags().StringVar(scope, "scope", "user", "user (this machine) or project (this repository)")
}

func (k skillKind) base(scope string) (string, error) {
	if err := validScope(scope); err != nil {
		return "", err
	}
	return skillsBase(scope, k.skillsDir)
}

func (k skillKind) invocation() string {
	if k.command == codexHandto.command {
		return "asking for it by name"
	}
	return "`/handto <worker>`"
}

// forEachSkill applies one operation to each skill of a harness and emits the
// results together, in the order of the skills. A skill that fails does not stop
// the others; it is reported when it has a status, and the first error is returned
// after the emission.
func forEachSkill(cmd *cobra.Command, skills []skillKind, scope string, do func(skills []skillKind, base string) ([]SkillStatus, error)) error {
	base, err := skills[0].base(scope)
	if err != nil {
		return err
	}
	results, first := do(skills, base)
	byName := make(map[string]skillKind, len(skills))
	for _, k := range skills {
		byName[k.name] = k
	}
	if err := emit(cmd, results, func(out io.Writer) error {
		for _, s := range results {
			if err := printSkill(out, byName[s.Name], s); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return first
}

// eachSkill lifts an operation on one skill to all of them, continuing after an error.
func eachSkill(op func(k skillKind, base string) (SkillStatus, error)) func([]skillKind, string) ([]SkillStatus, error) {
	return func(skills []skillKind, base string) ([]SkillStatus, error) {
		var first error
		results := make([]SkillStatus, 0, len(skills))
		for _, k := range skills {
			s, err := op(k, base)
			if s.Name != "" {
				results = append(results, s)
			}
			if err != nil && first == nil {
				first = err
			}
		}
		return results, first
	}
}

func newSkillInstallCmd(skills []skillKind, withStatusLine bool) *cobra.Command {
	h := skills[0]
	var scope string
	var force, dryRun, statusLine bool
	use := "install [--scope user|project] [--force] [--dry-run]"
	if withStatusLine {
		use += " [--statusline]"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: "Write the " + skillNames(skills) + " skills into " + filepath.ToSlash(filepath.Join("~", h.skillsDir)) + " (or the same directory of the repository)",
		Long: "Write the handto skill, which hands a " + h.label + " session over to a Worker by " + h.invocation() + ", and the workers skill, which lists the Workers that can take it.\n" +
			"They carry the version of this cf: the same version changes nothing and another one updates them.\n" +
			"A skill of those names that cf did not write is not overwritten without --force.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			err := forEachSkill(cmd, skills, scope, func(skills []skillKind, base string) ([]SkillStatus, error) {
				return installEach(skills, scope, base, force, dryRun)
			})
			if err != nil || !statusLine {
				return err
			}
			return installStatusLineFor(cmd, h, scope, dryRun)
		},
	}
	bindScope(cmd, &scope)
	cmd.Flags().BoolVar(&force, "force", false, "replace a skill that cf did not write")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "say what would be written without writing it")
	if withStatusLine {
		cmd.Flags().BoolVar(&statusLine, "statusline", false, "also set the status line of Claude Code to show the Runs on Workers, when it has none")
	}
	return cmd
}

func newSkillUninstallCmd(skills []skillKind) *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "uninstall [--scope user|project]",
		Short: "Remove the skills that cf wrote",
		Long:  "Remove the " + skillNames(skills) + " skills. One that cf did not write stays where it is.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return forEachSkill(cmd, skills, scope, eachSkill(func(k skillKind, base string) (SkillStatus, error) {
				return uninstallSkill(k, scope, base)
			}))
		},
	}
	bindScope(cmd, &scope)
	return cmd
}

func newSkillStatusCmd(skills []skillKind) *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "status [--scope user|project]",
		Short: "Say whether the skills are installed and of which version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return forEachSkill(cmd, skills, scope, eachSkill(func(k skillKind, base string) (SkillStatus, error) {
				return skillStatus(k, scope, base, buildinfo.Get().Version)
			}))
		},
	}
	bindScope(cmd, &scope)
	return cmd
}

func printSkill(out io.Writer, k skillKind, s SkillStatus) error {
	verb := s.Action
	switch {
	case verb == "":
		verb = s.State
		if s.Version != "" {
			verb += " (cf " + s.Version + ")"
		}
	case s.DryRun && s.Action != "unchanged" && s.Action != "skipped":
		verb = "would be " + verb
	}
	_, err := fmt.Fprintf(out, "%s skill: %s  %s\n", k.name, verb, printable(s.Path))
	if err != nil {
		return err
	}
	if s.Reason != "" {
		_, err = fmt.Fprintln(out, s.Reason)
		return err
	}
	if s.Action != "" {
		return nil
	}
	switch s.State {
	case skillOutdated:
		_, err = fmt.Fprintf(out, "It is of another version: run `%s` to update it.\n", k.installCommand())
	case skillModified:
		_, err = fmt.Fprintf(out, "It was edited since cf wrote it: run `%s` to restore it.\n", k.installCommand())
	}
	return err
}

// installStatusLineFor writes the status line of the scope. With --json the note goes
// to stderr so that stdout stays the one JSON value of the skills.
func installStatusLineFor(cmd *cobra.Command, h skillKind, scope string, dryRun bool) error {
	base, err := skillsBase(scope, filepath.Dir(h.skillsDir))
	if err != nil {
		return err
	}
	res, err := installStatusLine(settingsPath(base), dryRun)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if on, _ := cmd.Flags().GetBool(flagJSON); on {
		out = cmd.ErrOrStderr()
	}
	return printStatusLine(out, res)
}
