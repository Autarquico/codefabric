package domain

import (
	"errors"
	"testing"
)

const (
	taskA ID = "task_01J8ZQ4M5N6P7R8S9T0V1W2X3Y"
	taskB ID = "task_01J8ZQ7A5N6P7R8S9T0V1W2X3Y"
	taskC ID = "task_01J9AA4M5N6P7R8S9T0V1W2X3Y"
)

func requireResolves(t *testing.T, ref string, scope ID, cands []Candidate, want ID) {
	t.Helper()
	got, err := Resolve(ref, KindTask, scope, cands)
	if err != nil || got != want {
		t.Fatalf("Resolve(%q) = %q, %v; want %q", ref, got, err, want)
	}
}

func candidateIDs(t *testing.T, err error) []ID {
	t.Helper()
	var domainErr *Error
	if !errors.As(err, &domainErr) {
		t.Fatalf("error %v is not *Error", err)
	}
	ids, ok := domainErr.Details["candidates"].([]ID)
	if !ok {
		t.Fatalf("Details[candidates] = %#v, want []ID", domainErr.Details["candidates"])
	}
	return ids
}

func TestScenario_DomainCore_AmbiguousRef(t *testing.T) {
	twoAuthRace := []Candidate{
		{ID: taskA, Short: ShortID{KindTask, 1}, Name: "auth-race", ProjectID: "project_1"},
		{ID: taskB, Short: ShortID{KindTask, 2}, Name: "auth-race", ProjectID: "project_2"},
	}

	t.Run("two candidates with the same name give CF_AMBIGUOUS_REF listing both", func(t *testing.T) {
		_, err := Resolve("auth-race", KindTask, "", twoAuthRace)
		requireCode(t, err, CodeAmbiguousRef)
		if ids := candidateIDs(t, err); len(ids) != 2 || ids[0] != taskA || ids[1] != taskB {
			t.Fatalf("listed candidates = %v, want [%s %s]", ids, taskA, taskB)
		}
	})

	t.Run("the project filter disambiguates", func(t *testing.T) {
		requireResolves(t, "auth-race", "project_2", twoAuthRace, taskB)
	})

	t.Run("a project filter that matches nothing leaves it ambiguous", func(t *testing.T) {
		_, err := Resolve("auth-race", KindTask, "project_9", twoAuthRace)
		requireCode(t, err, CodeAmbiguousRef)
	})

	t.Run("a project filter that still matches two stays ambiguous", func(t *testing.T) {
		cands := []Candidate{
			{ID: taskA, Name: "auth-race", ProjectID: "project_1"},
			{ID: taskB, Name: "auth-race", ProjectID: "project_1"},
			{ID: taskC, Name: "auth-race", ProjectID: "project_2"},
		}
		_, err := Resolve("auth-race", KindTask, "project_1", cands)
		requireCode(t, err, CodeAmbiguousRef)
		if ids := candidateIDs(t, err); len(ids) != 2 {
			t.Fatalf("listed %v, want only the two in project_1", ids)
		}
	})

	t.Run("an unknown reference gives CF_NOT_FOUND", func(t *testing.T) {
		_, err := Resolve("nothing", KindTask, "", twoAuthRace)
		requireCode(t, err, CodeNotFound)
	})
}

func TestScenario_DomainCore_AmbiguousRef_ArchivedCandidates(t *testing.T) {
	cands := []Candidate{{ID: taskA, Short: ShortID{KindTask, 7}, Name: "old", Archived: true}}

	t.Run("an archived name does not resolve", func(t *testing.T) {
		_, err := Resolve("old", KindTask, "", cands)
		requireCode(t, err, CodeNotFound)
	})
	t.Run("an archived ID resolves", func(t *testing.T) {
		requireResolves(t, string(taskA), "", cands, taskA)
	})
	t.Run("an archived short ID resolves", func(t *testing.T) {
		requireResolves(t, "task-7", "", cands, taskA)
	})
	t.Run("an archived namesake does not make a live name ambiguous", func(t *testing.T) {
		both := []Candidate{
			{ID: taskA, Name: "auth-race", Archived: true},
			{ID: taskB, Name: "auth-race"},
		}
		requireResolves(t, "auth-race", "", both, taskB)
	})
}

func TestScenario_DomainCore_AmbiguousRef_ULIDPrefix(t *testing.T) {
	cands := []Candidate{{ID: taskA, Name: "a"}, {ID: taskC, Name: "c"}}

	t.Run("a 5-character prefix does not resolve even if unique", func(t *testing.T) {
		_, err := Resolve("01J8Z", KindTask, "", cands[:1])
		requireCode(t, err, CodeNotFound)
		_, err = Resolve("01J9A", KindTask, "", cands)
		requireCode(t, err, CodeNotFound)
	})
	t.Run("a unique 6-character prefix resolves", func(t *testing.T) {
		requireResolves(t, "01J8ZQ", "", cands, taskA)
		requireResolves(t, "01J9AA", "", cands, taskC)
	})
	t.Run("a 6-character prefix shared by two is ambiguous", func(t *testing.T) {
		shared := []Candidate{{ID: taskA}, {ID: taskB}}
		_, err := Resolve("01J8ZQ", KindTask, "", shared)
		requireCode(t, err, CodeAmbiguousRef)
	})
	t.Run("a longer unique prefix disambiguates what 6 characters cannot", func(t *testing.T) {
		shared := []Candidate{{ID: taskA}, {ID: taskB}}
		requireResolves(t, "01J8ZQ4", "", shared, taskA)
	})
}

func TestScenario_DomainCore_AmbiguousRef_ResolutionOrder(t *testing.T) {
	t.Run("canonical ID beats a name equal to it", func(t *testing.T) {
		cands := []Candidate{{ID: taskA, Name: "a"}, {ID: taskB, Name: string(taskA)}}
		requireResolves(t, string(taskA), "", cands, taskA)
	})
	t.Run("short ID beats a name equal to it", func(t *testing.T) {
		cands := []Candidate{{ID: taskA, Short: ShortID{KindTask, 1}, Name: "a"}, {ID: taskB, Name: "task-1"}}
		requireResolves(t, "task-1", "", cands, taskA)
	})
	t.Run("name beats a ULID prefix equal to it", func(t *testing.T) {
		cands := []Candidate{{ID: taskA, Name: "a"}, {ID: taskC, Name: "01J8ZQ"}}
		requireResolves(t, "01J8ZQ", "", cands, taskC)
	})
	t.Run("the ULID prefix is the last resort", func(t *testing.T) {
		requireResolves(t, "01J8ZQ", "", []Candidate{{ID: taskA, Name: "a"}}, taskA)
	})
	t.Run("empty ref does not resolve", func(t *testing.T) {
		cands := []Candidate{{ID: taskA}, {ID: taskB, Name: ""}}
		_, err := Resolve("", KindTask, "", cands)
		requireCode(t, err, CodeNotFound)
	})
	t.Run("an ID of another kind does not resolve", func(t *testing.T) {
		_, err := Resolve("run_01J8ZQ4M5N6P7R8S9T0V1W2X3Y", KindTask, "", []Candidate{{ID: taskA}})
		requireCode(t, err, CodeNotFound)
	})
	t.Run("a short ID of another kind does not resolve", func(t *testing.T) {
		_, err := Resolve("run-1", KindTask, "", []Candidate{{ID: taskA, Short: ShortID{KindTask, 1}}})
		requireCode(t, err, CodeNotFound)
	})
}

func TestParseShortID(t *testing.T) {
	valid := map[string]ShortID{
		"run-42":  {KindRun, 42},
		"task-1":  {KindTask, 1},
		"cp-3":    {KindCheckpoint, 3},
		"plan-10": {KindPlan, 10},
	}
	for text, want := range valid {
		got, err := ParseShortID(text)
		if err != nil || got != want || got.String() != text {
			t.Errorf("ParseShortID(%q) = %v, %v; want %v", text, got, err, want)
		}
	}
	for _, text := range []string{"", "run", "run-", "run-0", "run-042", "run--1", "run-+1", "run-1x", "nope-1", "Run-1", "run_1"} {
		if _, err := ParseShortID(text); !errors.Is(err, ErrMalformedID) {
			t.Errorf("ParseShortID(%q) error = %v, want ErrMalformedID", text, err)
		}
	}
}
