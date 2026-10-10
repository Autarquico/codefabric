package api_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/domain"
)

var clientsOnly = []domain.Kind{domain.KindClient}

// failureRoutes adds the routes that misbehave in each way the middlewares must contain.
func failureRoutes(s *api.Server) {
	s.AddRoute("GET /v1/test/panic", clientsOnly, func(http.ResponseWriter, *http.Request) error {
		panic("boom-secret")
	})
	s.AddRoute("GET /v1/test/error", clientsOnly, func(http.ResponseWriter, *http.Request) error {
		return errors.New("db exploded at /secret/path")
	})
	s.AddRoute("GET /v1/test/panic-after-header", clientsOnly, func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(http.StatusOK)
		panic("boom-secret")
	})
	s.AddRoute("GET /v1/test/panic-after-body", clientsOnly, func(w http.ResponseWriter, _ *http.Request) error {
		_, _ = io.WriteString(w, `{"partial":`)
		_ = http.NewResponseController(w).Flush()
		panic("boom-secret")
	})
	s.AddRoute("POST /v1/test/echo", clientsOnly, func(w http.ResponseWriter, r *http.Request) error {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
}

type reply struct {
	status int
	header http.Header
	body   []byte
}

// fetch performs a request and returns the response with its body read, or the
// transport error that stopped it.
func fetch(t *testing.T, hc *http.Client, url, method, body string, mod func(*http.Request)) (reply, error) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	must(t, err)
	if mod != nil {
		mod(r)
	}
	resp, err := hc.Do(r)
	if err != nil {
		return reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, resp.Header, raw}, err
}

func withID(id string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("X-Request-ID", id) }
}

// logLine is the access-log line of the request with that ID.
func logLine(t *testing.T, tc *testController, id string) string {
	t.Helper()
	for _, line := range strings.Split(tc.logs.String(), "\n") {
		if strings.Contains(line, "msg=request ") && strings.Contains(line+" ", "request_id="+id+" ") {
			return line
		}
	}
	t.Fatalf("no access-log line for request %s in:\n%s", id, tc.logs)
	return ""
}

func TestApiSdkCliStatus_RequestIDIsKeptOnlyWhenItIsPlain(t *testing.T) {
	tc := newController(t, time.Now())
	hc := tc.httpClientFor(tc.ownerDir)
	url := "https://" + tc.addr + "/v1/status"
	id64 := strings.Repeat("a", 64)

	for _, id := range []string{"abc-123_X.y", id64, "0"} {
		got, err := fetch(t, hc, url, http.MethodGet, "", withID(id))
		must(t, err)
		if gotID := got.header.Get("X-Request-ID"); gotID != id {
			t.Errorf("valid ID %q came back as %q", id, gotID)
		}
		logLine(t, tc, id)
	}

	invalid := map[string]func(*http.Request){
		"empty":     withID(""),
		"65 chars":  withID(id64 + "a"),
		"space":     withID("has space"),
		"tab":       withID("has\ttab"),
		"quote":     withID(`a"b`),
		"non-ASCII": withID("idé"),
		"repeated": func(r *http.Request) {
			r.Header.Add("X-Request-ID", "first")
			r.Header.Add("X-Request-ID", "second")
		},
	}
	for name, mod := range invalid {
		t.Run(name, func(t *testing.T) {
			got, err := fetch(t, hc, url, http.MethodGet, "", mod)
			must(t, err)
			id := got.header.Get("X-Request-ID")
			if len(id) != 26 || strings.Trim(id, "0123456789ABCDEFGHJKMNPQRSTVWXYZ") != "" {
				t.Fatalf("expected a generated ULID, got %q", id)
			}
			logLine(t, tc, id) // the same ID in the response and in the log
		})
	}
}

func TestApiSdkCliStatus_TheAccessLogHasNoBodyQueryOrHeaders(t *testing.T) {
	tc := newControllerWithRoutes(t, time.Now(), failureRoutes)
	hc := tc.httpClientFor(tc.ownerDir)
	_, err := fetch(t, hc, "https://"+tc.addr+"/v1/test/echo?token=QUERYSECRET", http.MethodPost, `{"password":"BODYSECRET"}`,
		func(r *http.Request) {
			withID("log-check")(r)
			r.Header.Set("Authorization", "Bearer HEADERSECRET")
		})
	must(t, err)

	line := logLine(t, tc, "log-check")
	for _, secret := range []string{"QUERYSECRET", "BODYSECRET", "HEADERSECRET"} {
		if strings.Contains(tc.logs.String(), secret) {
			t.Errorf("the log contains %q", secret)
		}
	}
	for _, want := range []string{"method=POST", "path=/v1/test/echo", "status=204", "duration=", "principal=client_"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %q: %s", want, line)
		}
	}
}

func TestApiSdkCliStatus_TheLogNamesThePrincipalOfAuthenticatedDeniedAndAnonymousRequests(t *testing.T) {
	tc := newController(t, time.Now())
	owner, err := tc.owner.Describe()
	must(t, err)
	url := "https://" + tc.addr + "/v1/status"

	_, err = fetch(t, tc.httpClientFor(tc.ownerDir), url, http.MethodGet, "", withID("authenticated"))
	must(t, err)
	_, err = fetch(t, tc.enrollWorker(), url, http.MethodGet, "", withID("denied"))
	must(t, err)
	_, err = fetch(t, tc.httpClient(), url, http.MethodGet, "", withID("anonymous"))
	must(t, err)

	if line := logLine(t, tc, "authenticated"); !strings.Contains(line, "status=200") || !strings.Contains(line, "principal="+owner.IdentityID) {
		t.Errorf("authenticated: %s", line)
	}
	if line := logLine(t, tc, "denied"); !strings.Contains(line, "status=403") || !strings.Contains(line, "principal=worker_") {
		t.Errorf("denied: %s", line)
	}
	if line := logLine(t, tc, "anonymous"); !strings.Contains(line, "status=401") || strings.Contains(line, "principal=") {
		t.Errorf("anonymous: %s", line)
	}
}

func TestApiSdkCliStatus_AnUnexpectedFailureIsAGeneric500WithTheRequestID(t *testing.T) {
	tc := newControllerWithRoutes(t, time.Now(), failureRoutes)
	hc := tc.httpClientFor(tc.ownerDir)

	for _, path := range []string{"/v1/test/panic", "/v1/test/error"} {
		t.Run(path, func(t *testing.T) {
			got, err := fetch(t, hc, "https://"+tc.addr+path, http.MethodGet, "", withID("fail"+strings.ReplaceAll(path, "/", "-")))
			must(t, err)
			id := got.header.Get("X-Request-ID")
			if got.status != http.StatusInternalServerError {
				t.Fatalf("status %d", got.status)
			}
			body := string(got.body)
			for _, want := range []string{`"code":"CF_INTERNAL"`, `"message":"internal error"`, `"request_id":"` + id + `"`} {
				if !strings.Contains(body, want) {
					t.Errorf("body lacks %s: %s", want, body)
				}
			}
			for _, leak := range []string{"boom-secret", "/secret/path", "goroutine", ".go:"} {
				if strings.Contains(body, leak) {
					t.Errorf("body leaks %q: %s", leak, body)
				}
			}
			if line := logLine(t, tc, id); !strings.Contains(line, "status=500") {
				t.Errorf("the 500 is not in the access log: %s", line)
			}
		})
	}
	logs := tc.logs.String()
	if !strings.Contains(logs, "boom-secret") || !strings.Contains(logs, "goroutine") || !strings.Contains(logs, "/secret/path") {
		t.Errorf("the panic with its stack, and the error, belong in the server log:\n%s", logs)
	}
}

func TestApiSdkCliStatus_APanicAfterTheResponseStartedAbortsTheConnection(t *testing.T) {
	tc := newControllerWithRoutes(t, time.Now(), failureRoutes)
	hc := tc.httpClientFor(tc.ownerDir)

	for _, path := range []string{"/v1/test/panic-after-header", "/v1/test/panic-after-body"} {
		t.Run(path, func(t *testing.T) {
			id := "abort" + strings.ReplaceAll(path, "/", "-")
			got, err := fetch(t, hc, "https://"+tc.addr+path, http.MethodGet, "", withID(id))
			if err == nil {
				t.Fatalf("the caller saw a complete response: %d %q", got.status, got.body)
			}
			if line := logLine(t, tc, id); !strings.Contains(line, "aborted=true") || !strings.Contains(line, "level=ERROR") {
				t.Errorf("an aborted response should be logged as such: %s", line)
			}
		})
	}
}

func TestApiSdkCliStatus_SDKSeesAnInternalErrorWithItsRequestID(t *testing.T) {
	tc := newController(t, time.Now(), withHealth(func(context.Context) error { panic("boom-secret") }))
	_, err := tc.owner.Status(t.Context())
	wantCode(t, err, domain.CodeInternal)
	var de *domain.Error
	_ = errors.As(err, &de)
	id, _ := de.Details["request_id"].(string)
	if de.Message != "internal error" || id == "" {
		t.Fatalf("got %+v", de)
	}
	logLine(t, tc, id)
	if strings.Contains(err.Error(), "boom-secret") {
		t.Errorf("the error leaks the panic: %v", err)
	}
}
