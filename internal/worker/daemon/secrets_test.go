package daemon

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	secretsfile "github.com/Autarquico/codefabric/internal/adapters/secrets/file"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/state"
	"github.com/Autarquico/codefabric/internal/worker/sysinfo"
)

// beating is a daemon that measures a fixed machine and holds the catalog at path.
func beating(t *testing.T, path string) *daemon {
	t.Helper()
	return &daemon{
		epoch: "01J00000000000000000000000", generation: 1, version: "v0.3.1", log: slog.New(slog.DiscardHandler),
		measure: func(string) (sysinfo.Info, error) {
			return sysinfo.Info{Hostname: "box", OS: "linux", Arch: "amd64", CPU: 4, MemoryMB: 4096, DiskMB: 1000}, nil
		},
		secrets: secretsfile.New(path, clock.NewFake(time.Unix(0, 0))),
	}
}

func catalogPath(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "worker")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "secrets.json")
}

func TestHeartbeat_CarriesTheMetadataOfTheCatalogAndNoValue(t *testing.T) {
	path := catalogPath(t)
	d := beating(t, path)
	sentinel := []byte("Zq8v-sentinel-7Hc2LmP0xR4tW9yB")
	project := "project_01J00000000000000000000000"
	if err := d.secrets.Set(t.Context(), "gh/token", sentinel, []string{project}, secrets.BackendFile); err != nil {
		t.Fatal(err)
	}

	req, err := d.heartbeat(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if req.Generation != 1 || req.Secrets == nil || req.Secrets.Seq != 1 || len(req.Secrets.Refs) != 1 ||
		req.Secrets.Refs[0].Ref != "gh/token" || req.Secrets.Refs[0].Projects[0] != project || req.Health.Status != v1.HealthOK {
		t.Fatalf("heartbeat = %+v", req)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, encoded := range []string{
		string(sentinel), hex.EncodeToString(sentinel), base64.StdEncoding.EncodeToString(sentinel),
		base64.RawURLEncoding.EncodeToString(sentinel), "sentinel",
	} {
		if strings.Contains(string(body), encoded) {
			t.Errorf("the heartbeat carries the value, as %q", encoded)
		}
	}

	next, err := d.heartbeat(t.Context())
	if err != nil || next.Secrets.Seq != 2 {
		t.Errorf("the next heartbeat has seq %d (%v), want 2", next.Secrets.Seq, err)
	}
}

func TestHeartbeat_AnEmptyOrMissingCatalogIsAnEmptyReport(t *testing.T) {
	req, err := beating(t, catalogPath(t)).heartbeat(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(req.Secrets)
	if err != nil || string(body) != `{"seq":1,"refs":[]}` || req.Health.Status != v1.HealthOK {
		t.Errorf("secrets = %s (%v), health %+v", body, err, req.Health)
	}
}

func TestHeartbeat_AnUnreadableCatalogIsReportedEmptyAndDegradesTheWorker(t *testing.T) {
	path := catalogPath(t)
	d := beating(t, path)
	if err := d.secrets.Set(t.Context(), "gh/token", []byte("long-enough-value"), nil, secrets.BackendFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // the point of the test
		t.Fatal(err)
	}

	req, err := d.heartbeat(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Secrets.Refs) != 0 || req.Health.Status != v1.HealthDegraded || req.Health.Message != catalogUnreadable {
		t.Errorf("secrets %+v, health %+v", req.Secrets, req.Health)
	}
}

func TestAnswerFence(t *testing.T) {
	retired := func(generation any) error {
		return domain.NewError(domain.CodeStaleLease, "retired", map[string]any{"reason": v1.FenceGenerationRetired, "generation": generation})
	}
	for _, tc := range []struct {
		name      string
		err       error
		laterRun  bool // another run on the same state.db took a generation
		again     bool
		wantGen   uint64
		wantError bool
	}{
		{name: "a retired generation takes one above the Controller's", err: retired(float64(7)), again: true, wantGen: 8},
		{name: "a retired generation superseded on this machine stops", err: retired(float64(7)), laterRun: true, wantError: true},
		{name: "a retired generation with no number is an error", err: retired("seven"), wantError: true, wantGen: 1},
		{name: "an overtaken heartbeat sends the next", err: domain.NewError(domain.CodeStaleLease, "late", map[string]any{"reason": v1.FenceOutOfOrder}), again: true, wantGen: 1},
		{name: "a stale lease of anything else is an error", err: domain.NewError(domain.CodeStaleLease, "x", nil), wantError: true, wantGen: 1},
		{name: "another error is an error", err: domain.NewError(domain.CodeInvalidRequest, "x", nil), wantError: true, wantGen: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := state.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			d := &daemon{stateDB: db, log: slog.New(slog.DiscardHandler)}
			if d.generation, err = db.StartGeneration(t.Context()); err != nil {
				t.Fatal(err)
			}
			if tc.laterRun {
				if _, err := db.StartGeneration(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			again, err := d.answerFence(t.Context(), tc.err)
			if again != tc.again || (err != nil) != tc.wantError {
				t.Fatalf("again %t, err %v; want %t, error %t", again, err, tc.again, tc.wantError)
			}
			if tc.laterRun && !errors.Is(err, state.ErrSuperseded) {
				t.Errorf("err = %v, want ErrSuperseded", err)
			}
			if !tc.laterRun && d.generation != tc.wantGen {
				t.Errorf("generation %d, want %d", d.generation, tc.wantGen)
			}
		})
	}
}

const pastedToken = "sk-ant-oat01-Zq8vSentinel7Hc2LmP0xR4tW9yB" //nolint:gosec // a fake token

func storedValue(t *testing.T, w *keyringWorker, ref string) string {
	t.Helper()
	got, err := w.store.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return string(got.Value)
}

func TestSecretsSet_OAuthTokenWrappedAcrossLinesSaysItWasJoined(t *testing.T) {
	w := newKeyringWorker(t)
	out, err := cf(t, pastedToken[:20]+"\r\n"+pastedToken[20:30]+"\r\n"+pastedToken[30:]+"\r\n", "set", "claude/oauth-token")
	if err != nil {
		t.Fatalf("set: %v: %s", err, out)
	}
	if !strings.Contains(out, "joined the token from 3 lines of the pasted output") || strings.Contains(out, "kept the token line") {
		t.Errorf("output = %s", out)
	}
	if got := storedValue(t, w, "claude/oauth-token"); got != pastedToken {
		t.Errorf("the stored value is not the whole token (%d bytes)", len(got))
	}
}

func TestSecretsSet_OAuthTokenKeepsTheTokenLineOfWhatSetupTokenPrints(t *testing.T) {
	w := newKeyringWorker(t)
	pasted := "Your OAuth token (valid for 1 year):\n\n" + pastedToken + "\n\nStore this token securely.\n"
	out, err := cf(t, pasted, "set", "claude/oauth-token")
	if err != nil {
		t.Fatalf("set: %v: %s", err, out)
	}
	if !strings.Contains(out, "kept the token line from the pasted output") || strings.Contains(out, pastedToken) {
		t.Errorf("output = %s", out)
	}
	if got := storedValue(t, w, "claude/oauth-token"); got != pastedToken {
		t.Errorf("the stored value is not the token line (%d bytes)", len(got))
	}
}

func TestSecretsSet_OAuthTokenThatIsNotATokenIsRefusedUnlessForced(t *testing.T) {
	w := newKeyringWorker(t)
	out, err := cf(t, "not-a-token-at-all-but-one-line\n", "set", "claude/oauth-token")
	if err == nil || !errors.Is(err, domain.NewError(domain.CodeInvalidRequest, "", nil)) || !strings.Contains(err.Error(), "--force") ||
		!strings.Contains(err.Error(), "claude setup-token") || strings.Contains(out+err.Error(), "not-a-token") {
		t.Fatalf("set: %v: %s", err, out)
	}
	if _, err := w.store.Get(t.Context(), "claude/oauth-token"); err == nil {
		t.Fatal("something was stored on refusal")
	}
	if out, err := cf(t, "not-a-token-at-all-but-one-line\n", "set", "claude/oauth-token", "--force"); err != nil || !strings.Contains(out, "will likely fail with 401") {
		t.Fatalf("forced set: %v: %s", err, out)
	}
	if got := storedValue(t, w, "claude/oauth-token"); got != "not-a-token-at-all-but-one-line" {
		t.Errorf("stored %q", got)
	}
	if m := metaOf(t, w, "claude/oauth-token"); m.Format != secrets.FormatUnexpected {
		t.Errorf("metadata = %+v, want an unexpected format", m)
	}
	// A token set again, even forced, leaves no doubt behind.
	if out, err := cf(t, pastedToken, "set", "claude/oauth-token", "--force"); err != nil || strings.Contains(out, "401") {
		t.Fatalf("set: %v: %s", err, out)
	}
	if m := metaOf(t, w, "claude/oauth-token"); m.Format != "" {
		t.Errorf("format = %q, want unknown", m.Format)
	}
}

func metaOf(t *testing.T, w *keyringWorker, ref string) secrets.Metadata {
	t.Helper()
	metas, err := w.store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range metas {
		if m.Ref == ref {
			return m
		}
	}
	t.Fatalf("no secret %s", ref)
	return secrets.Metadata{}
}

func TestSecretsSet_OtherRefsAreNotCheckedForATokenForm(t *testing.T) {
	w := newKeyringWorker(t)
	if out, err := cf(t, "not-a-token-at-all-but-one-line\n", "set", "other/secret"); err != nil {
		t.Fatalf("set: %v: %s", err, out)
	}
	if got := metaOf(t, w, "other/secret").Format; got != "" {
		t.Errorf("format = %q for another ref, want unknown", got)
	}
}

func TestNormalizeValue(t *testing.T) {
	tail := pastedToken[20:]
	head := pastedToken[:20]
	for name, tc := range map[string]struct{ raw, want string }{
		"one line":          {pastedToken + "\n", pastedToken},
		"wrapped":           {head + "\n" + tail + "\n", pastedToken},
		"wrapped in three":  {head + "\n" + tail[:5] + "\n" + tail[5:] + "\n", pastedToken},
		"setup-token":       {"Your OAuth token (valid for 1 year):\n\n" + pastedToken + "\n\nStore this token securely.\n", pastedToken},
		"wrapped in output": {"Your OAuth token:\n\n" + head + "\n" + tail + "\n\nStore this token securely.\n", pastedToken},
		"a word after":      {pastedToken + "\nDone\n", pastedToken + "Done"}, // indistinguishable from a wrapped line
		"crlf":              {pastedToken + "\r\n", pastedToken},
	} {
		t.Run(name, func(t *testing.T) {
			got, bad, err := normalizeValue("claude/oauth-token", []byte(tc.raw), io.Discard, false)
			if err != nil || bad || string(got) != tc.want {
				t.Errorf("= %q, %v, %v; want %q", got, bad, err, tc.want)
			}
		})
	}
}

func TestReadAnswer(t *testing.T) {
	never := func(time.Duration) bool { return false }
	t.Run("a wrapped paste is one answer", func(t *testing.T) {
		got, err := readAnswer(bufio.NewReader(strings.NewReader("abc\ndef\n")), never)
		if err != nil || string(got) != "abc\ndef" {
			t.Errorf("= %q, %v", got, err)
		}
	})
	t.Run("a single line", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader("abc\r\n"))
		got, err := readAnswer(r, never)
		if err != nil || string(got) != "abc" {
			t.Errorf("= %q, %v", got, err)
		}
	})
	t.Run("lines that arrive later within the window", func(t *testing.T) {
		pending := []string{"def\n"}
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte("abc\n"))
		}()
		r := bufio.NewReader(pr)
		more := func(time.Duration) bool {
			if len(pending) == 0 {
				return false
			}
			go func(s string) { _, _ = pw.Write([]byte(s)) }(pending[0])
			pending = pending[1:]
			return true
		}
		got, err := readAnswer(r, more)
		if err != nil || string(got) != "abc\ndef" {
			t.Errorf("= %q, %v", got, err)
		}
	})
	t.Run("two pastes that differ", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := bufio.NewReader(pr)
		go func() {
			_, _ = pw.Write([]byte("abc\ndef\n"))
			_, _ = pw.Write([]byte("abc\ndeX\n"))
		}()
		first, err1 := readAnswer(r, never)
		second, err2 := readAnswer(r, never)
		if err1 != nil || err2 != nil || string(first) != "abc\ndef" || string(second) != "abc\ndeX" || bytes.Equal(first, second) {
			t.Errorf("first = %q second = %q %v %v", first, second, err1, err2)
		}
	})
}

func TestSecretsSet_OAuthTokenOfSeveralLinesWithoutExactlyOneTokenIsRefused(t *testing.T) {
	for name, pasted := range map[string]string{
		"none": "Your OAuth token:\nsomething else\n",
		"two":  pastedToken + "\n" + pastedToken + "x\n",
	} {
		t.Run(name, func(t *testing.T) {
			w := newKeyringWorker(t)
			out, err := cf(t, pasted, "set", "claude/oauth-token")
			if err == nil || !strings.Contains(err.Error(), "sk-ant-oat") || strings.Contains(out+err.Error(), pastedToken) {
				t.Fatalf("set: %v: %s", err, out)
			}
			if _, err := w.store.Get(t.Context(), "claude/oauth-token"); err == nil {
				t.Error("something was stored")
			}
		})
	}
}

func TestSecretsSet_OAuthTokenCutByTheInputCapIsRefusedNotExtracted(t *testing.T) {
	w := newKeyringWorker(t)
	if out, err := cf(t, pastedToken, "set", "claude/oauth-token"); err != nil {
		t.Fatalf("set: %v: %s", err, out)
	}
	pasted := strings.Repeat("preamble\n", domain.MaxSecretValueLen/9) + pastedToken + "\nmore\n"
	out, err := cf(t, pasted, "set", "claude/oauth-token")
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("set: %v: %s", err, out)
	}
	if got := storedValue(t, w, "claude/oauth-token"); got != pastedToken {
		t.Errorf("the working credential was overwritten with %d bytes", len(got))
	}
}

func TestSecretsSet_OtherRefsKeepAMultilineValue(t *testing.T) {
	w := newKeyringWorker(t)
	value := "first line of a key\n" + pastedToken + "\nlast line"
	if out, err := cf(t, value, "set", "ssh/key"); err != nil {
		t.Fatalf("set: %v: %s", err, out)
	}
	if got := storedValue(t, w, "ssh/key"); got != value {
		t.Errorf("stored %q", got)
	}
}

func TestReadAnswer_Editing(t *testing.T) {
	never := func(time.Duration) bool { return false }
	read := func(in string, more func(time.Duration) bool) (string, error) {
		got, err := readAnswer(bufio.NewReader(strings.NewReader(in)), more)
		return string(got), err
	}
	for name, tc := range map[string]struct{ in, want string }{
		"backspace":                {"abd\x7fc\n", "abc"},
		"backspace at the start":   {"\x7fabc\n", "abc"},
		"ctrl-u":                   {"junk\x15abc\n", "abc"},
		"an arrow key is not text": {"ab\x1b[Ac\n", "abc"},
		"a lone escape":            {"ab\x1bc\n", "abc"},
		"bracketed paste":          {"\x1b[200~ab\ncd\x1b[201~\n", "ab\ncd"},
		"bracketed paste with CR":  {"\x1b[200~ab\r\ncd\x1b[201~\n", "ab\ncd"},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := read(tc.in, never); err != nil || got != tc.want {
				t.Errorf("= %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	t.Run("ctrl-d on an empty answer ends the input", func(t *testing.T) {
		if _, err := read("\x04", never); !errors.Is(err, io.EOF) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("ctrl-d after text is ignored", func(t *testing.T) {
		if got, err := read("ab\x04c\n", never); err != nil || got != "abc" {
			t.Errorf("= %q, %v", got, err)
		}
	})
	t.Run("ctrl-c aborts", func(t *testing.T) {
		if _, err := read("ab\x03", never); !errors.Is(err, errInterrupted) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a last fragment that arrives after the newline joins it", func(t *testing.T) {
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte("abc\n"))
			time.Sleep(50 * time.Millisecond)
			_, _ = pw.Write([]byte("def"))
			time.Sleep(50 * time.Millisecond)
			_, _ = pw.Write([]byte("\n"))
		}()
		calls := 0
		more := func(time.Duration) bool { calls++; return calls == 1 } // the fragment is coming; the final Enter is not a paste
		got, err := readAnswer(bufio.NewReader(pr), more)
		if err != nil || string(got) != "abc\ndef" {
			t.Errorf("= %q, %v", got, err)
		}
	})
}
