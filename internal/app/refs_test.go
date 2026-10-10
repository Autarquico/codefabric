package app_test

import (
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestResolve_AppliesTheReferenceOrderWithProjectScope(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	e.project("frontend")
	named := func(r *v1.CreateTaskRequest) { r.Name = "shared"; r.NoStart = true; r.SourceCommit = "" }
	a := e.mustTask("backend", "a", named).Task
	b := e.mustTask("frontend", "b", named).Task

	resolve := func(kind domain.Kind, ref, project string) (v1.Resolved, error) {
		return e.app.Resolve(t.Context(), kind, ref, project)
	}
	for _, c := range []struct {
		name, ref, project string
		want               v1.Task
	}{
		{"canonical ID", a.ID, "", a},
		{"short ID", b.Short, "", b},
		{"name narrowed by a Project name", "shared", "frontend", b},
		{"name narrowed by a Project short ID", "shared", "project-1", a},
		{"bare ULID", a.ID[len("task_"):], "", a},
	} {
		got, err := resolve(domain.KindTask, c.ref, c.project)
		if err != nil || got.ID != c.want.ID || got.Short != c.want.Short || got.Kind != domain.KindTask {
			t.Errorf("%s: %+v, %v", c.name, got, err)
		}
	}

	_, err := resolve(domain.KindTask, "shared", "")
	wantCode(t, err, domain.CodeAmbiguousRef) // never picked implicitly
	_, err = resolve(domain.KindTask, "missing", "")
	wantCode(t, err, domain.CodeNotFound)
	_, err = resolve(domain.KindTask, "shared", "nope")
	wantCode(t, err, domain.CodeNotFound)
	_, err = resolve(domain.KindTask, "run-1", "")
	wantCode(t, err, domain.CodeNotFound)
	_, err = resolve("worker", "x", "")
	wantCode(t, err, domain.CodeInvalidRequest)

	run := e.mustTask("backend", "go", func(r *v1.CreateTaskRequest) { r.Name = "go" }).Run
	if got, err := resolve(domain.KindRun, run.Short, ""); err != nil || got.ID != run.ID {
		t.Errorf("run by short ID: %+v, %v", got, err)
	}
	if got, err := resolve(domain.KindProject, "backend", ""); err != nil || got.Short != "project-1" {
		t.Errorf("project by name: %+v, %v", got, err)
	}
}
