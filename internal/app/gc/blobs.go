package gc

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/store"
)

// What refers to a blob. A blob nothing refers to is garbage once the grace
// period has passed.
const (
	// refOutput: a segment of the output of a Run (logs_index).
	refOutput uint8 = 1 << iota
	// refOutputKept: a segment of an output the retention does not purge. A dry
	// run takes it for refOutput, since the pass it reports would have purged
	// the rest before the sweep.
	refOutputKept
	// refWorkflow: a file named by the manifest of a version of a Workflow.
	refWorkflow
	// refRecent: an upload newer than the grace period, which its Workflow may not have named yet.
	refRecent
)

// markSource says which blobs one kind of reference keeps alive. A new kind of
// reference (artifacts, checkpoints) is one more entry of markSources.
type markSource struct {
	ref   uint8
	visit func(ctx context.Context, q store.Querier, visit func(string)) error
}

func (s *Service) markSources(now time.Time, planning bool) []markSource {
	rawCutoff := now.Add(-s.policy.RawOutput)
	sources := []markSource{
		{refOutput, func(ctx context.Context, q store.Querier, v func(string)) error {
			return store.LogsIndexDigests(ctx, q, false, rawCutoff, v)
		}},
		{refWorkflow, store.ManifestDigests},
		{refRecent, func(ctx context.Context, q store.Querier, v func(string)) error {
			return store.UploadedSince(ctx, q, now.Add(-s.policy.BlobGrace), v)
		}},
	}
	if planning {
		sources = append(sources, markSource{refOutputKept, func(ctx context.Context, q store.Querier, v func(string)) error {
			return store.LogsIndexDigests(ctx, q, true, rawCutoff, v)
		}})
	}
	return sources
}

// mark reads every reference in one snapshot.
func (s *Service) mark(ctx context.Context, now time.Time, planning bool) (map[string]uint8, error) {
	refs := map[string]uint8{}
	err := s.db.Read(ctx, func(q store.Querier) error {
		for _, src := range s.markSources(now, planning) {
			if err := src.visit(ctx, q, func(d string) { refs[d] |= src.ref }); err != nil {
				return err
			}
		}
		return nil
	})
	return refs, err
}

type garbage struct {
	digest blob.Digest
	size   int64
}

// blobScan is what the disk holds, by what keeps it, and the blobs a pass would delete.
type blobScan struct {
	outputBytes, workflowBytes, unreferencedBytes int64
	garbage                                       []garbage
}

// scan marks, then walks the disk. The mark comes first on purpose: a blob
// stored after it is younger than the grace period when the walk sees it.
func (s *Service) scan(ctx context.Context, now time.Time, planning bool) (blobScan, error) {
	refs, err := s.mark(ctx, now, planning)
	if err != nil {
		return blobScan{}, err
	}
	live := refOutput | refWorkflow | refRecent
	if planning {
		live = refOutputKept | refWorkflow | refRecent
	}
	var out blobScan
	notAfter := now.Add(-s.policy.BlobGrace)
	err = s.blobs.Walk(ctx, func(e blob.Entry) error {
		r := refs[string(e.Digest)]
		switch {
		case r&refOutput != 0:
			out.outputBytes += e.Size
		case r&refWorkflow != 0:
			out.workflowBytes += e.Size
		default:
			out.unreferencedBytes += e.Size
		}
		if r&live == 0 && !e.ModTime.After(notAfter) {
			out.garbage = append(out.garbage, garbage{e.Digest, e.Size})
		}
		return nil
	})
	return out, err
}

func (b blobScan) report() v1.GCCount {
	c := v1.GCCount{Kind: "blobs", Count: int64(len(b.garbage))}
	for _, g := range b.garbage {
		c.Bytes += g.size
	}
	return c
}

// plan is a pass that deletes nothing.
func (s *Service) plan(ctx context.Context) (v1.GCReport, blobScan, error) {
	now := s.clock.Now()
	rep := v1.GCReport{DryRun: true}
	err := s.db.Read(ctx, func(q store.Querier) error {
		for _, st := range s.steps(now) {
			n, err := st.due(ctx, q)
			if err != nil {
				return err
			}
			rep.Items = append(rep.Items, v1.GCCount{Kind: st.kind, Count: n})
			if st.held != nil {
				rep.HeldBack = st.held()
			}
		}
		return nil
	})
	if err != nil {
		return rep, blobScan{}, err
	}
	scan, err := s.scan(ctx, now, true)
	if err != nil {
		return rep, blobScan{}, err
	}
	rep.Items = append(rep.Items, scan.report())
	if s.relay != nil {
		refs, err := s.planRelay(ctx, now)
		if err != nil {
			return rep, blobScan{}, err
		}
		rep.Items = append(rep.Items, refs)
	}
	return rep, scan, nil
}

// sweepBlobs deletes the blobs nothing refers to, a batch at a time. Each batch
// holds the lock of the backups and the writer: the file and its blobs_meta row
// go together, and a Put of the same blob between the check of its mtime and the
// commit of the batch either shows in the mtime or waits for the writer.
func (s *Service) sweepBlobs(ctx context.Context, rep *v1.GCReport) error {
	now := s.clock.Now()
	scan, err := s.scan(ctx, now, false)
	if err != nil {
		return err
	}
	notAfter := now.Add(-s.policy.BlobGrace)
	done := v1.GCCount{Kind: "blobs"}
	defer func() { rep.Items = append(rep.Items, done) }()

	for batch := range chunks(scan.garbage, s.policy.BatchSize) {
		rows, err := s.deleteBatch(ctx, batch, notAfter)
		done.Count += int64(len(rows))
		for _, g := range rows {
			done.Bytes += g.size
		}
		if err != nil {
			return err
		}
		if s.hooks.AfterBatch != nil {
			s.hooks.AfterBatch("blobs", int64(len(rows)))
		}
		if err := yield(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) deleteBatch(ctx context.Context, batch []garbage, notAfter time.Time) (gone []garbage, err error) {
	release, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer release()
	err = s.db.WithTx(ctx, func(tx *store.Tx) error {
		gone = gone[:0]
		for _, g := range batch {
			if s.hooks.BeforeUnlink != nil {
				s.hooks.BeforeUnlink(g.digest)
			}
			removed, err := s.blobs.Delete(g.digest, notAfter)
			if err != nil {
				return err
			}
			if !removed {
				continue
			}
			gone = append(gone, g)
			if err := tx.DeleteBlobMeta(ctx, string(g.digest)); err != nil {
				return err
			}
		}
		return nil
	})
	return gone, err
}

func chunks[T any](all []T, size int) func(yield func([]T) bool) {
	return func(yield func([]T) bool) {
		for len(all) > 0 {
			n := min(size, len(all))
			if !yield(all[:n]) {
				return
			}
			all = all[n:]
		}
	}
}

// Stats measures what the Controller keeps and how much of it a pass would
// free. It changes nothing.
func (s *Service) Stats(ctx context.Context) (v1.StorageStats, error) {
	rep, scan, err := s.plan(ctx)
	if err != nil {
		return v1.StorageStats{}, err
	}
	st := v1.StorageStats{
		OutputBlobBytes:       scan.outputBytes,
		WorkflowBlobBytes:     scan.workflowBytes,
		UnreferencedBlobBytes: scan.unreferencedBytes,
		MaxStorageBytes:       s.policy.MaxStorage,
		Reclaimable:           rep,
	}
	if s.databasePath != "" {
		if st.DatabaseBytes, err = fileBytes(s.databasePath, s.databasePath+"-wal"); err != nil {
			return v1.StorageStats{}, err
		}
	}
	if st.RelayBytes, err = dirBytes(s.relayDir); err != nil {
		return v1.StorageStats{}, err
	}
	st.TotalBytes = st.OutputBlobBytes + st.WorkflowBlobBytes + st.UnreferencedBlobBytes + st.DatabaseBytes + st.RelayBytes
	st.OverMaxStorage = st.MaxStorageBytes > 0 && st.TotalBytes > st.MaxStorageBytes
	return st, nil
}

// fileBytes sums the sizes of the files that exist.
func fileBytes(paths ...string) (int64, error) {
	var total int64
	for _, p := range paths {
		info, err := os.Stat(p)
		switch {
		case os.IsNotExist(err):
		case err != nil:
			return 0, fmt.Errorf("gc: measuring %s: %w", p, err)
		default:
			total += info.Size()
		}
	}
	return total, nil
}

// dirBytes sums the regular files below dir; "" and an absent dir are 0.
func dirBytes(dir string) (int64, error) {
	if dir == "" {
		return 0, nil
	}
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("gc: measuring %s: %w", dir, err)
	}
	return total, nil
}
