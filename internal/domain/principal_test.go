package domain

import (
	"crypto/rand"
	"testing"
	"time"
)

func TestPrincipalActorKind(t *testing.T) {
	if got := (Principal{Kind: KindClient}).ActorKind(); got != ActorHuman {
		t.Errorf("client: got %s", got)
	}
	if got := (Principal{Kind: KindWorker}).ActorKind(); got != ActorWorker {
		t.Errorf("worker: got %s", got)
	}
}

func TestParseIDAcceptsClient(t *testing.T) {
	id := NewIDGenerator(func() time.Time { return time.Unix(1, 0) }, rand.Reader).New(KindClient)
	if kind, _, err := ParseID(string(id)); err != nil || kind != KindClient {
		t.Fatalf("ParseID(%s) = %s, %v", id, kind, err)
	}
}
