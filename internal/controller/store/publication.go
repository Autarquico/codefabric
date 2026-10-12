package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Autarquico/codefabric/internal/domain"
)

// excludedDoc is how result_excluded is stored.
type excludedDoc struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated"`
}

// SealRun records the commit the Worker sealed, with the paths the sealing left
// out. It is a compare-and-set on a Run whose publication is pending and that
// has no result yet: a result, once set, never changes.
func (t *Tx) SealRun(ctx context.Context, run Run, commit string, excluded []string, truncated bool) error {
	raw, err := json.Marshal(excludedDoc{Paths: excluded, Truncated: truncated})
	if err != nil {
		return fmt.Errorf("store: encoding the excluded paths: %w", err)
	}
	res, err := t.Exec(ctx,
		`UPDATE runs SET result_commit = ?, result_excluded = ?
		 WHERE id = ? AND publication = ? AND result_commit IS NULL AND lease_id = ? AND lease_epoch = ?`,
		commit, string(raw), string(run.ID), string(domain.PublicationPending), string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "sealing run")
}

// PublishRun is the result being in the relay: a compare-and-set on the pending
// publication of the result that was verified.
func (t *Tx) PublishRun(ctx context.Context, run Run, commit string) error {
	run.ResultCommit = commit
	return t.setPublication(ctx, run, domain.PublicationPending, domain.PublicationPublished, "publishing run")
}

// FailPublication is the Worker giving up, or the relay not holding what the
// Worker pushed. A Run that was not sealed has no result commit to match.
func (t *Tx) FailPublication(ctx context.Context, run Run) error {
	return t.setPublication(ctx, run, domain.PublicationPending, domain.PublicationFailed, "failing publication")
}

// RetryPublication puts a failed publication back to pending, for `cf run publish`.
func (t *Tx) RetryPublication(ctx context.Context, run Run) error {
	return t.setPublication(ctx, run, domain.PublicationFailed, domain.PublicationPending, "retrying publication")
}

// setPublication moves the publication of a Run from one state to another. It is
// a compare-and-set on the state, the result commit ("" until sealed) and the
// lease the caller read, and it must hit one row.
func (t *Tx) setPublication(ctx context.Context, run Run, from, to domain.Publication, what string) error {
	res, err := t.Exec(ctx,
		`UPDATE runs SET publication = ?
		 WHERE id = ? AND publication = ? AND COALESCE(result_commit, '') = ? AND lease_id = ? AND lease_epoch = ?`,
		string(to), string(run.ID), string(from), run.ResultCommit, string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, what)
}
