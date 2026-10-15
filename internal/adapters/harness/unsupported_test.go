package harness_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

// minimal is the least an adapter can be: Unsupported, an ID and its
// Capabilities.
type minimal struct {
	harness.Unsupported
	id string
}

var _ harness.Adapter = minimal{}

func (m minimal) ID() string { return m.id }

func (minimal) Capabilities() harness.Capabilities {
	return harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}}
}

func TestUnsupportedErrorsAreErrUnsupported(t *testing.T) {
	for name, err := range map[string]error{
		"NewUnsupportedError": harness.NewUnsupportedError("resume"),
		"ErrUnsupportedMode":  harness.ErrUnsupportedMode,
		"wrapped mode":        fmt.Errorf("start: %w", harness.ErrUnsupportedMode),
		"with harness":        &harness.UnsupportedError{Harness: "fake", Op: "stop"},
	} {
		if !errors.Is(err, harness.ErrUnsupported) {
			t.Errorf("%s: errors.Is(%v, ErrUnsupported) = false", name, err)
		}
	}
	if errors.Is(harness.NewUnsupportedError("resume"), harness.ErrUnsupportedMode) {
		t.Error("an unsupported operation is not an unsupported mode")
	}
}

func TestUnsupportedErrorSaysWhatAndWhose(t *testing.T) {
	if got := harness.NewUnsupportedError("resume").Error(); got != "harness: resume is unsupported" {
		t.Errorf("Error() = %q", got)
	}
	err := &harness.UnsupportedError{Harness: "fake", Op: "stop"}
	if got := err.Error(); got != "harness fake: stop is unsupported" {
		t.Errorf("Error() = %q", got)
	}
}

// TestUnsupportedReturnsZeroValues calls every method Unsupported provides,
// by reflection so that one added to Adapter cannot be missed, and checks
// each one gives an ErrUnsupported and nothing else.
func TestUnsupportedReturnsZeroValues(t *testing.T) {
	var a harness.Adapter = minimal{id: "fake"}
	v := reflect.ValueOf(a)
	tp := reflect.TypeFor[harness.Adapter]()
	ctx := reflect.TypeFor[context.Context]()
	src := reflect.TypeFor[io.ReadCloser]()
	for i := range tp.NumMethod() {
		m := tp.Method(i)
		if m.Name == "ID" || m.Name == "Capabilities" {
			continue
		}
		in := make([]reflect.Value, m.Type.NumIn())
		for j := range in {
			switch arg := m.Type.In(j); arg {
			case ctx:
				in[j] = reflect.ValueOf(context.Background())
			case src:
				in[j] = reflect.ValueOf(io.NopCloser(strings.NewReader("x")))
			default:
				in[j] = reflect.Zero(arg)
			}
		}
		out := v.MethodByName(m.Name).Call(in)
		err, _ := out[len(out)-1].Interface().(error)
		if !errors.Is(err, harness.ErrUnsupported) {
			t.Errorf("%s: error %v, want ErrUnsupported", m.Name, err)
		}
		for _, o := range out[:len(out)-1] {
			if !o.IsZero() {
				t.Errorf("%s: returned %v with ErrUnsupported, want the zero value", m.Name, o)
			}
		}
	}
}
