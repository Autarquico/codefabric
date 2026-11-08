package app_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/app/gc"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// afterOpenBlobs runs a hook once, right after the commit's verification
// opened a blob: the moment between verify and the commit transaction.
type afterOpenBlobs struct {
	*blob.Store
	after func()
}

func (b *afterOpenBlobs) Open(d blob.Digest) (io.ReadCloser, error) {
	rc, err := b.Store.Open(d)
	if f := b.after; f != nil {
		b.after = nil
		f()
	}
	return rc, err
}

func TestCommit_RefusesAManifestWhoseBlobTheSweepCollectedAfterVerify(t *testing.T) {
	cas := t.TempDir()
	var blobs *afterOpenBlobs
	var fake *clock.Fake
	e := newEnvWith(t, func(c *app.Config, clk *clock.Fake) {
		s, err := blob.New(cas, clk)
		must(t, err)
		fake, blobs = clk, &afterOpenBlobs{Store: s}
		c.Blobs = blobs
	})
	w := &wfEnv{env: e, cas: cas}
	m := manifest(w.component("codex", "agents", "an old upload nobody references"))
	d := m.Components[0].Files[0].SHA256
	fake.Advance(5 * 24 * time.Hour)
	hexd := strings.TrimPrefix(d, "sha256:")
	old := fake.Now().Add(-5 * 24 * time.Hour)
	must(t, os.Chtimes(filepath.Join(cas, "blobs", "sha256", hexd[:2], hexd[2:4], hexd), old, old))

	sweep := gc.New(e.db, fake, blobs.Store, gc.Policy{BlobGrace: 24 * time.Hour, BatchSize: 10})
	blobs.after = func() {
		if _, err := sweep.Run(context.Background(), false); err != nil {
			t.Errorf("gc: %v", err)
		}
	}
	if _, err := w.commit("review", 0, m); err == nil {
		t.Fatalf("committed a version naming %s after the sweep deleted it", d)
	}
	err := e.db.Read(t.Context(), func(q store.Querier) error {
		_, err := store.WorkflowByName(t.Context(), q, "review")
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the refused commit left a Workflow behind: %v", err)
	}
}
