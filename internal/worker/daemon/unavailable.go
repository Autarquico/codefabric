package daemon

import (
	"context"
	"errors"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
)

var errNoRuntime = errors.New("this Worker has no Runtime to run sessions in")

// unavailable stands in for a Runtime that is not there. The executor still
// runs, to answer what it is given; nothing is ever started.
type unavailable struct{}

func (unavailable) Start(context.Context, cfruntime.Spec) (cfruntime.Handle, error) {
	return cfruntime.Handle{}, errNoRuntime
}
func (unavailable) Stop(context.Context, cfruntime.Handle) error { return nil }
func (unavailable) Status(context.Context, cfruntime.Handle) (cfruntime.Status, error) {
	return cfruntime.Status{}, nil
}
func (unavailable) Attach(context.Context, cfruntime.Handle) error { return errNoRuntime }
func (unavailable) HandleFor(name string) cfruntime.Handle {
	return cfruntime.Handle{Runtime: "none", Ref: name}
}
