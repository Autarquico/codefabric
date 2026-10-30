package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestPrintProjectExplanation_ShowsEachFieldWithItsOrigin(t *testing.T) {
	var out strings.Builder
	err := printProjectExplanation(&out, v1.ProjectExplanation{
		Project: "backend", Commit: "abc", RepoConfig: domain.RepoConfigAllow, FilePresent: true,
		Fields: []v1.ExplainedField{
			{Name: "resources.cpu", Value: 2, Origin: domain.OriginRepo},
			{Name: "resources.memory_mb", Value: 4096, Origin: domain.OriginClamped},
			{Name: "mode", Value: "autonomous", Origin: domain.OriginGlobal},
			{Name: "workflow", Value: "", Origin: domain.OriginProject},
		},
		EffectiveDigest: "sha256:ff",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Project backend at abc", "repo_config allow", "resources.cpu", "2", "repo", "clamped", "global", "Effective digest: sha256:ff"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestPrintProjectExplanation_SaysWhyThereIsNoRepoLayer(t *testing.T) {
	for name, tc := range map[string]struct {
		e    v1.ProjectExplanation
		want string
	}{
		"deny":   {v1.ProjectExplanation{RepoConfig: domain.RepoConfigDeny, FilePresent: true, FileIgnored: true}, "present and ignored"},
		"absent": {v1.ProjectExplanation{RepoConfig: domain.RepoConfigAllow}, "has no .codefabric/project.yaml"},
	} {
		var out strings.Builder
		if err := printProjectExplanation(&out, tc.e); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: output lacks %q:\n%s", name, tc.want, out.String())
		}
	}
}

func TestReportProjectExplanation_AnInvalidFileFailsTheCommand(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().Bool(flagJSON, false, "")
	var out strings.Builder
	cmd.SetOut(&out)

	err := reportProjectExplanation(cmd, v1.ProjectExplanation{
		Invalid: &v1.ErrorBody{Code: domain.CodeRepoConfigInvalid, Message: "repo configuration: defaults.resources.gpu: unknown key"},
	})
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeRepoConfigInvalid || !strings.Contains(de.Message, "defaults.resources.gpu") {
		t.Fatalf("err = %v, want CF_REPO_CONFIG_INVALID naming the key", err)
	}
	if out.Len() != 0 {
		t.Errorf("human output on failure: %q", out.String())
	}
}
