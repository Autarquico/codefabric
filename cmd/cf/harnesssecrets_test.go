package main

import (
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The Controller reads domain.HarnessRequiredSecrets because it cannot import
// adapters; this keeps it equal to what the adapter declares.
func TestHarnessRequiredSecrets_MatchTheAdapterDeclaration(t *testing.T) {
	var adapter harness.SecretRequirer = claude.Adapter{}
	var declared []domain.SecretEnv
	for _, s := range adapter.RequiredSecrets() {
		declared = append(declared, domain.SecretEnv(s))
	}
	if got := domain.HarnessRequiredSecrets(claude.ID); !slices.Equal(got, declared) {
		t.Errorf("domain says %v, the adapter declares %v", got, declared)
	}
}
