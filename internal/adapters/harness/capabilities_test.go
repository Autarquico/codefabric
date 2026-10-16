package harness_test

import (
	"errors"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestCheckModeAcceptsOnlyTheDeclaredModes(t *testing.T) {
	headless := harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}}
	if err := headless.CheckMode(domain.ModeHeadless); err != nil {
		t.Errorf("CheckMode(headless): %v", err)
	}
	err := headless.CheckMode(domain.ModeInteractive)
	if !errors.Is(err, harness.ErrUnsupportedMode) || !errors.Is(err, harness.ErrUnsupported) {
		t.Errorf("CheckMode(interactive) = %v, want ErrUnsupportedMode", err)
	}
	if err := (harness.Capabilities{}).CheckMode(domain.ModeHeadless); !errors.Is(err, harness.ErrUnsupportedMode) {
		t.Errorf("no modes, CheckMode(headless) = %v, want ErrUnsupportedMode", err)
	}
}
