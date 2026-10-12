package relay

import (
	"errors"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	oidA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oidB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	zero = "0000000000000000000000000000000000000000"
)

type fakeGit struct {
	types map[string]string
	ff    bool
}

func (g fakeGit) ObjectType(oid string) (string, error) {
	if t, ok := g.types[oid]; ok {
		return t, nil
	}
	return "", errors.New("missing")
}
func (g fakeGit) IsAncestor(_, _ string) (bool, error) { return g.ff, nil }

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestPreReceiveAcceptsOnlyTheAuthorisedUpdate(t *testing.T) {
	const branch = "refs/heads/cf/auth-race/run-42"
	granted := map[string]string{envAllowedRef: branch, envExpectedOID: oidB}
	git := fakeGit{types: map[string]string{oidB: "commit", oidA: "commit"}, ff: true}

	for name, tc := range map[string]struct {
		env   map[string]string
		git   fakeGit
		input string
		deny  string // "" accepts; otherwise a part of the reason
	}{
		"creating the branch":        {granted, git, zero + " " + oidB + " " + branch + "\n", ""},
		"a fast-forward":             {granted, git, oidA + " " + oidB + " " + branch + "\n", ""},
		"main":                       {granted, git, zero + " " + oidB + " refs/heads/main\n", "may only update"},
		"the branch of another Run":  {granted, git, zero + " " + oidB + " refs/heads/cf/auth-race/run-43\n", "may only update"},
		"a tag":                      {granted, git, zero + " " + oidB + " refs/tags/v1\n", "may only update"},
		"the private refs":           {granted, git, zero + " " + oidB + " refs/cf/results/x\n", "may only update"},
		"a deletion":                 {granted, git, oidA + " " + zero + " " + branch + "\n", "deleting"},
		"another commit":             {granted, git, zero + " " + oidA + " " + branch + "\n", "only the commit sealed"},
		"not a commit":               {granted, fakeGit{types: map[string]string{oidB: "tree"}, ff: true}, zero + " " + oidB + " " + branch + "\n", "must point to a commit"},
		"an object that is missing":  {granted, fakeGit{ff: true}, zero + " " + oidB + " " + branch + "\n", "must point to a commit"},
		"a force-push":               {granted, fakeGit{types: git.types}, oidA + " " + oidB + " " + branch + "\n", "fast-forward"},
		"several refs":               {granted, git, zero + " " + oidB + " " + branch + "\n" + zero + " " + oidB + " refs/heads/main\n", "exactly one"},
		"no update":                  {granted, git, "", "exactly one"},
		"a malformed line":           {granted, git, "nonsense\n", "malformed"},
		"a malformed oid":            {granted, git, "xyz " + oidB + " " + branch + "\n", "malformed"},
		"no allowed ref (fail shut)": {map[string]string{envExpectedOID: oidB}, git, zero + " " + oidB + " " + branch + "\n", "not authorised"},
		"no expected oid":            {map[string]string{envAllowedRef: branch}, git, zero + " " + oidB + " " + branch + "\n", "not authorised"},
		"an allowed ref off heads":   {map[string]string{envAllowedRef: "refs/tags/x", envExpectedOID: oidB}, git, zero + " " + oidB + " refs/tags/x\n", "not authorised"},
		"main as the allowed ref":    {map[string]string{envAllowedRef: "refs/heads/main", envExpectedOID: oidB}, git, zero + " " + oidB + " refs/heads/main\n", "not authorised"},
	} {
		t.Run(name, func(t *testing.T) {
			err := PreReceive(strings.NewReader(tc.input), envOf(tc.env), tc.git)
			if tc.deny == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var coded *domain.Error
			if !errors.As(err, &coded) || coded.Code != domain.CodePolicyDenied || !strings.Contains(coded.Message, tc.deny) {
				t.Fatalf("got %v, want a denial mentioning %q", err, tc.deny)
			}
		})
	}
}

func TestHookScriptQuotesEveryWord(t *testing.T) {
	got := string(hookScript([]string{"/opt/it's here/cf", "relay", "hook", "pre-receive"}))
	want := "#!/bin/sh\nexec '/opt/it'\\''s here/cf' 'relay' 'hook' 'pre-receive'\n"
	if got != want {
		t.Errorf("script = %q, want %q", got, want)
	}
}

func TestPreReceiveOnlyCreatesTheRefOfAnOrphan(t *testing.T) {
	orphan := domain.OrphanRef("run_01J00000000000000000000000", 1)
	granted := map[string]string{envAllowedRef: orphan, envExpectedOID: oidB}
	git := fakeGit{types: map[string]string{oidB: "commit", oidA: "commit"}, ff: true}
	for name, tc := range map[string]struct {
		env   map[string]string
		input string
		deny  string
	}{
		"creating it":                    {granted, zero + " " + oidB + " " + orphan + "\n", ""},
		"moving it":                      {granted, oidA + " " + oidB + " " + orphan + "\n", "never moved"},
		"moving it without the flag":     {map[string]string{envAllowedRef: orphan, envExpectedOID: oidB}, oidA + " " + oidB + " " + orphan + "\n", "never moved"},
		"deleting it":                    {granted, oidB + " " + zero + " " + orphan + "\n", "deleting"},
		"another orphan":                 {granted, zero + " " + oidB + " " + domain.OrphanRef("run_01J00000000000000000000000", 2) + "\n", "may only update"},
		"another commit":                 {granted, zero + " " + oidA + " " + orphan + "\n", "only the commit"},
		"a ref under cf that is not one": {map[string]string{envAllowedRef: "refs/cf/orphans/x", envExpectedOID: oidB}, zero + " " + oidB + " refs/cf/orphans/x\n", "not authorised"},
	} {
		t.Run(name, func(t *testing.T) {
			err := PreReceive(strings.NewReader(tc.input), envOf(tc.env), git)
			if tc.deny == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var coded *domain.Error
			if !errors.As(err, &coded) || !strings.Contains(coded.Message, tc.deny) {
				t.Fatalf("got %v, want a denial mentioning %q", err, tc.deny)
			}
		})
	}
}
