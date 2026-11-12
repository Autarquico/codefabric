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

func TestPreReceiveOnlyCreatesTheRefOfACheckpoint(t *testing.T) {
	cp := domain.CheckpointRef("cp_01J00000000000000000000000")
	other := domain.CheckpointRef("cp_01J00000000000000000000001")
	granted := map[string]string{envAllowedRef: cp, envExpectedOID: oidB}
	git := fakeGit{types: map[string]string{oidB: "commit", oidA: "commit"}, ff: true}
	bad := func(ref string) map[string]string { return map[string]string{envAllowedRef: ref, envExpectedOID: oidB} }
	for name, tc := range map[string]struct {
		env   map[string]string
		input string
		deny  string
	}{
		"creating it":                {granted, zero + " " + oidB + " " + cp + "\n", ""},
		"moving it":                  {granted, oidA + " " + oidB + " " + cp + "\n", "never moved"},
		"deleting it":                {granted, oidB + " " + zero + " " + cp + "\n", "deleting"},
		"another checkpoint":         {granted, zero + " " + oidB + " " + other + "\n", "may only update"},
		"another commit":             {granted, zero + " " + oidA + " " + cp + "\n", "only the commit"},
		"two refs at once":           {granted, zero + " " + oidB + " " + cp + "\n" + zero + " " + oidB + " " + other + "\n", "exactly one ref"},
		"a branch with it granted":   {granted, zero + " " + oidB + " refs/heads/cf/t/r\n", "may only update"},
		"no grant":                   {map[string]string{}, zero + " " + oidB + " " + cp + "\n", "not authorised"},
		"a path out of the prefix":   {bad("refs/cf/checkpoints/../x"), zero + " " + oidB + " refs/cf/checkpoints/../x\n", "not authorised"},
		"an empty id":                {bad("refs/cf/checkpoints/"), zero + " " + oidB + " refs/cf/checkpoints/\n", "not authorised"},
		"a malformed id":             {bad("refs/cf/checkpoints/cp_x"), zero + " " + oidB + " refs/cf/checkpoints/cp_x\n", "not authorised"},
		"an id of another kind":      {bad("refs/cf/checkpoints/run_01J00000000000000000000000"), zero + " " + oidB + " refs/cf/checkpoints/run_01J00000000000000000000000\n", "not authorised"},
		"a ref of the prefix itself": {bad("refs/cf/checkpoints"), zero + " " + oidB + " refs/cf/checkpoints\n", "not authorised"},
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

func TestPreReceiveSnapshotsOnlyCreatesTheRefOfItsDigest(t *testing.T) {
	snapshots := map[string]string{envSnapshots: "1"}
	repo := fakeGit{types: map[string]string{oidA: "commit", oidB: "tree"}}
	ref := domain.SnapshotRef(oidA)

	for name, tc := range map[string]struct {
		env   map[string]string
		input string
		deny  string
	}{
		"the snapshot":              {snapshots, zero + " " + oidA + " " + ref + "\n", ""},
		"another digest":            {snapshots, zero + " " + oidA + " " + domain.SnapshotRef(oidB) + "\n", "ID of its commit"},
		"main":                      {snapshots, zero + " " + oidA + " refs/heads/main\n", "may only create"},
		"a checkpoint":              {snapshots, zero + " " + oidA + " refs/cf/checkpoints/x\n", "may only create"},
		"a branch of a Task":        {snapshots, zero + " " + oidA + " refs/heads/cf/a/b\n", "may only create"},
		"moving a snapshot":         {snapshots, oidB + " " + oidA + " " + ref + "\n", "never moved"},
		"deleting a snapshot":       {snapshots, oidA + " " + zero + " " + ref + "\n", "never moved"},
		"not a commit":              {snapshots, zero + " " + oidB + " " + domain.SnapshotRef(oidB) + "\n", "must point to a commit"},
		"several refs":              {snapshots, zero + " " + oidA + " " + ref + "\n" + zero + " " + oidA + " refs/heads/main\n", "exactly one"},
		"not asked for (fail shut)": {nil, zero + " " + oidA + " " + ref + "\n", "not authorised"},
	} {
		t.Run(name, func(t *testing.T) {
			err := PreReceive(strings.NewReader(tc.input), envOf(tc.env), repo)
			switch {
			case tc.deny == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.deny != "" && (err == nil || !strings.Contains(err.Error(), tc.deny)):
				t.Fatalf("err = %v, want it to contain %q", err, tc.deny)
			}
		})
	}
}
