package mutation

import (
	"testing"

	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestActorOf(t *testing.T) {
	client := domain.Principal{ID: "client_1", Kind: domain.KindClient}
	worker := domain.Principal{ID: "worker_1", Kind: domain.KindWorker}
	for _, tt := range []struct {
		name string
		by   pki.Session
		want domain.ActorRef
	}{
		{"a client with no session is human", pki.Session{Principal: client}, domain.ActorRef{Kind: domain.ActorHuman, ID: "client_1"}},
		{
			"a client with a session is mcp, with the ID of its certificate",
			pki.Session{Principal: client, MCPSession: "session-1234"},
			domain.ActorRef{Kind: domain.ActorMCP, ID: "client_1", Session: "session-1234"},
		},
		{"a worker is never raised to mcp", pki.Session{Principal: worker, MCPSession: "session-1234"}, domain.ActorRef{Kind: domain.ActorWorker, ID: "worker_1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := actorOf(tt.by); got != tt.want {
				t.Errorf("actor = %+v, want %+v", got, tt.want)
			}
		})
	}
}
