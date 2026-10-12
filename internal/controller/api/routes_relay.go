package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/oklog/ulid/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/relay"
	"github.com/Autarquico/codefabric/internal/domain"
)

// registerRelayRoutes adds the git relay (§16.8, §22.1): the token a Worker
// asks for, the four requests of git's smart HTTP protocol, and the manual
// retry of a publication. Nothing else of git's HTTP protocol is routed.
func (s *Server) registerRelayRoutes() {
	s.handle("POST /v1/worker/runs/{run}/relay-token", workersOnly, s.relayToken)
	s.handle("POST /v1/worker/runs/{run}/orphan-token", workersOnly, s.orphanToken)
	s.handle("GET /v1/relay/{repo}/info/refs", workersOnly, s.relayInfoRefs)
	s.handle("POST /v1/relay/{repo}/git-upload-pack", workersOnly, s.relayRequest(relay.UploadPack))
	s.handle("POST /v1/relay/{repo}/git-receive-pack", workersOnly, s.relayRequest(relay.ReceivePack))
	s.handle("POST /v1/runs/{ref}/publish", clientsOnly, s.publishRun)
}

func (s *Server) relayToken(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.RelayTokenRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Runs.MintRelayToken(r.Context(), c.Session, r.PathValue("run"), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) orphanToken(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.OrphanTokenRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Runs.MintOrphanToken(r.Context(), c.Session, r.PathValue("run"), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) publishRun(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.ActionRequest
	if err := decodeOptionalBody(w, r, &req); err != nil {
		return err
	}
	run, err := s.work.Runs.Publish(r.Context(), c.Session, r.PathValue("ref"), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, run)
	return nil
}

func (s *Server) relayInfoRefs(w http.ResponseWriter, r *http.Request, c caller) error {
	switch r.URL.Query().Get("service") {
	case "git-upload-pack":
		return s.serveRelay(w, r, c, relay.InfoRefsUpload)
	case "git-receive-pack":
		return s.serveRelay(w, r, c, relay.InfoRefsReceive)
	}
	// The dumb protocol, which asks without a service, is not served.
	return domain.NewError(domain.CodeNotFound, "not found", nil)
}

func (s *Server) relayRequest(ep relay.Endpoint) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request, c caller) error { return s.serveRelay(w, r, c, ep) }
}

// serveRelay authorises one request from the server's own state and hands it to
// the relay. The authorisation is made for each request, whatever came before.
func (s *Server) serveRelay(w http.ResponseWriter, r *http.Request, c caller, ep relay.Endpoint) error {
	project, ok := projectOfRepo(r.PathValue("repo"))
	if !ok {
		return domain.NewError(domain.CodeNotFound, "not found", nil)
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	// Nothing downstream, the environment of the hooks included, ever sees the token.
	r.Header.Del("Authorization")
	grant, err := s.work.Runs.AuthorizeRelay(r.Context(), c.Session, string(project), ep == relay.InfoRefsReceive || ep == relay.ReceivePack, token)
	if err != nil {
		return err
	}
	if s.relay == nil {
		return domain.NewError(domain.CodeInternal, "the git relay is not available on this Controller", nil)
	}
	err = s.relay.Serve(w, r, ep, relay.Grant{
		Project: grant.Project, Worker: grant.Worker, Write: grant.Write, Branch: grant.Branch, Ref: grant.Ref,
		CreateOnly: grant.CreateOnly, ExpectedOID: grant.ExpectedOID,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, relay.ErrBusy):
		writeJSON(w, http.StatusTooManyRequests, v1.ErrorEnvelope{Error: v1.ErrorBody{Code: domain.CodeWorkerBusy, Message: "the git relay is busy: try again"}})
		return nil
	case errors.Is(err, relay.ErrTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, v1.ErrorEnvelope{Error: v1.ErrorBody{Code: domain.CodeInvalidRequest, Message: "the request is larger than the relay accepts"}})
		return nil
	case errors.Is(err, relay.ErrBadBody):
		return domain.NewError(domain.CodeInvalidRequest, "the request body cannot be read", nil)
	}
	return err
}

// projectOfRepo reads the repository name of a URL, `<project>.git`, and
// accepts only what the relay itself names: a canonical Project ID. Nothing in
// the path (encoded separators included) reaches the file system by any other way.
func projectOfRepo(repo string) (domain.ID, bool) {
	name, ok := strings.CutSuffix(repo, ".git")
	if !ok {
		return "", false
	}
	kind, id, err := domain.ParseID(name)
	if err != nil || kind != domain.KindProject || name != string(kind)+"_"+ulid.ULID(id).String() {
		return "", false
	}
	return domain.ID(name), true
}
