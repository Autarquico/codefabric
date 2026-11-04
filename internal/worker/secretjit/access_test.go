package secretjit

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

var (
	origin      = redact.Needle{Ref: "git.origin", Value: []byte("origin-token-value-3")}
	askpassOnly = []Consumer{Askpass}
)

func deliverFor(t *testing.T, dir string, attempt uint64, access []Consumer, secrets ...redact.Needle) {
	t.Helper()
	if err := New().DeliverFor(dir, attempt, access, secrets, allow); err != nil {
		t.Fatalf("DeliverFor(attempt %d): %v", attempt, err)
	}
}

func TestReaders_EachConsumerSeesOnlyWhatItWasGranted(t *testing.T) {
	dir := runDir(t)
	deliver(t, dir, 1, v1)
	deliverFor(t, dir, 1, askpassOnly, origin)

	if got, err := AskpassReader(dir)(origin.Ref); err != nil || got != string(origin.Value) {
		t.Fatalf("askpass reads its ref: %q, %v", got, err)
	}
	if _, err := Resolver(dir)(origin.Ref); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("the harness reads an askpass ref: %v", err)
	}
	if _, err := AskpassReader(dir)(v1[0].Ref); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("askpass reads a harness ref: %v", err)
	}
	if got, err := Resolver(dir)(v1[0].Ref); err != nil || got != string(v1[0].Value) {
		t.Fatalf("the harness reads its ref: %q, %v", got, err)
	}
}

func TestRevoke_TakesAccessOnlyFromTheNamedConsumer(t *testing.T) {
	dir := runDir(t)
	deliverFor(t, dir, 1, []Consumer{Harness, Askpass}, origin)

	if err := New().Revoke(dir, Askpass, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := AskpassReader(dir)(origin.Ref); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("askpass after its revoke: %v", err)
	}
	if _, err := Resolver(dir)(origin.Ref); err != nil {
		t.Fatalf("the harness lost its access: %v", err)
	}
	wantLoad(t, dir, []redact.Needle{origin}) // retention outlives access

	deliverFor(t, dir, 1, []Consumer{Harness, Askpass}, origin)
	if _, err := AskpassReader(dir)(origin.Ref); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("a repeated grant widened a revoked ref: %v", err)
	}
}

func TestDeliver_MergesIntoTheDeliveryOfTheSameAttempt(t *testing.T) {
	dir := runDir(t)
	deliverFor(t, dir, 1, askpassOnly, origin)
	deliver(t, dir, 1, v1)

	wantLoad(t, dir, []redact.Needle{origin, v1[0], v1[1]})
	if got, err := AskpassReader(dir)(origin.Ref); err != nil || got != string(origin.Value) {
		t.Fatalf("the first delivery was lost: %q, %v", got, err)
	}
	if _, err := AskpassReader(dir)(v1[0].Ref); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("the second delivery reached askpass: %v", err)
	}
	wantOnly(t, dir, liveName)
	wantPrivate(t, dir)
}

func TestDeliver_AGrantRepeatedIsNotRewrittenWidenedOrDuplicated(t *testing.T) {
	dir := runDir(t)
	deliverFor(t, dir, 1, askpassOnly, origin)

	f := &faults{}
	if err := f.manager().DeliverFor(dir, 1, []Consumer{Harness, Askpass}, []redact.Needle{origin}, allow); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.steps {
		if strings.HasPrefix(s, "write") || strings.HasPrefix(s, "rename") {
			t.Fatalf("a repeated grant rewrote the delivery: %q", f.steps)
		}
	}
	wantLoad(t, dir, []redact.Needle{origin})
	if _, err := Resolver(dir)(origin.Ref); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("a repeated grant widened the access: %v", err)
	}
}

func TestDeliver_ARefWithAnotherValueIsAConflictAndChangesNothing(t *testing.T) {
	dir := runDir(t)
	deliverFor(t, dir, 1, askpassOnly, origin)

	other := redact.Needle{Ref: origin.Ref, Value: []byte("a-different-token-4")}
	err := New().DeliverFor(dir, 1, askpassOnly, []redact.Needle{other, v1[0]}, allow)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a changed value: %v, want ErrConflict", err)
	}
	noValue(t, err)
	if strings.Contains(err.Error(), string(other.Value)) || strings.Contains(err.Error(), string(origin.Value)) {
		t.Fatalf("the conflict says a value: %v", err)
	}
	wantLoad(t, dir, []redact.Needle{origin})
}

func TestDeliver_TheMergedDeliveryRespectsTheLimits(t *testing.T) {
	dir := runDir(t)
	deliver(t, dir, 1, v1)
	// A value that is a substring of one already held cannot be told apart by
	// the redactor; whatever NewEscapedSet refuses of the union is refused.
	var many []redact.Needle
	for i := range 64 {
		many = append(many, redact.Needle{Ref: fmt.Sprintf("extra/%02d", i), Value: fmt.Appendf(nil, "extra-value-%02d-padding", i)})
	}
	if err := New().DeliverFor(dir, 1, askpassOnly, many, allow); err == nil {
		t.Fatal("a merge past the limit of refs was accepted")
	}
	wantLoad(t, dir, v1)
}

func TestDeliverFor_RefusesNoConsumerOrAnUnknownOne(t *testing.T) {
	dir := runDir(t)
	for _, access := range [][]Consumer{nil, {}, {"root"}, {Harness, ""}} {
		if err := New().DeliverFor(dir, 1, access, v1, allow); err == nil {
			t.Errorf("access %q accepted", access)
		}
	}
	wantOnly(t, dir)
}

func TestLoad_ReadsTheManifestsWrittenBeforeAccessSets(t *testing.T) {
	dir := runDir(t)
	deliver(t, dir, 1, v1)
	if err := New().Revoke(dir, Harness, []string{v1[1].Ref}); err != nil {
		t.Fatal(err)
	}
	legacy := fmt.Sprintf(`{"schema":1,"attempt":1,"refs":[{"ref":%q,"file":%q,"harness":true},{"ref":%q,"file":%q,"harness":false}]}`,
		v1[0].Ref, fileName(v1[0].Ref), v1[1].Ref, fileName(v1[1].Ref))
	if err := os.WriteFile(filepath.Join(dir, liveName, manifestName), []byte(legacy), filePerm); err != nil {
		t.Fatal(err)
	}

	if got, err := Resolver(dir)(v1[0].Ref); err != nil || got != string(v1[0].Value) {
		t.Fatalf("harness:true is not {harness}: %q, %v", got, err)
	}
	if _, err := Resolver(dir)(v1[1].Ref); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("harness:false is access: %v", err)
	}
	if _, err := AskpassReader(dir)(v1[0].Ref); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("a legacy entry reached askpass: %v", err)
	}
	wantLoad(t, dir, v1)

	deliverFor(t, dir, 1, askpassOnly, origin) // merges into a legacy delivery
	if got, err := Resolver(dir)(v1[0].Ref); err != nil || got != string(v1[0].Value) {
		t.Fatalf("the merge lost the legacy access: %q, %v", got, err)
	}
}

func TestLoad_RefusesAnUnknownConsumerInTheManifest(t *testing.T) {
	dir := runDir(t)
	deliver(t, dir, 1, v1[:1])
	bad := fmt.Sprintf(`{"schema":1,"attempt":1,"refs":[{"ref":%q,"file":%q,"access":["everyone"]}]}`, v1[0].Ref, fileName(v1[0].Ref))
	if err := os.WriteFile(filepath.Join(dir, liveName, manifestName), []byte(bad), filePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Load = %v, want ErrCorrupt", err)
	}
}

func TestDeliver_MergeFaultAtEveryStepLeavesTheOldOrTheMergedDelivery(t *testing.T) {
	s := setup{name: "merging", prep: func(t *testing.T, dir string) { deliver(t, dir, 2, v1) }}
	merged := []redact.Needle{v1[0], v1[1], origin}
	op := func(m *Manager, dir string) error {
		return m.DeliverFor(dir, 2, askpassOnly, []redact.Needle{origin}, allow)
	}
	all := steps(t, s, op)
	for at, step := range all {
		for _, md := range modes {
			t.Run(fmt.Sprintf("%02d %s/%s", at+1, step, md.name), func(t *testing.T) {
				t.Parallel()
				dir := runDir(t)
				s.prep(t, dir)
				f := &faults{at: at + 1, mode: md}
				crashed, err := run(func() error { return op(f.manager(), dir) })
				if !crashed && err == nil {
					t.Fatalf("a fault at %q went unreported", step)
				}
				noValue(t, err)
				got, lerr := Load(dir)
				if lerr != nil || (!sameNeedles(got, v1) && !sameNeedles(got, merged)) {
					t.Fatalf("after the fault Load = %q, %v: neither the old delivery nor the merged one", got, lerr)
				}
				if err := op(New(), dir); err != nil {
					t.Fatalf("again: %v", err)
				}
				wantOnly(t, dir, liveName)
				wantLoad(t, dir, merged)
				wantPrivate(t, dir)
			})
		}
	}
}

func TestBindAndAskpassGrant(t *testing.T) {
	dir := runDir(t)
	m := New()
	if err := m.DeliverFor(dir, 1, []Consumer{Askpass}, v1[:1], allow); err != nil {
		t.Fatal(err)
	}
	if _, err := AskpassGrant(dir); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("a ref not bound to an origin: %v", err)
	}
	b := Binding{Origin: "https://example.com/org/repo.git", User: "x-access-token"}
	if err := m.Bind(dir, "api/token", b); err != nil {
		t.Fatal(err)
	}
	g, err := AskpassGrant(dir)
	if err != nil || g.Ref != "api/token" || g.Origin != b.Origin || g.User != b.User || g.Value != "old-token-value-1" {
		t.Fatalf("AskpassGrant = %+v, %v", g, err)
	}
	// A merge of the same attempt keeps the binding.
	if err := m.DeliverFor(dir, 1, []Consumer{Harness}, v1[1:], allow); err != nil {
		t.Fatal(err)
	}
	if _, err := AskpassGrant(dir); err != nil {
		t.Fatalf("after a merge: %v", err)
	}
	needles, err := Load(dir)
	if err != nil || needles[0].User != b.User {
		t.Fatalf("Load = %+v, %v: the user is not carried to the redaction", needles, err)
	}
	if err := m.Revoke(dir, Askpass, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := AskpassGrant(dir); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("after the revocation: %v", err)
	}
}

func TestBindRefusesWhatIsNotAFrame(t *testing.T) {
	dir := runDir(t)
	m := New()
	if err := m.DeliverFor(dir, 1, []Consumer{Askpass}, v1[:1], allow); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]Binding{
		"no user":      {Origin: "https://h/r", User: ""},
		"colon":        {Origin: "https://h/r", User: "a:b"},
		"at":           {Origin: "https://h/r", User: "a@b"},
		"newline":      {Origin: "https://h/r", User: "a\nb"},
		"space":        {Origin: "https://h/r", User: "a b"},
		"no origin":    {Origin: "", User: "u"},
		"control char": {Origin: "https://h/r\n", User: "u"},
	} {
		if err := m.Bind(dir, "api/token", b); err == nil {
			t.Errorf("%s: bound", name)
		}
	}
	if err := m.Bind(dir, "other", Binding{Origin: "https://h/r", User: "u"}); !errors.Is(err, ErrNotDelivered) {
		t.Errorf("a ref not delivered: %v", err)
	}
}

func TestRevokeAttempt_ALateRevokeOfAnOlderAttemptLeavesTheNewerDelivery(t *testing.T) {
	dir := runDir(t)
	deliverFor(t, dir, 1, askpassOnly, origin)
	deliverFor(t, dir, 2, askpassOnly, origin)

	if err := New().RevokeAttempt(dir, 1, Askpass, nil); !errors.Is(err, ErrStale) {
		t.Fatalf("revoke of attempt 1 with 2 in place: %v", err)
	}
	if _, err := AskpassReader(dir)(origin.Ref); err != nil {
		t.Fatalf("the newer delivery lost its access: %v", err)
	}
	if err := New().RevokeAttempt(dir, 2, Askpass, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := AskpassReader(dir)(origin.Ref); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("askpass after the revoke of its own attempt: %v", err)
	}
}

// `secrets set --user` and the binding of a prepare apply one rule: a user the
// first stores is a user the second binds.
func TestBind_RefusesExactlyTheUsersSecretsSetRefuses(t *testing.T) {
	for _, user := range []string{"x-access-token", "svc-bot", "a.b_c", "a@b", "a b", "a/b", "a%40b", "a\tb", "a\u00a0b", "a:b", "a\x00b", "a\nb", strings.Repeat("u", secrets.MaxUserLen+1)} {
		dir := runDir(t)
		m := New()
		if err := m.DeliverFor(dir, 1, []Consumer{Askpass}, v1[:1], allow); err != nil {
			t.Fatal(err)
		}
		stored := secrets.CheckUser(user) == nil
		bound := m.Bind(dir, "api/token", Binding{Origin: "https://h/r", User: user}) == nil
		if stored != bound {
			t.Errorf("user %q: secrets set accepts it = %v, Bind accepts it = %v", user, stored, bound)
		}
	}
}

func TestDeliver_ADeliveryThatCannotBeReadInPlaceFailsAndIsNotReplaced(t *testing.T) {
	dir := runDir(t)
	deliver(t, dir, 1, v1)
	if err := os.Remove(filepath.Join(dir, liveName, fileName(v1[0].Ref))); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, liveName, manifestName)) //nolint:gosec // a test
	if err != nil {
		t.Fatal(err)
	}
	err = New().DeliverFor(dir, 1, askpassOnly, []redact.Needle{origin}, allow)
	if err == nil {
		t.Fatal("a delivery that cannot be read was merged into")
	}
	noValue(t, err)
	after, rerr := os.ReadFile(filepath.Join(dir, liveName, manifestName)) //nolint:gosec // a test
	if rerr != nil || string(after) != string(before) {
		t.Fatalf("the manifest changed: %v\n%s\n%s", rerr, before, after)
	}
}

// A later attempt replaces the access, never the retention: the token and the
// user of the first one stay in Load, so the redaction and the guard of
// publication cover both pairings until Release.
func TestDeliver_ANewerAttemptRetainsTheRotatedTokenAndUser(t *testing.T) {
	dir := runDir(t)
	m := New()
	first := redact.Needle{Ref: "origin/token", Value: []byte("first-token-value-aaaa")}
	second := redact.Needle{Ref: "origin/token", Value: []byte("second-token-value-bbb")}
	bind := func(attempt uint64, n redact.Needle, user string) {
		t.Helper()
		if err := m.DeliverFor(dir, attempt, askpassOnly, []redact.Needle{n}, allow); err != nil {
			t.Fatal(err)
		}
		if err := m.Bind(dir, n.Ref, Binding{Origin: "https://git.example/org/repo.git", User: user}); err != nil {
			t.Fatal(err)
		}
	}
	bind(1, first, "x-access-token")
	bind(2, second, "deploy-bot")

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	set, err := redact.NewEscapedSet(got)
	if err != nil {
		t.Fatal(err)
	}
	basic := func(user string, n redact.Needle) string {
		return base64.StdEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(user + ":" + string(n.Value)))
	}
	for _, leak := range []string{
		string(first.Value), string(second.Value),
		basic("x-access-token", first), basic("deploy-bot", second),
	} {
		var out bytes.Buffer
		w := redact.NewWriter(&out, set)
		_, _ = w.Write([]byte("log " + leak + " end"))
		_ = w.Close()
		if !w.Matched() || bytes.Contains(out.Bytes(), []byte(leak)) {
			t.Errorf("%q survives the redaction", leak)
		}
	}
	if v, err := AskpassReader(dir)(first.Ref); err != nil || v != string(second.Value) {
		t.Fatalf("askpass reads %q, %v: want the second attempt's value", v, err)
	}

	if err := m.Release(dir, allow); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); !errors.Is(err, ErrNotDelivered) {
		t.Fatalf("Load after Release = %v", err)
	}
}

// A delivery in place that cannot be read whole is never replaced by a newer
// attempt: that would drop the values it holds and their redaction. Only a
// confirmed absence starts from nothing; Release is the way out.
func TestDeliver_ANewerAttemptDoesNotReplaceADeliveryWithAMissingFile(t *testing.T) {
	second := redact.Needle{Ref: "other/token", Value: []byte("second-token-value-bbb")}
	for name, c := range map[string]struct {
		prep func(t *testing.T, dir string) // leaves attempt 1 in place
		file func(dir string) string        // the file that goes missing
	}{
		"a current value": {
			prep: func(t *testing.T, dir string) { deliverFor(t, dir, 1, askpassOnly, origin) },
			file: func(string) string { return fileName(origin.Ref) },
		},
		"a retained value": {
			prep: func(t *testing.T, dir string) {
				deliverFor(t, dir, 1, askpassOnly, origin)
				deliverFor(t, dir, 2, askpassOnly, second)
			},
			file: func(string) string { return retainedFile(0) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := runDir(t)
			c.prep(t, dir)
			live := filepath.Join(dir, liveName)
			if err := os.Remove(filepath.Join(live, c.file(dir))); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(live, manifestName)) //nolint:gosec // a test
			if err != nil {
				t.Fatal(err)
			}
			attempt := uint64(3)
			if err := New().DeliverFor(dir, attempt, askpassOnly, []redact.Needle{{Ref: "third/token", Value: []byte("third-token-value-cccc")}}, allow); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("DeliverFor = %v, want ErrCorrupt", err)
			}
			after, err := os.ReadFile(filepath.Join(live, manifestName)) //nolint:gosec // a test
			if err != nil || string(after) != string(before) {
				t.Fatalf("the delivery was replaced: %v\n%s\n%s", err, before, after)
			}
			if _, err := Load(dir); err == nil {
				t.Fatal("Load returned a partial set")
			}
			wantOnly(t, dir, liveName)

			// Release is the recovery: after it a delivery starts from nothing.
			if err := New().Release(dir, allow); err != nil {
				t.Fatal(err)
			}
			deliverFor(t, dir, attempt, askpassOnly, second)
		})
	}
}
