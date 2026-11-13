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
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/client/gitctx"
	"github.com/Autarquico/codefabric/internal/client/session"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Controller is what a handoff asks of the Controller: the narrow face of *client.Client.
type Controller interface {
	Workers(ctx context.Context, includeRemoved bool) ([]v1.Worker, error)
	Task(ctx context.Context, ref, project string) (v1.Task, error)
	Runs(ctx context.Context, f client.RunQuery) ([]v1.Run, error)
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
// takes no Runs, a session that is open elsewhere, one that holds a secret and one that
// was handed off already, unless the request overrides the last two.
func Do(ctx context.Context, c Controller, sessions *session.Local, req Request) (Result, error) {
	if req.Worker == "" {
		return Result{}, domain.NewError(domain.CodeInvalidRequest, "name the Worker with --to", nil)
	}
	if err := requireUp(ctx, c, req.Worker); err != nil {
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
	if rec, err = releaseEnded(ctx, c, sessions.State, ref.ID, rec); err != nil {
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
		sent string // the digest of the seed that went, which a replay keeps from its first try
	)
	if rec.Task != "" {
		task, tree, sent, err = resume(ctx, c, sessions.State, ref, rec, req, exp, seed)
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

// releaseEnded unlinks the session from its Task when that Task was cancelled
// or archived and none of its Runs is live: nothing goes on there, so the
// session is the work, and it is handed off as one that never was, but for its
// name (see create). A Run still stopping keeps the guard: its harness may
// still be writing.
func releaseEnded(ctx context.Context, c Controller, state session.State, id string, rec session.Record) (session.Record, error) {
	if rec.Task == "" {
		return rec, nil
	}
	t, err := c.Task(ctx, rec.Task, "")
	if err != nil {
		return rec, err
	}
	if t.State != domain.TaskCancelled && t.State != domain.TaskArchived {
		return rec, nil
	}
	runs, err := c.Runs(ctx, client.RunQuery{Task: t.ID})
	if err != nil {
		return rec, err
	}
	for _, r := range runs {
		if !r.State.IsTerminal() {
			return rec, nil
		}
	}
	if err := state.Unlink(id); err != nil {
		return rec, err
	}
	return state.Get(id)
}

// requireUp lets through an online Worker and a degraded one: the scheduler
// decides whether what is failing there concerns the harness of this session.
func requireUp(ctx context.Context, c Controller, name string) error {
	workers, err := c.Workers(ctx, false)
	if err != nil {
		return err
	}
	var known []string
	for _, w := range workers {
		if w.Name == name && (w.State == domain.WorkerOnline || w.State == domain.WorkerDegraded) {
			return nil
		}
		known = append(known, fmt.Sprintf("%s (%s)", w.Name, w.State))
	}
	list := "none"
	if len(known) > 0 {
		list = strings.Join(known, ", ")
	}
	return domain.NewError(domain.CodeNotFound, fmt.Sprintf("no online or degraded Worker named %q; Workers: %s", name, list), nil)
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
		// The snapshot is taken once, here: the Task starts from this very commit, and
		// a replay sends it again whatever the files are by then.
		snap, err := c.TakeSnapshot(ctx, req.Dir)
		if err != nil {
			return v1.Task{}, "", "", err
		}
		if err := c.PushSnapshot(ctx, req.Dir, project.ID, snap.Commit); err != nil {
			return v1.Task{}, "", "", err
		}
		// A session seeded before was freed from a Task that ended, and that Task keeps
		// the name the session would give: the new one takes a suffix of its request.
		requestID, name := newRequestID(), TaskName(last, ref.ID)
		if rec.Seed != "" {
			name += "-" + requestID[len(requestID)-4:]
		}
		p = &session.Pending{
			RequestID: requestID, Worker: req.Worker, Tree: snap.Tree,
			Name: name, Prompt: Prompt(last),
			SeedAdapter: seed.Adapter, SeedVersion: seed.AdapterVersion, SeedDigest: seed.Digest,
			SnapshotCommit: snap.Commit, SnapshotBranch: snap.Branch,
		}
		if err := state.SetPending(ref.ID, *p); err != nil {
			return v1.Task{}, "", "", err
		}
	}
	sent := v1.NativeStateRef{Adapter: p.SeedAdapter, AdapterVersion: p.SeedVersion, Digest: p.SeedDigest}
	out, err := c.NewTask(ctx, req.Dir, v1.CreateTaskRequest{
		RequestID: p.RequestID,
		Project:   project.ID, Name: p.Name, Prompt: p.Prompt,
		Mode: domain.ModeInteractive, SourceSnapshot: p.SnapshotCommit, SourceBranch: p.SnapshotBranch, NativeSeed: &sent,
		Requirements: v1.Requirements{Harness: ref.Adapter, Workers: v1.WorkerPreferences{Require: []string{req.Worker}}},
	})
	if err != nil {
		return v1.Task{}, "", "", forgetRefused(state, ref.ID, err)
	}
	return out.Task, p.Tree, p.SeedDigest, nil
}

// resume continues the Task the session already has, and is retried like a creation: what
// is sent is remembered before it is sent, so that a retry after a lost answer replays it
// with the same request id, seed and snapshot, without uploading again.
//
// The tree travels only if it is not the one the Controller was told about at the last
// handoff: then the Worker has the work of the user. Unchanged, the Run continues from the
// last checkpoint of the Task, which holds what the Worker did since, and a snapshot of the
// old tree would take it back.
func resume(ctx context.Context, c Controller, state session.State, ref session.Ref, rec session.Record, req Request, exp session.Export, seed v1.NativeStateRef) (v1.Task, string, string, error) {
	p := rec.Pending
	if p == nil || p.Worker != req.Worker {
		t, err := c.Task(ctx, rec.Task, "")
		if err != nil {
			return v1.Task{}, "", "", err
		}
		if err := requireResumable(t); err != nil {
			return v1.Task{}, "", "", err
		}
		snap, err := c.TakeSnapshot(ctx, req.Dir)
		if err != nil {
			return v1.Task{}, "", "", err
		}
		if err := upload(ctx, c, t.ProjectID, exp); err != nil {
			return v1.Task{}, "", "", err
		}
		p = &session.Pending{
			RequestID: newRequestID(), Worker: req.Worker, Tree: snap.Tree,
			SeedAdapter: seed.Adapter, SeedVersion: seed.AdapterVersion, SeedDigest: seed.Digest,
		}
		if snap.Tree != rec.Tree {
			if err := c.PushSnapshot(ctx, req.Dir, t.ProjectID, snap.Commit); err != nil {
				return v1.Task{}, "", "", err
			}
			p.SnapshotCommit, p.SnapshotBranch = snap.Commit, snap.Branch
		}
		if err := state.SetPending(ref.ID, *p); err != nil {
			return v1.Task{}, "", "", err
		}
	}
	sent := v1.NativeStateRef{Adapter: p.SeedAdapter, AdapterVersion: p.SeedVersion, Digest: p.SeedDigest}
	out, err := c.ResumeTaskWith(ctx, rec.Task, "", v1.ResumeTaskRequest{
		RequestID: p.RequestID, Worker: req.Worker, NativeSeed: &sent, Force: req.Force,
		SourceSnapshot: p.SnapshotCommit, SourceBranch: p.SnapshotBranch,
	})
	if err != nil {
		return v1.Task{}, "", "", forgetRefused(state, ref.ID, err)
	}
	return out.Task, p.Tree, p.SeedDigest, nil
}

// requireResumable refuses, before anything is uploaded, a Task whose Run is still live:
// only a deferred Task, or one whose Run was lost or failed, can be resumed.
func requireResumable(t v1.Task) error {
	switch {
	case t.State == domain.TaskFailed,
		t.State == domain.TaskWaiting && (t.WaitingReason == domain.WaitingDeferred || t.WaitingReason == domain.WaitingDecision):
		return nil
	}
	return domain.NewError(domain.CodeInvalidTransition,
		fmt.Sprintf("task %s is %s: only a deferred Task can be resumed; to move it to another Worker while it runs use `cf task move %s <worker>`", t.Name, t.State, t.Name),
		map[string]any{"state": string(t.State)})
}

// forgetRefused drops the pending request when the Controller refused it (an
// invalid request, a Task in the wrong state, a policy): the answer came, so a
// retry must not replay it. Any other error may be a lost answer, and the request stays.
func forgetRefused(state session.State, id string, err error) error {
	var de *domain.Error
	if errors.As(err, &de) {
		switch de.Code {
		case domain.CodeInvalidRequest, domain.CodeNotFound, domain.CodePolicyDenied, domain.CodeInvalidTransition:
			if cerr := state.ClearPending(id); cerr != nil {
				return errors.Join(err, cerr)
			}
		}
	}
	return err
}

// newRequestID is the request id of a creation or resume, kept in the local record so
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
