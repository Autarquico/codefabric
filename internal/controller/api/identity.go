package api

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	maxBodyBytes  = 64 << 10
	maxTTLSeconds = int64(math.MaxInt64 / int64(time.Second))
)

const renewPattern = "POST /v1/identity/renew"

func (s *Server) registerRoutes() {
	s.handle("POST /v1/join", nil, s.join)
	s.handle(renewPattern, anyIdentity, s.renew)
	s.handle("POST /v1/tokens", clientsOnly, s.createToken)
	s.handle("GET /v1/status", clientsOnly, s.status)
	s.handle("GET /v1/clients", clientsOnly, s.listClients)
	s.handle("DELETE /v1/clients/{id}", clientsOnly, s.removeClient)
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	return decodeBodyUpTo(w, r, dst, maxBodyBytes)
}

// decodeBodyUpTo is decodeBody for an endpoint whose body is bigger than the common limit.
func decodeBodyUpTo(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return domain.NewError(domain.CodeInvalidRequest, "request body is not valid JSON for this endpoint", nil)
	}
	return nil
}

func certificateResponse(j pki.Joined) v1.CertificateResponse {
	return v1.CertificateResponse{IdentityID: string(j.ID), Certificate: string(j.Certificate), NotAfter: j.NotAfter}
}

// join redeems a token. The response is built only after the transaction
// committed, and is never stored: it is not idempotent by design.
func (s *Server) join(w http.ResponseWriter, r *http.Request, _ caller) error {
	var req v1.JoinRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	joined, err := s.authority.Join(r.Context(), req.Token, req.CSR, req.Name, req.Worker)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, certificateResponse(joined))
	return nil
}

func (s *Server) renew(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.RenewRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	joined, err := s.authority.Renew(r.Context(), c.Session, req.CSR)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, certificateResponse(joined))
	return nil
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.CreateTokenRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	ttl := secondsToDuration(req.TTLSeconds)
	invite, expires, err := s.authority.CreateToken(r.Context(), c.Session, domain.Kind(req.Kind), ttl)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, v1.CreateTokenResponse{Kind: req.Kind, ExpiresAt: expires, Invite: invite.String()})
	return nil
}

func (s *Server) listClients(w http.ResponseWriter, r *http.Request, _ caller) error {
	identities, err := s.authority.Clients(r.Context())
	if err != nil {
		return err
	}
	out := v1.ListClientsResponse{Clients: make([]v1.Client, 0, len(identities))}
	for _, i := range identities {
		c := v1.Client{ID: string(i.ID), Name: i.Name, CreatedAt: i.CreatedAt, NotAfter: i.NotAfter}
		if i.Revoked() {
			revoked := i.RevokedAt
			c.RevokedAt = &revoked
		}
		out.Clients = append(out.Clients, c)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) removeClient(w http.ResponseWriter, r *http.Request, c caller) error {
	id := r.PathValue("id")
	if kind, _, err := domain.ParseID(id); err != nil || kind != domain.KindClient {
		return domain.NewError(domain.CodeNotFound, "client not found", nil)
	}
	if err := s.authority.RevokeClient(r.Context(), c.Session, domain.ID(id)); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// secondsToDuration converts without overflowing; the Authority owns the TTL
// policy and rejects out-of-range values itself.
func secondsToDuration(seconds int64) time.Duration {
	return time.Duration(max(-maxTTLSeconds, min(seconds, maxTTLSeconds))) * time.Second
}
