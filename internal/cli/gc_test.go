package cli

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

func TestScenario_RetentionGc_StorageStatsTextShowsEveryCategoryAndWhatIsReclaimable(t *testing.T) {
	st := v1.StorageStats{
		OutputBlobBytes: 2048, WorkflowBlobBytes: 7, UnreferencedBlobBytes: 3 * 1024 * 1024, DatabaseBytes: 512, RelayBytes: 100,
		TotalBytes: 2048 + 7 + 3*1024*1024 + 512 + 100,
		Reclaimable: v1.GCReport{Items: []v1.GCCount{
			{Kind: "events", Count: 12}, {Kind: "blobs", Count: 4, Bytes: 3 * 1024 * 1024}, {Kind: "relay_refs", Count: 2},
		}},
	}
	var out bytes.Buffer
	if err := printStorageStats(&out, st); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"output blobs", "2.0 KiB", "workflow blobs", "7 B", "unreferenced blobs", "3.0 MiB", "database", "512 B",
		"git relay", "100 B", "total", "A GC pass would free:", "Would delete", "events", "12", "blobs", "relay_refs",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "max_storage") {
		t.Errorf("no max_storage is configured:\n%s", text)
	}
}

func TestScenario_RetentionGc_StorageStatsTextSaysWhenMaxStorageIsOver(t *testing.T) {
	var out bytes.Buffer
	if err := printStorageStats(&out, v1.StorageStats{TotalBytes: 2048, MaxStorageBytes: 1024, OverMaxStorage: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "max_storage 1.0 KiB: over it") {
		t.Errorf("output: %s", out.String())
	}
}

func TestScenario_RetentionGc_DryRunTextSaysNothingWasDeleted(t *testing.T) {
	rep := v1.GCReport{DryRun: true, Items: []v1.GCCount{{Kind: "blobs", Count: 1, Bytes: 17}}}
	var dry, applied bytes.Buffer
	if err := printGC(&dry, rep); err != nil {
		t.Fatal(err)
	}
	rep.DryRun = false
	if err := printGC(&applied, rep); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dry.String(), "Dry run: nothing was deleted.") || !strings.Contains(dry.String(), "Would delete") {
		t.Errorf("dry run: %s", dry.String())
	}
	if strings.Contains(applied.String(), "Dry run") || !strings.Contains(applied.String(), "Deleted") {
		t.Errorf("pass: %s", applied.String())
	}
}
