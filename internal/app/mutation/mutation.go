// Package mutation is the one transactional wrapper every state-changing use
// case runs inside (§27.4).
package mutation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const maxRequestIDLen = 64

// Act says who causes a change and when. It travels with the *store.Tx into
// the operations a use case is made of, so that every event they write carries
// the same actor, request ID and instant.
type Act struct {
	Actor     domain.ActorRef
	RequestID string
	Now       time.Time
}

// Event starts a domain event caused by this Act; the caller fills in the IDs and data.
func (a Act) Event(eventType domain.EventType, message string) domain.Event {
	return domain.Event{
		Type: eventType, Level: "info", Message: message,
		Timestamp: a.Now, RequestID: a.RequestID, Actor: a.Actor,
	}
}

// Do runs fn as a single use case: in one transaction it applies the
// request_id idempotency contract, revalidates the caller and runs fn. fn
// receives the transaction and never opens its own; the clock is read inside
// it, after the writer was acquired. The result is saved as the response of
// the request_id and returned from it when the request repeats.
func Do[T any](ctx context.Context, db *store.DB, clk clock.Clock, by pki.Session,
	operation, requestID string, request any, fn func(*store.Tx, Act) (T, error),
) (T, error) {
	var zero T
	if !validRequestID(requestID) {
		return zero, domain.NewError(domain.CodeInvalidRequest,
			fmt.Sprintf("request_id must be at most %d characters of letters, digits, '.', '_' or '-'", maxRequestIDLen), nil)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return zero, fmt.Errorf("mutation: encoding the request: %w", err)
	}
	key := store.IdemKey{Principal: string(by.Principal.ID), Operation: operation, RequestID: requestID}
	saved, err := db.Idempotent(ctx, key, payload, func(tx *store.Tx) ([]byte, error) {
		now := clk.Now()
		if _, err := pki.RequireCaller(ctx, tx, by, now); err != nil {
			return nil, err
		}
		out, err := fn(tx, Act{
			Actor:     domain.ActorRef{Kind: by.Principal.ActorKind(), ID: string(by.Principal.ID)},
			RequestID: requestID, Now: now,
		})
		if err != nil {
			return nil, err
		}
		return json.Marshal(out)
	})
	if err != nil {
		return zero, err
	}
	var out T
	if err := json.Unmarshal(saved, &out); err != nil {
		return zero, fmt.Errorf("mutation: decoding the response: %w", err)
	}
	return out, nil
}

func validRequestID(id string) bool {
	if len(id) > maxRequestIDLen {
		return false
	}
	for _, c := range []byte(id) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}
