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

// handtoKind is the handto skill of one harness: what is written, where that harness
// reads it, and the command that manages it.
type handtoKind struct {
	// command is the `cf` command that installs it (claude, codex); label how a person
	// calls the harness.
	command, label string
	template       []byte
	// skillsDir is where the harness reads skills, under the base of a scope.
	skillsDir string
}

var (
	claudeHandto = handtoKind{command: "claude", label: "Claude Code", template: handtoSkillTemplate, skillsDir: filepath.Join(".claude", "skills")}
	codexHandto  = handtoKind{command: "codex", label: "Codex", template: handtoCodexTemplate, skillsDir: filepath.Join(".agents", "skills")}
)

func (k handtoKind) installCommand() string { return "cf " + k.command + " install" }

const handtoVersionVar = "{{cf-version}}"

var (
	handtoVersionRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
	handtoMarkerRE  = regexp.MustCompile(`^<!-- cf-version: (\S+) -->\r?(\n|$)`)
)

// Skill states, as `cf claude status` and `cf doctor` report them.
const (
	skillMissing  = "missing"
	skillCurrent  = "current"  // written by this cf, byte for byte
	skillOutdated = "outdated" // written by cf, of another version
	skillModified = "modified" // written by this cf and edited since
	skillForeign  = "foreign"  // a handto skill that cf did not write
)

// HandtoSkill is the handto skill under a scope, as `cf claude` reports it.
type HandtoSkill struct {
	Scope   string `json:"scope"`
	Path    string `json:"path"`
	State   string `json:"state"`
	Action  string `json:"action,omitempty"`
	Version string `json:"version,omitempty"` // of the cf that wrote it, when it carries one
	DryRun  bool   `json:"dry_run,omitempty"`
}

// handtoMarkerVersion is the cf version in the marker, which sits on the line
// after the frontmatter. "" when the file does not have it there.
func handtoMarkerVersion(file []byte) string {
	rest, ok := bytes.CutPrefix(file, []byte("---\n"))
	if !ok {
		return ""
	}
	_, afterFrontmatter, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		return ""
	}
	if m := handtoMarkerRE.FindSubmatch(afterFrontmatter); m != nil {
		return string(m[1])
	}
	return ""
}

// renderHandto is the SKILL.md that the cf of this version writes.
func renderHandto(k handtoKind, version string) ([]byte, error) {
	if !handtoVersionRE.MatchString(version) {
		return nil, fmt.Errorf("version %q cannot be written into the skill", version)
	}
	return bytes.ReplaceAll(k.template, []byte(handtoVersionVar), []byte(version)), nil
}

func handtoPath(k handtoKind, base string) string {
	return filepath.Join(base, k.skillsDir, "handto", "SKILL.md")
}

// handtoStatus says how the skill stands under base for the cf of this version.
func handtoStatus(k handtoKind, scope, base, version string) (HandtoSkill, error) {
	want, err := renderHandto(k, version)
	if err != nil {
		return HandtoSkill{}, err
	}
	return classifyHandto(k, scope, base, version, want)
}

func classifyHandto(k handtoKind, scope, base, version string, want []byte) (HandtoSkill, error) {
	s := HandtoSkill{Scope: scope, Path: handtoPath(k, base)}
	have, err := readRegular(s.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.State = skillMissing
	case err != nil:
		return HandtoSkill{}, err
	case bytes.Equal(have, want):
		s.State, s.Version = skillCurrent, version
	default:
		s.Version = handtoMarkerVersion(have)
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

// installHandto writes the skill. It does nothing when it is current, updates
// one written by another cf and refuses one that cf did not write unless force.
func installHandto(k handtoKind, scope, base, version string, force, dryRun bool) (HandtoSkill, error) {
	want, err := renderHandto(k, version)
	if err != nil {
		return HandtoSkill{}, err
	}
	s, err := classifyHandto(k, scope, base, version, want)
	if err != nil {
		return HandtoSkill{}, err
	}
	s.DryRun = dryRun
	switch s.State {
	case skillCurrent:
		s.Action = "unchanged"
		return s, nil
	case skillForeign:
		if !force {
			return HandtoSkill{}, domain.NewError(domain.CodeConfirmationRequired,
				fmt.Sprintf("%s is a skill named handto that cf did not write; use --force to replace it", s.Path), nil)
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
		return HandtoSkill{}, err
	}
	s.State, s.Version = skillCurrent, version
	return s, nil
}

// uninstallHandto removes the skill that cf wrote. One that cf did not write stays.
func uninstallHandto(k handtoKind, scope, base string) (HandtoSkill, error) {
	s := HandtoSkill{Scope: scope, Path: handtoPath(k, base)}
	have, err := readRegular(s.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.State, s.Action = skillMissing, "absent"
	case err != nil:
		return HandtoSkill{}, err
	case handtoMarkerVersion(have) == "":
		s.State, s.Action = skillForeign, "kept"
	default:
		if _, err := removeSkill(s.Path); err != nil {
			return HandtoSkill{}, err
		}
		s.State, s.Action = skillMissing, "removed"
	}
	return s, nil
}

// InstallUserHandtoSkill is what `cf setup` offers a client: the skill under
// the home of the user. A handto skill that cf did not write is left alone.
func InstallUserHandtoSkill() (path string, err error) {
	base, err := scopeBase("user")
	if err != nil {
		return "", err
	}
	s, err := installHandto(claudeHandto, "user", base, buildinfo.Get().Version, false, false)
	return s.Path, err
}

// userHandtoSkill is the state of the skill under the home, or nil when it
// cannot be told (no home directory, an unreadable file).
func userHandtoSkill(k handtoKind) *HandtoSkill {
	base, err := scopeBase("user")
	if err != nil {
		return nil
	}
	s, err := handtoStatus(k, "user", base, buildinfo.Get().Version)
	if err != nil {
		return nil
	}
	return &s
}

func newClaudeCmd() *cobra.Command { return newHandtoCmd(claudeHandto) }

// newHandtoCmd is `cf claude` or `cf codex`: install, uninstall and status of the
// handto skill of that harness.
func newHandtoCmd(k handtoKind) *cobra.Command {
	cmd := &cobra.Command{Use: k.command, Short: "Install into " + k.label + " what cf needs there: the handto skill"}
	cmd.AddCommand(newHandtoInstallCmd(k), newHandtoUninstallCmd(k), newHandtoStatusCmd(k))
	return cmd
}

func bindScope(cmd *cobra.Command, scope *string) {
	cmd.Flags().StringVar(scope, "scope", "user", "user (this machine) or project (this repository)")
}

func (k handtoKind) base(scope string) (string, error) {
	if err := validScope(scope); err != nil {
		return "", err
	}
	return skillsBase(scope, k.skillsDir)
}

func (k handtoKind) invocation() string {
	if k.command == codexHandto.command {
		return "asking for it by name"
	}
	return "`/handto <worker>`"
}

func newHandtoInstallCmd(k handtoKind) *cobra.Command {
	var scope string
	var force, dryRun bool
	cmd := &cobra.Command{
		Use:   "install [--scope user|project] [--force] [--dry-run]",
		Short: "Write the handto skill into " + filepath.ToSlash(filepath.Join("~", k.skillsDir)) + " (or the same directory of the repository)",
		Long: "Write the handto skill, which hands a " + k.label + " session over to a Worker by " + k.invocation() + ".\n" +
			"It carries the version of this cf: the same version changes nothing and another one updates it.\n" +
			"A handto skill that cf did not write is not overwritten without --force.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			base, err := k.base(scope)
			if err != nil {
				return err
			}
			s, err := installHandto(k, scope, base, buildinfo.Get().Version, force, dryRun)
			if err != nil {
				return err
			}
			return emit(cmd, s, func(out io.Writer) error { return printHandtoSkill(out, k, s) })
		},
	}
	bindScope(cmd, &scope)
	cmd.Flags().BoolVar(&force, "force", false, "replace a handto skill that cf did not write")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "say what would be written without writing it")
	return cmd
}

func newHandtoUninstallCmd(k handtoKind) *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "uninstall [--scope user|project]",
		Short: "Remove the handto skill that cf wrote",
		Long:  "Remove the handto skill. One that cf did not write stays where it is.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			base, err := k.base(scope)
			if err != nil {
				return err
			}
			s, err := uninstallHandto(k, scope, base)
			if err != nil {
				return err
			}
			return emit(cmd, s, func(out io.Writer) error { return printHandtoSkill(out, k, s) })
		},
	}
	bindScope(cmd, &scope)
	return cmd
}

func newHandtoStatusCmd(k handtoKind) *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "status [--scope user|project]",
		Short: "Say whether the handto skill is installed and of which version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			base, err := k.base(scope)
			if err != nil {
				return err
			}
			s, err := handtoStatus(k, scope, base, buildinfo.Get().Version)
			if err != nil {
				return err
			}
			return emit(cmd, s, func(out io.Writer) error { return printHandtoSkill(out, k, s) })
		},
	}
	bindScope(cmd, &scope)
	return cmd
}

func printHandtoSkill(out io.Writer, k handtoKind, s HandtoSkill) error {
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
	_, err := fmt.Fprintf(out, "handto skill: %s  %s\n", verb, printable(s.Path))
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
