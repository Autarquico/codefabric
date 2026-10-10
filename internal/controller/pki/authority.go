package pki

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// MaxTokenTTL bounds how long a join token may live.
const MaxTokenTTL = 7 * 24 * time.Hour

const maxNameLen = 64

// renewGrace is how long a replaced certificate may still renew, so that a
// client whose renewal response was lost can retry with what it still holds.
const renewGrace = 10 * time.Minute

// Authority issues, renews and revokes identities. Every change commits with
// its audit entry in one transaction, and secrets never reach the audit trail.
type Authority struct {
	db      *store.DB
	ca      *CA
	clock   clock.Clock
	ids     *domain.IDGenerator
	certTTL time.Duration
	host    string // advertised host
	port    string
}

// AuthorityConfig wires an Authority.
type AuthorityConfig struct {
	DB      *store.DB
	CA      *CA
	Clock   clock.Clock
	IDs     *domain.IDGenerator
	CertTTL time.Duration // client certificate lifetime
	// Host and Port are what invitations point at.
	Host, Port string
}

// NewAuthority builds an Authority.
func NewAuthority(c AuthorityConfig) *Authority {
	return &Authority{
		db: c.DB, ca: c.CA, clock: c.Clock, ids: c.IDs, certTTL: c.CertTTL,
		host: c.Host, port: c.Port,
	}
}

// CreateToken issues a single-use join token. The only copy of the token is in
// the returned invitation.
func (a *Authority) CreateToken(ctx context.Context, by domain.Principal, kind domain.Kind, ttl time.Duration) (v1.Invite, time.Time, error) {
	if kind != domain.KindClient && kind != domain.KindWorker {
		return v1.Invite{}, time.Time{}, invalidRequest("kind must be client or worker")
	}
	if ttl <= 0 || ttl > MaxTokenTTL {
		return v1.Invite{}, time.Time{}, invalidRequest(fmt.Sprintf("ttl must be positive and at most %s", MaxTokenTTL))
	}
	token, err := newToken()
	if err != nil {
		return v1.Invite{}, time.Time{}, err
	}
	hash := hashToken(token)
	now := a.clock.Now()
	expires := now.Add(ttl)
	err = a.db.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.InsertJoinToken(ctx, hash, kind, string(by.ID), now, expires); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, store.AuditEntry{
			ActorKind: by.ActorKind(), ActorID: string(by.ID), Action: "token.created",
			ResourceKind: "join_token", ResourceID: hash[:12], Result: "ok",
			After: map[string]any{"kind": kind, "expires_at": expires},
		})
	})
	if err != nil {
		return v1.Invite{}, time.Time{}, err
	}
	return v1.Invite{Host: a.host, Port: a.port, Token: token, Fingerprint: a.ca.Fingerprint()}, expires, nil
}

// Joined is the outcome of a successful join or owner bootstrap.
type Joined struct {
	ID          domain.ID
	Certificate []byte // PEM
	NotAfter    time.Time
}

// Join redeems a token for a client certificate. Order matters: the CSR is
// validated before the database is touched, and everything else (consuming
// the token, creating the identity, signing, auditing) is one transaction, so
// a failure anywhere leaves the token unspent.
func (a *Authority) Join(ctx context.Context, token, csrPEM, name string) (Joined, error) {
	csr, err := ParseCSR(csrPEM)
	if err != nil {
		return Joined{}, err
	}
	name, err = cleanName(name)
	if err != nil {
		return Joined{}, err
	}
	if token == "" {
		return Joined{}, errBadToken()
	}
	hash := hashToken(token)
	now := a.clock.Now()
	var out Joined
	err = a.db.WithTx(ctx, func(tx *store.Tx) error {
		kind, ok, err := tx.ConsumeJoinToken(ctx, hash, now)
		if err != nil {
			return err
		}
		if !ok {
			return errBadToken()
		}
		if kind != domain.KindClient {
			// Returning the error rolls the consumption back: the token stays valid.
			return domain.NewError(domain.CodeNotImplemented, "worker enrolment is not available yet", nil)
		}
		out, err = a.createIdentity(ctx, tx, kind, name, csr, now, store.AuditEntry{
			ActorKind: domain.ActorHuman,
		})
		if err != nil {
			return err
		}
		return tx.LinkJoinToken(ctx, hash, out.ID)
	})
	return out, err
}

// BootstrapOwner creates the first client identity without a token: whoever
// has a shell on the Controller host is the owner (§16.6).
func (a *Authority) BootstrapOwner(ctx context.Context, csrPEM, name string) (Joined, error) {
	csr, err := ParseCSR(csrPEM)
	if err != nil {
		return Joined{}, err
	}
	name, err = cleanName(name)
	if err != nil {
		return Joined{}, err
	}
	var out Joined
	err = a.db.WithTx(ctx, func(tx *store.Tx) error {
		out, err = a.createIdentity(ctx, tx, domain.KindClient, name, csr, a.clock.Now(), store.AuditEntry{
			ActorKind: domain.ActorSystem, ActorID: "controller-init",
		})
		return err
	})
	return out, err
}

func (a *Authority) createIdentity(ctx context.Context, tx *store.Tx, kind domain.Kind, name string,
	csr *x509.CertificateRequest, now time.Time, audit store.AuditEntry,
) (Joined, error) {
	id := a.ids.New(kind)
	der, serial, notAfter, err := a.ca.signClient(csr, id, now, a.certTTL)
	if err != nil {
		return Joined{}, err
	}
	err = tx.InsertIdentity(ctx, store.Identity{
		ID: id, Kind: kind, Name: name, CertSerial: serial, NotAfter: notAfter, CreatedAt: now,
	})
	if err != nil {
		return Joined{}, err
	}
	if audit.ActorID == "" {
		audit.ActorID = string(id) // a join is authorised by the token: the new identity acts
	}
	audit.Action, audit.ResourceKind, audit.ResourceID, audit.Result = "identity.joined", "identity", string(id), "ok"
	audit.After = map[string]any{"kind": kind, "name": name, "cert_serial": serial, "not_after": notAfter}
	if err := tx.AppendAudit(ctx, audit); err != nil {
		return Joined{}, err
	}
	return Joined{ID: id, Certificate: pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: der}), NotAfter: notAfter}, nil
}

// Renew replaces the certificate of the authenticated identity. presented is
// the certificate of the request; it must be the identity's current one, or
// its previous one within the grace period. Everything is checked again inside
// the transaction against a.clock, so a certificate that expired, or an
// identity revoked, between authentication and commit does not renew.
func (a *Authority) Renew(ctx context.Context, p domain.Principal, presented *x509.Certificate, csrPEM string) (Joined, error) {
	csr, err := ParseCSR(csrPEM)
	if err != nil {
		return Joined{}, err
	}
	presentedSerial := presented.SerialNumber.Text(16)
	now := a.clock.Now()
	var out Joined
	err = a.db.WithTx(ctx, func(tx *store.Tx) error {
		ident, err := tx.IdentityByID(ctx, p.ID)
		if errors.Is(err, store.ErrNotFound) {
			return errNotAuthenticated()
		}
		if err != nil {
			return err
		}
		if !validAt(presented, now) || !ident.UsableAt(now) {
			return errNotAuthenticated()
		}
		der, serial, notAfter, err := a.ca.signClient(csr, ident.ID, now, a.certTTL)
		if err != nil {
			return err
		}
		var swapped bool
		switch {
		case ident.CertSerial == presentedSerial:
			swapped, err = tx.ReplaceCertificate(ctx, ident.ID, presentedSerial, serial, notAfter, now.Add(renewGrace))
		case ident.PreviousSerial == presentedSerial:
			swapped, err = tx.ReplaceFromPrevious(ctx, ident.ID, presentedSerial, serial, notAfter, now)
		}
		if err != nil {
			return err
		}
		if !swapped {
			return errNotAuthenticated()
		}
		out = Joined{ID: ident.ID, Certificate: pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: der}), NotAfter: notAfter}
		return tx.AppendAudit(ctx, store.AuditEntry{
			ActorKind: p.ActorKind(), ActorID: string(p.ID), Action: "identity.renewed",
			ResourceKind: "identity", ResourceID: string(ident.ID), Result: "ok",
			Before: map[string]any{"cert_serial": presentedSerial},
			After:  map[string]any{"cert_serial": serial, "not_after": notAfter},
		})
	})
	return out, err
}

func validAt(cert *x509.Certificate, now time.Time) bool {
	return !now.Before(cert.NotBefore) && now.Before(cert.NotAfter)
}

// RevokeClient revokes a client identity. Revoking twice succeeds silently.
// The last usable client cannot be revoked: nobody would be left to administer
// the Controller. Revoking oneself is fine while others remain, and a client
// that is already expired is not usable, so it can always be revoked.
func (a *Authority) RevokeClient(ctx context.Context, by domain.Principal, id domain.ID) error {
	now := a.clock.Now()
	return a.db.WithTx(ctx, func(tx *store.Tx) error {
		ident, err := tx.IdentityByID(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && ident.Kind != domain.KindClient) {
			return domain.NewError(domain.CodeNotFound, "client not found", map[string]any{"id": string(id)})
		}
		if err != nil {
			return err
		}
		if ident.Revoked() {
			return nil
		}
		if ident.UsableAt(now) {
			usable, err := tx.CountUsableIdentities(ctx, domain.KindClient, now)
			if err != nil {
				return err
			}
			if usable <= 1 {
				return domain.NewError(domain.CodePolicyDenied, "the last usable client cannot be revoked", nil)
			}
		}
		if err := tx.Revoke(ctx, id, domain.KindClient, now); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, store.AuditEntry{
			ActorKind: by.ActorKind(), ActorID: string(by.ID), Action: "identity.revoked",
			ResourceKind: "identity", ResourceID: string(id), Result: "ok",
		})
	})
}

// Clients lists the client identities.
func (a *Authority) Clients(ctx context.Context) ([]store.Identity, error) {
	return a.db.Identities(ctx, domain.KindClient)
}

// Session is an authenticated request. Superseded is true when the certificate
// is the one a renewal replaced, still within its grace: it may only renew.
type Session struct {
	Principal  domain.Principal
	Superseded bool
}

// Authenticate maps a certificate that already passed TLS verification to a
// live identity. Unknown serials, revoked identities and certificates outside
// their validity window are rejected, so revocation and expiry apply to every
// request, including those on a connection that was opened while the
// certificate was still valid. A replaced certificate within its renewal grace
// is accepted but marked Superseded.
func (a *Authority) Authenticate(ctx context.Context, cert *x509.Certificate) (Session, error) {
	now := a.clock.Now()
	if !validAt(cert, now) {
		return Session{}, errNotAuthenticated()
	}
	serial := cert.SerialNumber.Text(16)
	ident, err := a.db.IdentityBySerial(ctx, serial)
	if errors.Is(err, store.ErrNotFound) {
		return Session{}, errNotAuthenticated()
	}
	if err != nil {
		return Session{}, err
	}
	if !ident.UsableAt(now) || string(ident.ID) != cert.Subject.CommonName {
		return Session{}, errNotAuthenticated()
	}
	session := Session{Principal: domain.Principal{ID: ident.ID, Kind: ident.Kind}}
	if ident.CertSerial != serial {
		if !now.Before(ident.PreviousValidUntil) {
			return Session{}, errNotAuthenticated()
		}
		session.Superseded = true
	}
	return session, nil
}

func errNotAuthenticated() *domain.Error {
	return domain.NewError(domain.CodeUnauthenticated, "certificate is not valid for this Controller", nil)
}

func errBadToken() *domain.Error {
	return domain.NewError(domain.CodeUnauthenticated, "token is invalid, expired or already used", nil)
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unnamed", nil
	}
	if len([]rune(name)) > maxNameLen || strings.IndexFunc(name, isHidden) >= 0 {
		return "", invalidRequest(fmt.Sprintf("name must be at most %d printable characters", maxNameLen))
	}
	return name, nil
}

// isHidden is true for characters that print as nothing or reorder text
// (control characters and Unicode format characters such as U+202E), which
// would let a name impersonate another in listings.
func isHidden(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }
