package pki_test

import (
	"crypto/rand"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

func TestRequireCaller_ChecksTheIdentityInsideTheTransaction(t *testing.T) {
	clk := clock.NewFake(time.Now().UTC())
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "controller.db"), store.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ca, err := pki.NewCA(clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	authority := pki.NewAuthority(pki.AuthorityConfig{
		DB: db, CA: ca, Clock: clk, IDs: domain.NewIDGenerator(clk.Now, rand.Reader),
		CertTTL: domain.Defaults().IdentityCertTTL,
	})
	owner := func(name string) pki.Session {
		t.Helper()
		_, csr, err := client.GenerateKeyAndCSR()
		if err != nil {
			t.Fatal(err)
		}
		joined, err := authority.BootstrapOwner(t.Context(), csr, name)
		if err != nil {
			t.Fatal(err)
		}
		s, err := joined.Session()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	require := func(s pki.Session) error {
		return db.WithTx(t.Context(), func(tx *store.Tx) error {
			_, err := pki.RequireCaller(t.Context(), tx, s, clk.Now())
			return err
		})
	}
	wantUnauthenticated := func(err error) {
		t.Helper()
		var de *domain.Error
		if !errors.As(err, &de) || de.Code != domain.CodeUnauthenticated {
			t.Fatalf("got %v, want %s", err, domain.CodeUnauthenticated)
		}
	}

	first, second := owner("first"), owner("second")
	if err := require(first); err != nil {
		t.Fatalf("a live identity: %v", err)
	}
	if err := authority.RevokeClient(t.Context(), second, first.Principal.ID); err != nil {
		t.Fatal(err)
	}
	wantUnauthenticated(require(first)) // revoked after it authenticated
	if err := require(second); err != nil {
		t.Fatalf("the other identity: %v", err)
	}

	clk.Advance(domain.Defaults().IdentityCertTTL + time.Hour)
	wantUnauthenticated(require(second)) // expired
}
