// Package handoff is what `cf handoff` does from the machine of the user: it takes
// the session that calls it, with the working tree of its repository, and makes a
// Worker continue it, as a new interactive Task or, if the session already has a Task,
// as a resume of that one. What reaches the Controller is the seed of the session (cut
// at the call that handed it off) and a snapshot of the tree.
package handoff

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client/gitctx"
	"github.com/Autarquico/codefabric/internal/client/session"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Controller is what a handoff asks of the Controller: the narrow face of *client.Client.
type Controller interface {
	Workers(ctx context.Context, includeRemoved bool) ([]v1.Worker, error)
	Task(ctx context.Context, ref, project string) (v1.Task, error)
	ProjectForDir(ctx context.Context, dir string) (v1.Project, error)
	PutNativeSeed(ctx context.Context, project, digest string, r io.Reader, size int64) (v1.NativeSeedUploaded, error)
	NewTask(ctx context.Context, dir string, req v1.CreateTaskRequest) (v1.CreateTaskResponse, error)
	ResumeTaskWith(ctx context.Context, ref, project string, req v1.ResumeTaskRequest) (v1.ResumeTaskResponse, error)
	// TakeSnapshot commits the tree of dir locally, leaving out checkpoint.exclude; PushSnapshot sends it.
	TakeSnapshot(ctx context.Context, dir string) (gitctx.Snapshot, error)
	PushSnapshot(ctx context.Context, dir, project, commit string) error
}

// Request is one handoff.
type Request struct {
	// SessionID is the session to hand over; Worker the name of the Worker that continues it.
	SessionID, Worker string
	// Dir is the working directory of the session: the repository that is snapshotted.
	Dir string
	// AllowSensitive and Force override the guards of the secret and of the double writer.
	AllowSensitive, Force bool
}

// Result says where the session went.
type Result struct {
	Task   string `json:"task"`
	Worker string `json:"worker"`
	// Resumed is true when the session already had a Task and it was resumed.
	Resumed bool   `json:"resumed"`
	Attach  string `json:"attach"`
}

// Do hands the session over. It refuses, before anything is created, a Worker that
// is not online, a session that is open elsewhere, one that holds a secret and one that
// was handed off already, unless the request overrides the last two.
func Do(ctx context.Context, c Controller, sessions *session.Local, req Request) (Result, error) {
	if req.Worker == "" {
		return Result{}, domain.NewError(domain.CodeInvalidRequest, "name the Worker with --to", nil)
	}
	if err := requireOnline(ctx, c, req.Worker); err != nil {
		return Result{}, err
	}
	ref, err := sessions.Find(ctx, req.SessionID, "")
	if err != nil {
		return Result{}, err
	}
	rec, err := sessions.State.Get(ref.ID)
	if err != nil {
		return Result{}, err
	}
	exp, err := sessions.Export(ctx, session.ExportRequest{
		SessionID: ref.ID, Adapter: ref.Adapter, Caller: true,
		AllowSensitive: req.AllowSensitive, Force: req.Force,
	})
	if err != nil {
		return Result{}, err
	}
	seed := v1.NativeStateRef{Adapter: ref.Adapter, AdapterVersion: exp.Version, Digest: exp.Digest}

	var (
		task v1.Task
		tree string
		res  Result
		sent = exp.Digest // the seed that went, which a replay keeps from its first try
	)
	if rec.Task != "" {
		task, tree, err = resume(ctx, c, rec, req, exp, seed)
		res.Resumed = true
	} else {
		task, tree, sent, err = create(ctx, c, sessions.State, ref, rec, req, exp, seed)
	}
	if err != nil {
		return Result{}, err
	}
	if err := sessions.State.MarkHandedOff(ref.ID, task.ID, sent, tree, time.Now()); err != nil {
		return Result{}, err
	}
	res.Task, res.Worker, res.Attach = task.Name, req.Worker, "cf attach "+task.Name
	return res, nil
}

func requireOnline(ctx context.Context, c Controller, name string) error {
	workers, err := c.Workers(ctx, false)
	if err != nil {
		return err
	}
	var known []string
	for _, w := range workers {
		if w.Name == name && w.State == domain.WorkerOnline {
			return nil
		}
		known = append(known, fmt.Sprintf("%s (%s)", w.Name, w.State))
	}
	list := "none"
	if len(known) > 0 {
		list = strings.Join(known, ", ")
	}
	return domain.NewError(domain.CodeNotFound, fmt.Sprintf("no online Worker named %q; Workers: %s", name, list), nil)
}

func create(ctx context.Context, c Controller, state session.State, ref session.Ref, rec session.Record, req Request, exp session.Export, seed v1.NativeStateRef) (v1.Task, string, string, error) {
	project, err := c.ProjectForDir(ctx, req.Dir)
	if err != nil {
		return v1.Task{}, "", "", err
	}
	p := rec.Pending
	if p == nil || p.Worker != req.Worker {
		// A first try: the seed goes up and the request is remembered before it is
		// sent, so that a retry after a lost answer is a replay, whatever the
		// session holds by then.
		if err := upload(ctx, c, project.ID, exp); err != nil {
			return v1.Task{}, "", "", err
		}
		last, err := LastUserMessage(ref.Adapter, ref.Transcript)
		if err != nil {
			return v1.Task{}, "", "", err
		}
		p = &session.Pending{
			RequestID: newRequestID(), Worker: req.Worker,
			Name: TaskName(last, ref.ID), Prompt: Prompt(last),
			SeedAdapter: seed.Adapter, SeedVersion: seed.AdapterVersion, SeedDigest: seed.Digest,
		}
		if err := state.SetPending(ref.ID, *p); err != nil {
			return v1.Task{}, "", "", err
		}
	}
	sent := v1.NativeStateRef{Adapter: p.SeedAdapter, AdapterVersion: p.SeedVersion, Digest: p.SeedDigest}
	var snapshot v1.SnapshotReport
	out, err := c.NewTask(ctx, req.Dir, v1.CreateTaskRequest{
		RequestID: p.RequestID,
		Project:   project.ID, Name: p.Name, Prompt: p.Prompt,
		Mode: domain.ModeInteractive, Snapshot: true, SnapshotReport: &snapshot, NativeSeed: &sent,
		Requirements: v1.Requirements{Harness: ref.Adapter, Workers: v1.WorkerPreferences{Require: []string{req.Worker}}},
	})
	if err != nil {
		return v1.Task{}, "", "", err
	}
	return out.Task, snapshot.Tree, p.SeedDigest, nil
}

// resume continues the Task the session already has. The tree travels only if it is not the
// one the Controller was told about at the last handoff: then the Worker has the work of the
// user. Unchanged, the Run continues from the last checkpoint of the Task, which holds what
// the Worker did since, and a snapshot of the old tree would take it back.
func resume(ctx context.Context, c Controller, rec session.Record, req Request, exp session.Export, seed v1.NativeStateRef) (v1.Task, string, error) {
	t, err := c.Task(ctx, rec.Task, "")
	if err != nil {
		return v1.Task{}, "", err
	}
	snap, err := c.TakeSnapshot(ctx, req.Dir)
	if err != nil {
		return v1.Task{}, "", err
	}
	if err := upload(ctx, c, t.ProjectID, exp); err != nil {
		return v1.Task{}, "", err
	}
	resume := v1.ResumeTaskRequest{Worker: req.Worker, NativeSeed: &seed, Force: req.Force}
	if snap.Tree != rec.Tree {
		if err := c.PushSnapshot(ctx, req.Dir, t.ProjectID, snap.Commit); err != nil {
			return v1.Task{}, "", err
		}
		resume.SourceSnapshot, resume.SourceBranch = snap.Commit, snap.Branch
	}
	out, err := c.ResumeTaskWith(ctx, rec.Task, "", resume)
	if err != nil {
		return v1.Task{}, "", err
	}
	return out.Task, snap.Tree, nil
}

// newRequestID is the request id of a Task creation, kept in the local record so
// that a retry sends it again.
func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "handoff-" + hex.EncodeToString(b[:])
}

func upload(ctx context.Context, c Controller, project string, exp session.Export) error {
	_, err := c.PutNativeSeed(ctx, project, exp.Digest, bytes.NewReader(exp.Archive), int64(len(exp.Archive)))
	return err
}
