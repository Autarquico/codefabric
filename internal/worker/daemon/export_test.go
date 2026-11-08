package daemon

import (
	"testing"

	"github.com/Autarquico/codefabric/internal/worker/executor"
)

// WatchExecutorOptions has Run hand seen the options it builds the executor
// with, for the length of the test.
func WatchExecutorOptions(t *testing.T, seen func(executor.Options)) {
	t.Helper()
	build := newExecutor
	newExecutor = func(o executor.Options) (*executor.Executor, error) {
		seen(o)
		return build(o)
	}
	t.Cleanup(func() { newExecutor = build })
}
