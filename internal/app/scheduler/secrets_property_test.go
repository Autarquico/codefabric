package scheduler_test

import (
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/Autarquico/codefabric/internal/app/scheduler"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The property of the secrets filter of #23 (P0 of §7): with Workers, an
// allowlist and a Run drawn from a seed, Decide only ever chooses a Worker
// that holds every secret of the Run for its Project, all of them allowed; the
// secrets filter fails exactly the Workers the model says, with the code and
// the refs it says; and every verdict lists its failures in the fixed order.
//
// CF_SECRETS_SEED replays one seed; CF_SECRETS_SEEDS runs that many from
// CF_SECRETS_FROM (1).

var (
	propertyRefs     = []string{"gh/token", "npm/token", "pg/url", "aws/key"}
	propertyProjects = []domain.ID{"project_a", "project_b"}
	filterOrder      = []string{
		scheduler.FilterState, scheduler.FilterAvailability, scheduler.FilterRequire, scheduler.FilterAvoid,
		scheduler.FilterCapabilities, scheduler.FilterHarness, scheduler.FilterSecrets, scheduler.FilterResources,
		scheduler.FilterConcurrency, scheduler.FilterCapacity, scheduler.FilterRecentMiss, scheduler.FilterReconcile,
	}
)

func propertySeeds(t *testing.T, def uint64) []uint64 {
	t.Helper()
	if text := os.Getenv("CF_SECRETS_SEED"); text != "" {
		seed, err := strconv.ParseUint(text, 0, 64)
		if err != nil {
			t.Fatalf("CF_SECRETS_SEED=%q is not a seed", text)
		}
		return []uint64{seed}
	}
	n, from := def, uint64(1)
	for name, into := range map[string]*uint64{"CF_SECRETS_SEEDS": &n, "CF_SECRETS_FROM": &from} {
		if text := os.Getenv(name); text != "" {
			v, err := strconv.ParseUint(text, 0, 64)
			if err != nil || v == 0 {
				t.Fatalf("%s=%q is not a positive number", name, text)
			}
			*into = v
		}
	}
	seeds := make([]uint64, n)
	for i := range seeds {
		seeds[i] = from + uint64(i)
	}
	return seeds
}

func subset(rng *rand.Rand, of []string, p int) []string {
	var out []string
	for _, s := range of {
		if rng.IntN(p) == 0 {
			out = append(out, s)
		}
	}
	return out
}

// randomWorker is a Worker that holds some refs, each for any Project or for
// some, and at times fails another filter too.
func randomWorker(rng *rand.Rand, i int) scheduler.Worker {
	return worker(fmt.Sprintf("w%d", i), func(w *scheduler.Worker) {
		w.Secrets = map[string][]string{}
		for _, ref := range subset(rng, propertyRefs, 2) {
			var scope []string
			for _, p := range propertyProjects {
				if rng.IntN(3) == 0 {
					scope = append(scope, string(p))
				}
			}
			w.Secrets[ref] = scope
		}
		switch rng.IntN(6) {
		case 0:
			w.State = domain.WorkerOffline
		case 1:
			w.Allocatable = domain.Capacity{CPU: 1, MemoryMB: 1024}
		case 2:
			w.Reservations = w.MaxConcurrentRuns
		}
	})
}

// secretsVerdictOf is the model of the secrets filter: "" if the Worker
// passes it, else the code, and the refs not allowed, missing and out of scope.
func secretsVerdictOf(refs, allowlist []string, project domain.ID, w scheduler.Worker) (code domain.Code, notAllowed, missing, outOfScope []string) {
	if len(refs) == 0 {
		return "", nil, nil, nil
	}
	for _, ref := range refs {
		if !slices.Contains(allowlist, ref) {
			notAllowed = append(notAllowed, ref)
		}
	}
	if len(notAllowed) > 0 {
		return domain.CodePolicyDenied, notAllowed, nil, nil
	}
	for _, ref := range refs {
		scope, held := w.Secrets[ref]
		switch {
		case !held:
			missing = append(missing, ref)
		case len(scope) > 0 && !slices.Contains(scope, string(project)):
			outOfScope = append(outOfScope, ref)
		}
	}
	if len(missing)+len(outOfScope) > 0 {
		return domain.CodeSecretUnavailable, nil, missing, outOfScope
	}
	return "", nil, nil, nil
}

func detail(f scheduler.Failure, key string) []string {
	v, _ := f.Details[key].([]string)
	return v
}

func TestProperty_SecretsDecide(t *testing.T) {
	seeds := propertySeeds(t, 200)
	t.Logf("seeds %d to %d (replay one: CF_SECRETS_SEED=<seed> go test -run '^TestProperty_SecretsDecide$' ./internal/app/scheduler/)", seeds[0], seeds[len(seeds)-1])
	for _, seed := range seeds {
		rng := rand.New(rand.NewPCG(seed, seed^0x5eed)) //nolint:gosec // a seed to replay, not a secret
		for round := range 25 {
			project := propertyProjects[rng.IntN(len(propertyProjects))]
			refs := subset(rng, propertyRefs, 3)
			allowlist := subset(rng, propertyRefs, 2)
			if rng.IntN(2) == 0 {
				allowlist = append(allowlist, refs...)
			}
			workers := make([]scheduler.Worker, 1+rng.IntN(5))
			for i := range workers {
				workers[i] = randomWorker(rng, i)
			}
			req := scheduler.NewRequest(request(func(p *domain.Placement) { p.Secrets = refs }).Placement, project, allowlist)
			d := scheduler.Decide(req, workers, now)

			fail := func(format string, args ...any) {
				t.Helper()
				t.Fatalf("seed %d round %d (replay: CF_SECRETS_SEED=%d go test -run '^TestProperty_SecretsDecide$' ./internal/app/scheduler/): %s; refs %v allowlist %v project %s",
					seed, round, seed, fmt.Sprintf(format, args...), refs, allowlist, project)
			}
			byName := map[string]scheduler.Worker{}
			for _, w := range workers {
				byName[w.Name] = w
			}
			if d.Chosen != nil {
				if code, _, _, _ := secretsVerdictOf(refs, allowlist, project, byName[d.Chosen.Name]); code != "" {
					fail("chose %s, which the model fails with %s", d.Chosen.Name, code)
				}
			}
			for _, v := range d.Verdicts {
				var got *scheduler.Failure
				order := -1
				for i := range v.Failed {
					f := v.Failed[i]
					at := slices.Index(filterOrder, f.Filter)
					if at <= order {
						fail("%s lists %s out of the fixed order: %+v", v.Name, f.Filter, v.Failed)
					}
					order = at
					if f.Filter == scheduler.FilterSecrets {
						got = &f
					}
				}
				code, notAllowed, missing, outOfScope := secretsVerdictOf(refs, allowlist, project, byName[v.Name])
				switch {
				case code == "" && got != nil:
					fail("%s fails the secrets filter (%s); the model passes it", v.Name, got.Code)
				case code != "" && got == nil:
					fail("%s passes the secrets filter; the model fails it with %s", v.Name, code)
				case got != nil && got.Code != code:
					fail("%s fails with %s; the model says %s", v.Name, got.Code, code)
				case got != nil && (!slices.Equal(detail(*got, "not_allowed"), notAllowed) ||
					!slices.Equal(detail(*got, "missing"), missing) || !slices.Equal(detail(*got, "out_of_scope"), outOfScope)):
					fail("%s fails with %v; the model says not allowed %v, missing %v, out of scope %v", v.Name, got.Details, notAllowed, missing, outOfScope)
				}
			}
		}
	}
}
