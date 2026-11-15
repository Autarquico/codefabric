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

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/buildinfo"
	"github.com/Autarquico/codefabric/internal/domain"
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
// one written by another cf and refuses one that cf did not write unless force.
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
			return SkillStatus{}, domain.NewError(domain.CodeConfirmationRequired,
				fmt.Sprintf("%s is a skill named %s that cf did not write; use --force to replace it", s.Path, k.name), nil)
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

// InstallUserHandtoSkill is what `cf setup` offers a client: the skills of Claude Code
// under the home of the user, and the path of the handto one. A skill that cf did not
// write is left alone.
func InstallUserHandtoSkill() (path string, err error) {
	base, err := scopeBase("user")
	if err != nil {
		return "", err
	}
	for _, k := range claudeSkills {
		s, err := installSkill(k, "user", base, buildinfo.Get().Version, false, false)
		if err != nil {
			return "", err
		}
		if k.name == claudeHandto.name {
			path = s.Path
		}
	}
	return path, nil
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

func newClaudeCmd() *cobra.Command { return newSkillsCmd(claudeSkills) }
func newCodexCmd() *cobra.Command  { return newSkillsCmd(codexSkills) }

// newSkillsCmd is `cf claude` or `cf codex`: install, uninstall and status of the
// skills of that harness, which share its command, label and directory.
func newSkillsCmd(skills []skillKind) *cobra.Command {
	h := skills[0]
	cmd := &cobra.Command{Use: h.command, Short: "Install into " + h.label + " what cf needs there: the handto and workers skills"}
	cmd.AddCommand(newSkillInstallCmd(skills), newSkillUninstallCmd(skills), newSkillStatusCmd(skills))
	return cmd
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
// results together, in the order of the skills. It stops at the first error.
func forEachSkill(cmd *cobra.Command, skills []skillKind, scope string, do func(k skillKind, base string) (SkillStatus, error)) error {
	base, err := skills[0].base(scope)
	if err != nil {
		return err
	}
	results := make([]SkillStatus, 0, len(skills))
	for _, k := range skills {
		s, err := do(k, base)
		if err != nil {
			return err
		}
		results = append(results, s)
	}
	return emit(cmd, results, func(out io.Writer) error {
		for i, s := range results {
			if err := printSkill(out, skills[i], s); err != nil {
				return err
			}
		}
		return nil
	})
}

func newSkillInstallCmd(skills []skillKind) *cobra.Command {
	h := skills[0]
	var scope string
	var force, dryRun bool
	cmd := &cobra.Command{
		Use:   "install [--scope user|project] [--force] [--dry-run]",
		Short: "Write the handto and workers skills into " + filepath.ToSlash(filepath.Join("~", h.skillsDir)) + " (or the same directory of the repository)",
		Long: "Write the handto skill, which hands a " + h.label + " session over to a Worker by " + h.invocation() + ", and the workers skill, which lists the Workers that can take it.\n" +
			"They carry the version of this cf: the same version changes nothing and another one updates them.\n" +
			"A skill of those names that cf did not write is not overwritten without --force.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return forEachSkill(cmd, skills, scope, func(k skillKind, base string) (SkillStatus, error) {
				return installSkill(k, scope, base, buildinfo.Get().Version, force, dryRun)
			})
		},
	}
	bindScope(cmd, &scope)
	cmd.Flags().BoolVar(&force, "force", false, "replace a skill that cf did not write")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "say what would be written without writing it")
	return cmd
}

func newSkillUninstallCmd(skills []skillKind) *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "uninstall [--scope user|project]",
		Short: "Remove the skills that cf wrote",
		Long:  "Remove the handto and workers skills. One that cf did not write stays where it is.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return forEachSkill(cmd, skills, scope, func(k skillKind, base string) (SkillStatus, error) {
				return uninstallSkill(k, scope, base)
			})
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
			return forEachSkill(cmd, skills, scope, func(k skillKind, base string) (SkillStatus, error) {
				return skillStatus(k, scope, base, buildinfo.Get().Version)
			})
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
	case s.DryRun && s.Action != "unchanged":
		verb = "would be " + verb
	}
	_, err := fmt.Fprintf(out, "%s skill: %s  %s\n", k.name, verb, printable(s.Path))
	if err != nil || s.Action != "" {
		return err
	}
	switch s.State {
	case skillOutdated:
		_, err = fmt.Fprintf(out, "It is of another version: run `%s` to update it.\n", k.installCommand())
	case skillModified:
		_, err = fmt.Fprintf(out, "It was edited since cf wrote it: run `%s` to restore it.\n", k.installCommand())
	}
	return err
}
