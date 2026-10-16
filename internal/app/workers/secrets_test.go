package workers_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/workers"
	"github.com/Autarquico/codefabric/internal/domain"
)

var secretsAt = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// daemonOf makes a heartbeat come from the run of a daemon with generation gen,
// as its seq-th report, holding refs.
func daemonOf(gen, seq uint64, refs ...string) func(*v1.HeartbeatRequest) {
	return func(r *v1.HeartbeatRequest) {
		r.Generation = gen
		if gen == 0 {
			return
		}
		r.Secrets = &v1.SecretsReport{Seq: seq, Refs: []v1.SecretMeta{}}
		for _, ref := range refs {
			r.Secrets.Refs = append(r.Secrets.Refs, v1.SecretMeta{Ref: ref, Backend: v1.SecretBackendFile, UpdatedAt: secretsAt})
		}
	}
}

func refsOf(metas []v1.SecretMeta) []string {
	out := []string{}
	for _, m := range metas {
		out = append(out, m.Ref)
	}
	return out
}

// fenceReason is the reason of a heartbeat the fence refused, or "" if it was taken.
func fenceReason(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeStaleLease {
		t.Fatalf("heartbeat refused with %v, want a fence refusal", err)
	}
	reason, _ := de.Details["reason"].(string)
	return reason
}

func TestFence(t *testing.T) {
	const (
		ok      = ""
		retired = workers.FenceGenerationRetired
		late    = workers.FenceOutOfOrder
	)
	type beat struct {
		epoch     int // index into epochs, made in order: a higher one is a later run
		gen, seq  uint64
		refs      []string
		want      string
		hostname  string
		wantState domain.WorkerState
	}
	for _, tc := range []struct {
		name        string
		beats       []beat
		wantEpoch   int
		wantSecrets []string
		wantSyncs   int // worker.secrets.sync audit entries
	}{
		{
			name: "a late heartbeat of E1 after E2 is refused and restores nothing",
			beats: []beat{
				{epoch: 1, gen: 1, seq: 1, refs: []string{"a"}},
				{epoch: 2, gen: 2, seq: 1, refs: []string{"b"}},
				{epoch: 1, gen: 1, seq: 2, refs: []string{"a"}, want: retired, hostname: "ghost"},
			},
			wantEpoch: 2, wantSecrets: []string{"b"}, wantSyncs: 2,
		},
		{
			name: "a new generation applies whatever its epoch",
			beats: []beat{
				{epoch: 2, gen: 1, seq: 9, refs: []string{"a"}},
				{epoch: 1, gen: 2, seq: 1, refs: []string{"b"}},
			},
			wantEpoch: 1, wantSecrets: []string{"b"}, wantSyncs: 2,
		},
		{
			name: "heartbeats of one daemon are ordered by seq",
			beats: []beat{
				{epoch: 1, gen: 1, seq: 2, refs: []string{"b"}},
				{epoch: 1, gen: 1, seq: 1, refs: []string{"a"}, want: late, hostname: "ghost"},
				{epoch: 1, gen: 1, seq: 3, refs: []string{"b", "c"}},
			},
			wantEpoch: 1, wantSecrets: []string{"b", "c"}, wantSyncs: 2,
		},
		{
			name: "the same heartbeat again is taken and changes nothing",
			beats: []beat{
				{epoch: 1, gen: 1, seq: 1, refs: []string{"a"}},
				{epoch: 1, gen: 1, seq: 1, refs: []string{"a"}},
				{epoch: 1, gen: 1, seq: 2, refs: []string{"a"}},
			},
			wantEpoch: 1, wantSecrets: []string{"a"}, wantSyncs: 1,
		},
		{
			name: "a lost state.db reuses a generation: refused, then a higher one is taken",
			beats: []beat{
				{epoch: 1, gen: 3, seq: 1, refs: []string{"a"}},
				{epoch: 2, gen: 1, seq: 1, refs: []string{"b"}, want: retired},
				{epoch: 2, gen: 3, seq: 2, refs: []string{"b"}, want: retired},
				{epoch: 2, gen: 4, seq: 3, refs: []string{"b"}},
			},
			wantEpoch: 2, wantSecrets: []string{"b"}, wantSyncs: 2,
		},
		{
			name: "an N−1 daemon with no generation keeps working as in Phase 1",
			beats: []beat{
				{epoch: 1}, {epoch: 1}, {epoch: 2}, {epoch: 1},
			},
			wantEpoch: 1, wantSecrets: []string{}, wantSyncs: 0,
		},
		{
			name: "an upgrade from N−1 takes the first generation",
			beats: []beat{
				{epoch: 1},
				{epoch: 2, gen: 1, seq: 1, refs: []string{"a"}},
			},
			wantEpoch: 2, wantSecrets: []string{"a"}, wantSyncs: 1,
		},
		{
			name: "a late N−1 heartbeat from before the upgrade is refused",
			beats: []beat{
				{epoch: 1},
				{epoch: 2, gen: 1, seq: 1, refs: []string{"a"}},
				{epoch: 1, want: retired, hostname: "ghost"},
			},
			wantEpoch: 2, wantSecrets: []string{"a"}, wantSyncs: 1,
		},
		{
			name: "a downgrade is taken, degraded, and forgets the secrets until the next upgrade",
			beats: []beat{
				{epoch: 1, gen: 1, seq: 1, refs: []string{"a"}},
				{epoch: 2, wantState: domain.WorkerDegraded},
				{epoch: 2, wantState: domain.WorkerDegraded},
				{epoch: 3, gen: 2, seq: 1, refs: []string{"a"}, wantState: domain.WorkerOnline},
			},
			wantEpoch: 3, wantSecrets: []string{"a"}, wantSyncs: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, controllerVersion)
			w := e.join("box")
			epochs := make([]string, 4)
			for i := range epochs {
				epochs[i] = newEpoch()
			}
			for i, b := range tc.beats {
				mutate := []func(*v1.HeartbeatRequest){daemonOf(b.gen, b.seq, b.refs...)}
				if b.hostname != "" {
					mutate = append(mutate, func(r *v1.HeartbeatRequest) { r.Hostname = b.hostname })
				}
				out, err := e.svc.Heartbeat(t.Context(), w, request(epochs[b.epoch], mutate...))
				if got := fenceReason(t, err); got != b.want {
					t.Fatalf("heartbeat %d: refused for %q, want %q", i, got, b.want)
				}
				if b.wantState != "" && out.State != b.wantState {
					t.Fatalf("heartbeat %d: %s, want %s", i, out.State, b.wantState)
				}
			}
			got := e.worker("box")
			if got.Epoch != epochs[tc.wantEpoch] {
				t.Errorf("epoch is that of run %d, want %d", slices.Index(epochs, got.Epoch), tc.wantEpoch)
			}
			if !slices.Equal(refsOf(got.Secrets), tc.wantSecrets) {
				t.Errorf("secrets = %v, want %v", refsOf(got.Secrets), tc.wantSecrets)
			}
			if got.Hostname == "ghost" {
				t.Error("a refused heartbeat changed the hostname")
			}
			if n := e.count(`SELECT COUNT(*) FROM audit_entries WHERE action = 'worker.secrets.sync'`); n != tc.wantSyncs {
				t.Errorf("%d worker.secrets.sync audit entries, want %d", n, tc.wantSyncs)
			}
		})
	}
}

func TestFence_ARetiredGenerationIsToldTheControllersOne(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	e.beat(w, request(newEpoch(), daemonOf(5, 1)))

	_, err := e.svc.Heartbeat(t.Context(), w, request(newEpoch(), daemonOf(2, 1)))
	var de *domain.Error
	if !errors.As(err, &de) || de.Details["generation"] != uint64(5) {
		t.Fatalf("got %v (%+v), want the Controller's generation, 5", err, de)
	}
	if n := len(e.events(domain.EventWorkerRestarted)); n != 0 {
		t.Errorf("a retired daemon wrote %d WorkerRestarted", n)
	}
}

func TestFence_ADowngradeSaysWhyTheWorkerIsDegraded(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	e.beat(w, request(newEpoch(), daemonOf(3, 1, "a")))

	if got := e.beat(w, request(newEpoch())); got != domain.WorkerDegraded {
		t.Fatalf("downgraded: %s", got)
	}
	h := e.worker("box").Health
	if h.Status != v1.HealthDegraded || !strings.Contains(h.Message, "downgraded") || !strings.Contains(h.Message, "3") {
		t.Errorf("health = %+v, want one that says it was downgraded after generation 3", h)
	}
}

func TestSecrets_TheMetadataIsKeptAsReported(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	project := string(e.ids.New(domain.KindProject))
	meta := v1.SecretMeta{Ref: "gh/token", Backend: v1.SecretBackendFile, Projects: []string{project}, UpdatedAt: secretsAt}
	e.beat(w, request(newEpoch(), daemonOf(1, 1), func(r *v1.HeartbeatRequest) { r.Secrets.Refs = []v1.SecretMeta{meta} }))

	got := e.worker("box").Secrets
	if len(got) != 1 || got[0].Ref != meta.Ref || got[0].Backend != meta.Backend ||
		!slices.Equal(got[0].Projects, meta.Projects) || !got[0].UpdatedAt.Equal(meta.UpdatedAt) {
		t.Errorf("secrets = %+v, want %+v", got, meta)
	}
}

func TestSecrets_ARemovedWorkerHoldsNone(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	e.beat(w, request(newEpoch(), daemonOf(1, 1, "a")))

	removed, err := e.svc.Remove(t.Context(), e.owner, "box", v1.ActionRequest{})
	must(t, err)
	if len(removed.Secrets) != 0 {
		t.Errorf("a removed Worker still holds %v", refsOf(removed.Secrets))
	}
	if n := e.count(`SELECT COUNT(*) FROM worker_secrets WHERE worker_id = ?`, string(w.Principal.ID)); n != 0 {
		t.Errorf("%d rows of metadata left behind by the removal", n)
	}
}

func TestHeartbeat_SecretsValidation(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	project := string(e.ids.New(domain.KindProject))
	refs := func(metas ...v1.SecretMeta) func(*v1.HeartbeatRequest) {
		return func(r *v1.HeartbeatRequest) { r.Secrets.Refs = metas }
	}
	meta := func(ref string, mutate ...func(*v1.SecretMeta)) v1.SecretMeta {
		m := v1.SecretMeta{Ref: ref, Backend: v1.SecretBackendFile, UpdatedAt: secretsAt}
		for _, f := range mutate {
			f(&m)
		}
		return m
	}
	scoped := func(projects ...string) func(*v1.SecretMeta) {
		return func(m *v1.SecretMeta) { m.Projects = projects }
	}
	many := make([]v1.SecretMeta, domain.MaxWorkerSecrets+1)
	for i := range many {
		many[i] = meta(fmt.Sprintf("s%d", i))
	}
	nine := make([]string, domain.MaxSecretProjects+1)
	for i := range nine {
		nine[i] = string(e.ids.New(domain.KindProject))
	}
	// Few refs, each long and scoped to as many Projects as it may be: past the budget long before the count.
	heavy := make([]v1.SecretMeta, 200)
	for i := range heavy {
		heavy[i] = meta(fmt.Sprintf("%s%03d", strings.Repeat("r", domain.MaxSecretRefLen-3), i), scoped(nine[:domain.MaxSecretProjects]...))
	}

	for name, mutate := range map[string][]func(*v1.HeartbeatRequest){
		"secrets without a generation": {func(r *v1.HeartbeatRequest) { r.Secrets = &v1.SecretsReport{Seq: 1} }},
		"a generation without secrets": {func(r *v1.HeartbeatRequest) { r.Generation = 1 }},
		"an invalid ref":               {daemonOf(1, 1), refs(meta("-x"))},
		"a ref twice":                  {daemonOf(1, 1), refs(meta("a"), meta("a"))},
		"an unknown backend":           {daemonOf(1, 1), refs(meta("a", func(m *v1.SecretMeta) { m.Backend = "keyring" }))},
		"no updated_at":                {daemonOf(1, 1), refs(meta("a", func(m *v1.SecretMeta) { m.UpdatedAt = time.Time{} }))},
		"too many Projects":            {daemonOf(1, 1), refs(meta("a", scoped(nine...)))},
		"a scope that is no Project":   {daemonOf(1, 1), refs(meta("a", scoped("task_01J00000000000000000000000")))},
		"a Project twice":              {daemonOf(1, 1), refs(meta("a", scoped(project, project)))},
		"too many secrets":             {daemonOf(1, 1), refs(many...)},
		"over the budget":              {daemonOf(1, 1), refs(heavy...)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.svc.Heartbeat(t.Context(), w, request(newEpoch(), mutate...))
			wantCode(t, err, domain.CodeInvalidRequest)
		})
	}
	if got := e.state(w); got != domain.WorkerJoining {
		t.Errorf("an invalid heartbeat registered the Worker: %s", got)
	}
}

// TestHeartbeat_TheLargestValidHeartbeatFitsInTheBody fixes the budget of
// §11.10: every field at its limit, the secrets at theirs, still inside the
// 64 KiB the API reads of a body.
func TestHeartbeat_TheLargestValidHeartbeatFitsInTheBody(t *testing.T) {
	const bodyLimit = 64 << 10
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	long := func(n int) string { return strings.Repeat("x", n) }

	req := request(newEpoch(), daemonOf(1, 1), func(r *v1.HeartbeatRequest) {
		r.DaemonVersion, r.Hostname, r.OS, r.Arch = controllerVersion, long(256), long(256), long(256)
		r.Health = v1.Health{Status: v1.HealthDegraded, Message: long(256)}
		for i := range 64 {
			r.Capabilities = append(r.Capabilities, fmt.Sprintf("%03d%s", i, long(252)))
		}
		r.SSHTarget = long(64) + "@" + long(180) + ":65535"
		r.AttachBinaries = map[string]string{}
		for i := range 4 {
			r.AttachBinaries[fmt.Sprintf("%d%s", i, long(31))] = "/" + long(254)
		}
	})
	// Fill the secrets up to their budget with refs scoped to one Project.
	project := string(e.ids.New(domain.KindProject))
	for i := 0; ; i++ {
		m := v1.SecretMeta{
			Ref: fmt.Sprintf("%s%03d", long(domain.MaxSecretRefLen-3), i), Backend: v1.SecretBackendFile,
			Projects: []string{project}, UpdatedAt: secretsAt,
		}
		next := append(slices.Clone(req.Secrets.Refs), m)
		encoded, err := json.Marshal(v1.SecretsReport{Seq: req.Secrets.Seq, Refs: next})
		must(t, err)
		if len(encoded) > v1.MaxSecretsReportBytes || len(next) > domain.MaxWorkerSecrets {
			break
		}
		req.Secrets.Refs = next
	}

	if _, err := e.svc.Heartbeat(t.Context(), w, req); err != nil {
		t.Fatalf("the largest valid heartbeat was refused: %v", err)
	}
	body, err := json.Marshal(req)
	must(t, err)
	if len(body) > bodyLimit {
		t.Errorf("the largest valid heartbeat takes %d bytes, over the %d of a body", len(body), bodyLimit)
	}
	t.Logf("largest heartbeat: %d bytes, %d secrets", len(body), len(req.Secrets.Refs))
}

func TestHeartbeat_NormalCatalogsFit(t *testing.T) {
	// D-S2: the whole catalog, scoped to one Project each, fits.
	metas := make([]v1.SecretMeta, domain.MaxWorkerSecrets)
	for i := range metas {
		metas[i] = v1.SecretMeta{
			Ref: fmt.Sprintf("team/service-%03d/token", i), Backend: v1.SecretBackendFile,
			Projects: []string{"project_01J00000000000000000000000"}, UpdatedAt: secretsAt,
		}
	}
	encoded, err := json.Marshal(v1.SecretsReport{Seq: 1 << 40, Refs: metas})
	must(t, err)
	if len(encoded) > v1.MaxSecretsReportBytes {
		t.Errorf("%d secrets of an ordinary size take %d bytes, over %d", len(metas), len(encoded), v1.MaxSecretsReportBytes)
	}
}
