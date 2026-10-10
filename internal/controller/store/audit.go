package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// AuditEntry is one §16.5 record. Before and After are marshalled to JSON when
// non-nil; ConfirmationID is set for sensitive actions only.
type AuditEntry struct {
	Time           time.Time // defaults to the store's clock
	ActorKind      domain.ActorKind
	ActorID        string
	Action         string
	ResourceKind   string
	ResourceID     string
	Before         any
	After          any
	Result         string
	ConfirmationID string
}

// AppendAudit records e in the transaction, so the audit trail can never
// disagree with the state change it describes.
func (t *Tx) AppendAudit(ctx context.Context, e AuditEntry) error {
	if e.Time.IsZero() {
		e.Time = t.db.clock.Now()
	}
	before, err := marshalOptional(e.Before)
	if err != nil {
		return err
	}
	after, err := marshalOptional(e.After)
	if err != nil {
		return err
	}
	_, err = t.Exec(ctx,
		`INSERT INTO audit_entries (ts, actor_kind, actor_id, action, resource_kind, resource_id,
		     before, after, result, confirmation_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Time.UnixNano(), string(e.ActorKind), e.ActorID, e.Action, e.ResourceKind, e.ResourceID,
		before, after, e.Result, nullable(e.ConfirmationID))
	if err != nil {
		return fmt.Errorf("store: appending audit entry: %w", err)
	}
	return nil
}

func marshalOptional(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("store: audit snapshot is not JSON: %w", err)
	}
	return string(raw), nil
}
