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

// CreateToken issues a single-use join token on behalf of an authenticated
// client, who is checked again inside the transaction. The only copy of the
// token is in the returned invitation.
func (a *Authority) CreateToken(ctx context.Context, by Session, kind domain.Kind, ttl time.Duration) (v1.Invite, time.Time, error) {
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
	var expires time.Time
	err = a.db.WithTx(ctx, func(tx *store.Tx) error {
		now := a.clock.Now()
		if _, err := a.requireClient(ctx, tx, by, now); err != nil {
			return err
		}
		expires = now.Add(ttl)
		if err := tx.InsertJoinToken(ctx, hash, kind, string(by.Principal.ID), now, expires); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, store.AuditEntry{
			ActorKind: by.Principal.ActorKind(), ActorID: string(by.Principal.ID), Action: "token.created",
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
	var out Joined
	err = a.db.WithTx(ctx, func(tx *store.Tx) error {
		now := a.clock.Now()
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
// its previous one within the grace period. The caller is checked again inside
// the transaction (see requireCaller), so a certificate that expired, or an
// identity revoked, between authentication and commit does not renew.
func (a *Authority) Renew(ctx context.Context, s Session, csrPEM string) (Joined, error) {
	csr, err := ParseCSR(csrPEM)
	if err != nil {
		return Joined{}, err
	}
	p := s.Principal
	presentedSerial := s.Certificate.SerialNumber.Text(16)
	var out Joined
	err = a.db.WithTx(ctx, func(tx *store.Tx) error {
		now := a.clock.Now()
		ident, err := requireCaller(ctx, tx, s, now, true)
		if err != nil {
			return err
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
func (a *Authority) RevokeClient(ctx context.Context, by Session, id domain.ID) error {
	return a.db.WithTx(ctx, func(tx *store.Tx) error {
		now := a.clock.Now()
		if _, err := a.requireClient(ctx, tx, by, now); err != nil {
			return err
		}
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
			ActorKind: by.Principal.ActorKind(), ActorID: string(by.Principal.ID), Action: "identity.revoked",
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
	Principal   domain.Principal
	Certificate *x509.Certificate // the certificate presented on the request
	Superseded  bool
}

// Session builds the session of an identity that has just been issued a
// certificate, for callers that act as it without a TLS request (init).
func (j Joined) Session() (Session, error) {
	cert, err := parseCertPEM(j.Certificate)
	if err != nil {
		return Session{}, err
	}
	kind, _, err := domain.ParseID(string(j.ID))
	if err != nil {
		return Session{}, err
	}
	return Session{Principal: domain.Principal{ID: j.ID, Kind: kind}, Certificate: cert}, nil
}

// RequireCaller is the authorisation decision of every mutating operation,
// inside the caller's own transaction (see requireCaller). Operations outside
// this package use it so that their decision is the same as the Authority's.
func RequireCaller(ctx context.Context, tx *store.Tx, s Session, now time.Time) (store.Identity, error) {
	return requireCaller(ctx, tx, s, now, false)
}

// requireCaller is the authorisation decision of every mutating operation:
// inside the transaction, with the instant read after the writer was acquired,
// the identity must exist, be usable, and the session's certificate must be in
// validity and be the identity's current one (or, only when allowSuperseded,
// its previous one within the grace). Authenticate is just a per-request filter.
func requireCaller(ctx context.Context, tx *store.Tx, s Session, now time.Time, allowSuperseded bool) (store.Identity, error) {
	ident, err := tx.IdentityByID(ctx, s.Principal.ID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Identity{}, errNotAuthenticated()
	}
	if err != nil {
		return store.Identity{}, err
	}
	if s.Certificate == nil || !validAt(s.Certificate, now) || !ident.UsableAt(now) ||
		ident.Kind != s.Principal.Kind || string(ident.ID) != s.Certificate.Subject.CommonName {
		return store.Identity{}, errNotAuthenticated()
	}
	switch serial := s.Certificate.SerialNumber.Text(16); {
	case ident.CertSerial == serial:
	case allowSuperseded && ident.PreviousSerial == serial && now.Before(ident.PreviousValidUntil):
	default:
		return store.Identity{}, errNotAuthenticated()
	}
	return ident, nil
}

// requireClient is requireCaller for operations only clients may perform.
func (a *Authority) requireClient(ctx context.Context, tx *store.Tx, s Session, now time.Time) (store.Identity, error) {
	ident, err := requireCaller(ctx, tx, s, now, false)
	if err != nil {
		return ident, err
	}
	if ident.Kind != domain.KindClient {
		return ident, domain.NewError(domain.CodePolicyDenied, "this identity may not do this", nil)
	}
	return ident, nil
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
	session := Session{Principal: domain.Principal{ID: ident.ID, Kind: ident.Kind}, Certificate: cert}
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
