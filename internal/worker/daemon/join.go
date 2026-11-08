// Package daemon is the Worker's own process: `cf worker join` enrols the
// machine with a Controller and `cf worker run` keeps it registered, with a
// heartbeat every few seconds.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// JoinOptions parameterise Join.
type JoinOptions struct {
	Paths      config.Paths
	Invitation string
	Name       string
	Settings   v1.WorkerSettings
	// Force replaces the Worker credentials already stored on this machine.
	Force bool
}

// Join redeems a worker invitation and stores the Worker's credentials, apart
// from the client's. It refuses before spending the token if credentials are
// already there, since the join would otherwise strand a Worker that nobody
// can reach.
func Join(ctx context.Context, o JoinOptions) (client.Connected, error) {
	dir := o.Paths.WorkerCredentialsDir()
	if !o.Force {
		exists, err := client.CredentialsExist(dir)
		if err != nil {
			return client.Connected{}, err
		}
		if exists {
			return client.Connected{}, fmt.Errorf("this machine already has Worker credentials in %s; `cf worker join --force` replaces them "+
				"(the old Worker stays registered at its Controller until a client removes it)", dir)
		}
	}
	joined, err := client.Connect(ctx, o.Invitation, o.Name, dir, client.AsWorker(o.Settings))
	if err != nil {
		return client.Connected{}, err
	}
	if err := forgetOrderCursor(ctx, o.Paths.WorkerStateFile()); err != nil {
		return client.Connected{}, err
	}
	return joined, nil
}

// forgetOrderCursor makes a new identity start at the beginning of its own
// queue of orders, which the cursor of the previous one would skip.
func forgetOrderCursor(ctx context.Context, path string) error {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	db, err := state.Open(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return db.WithTx(ctx, func(tx *state.Tx) error { return state.ResetCursor(ctx, tx) })
}
