package daemon

import (
	"context"
	"testing"

	"github.com/Autarquico/codefabric/internal/controller/store"
)

// WatchStore has Start hand the options and the store it opens to seen, for
// the length of the test.
func WatchStore(t *testing.T, seen func(store.Options, *store.DB)) {
	t.Helper()
	open := openStore
	openStore = func(ctx context.Context, path string, o store.Options) (*store.DB, error) {
		db, err := open(ctx, path, o)
		if err == nil {
			seen(o, db)
		}
		return db, err
	}
	t.Cleanup(func() { openStore = open })
}
