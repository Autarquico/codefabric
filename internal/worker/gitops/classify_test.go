package gitops

import (
	"strings"
	"testing"
)

func TestClassifyPushFailures(t *testing.T) {
	const token = "very-secret-token-value"
	for name, tc := range map[string]struct {
		stdout, stderr string
		want           PushClass
	}{
		"dns":        {"", "fatal: unable to access 'https://h/r.git/': Could not resolve host: h", PushUnreachable},
		"refused":    {"", "fatal: unable to access 'https://h/': Failed to connect to h port 7443: Connection refused", PushUnreachable},
		"reset":      {"", "fatal: unable to access 'https://h/': OpenSSL SSL_read: Connection reset by peer, errno 54", PushUnreachable},
		"syscall":    {"", "fatal: unable to access: LibreSSL SSL_connect: SSL_ERROR_SYSCALL in connection to h:7443", PushUnreachable},
		"timeout":    {"", "fatal: unable to access: Operation too slow. Less than 1000 bytes/sec transferred", PushUnreachable},
		"5xx":        {"", "fatal: unable to access 'https://h/': The requested URL returned error: 503", PushUnreachable},
		"busy":       {"", "fatal: unable to access 'https://h/': The requested URL returned error: 429", PushUnreachable},
		"413":        {"", "fatal: unable to access 'https://h/': The requested URL returned error: 413", PushPermanent},
		"400":        {"", "error: RPC failed; HTTP 400 curl 22 The requested URL returned error: 400", PushPermanent},
		"rpc 500":    {"", "error: RPC failed; HTTP 500 curl 22 The requested URL returned error: 500\nfatal: the remote end hung up unexpectedly", PushUnreachable},
		"timeout408": {"", "fatal: unable to access: The requested URL returned error: 408", PushUnreachable},
		"gnutls cut": {"", "fatal: unable to access 'https://h/': gnutls_handshake() failed: The TLS connection was non-properly terminated.", PushUnreachable},
		"gnutls ca":  {"", "fatal: unable to access 'https://h/': server certificate verification failed. CAfile: /x CRLfile: none", PushPermanent},
		"gnutls ca2": {"", "fatal: unable to access 'https://h/': The certificate signer not trusted", PushPermanent},
		"403":        {"", "fatal: unable to access 'https://h/': The requested URL returned error: 403", PushForbidden},
		"401":        {"", "fatal: unable to access 'https://h/': The requested URL returned error: 401", PushPermanent},
		"bad cert":   {"", "fatal: unable to access 'https://h/': SSL certificate problem: unable to get local issuer certificate", PushPermanent},
		"hook":       {"!\tabc:refs/heads/x\t[remote rejected] (pre-receive hook declined)\n", "remote: rejected by the CodeFabric relay", PushRefused},
		"rejected":   {"!\tabc:refs/heads/x\t[rejected] (non-fast-forward)\n", "", PushRefused},
		"local":      {"", "fatal: unable to write sha1 filename: No space left on device", PushLocal},
		"with token": {"", "fatal: Authorization: Bearer " + token + " leaked: The requested URL returned error: 403", PushForbidden},
	} {
		e := classify(result{stdout: tc.stdout, stderr: tc.stderr, exit: 1}, token)
		if e.Class != tc.want {
			t.Errorf("%s: class %d, want %d (%s)", name, e.Class, tc.want, e.Message)
		}
		if strings.Contains(e.Message, token) {
			t.Errorf("%s: the token is in the message", name)
		}
		if e.Counts() != (tc.want != PushUnreachable) {
			t.Errorf("%s: Counts = %v", name, e.Counts())
		}
	}
}

func TestBoundCutsAndDropsWhatTheControllerWouldRefuse(t *testing.T) {
	var many []string
	for i := range 150 {
		many = append(many, "dir/file"+string(rune('a'+i%26))+string(rune('a'+i/26))+".env")
	}
	got, truncated := bound(many)
	if len(got) != 100 || !truncated {
		t.Errorf("got %d paths, truncated %v", len(got), truncated)
	}
	got, truncated = bound([]string{"a.env", "b\nc.env", "d\u200be.env", "ok.pem"})
	if len(got) != 2 || !truncated {
		t.Errorf("hidden paths: %v %v", got, truncated)
	}
}
