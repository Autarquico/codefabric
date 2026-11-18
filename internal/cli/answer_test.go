package cli

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

// answerController serves one Run, and the output of the stream it has.
func answerController(t *testing.T, r v1.Run, output string) {
	t.Helper()
	isolatedMachine(t)
	ca := newTestCA(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/runs/"+r.ID, func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(r) })
	mux.HandleFunc("/v1/runs/"+r.ID+"/output", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(v1.RunOutput{RunID: r.ID, LeaseEpoch: 1, End: uint64(len(output)), Data: []byte(output), Closed: true})
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.serverCert(t)}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	paths, err := config.ResolveFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	storeBundle(t, paths.CredentialsDir(), ca, srv.Listener.Addr().String())
}

func finishedRun(answer string) v1.Run {
	return v1.Run{ID: "run_1", Short: "R-1", TaskID: "task_1", Lineage: domain.LineageInitial, State: domain.RunCompleted, Answer: answer}
}

func TestScenario_HeadlessAnswer_RunShowPrintsTheFullAnswer(t *testing.T) {
	answerController(t, finishedRun("The fix is in.\n\nSecond paragraph."), "")
	code, out, errOut := run(t, "run", "show", "run_1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "\nAnswer\nThe fix is in.\n\nSecond paragraph.\n") {
		t.Errorf("the answer is not in the output:\n%s", out)
	}
}

func TestScenario_HeadlessAnswer_RunShowWithoutAnswerHasNoSection(t *testing.T) {
	answerController(t, finishedRun(""), "")
	if _, out, _ := run(t, "run", "show", "run_1"); strings.Contains(out, "Answer") {
		t.Errorf("an Answer section without an answer:\n%s", out)
	}
}

func TestScenario_HeadlessAnswer_LogsEndWithTheAnswerOnlyForAHuman(t *testing.T) {
	answerController(t, finishedRun("done and dusted"), "some log line\n")

	code, out, errOut := run(t, "logs", "--run", "run_1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if want := "some log line\n── answer ──\ndone and dusted\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	for _, mode := range [][]string{{"--json"}, {"--raw"}} {
		code, out, errOut := run(t, append([]string{"logs", "--run", "run_1"}, mode...)...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", mode, code, errOut)
		}
		if strings.Contains(out, "answer") {
			t.Errorf("%v: the answer leaked into a machine or raw mode:\n%s", mode, out)
		}
	}
}

func TestScenario_HeadlessAnswer_LogsOfARunThatIsNotOverAddNothing(t *testing.T) {
	r := finishedRun("not yet")
	r.State = domain.RunRunning
	answerController(t, r, "some log line\n")
	if _, out, _ := run(t, "logs", "--run", "run_1"); out != "some log line\n" {
		t.Errorf("stdout = %q", out)
	}
}
