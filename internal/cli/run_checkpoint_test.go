package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestPrintCheckpoints_ShowsNativeStateAndWhatWasLeftOut(t *testing.T) {
	var out bytes.Buffer
	err := printCheckpoints(&out, []v1.Checkpoint{
		{
			ID: "cp1", RunID: "run1", Seq: 1, Trigger: domain.CheckpointManual, Excluded: []string{".env", "id.pem"}, CreatedAt: time.Unix(0, 0),
			NativeState: &v1.NativeStateRef{Adapter: "claude", AdapterVersion: "2.1", Digest: "sha256:x"},
		},
		{ID: "cp2", RunID: "run1", Seq: 2, Trigger: domain.CheckpointDefer, Excluded: []string{}, CreatedAt: time.Unix(0, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"claude 2.1", "Left out of cp1: .env, id.pem"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Left out of cp2") {
		t.Errorf("a checkpoint that left nothing out says so:\n%s", got)
	}
}

func TestRunCheckpointList_NamesARunOrATaskNotBothNorNeither(t *testing.T) {
	for _, args := range [][]string{{}, {"run1", "--task", "t"}} {
		cmd := newRunCheckpointListCmd()
		cmd.SetArgs(args)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not both") {
			t.Errorf("args %v: err = %v", args, err)
		}
	}
}
