package daemon_test

import (
	"bytes"
	"crypto/rand"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/controller/relay"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/gittest"
)

type plantedCheckpoint struct {
	id      domain.ID
	ref     string
	commit  string
	digest  blob.Digest
	content string
}

// plantCheckpoint gives a Controller that is not running a published checkpoint
// with its three parts: the row, the ref of its source in the relay, and the blob
// of its native state.
func plantCheckpoint(t *testing.T, p config.Paths) plantedCheckpoint {
	t.Helper()
	gittest.Require(t)
	clk := clock.NewFake(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	ids := domain.NewIDGenerator(time.Now, rand.Reader)
	project, task, run, cp := ids.New(domain.KindProject), ids.New(domain.KindTask), ids.New(domain.KindRun), ids.New(domain.KindCheckpoint)

	blobs, err := blob.New(p.DataDir, clk)
	if err != nil {
		t.Fatal(err)
	}
	const content = "native state of the session"
	digest, _, err := blobs.Put(t.Context(), strings.NewReader(content), "")
	if err != nil {
		t.Fatal(err)
	}

	rel, err := relay.Open(t.Context(), relay.Config{Dir: p.RelayDir(), HookArgv: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := rel.Ensure(t.Context(), project)
	if err != nil {
		t.Fatal(err)
	}
	work := gittest.InitRepo(t)
	commit := gittest.Commit(t, work, "f", "captured worktree", "checkpoint")
	gittest.Run(t, repo, "fetch", "--quiet", work, commit)
	ref := domain.CheckpointRef(cp)
	gittest.Run(t, repo, "update-ref", ref, commit)

	db, err := store.Open(t.Context(), p.DatabaseFile(), store.Options{Clock: clk, Blobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WithTx(t.Context(), func(tx *store.Tx) error {
		ctx := t.Context()
		if err := tx.InsertProject(ctx, store.Project{ID: project, Short: 1, Name: "p", Repo: "r", DefaultBranch: "main", CreatedAt: clk.Now()}); err != nil {
			return err
		}
		if err := tx.InsertTask(ctx, store.Task{
			ID: task, Short: 1, ProjectID: project, Name: "t", Prompt: "p",
			State: domain.TaskQueued, CreatedBy: domain.CreatorHuman, CreatedAt: clk.Now(),
		}); err != nil {
			return err
		}
		if err := tx.InsertRun(ctx, store.Run{
			ID: run, Short: 1, TaskID: task, Attempt: 1, Lineage: domain.LineageInitial,
			Spec: []byte(`{}`), SpecDigest: "d", SourceCommit: "c", State: domain.RunQueued, Publication: domain.PublicationNone,
			Evaluation: domain.EvaluationNone, Runtime: domain.RuntimeNone, CreatedAt: clk.Now(),
		}); err != nil {
			return err
		}
		if _, err := tx.InsertCheckpoint(ctx, store.Checkpoint{Checkpoint: domain.Checkpoint{
			ID: cp, RunID: run, Trigger: domain.CheckpointManual, SourceRef: ref, CreatedAt: clk.Now(),
		}}); err != nil {
			return err
		}
		native := &domain.NativeState{Adapter: "claude", AdapterVersion: "1", Digest: string(digest)}
		return tx.PublishCheckpoint(ctx, run, cp, native, nil, domain.CheckpointSummary{Progress: "halfway"}, clk.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return plantedCheckpoint{id: cp, ref: ref, commit: commit, digest: digest, content: content}
}

func TestScenario_BackupAndRestoreCarryCheckpointsTheirRefsAndNativeState(t *testing.T) {
	src := newPaths(t)
	initController(t, src)
	planted := plantCheckpoint(t, src)
	res, _ := backupOf(t, src, "")

	dst := newPaths(t)
	addr := freeAddr(t)
	if _, err := daemon.Restore(t.Context(), daemon.RestoreOptions{Paths: dst, Backup: res.Path, Advertise: addr, Listen: addr}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	db, err := store.Open(t.Context(), dst.DatabaseFile(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var got store.Checkpoint
	if err := db.Read(t.Context(), func(q store.Querier) (err error) {
		got, err = store.CheckpointByID(t.Context(), q, planted.id)
		return err
	}); err != nil {
		t.Fatalf("the checkpoint is not in the restored database: %v", err)
	}
	if got.NativeState == nil || got.NativeState.Digest != string(planted.digest) || got.Summary.Progress != "halfway" {
		t.Errorf("restored checkpoint = %+v", got)
	}

	blobs, err := blob.New(dst.DataDir, clock.NewFake(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	r, err := blobs.Open(planted.digest)
	if err != nil {
		t.Fatalf("the native state is not in the restored blobs: %v", err)
	}
	defer func() { _ = r.Close() }()
	if body, _ := io.ReadAll(r); !bytes.Equal(body, []byte(planted.content)) {
		t.Errorf("native state = %q", body)
	}

	repos, _ := filepath.Glob(filepath.Join(dst.RelayDir(), "*.git"))
	if len(repos) != 1 {
		t.Fatalf("restored repositories: %v", repos)
	}
	if head := strings.TrimSpace(gittest.Run(t, repos[0], "rev-parse", planted.ref)); head != planted.commit {
		t.Errorf("%s = %s, want %s", planted.ref, head, planted.commit)
	}
	gittest.Run(t, repos[0], "cat-file", "-e", planted.commit+"^{commit}")
}
