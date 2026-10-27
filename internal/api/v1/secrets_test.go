package v1

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Without secrets, what a daemon or a client sends is what it sent before them.
func TestSecretFieldsAreOmittedWhenUnused(t *testing.T) {
	for name, v := range map[string]any{
		"heartbeat":    HeartbeatRequest{Epoch: "e", Total: Capacity{CPU: 1}, Health: Health{Status: HealthOK}},
		"requirements": Requirements{Harness: "command"},
		"worker":       Worker{ID: "w"},
	} {
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{`"secrets"`, `"secret_env"`, `"generation"`} {
			if strings.Contains(string(body), field) {
				t.Errorf("%s: %s carries %s", name, body, field)
			}
		}
	}
}

// A daemon that holds no secret says so: an empty list, not an absent one.
func TestEmptySecretsReportIsSent(t *testing.T) {
	body, err := json.Marshal(HeartbeatRequest{Secrets: &SecretsReport{Seq: 1, Refs: []SecretMeta{}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"secrets":{"seq":1,"refs":[]}`) {
		t.Errorf("an empty report is %s", body)
	}
}

// A full catalog of ordinary refs, each scoped to a Project, fits the budget,
// and the budget leaves the rest of a 64 KiB heartbeat room for its
// capabilities at their limit (64 of 256 bytes).
func TestSecretsReportBudget(t *testing.T) {
	at := time.Date(2026, 10, 16, 12, 0, 0, 123456789, time.UTC)
	report := SecretsReport{Seq: 1 << 40}
	for i := range domain.MaxWorkerSecrets {
		report.Refs = append(report.Refs, SecretMeta{
			Ref:       fmt.Sprintf("team-%03d/deploy-token-%012d", i, i),
			Backend:   SecretBackendFile,
			Projects:  []string{"prj_01JABCDEFGHJKMNPQRSTVWXYZ0"},
			UpdatedAt: at,
		})
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > MaxSecretsReportBytes {
		t.Errorf("a full catalog is %d bytes, over the budget of %d", len(body), MaxSecretsReportBytes)
	}
	const heartbeatBody, capabilities, rest = 64 << 10, 64 * 256, 4 << 10
	if MaxSecretsReportBytes+capabilities+rest > heartbeatBody {
		t.Errorf("a report of %d bytes leaves no room in a heartbeat", MaxSecretsReportBytes)
	}
}
