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

// PublishRun is the result being in the relay. It is a compare-and-set on the
// pending publication of the result that was verified.
func (t *Tx) PublishRun(ctx context.Context, run Run, commit string) error {
	return t.setPublication(ctx, run, commit, domain.PublicationPending, domain.PublicationPublished, "publishing run")
}

// FailPublication is the Worker giving up, or the relay not holding what the
// Worker pushed. A Run that was not sealed has no result commit to match.
func (t *Tx) FailPublication(ctx context.Context, run Run) error {
	res, err := t.Exec(ctx,
		`UPDATE runs SET publication = ?
		 WHERE id = ? AND publication = ? AND COALESCE(result_commit, '') = ? AND lease_id = ? AND lease_epoch = ?`,
		string(domain.PublicationFailed), string(run.ID), string(domain.PublicationPending), run.ResultCommit,
		string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "failing publication")
}

// RetryPublication puts a failed publication back to pending, for `cf run publish`.
func (t *Tx) RetryPublication(ctx context.Context, run Run) error {
	res, err := t.Exec(ctx,
		`UPDATE runs SET publication = ?
		 WHERE id = ? AND publication = ? AND COALESCE(result_commit, '') = ? AND lease_id = ? AND lease_epoch = ?`,
		string(domain.PublicationPending), string(run.ID), string(domain.PublicationFailed), run.ResultCommit,
		string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "retrying publication")
}

func (t *Tx) setPublication(ctx context.Context, run Run, commit string, from, to domain.Publication, what string) error {
	res, err := t.Exec(ctx,
		`UPDATE runs SET publication = ?
		 WHERE id = ? AND publication = ? AND result_commit = ? AND lease_id = ? AND lease_epoch = ?`,
		string(to), string(run.ID), string(from), commit, string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, what)
}
