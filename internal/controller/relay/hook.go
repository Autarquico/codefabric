package relay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/domain"
)

// The environment the relay gives the hook (cgi.go). It is the only source of
// what a push may do: the pushed commands are only what the peer asks for.
const (
	envAllowedRef  = "CF_RELAY_ALLOWED_REF"
	envExpectedOID = "CF_RELAY_EXPECTED_OID"
	// envCreateOnly says the ref may only be created, never moved: the ref of an orphan.
	envCreateOnly = "CF_RELAY_CREATE_ONLY"
)

var zeroOID = regexp.MustCompile(`^0{40}(?:0{24})?$`)

// Git is how the hook asks the repository questions. The real one runs git in
// the environment the hook was given (quarantine included).
type Git interface {
	ObjectType(oid string) (string, error)
	IsAncestor(ancestor, descendant string) (bool, error)
}

// PreReceive decides a push. The stdin is the list of updates `<old> <new> <ref>`
// that git hands to the hook. The push is refused as a whole, with the reason
// in the error, unless it is exactly one update: of the one ref the request was
// authorised for, to the one commit it was authorised to set, as a fast-forward.
// Anything missing from the environment refuses: the hook fails closed.
func PreReceive(stdin io.Reader, getenv func(string) string, git Git) error {
	allowed, expected := getenv(envAllowedRef), getenv(envExpectedOID)
	if allowed == "" || expected == "" {
		return deny("this request is not authorised to push")
	}
	// The ref of an orphan is only ever created; a branch moves by fast-forwards.
	branch, isBranch := strings.CutPrefix(allowed, "refs/heads/")
	createOnly := getenv(envCreateOnly) == "1" || !isBranch
	switch {
	case !domain.ValidCommitID(expected):
		return deny("this request is not authorised to push")
	case isBranch && !domain.ValidBranch(branch):
		return deny("this request is not authorised to push")
	case !isBranch && !domain.ValidOrphanRef(allowed):
		return deny("this request is not authorised to push")
	case !isBranch:
		branch = allowed
	}
	var updates [][3]string
	sc := bufio.NewScanner(io.LimitReader(stdin, 1<<20))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 3 {
			return deny("malformed update")
		}
		updates = append(updates, [3]string{fields[0], fields[1], fields[2]})
	}
	if err := sc.Err(); err != nil {
		return deny("malformed update")
	}
	if len(updates) != 1 {
		return deny("a push updates exactly one ref: " + branch)
	}
	oldOID, newOID, ref := updates[0][0], updates[0][1], updates[0][2]
	switch {
	case ref != allowed:
		return deny("this push may only update " + branch)
	case !domain.ValidCommitID(oldOID) || !domain.ValidCommitID(newOID):
		return deny("malformed update")
	case zeroOID.MatchString(newOID):
		return deny("deleting " + branch + " is not allowed")
	case newOID != expected:
		return deny("only the commit sealed for this Run may be pushed to " + branch)
	}
	if typ, err := git.ObjectType(newOID); err != nil || typ != "commit" {
		return deny(branch + " must point to a commit")
	}
	if createOnly && !zeroOID.MatchString(oldOID) {
		return deny(branch + " exists: it is never moved")
	}
	if !zeroOID.MatchString(oldOID) {
		ff, err := git.IsAncestor(oldOID, newOID)
		if err != nil {
			return fmt.Errorf("relay hook: checking the history: %w", err)
		}
		if !ff {
			return deny("updating " + branch + " would not be a fast-forward")
		}
	}
	return nil
}

func deny(why string) error {
	return domain.NewError(domain.CodePolicyDenied, "rejected by the CodeFabric relay: "+why, nil)
}

// repoGit asks the repository the hook runs in.
type repoGit struct {
	ctx  context.Context
	path string
}

func (g repoGit) run(args ...string) (string, int, error) {
	cmd := exec.CommandContext(g.ctx, g.path, args...) //nolint:gosec // the git on the hook's PATH, with arguments built here
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return strings.TrimSpace(string(out)), exit.ExitCode(), nil
	}
	return strings.TrimSpace(string(out)), 0, err
}

func (g repoGit) ObjectType(oid string) (string, error) {
	out, code, err := g.run("cat-file", "-t", oid)
	if err != nil || code != 0 {
		return "", errors.New("unknown object")
	}
	return out, nil
}

func (g repoGit) IsAncestor(ancestor, descendant string) (bool, error) {
	_, code, err := g.run("merge-base", "--is-ancestor", ancestor, descendant)
	switch {
	case err != nil:
		return false, err
	case code == 0:
		return true, nil
	case code == 1:
		return false, nil
	}
	return false, fmt.Errorf("git merge-base exited %d", code)
}

// Command is `cf relay`: the entry points git calls into the binary. They are
// not for people, and hidden.
func Command() *cobra.Command {
	hook := &cobra.Command{Use: "hook", Short: "Hooks of the git relay", Hidden: true}
	hook.AddCommand(&cobra.Command{
		Use:    "pre-receive",
		Short:  "Refuse every push but the one authorised for the request",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			git, err := exec.LookPath("git")
			if err != nil {
				return fmt.Errorf("relay hook: git is not on the PATH: %w", err)
			}
			cmd.SilenceUsage = true
			return PreReceive(cmd.InOrStdin(), os.Getenv, repoGit{ctx: cmd.Context(), path: git})
		},
	})
	root := &cobra.Command{Use: "relay", Short: "Internal commands of the git relay", Hidden: true}
	root.AddCommand(hook)
	return root
}
