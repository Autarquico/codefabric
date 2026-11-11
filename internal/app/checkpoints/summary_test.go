package checkpoints

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/controller/relay"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	base = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tip  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func sample() captured {
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	return captured{
		Task:       store.Task{Name: "fix login", Prompt: "Make the login\nredirect work.\n# not a heading"},
		Run:        store.Run{SourceCommit: base},
		Checkpoint: store.Checkpoint{Checkpoint: domain.Checkpoint{Seq: 2, Trigger: domain.CheckpointDefer}},
		Origin:     domain.RunRunning,
		Commit:     tip,
		Commits: []relay.Commit{
			{ID: strings.Repeat("c", 40), Subject: "redirect after login", Files: []string{"auth.go", "auth_test.go"}},
		},
		Events: []domain.Event{
			{Type: domain.EventRunQueued, Level: domain.LevelInfo, Message: "queued", ReceivedAt: at},
			{Type: domain.EventRunStarted, Level: domain.LevelInfo, Message: "run started", ReceivedAt: at},
			{Type: domain.EventEnvironmentStopFailed, Level: domain.LevelWarn, Message: "service did not stop", ReceivedAt: at.Add(time.Minute)},
		},
		Excluded:    []string{".env"},
		NativeState: &domain.NativeState{Adapter: "claude", AdapterVersion: "1.4.0", Digest: "sha256:x"},
	}
}

func TestCompose_IsDeterministicAndMarkdown(t *testing.T) {
	a, b := compose(sample()), compose(sample())
	if a != b {
		t.Fatalf("two compositions differ:\n%+v\n%+v", a, b)
	}
	for _, want := range []string{"Checkpoint 2 of the Run (defer) at commit bbbbbbb", "- ccccccc redirect after login (auth.go, auth_test.go)", "since aaaaaaa"} {
		if !strings.Contains(a.Progress, want) {
			t.Errorf("progress lacks %q:\n%s", want, a.Progress)
		}
	}
	if !strings.Contains(a.Next, "> Make the login\n> redirect work.\n> # not a heading") {
		t.Errorf("the prompt is not quoted:\n%s", a.Next)
	}
	if !strings.Contains(a.Decisions, "claude 1.4.0") || !strings.Contains(a.Decisions, "- .env") {
		t.Errorf("decisions:\n%s", a.Decisions)
	}
	if !strings.Contains(a.Commands, "2026-10-10T12:00:00Z info RunStarted: run started") ||
		!strings.Contains(a.Commands, "warn EnvironmentStopFailed") || strings.Contains(a.Commands, "RunQueued") {
		t.Errorf("commands:\n%s", a.Commands)
	}
}

func TestCompose_WithoutNativeStateOrExcluded(t *testing.T) {
	in := sample()
	in.NativeState, in.Excluded = nil, nil
	got := compose(in)
	if !strings.Contains(got.Decisions, "No native state") || !strings.Contains(got.Decisions, "Nothing was left out") {
		t.Errorf("decisions:\n%s", got.Decisions)
	}
}

func TestCompose_SaysWhenThereIsNoHistoryOrNoCommits(t *testing.T) {
	in := sample()
	in.NoHistory, in.Commits = true, nil
	if p := compose(in).Progress; !strings.Contains(p, "history of the Run is not available") {
		t.Errorf("progress:\n%s", p)
	}
	in.NoHistory = false
	if p := compose(in).Progress; !strings.Contains(p, "No commits since aaaaaaa") {
		t.Errorf("progress:\n%s", p)
	}
}

func TestCompose_TruncatesAndSaysSo(t *testing.T) {
	in := sample()
	in.Task.Prompt = strings.Repeat("é", maxPromptRunes+50)
	in.Commits = nil
	for i := range maxCommits {
		files := make([]string, maxFilesPerCommit+3)
		for j := range files {
			files[j] = fmt.Sprintf("dir/%d/%d.go", i, j)
		}
		in.Commits = append(in.Commits, relay.Commit{ID: strings.Repeat("d", 40), Subject: "s", Files: files})
	}
	in.MoreCommits = true
	in.Excluded = nil
	for i := range maxExcluded + 5 {
		in.Excluded = append(in.Excluded, fmt.Sprintf("secret-%d", i))
	}
	in.Events = nil
	for i := range maxEvents + 4 {
		in.Events = append(in.Events, domain.Event{Type: domain.EventRunStarted, Level: domain.LevelInfo, Message: fmt.Sprint("e", i)})
	}
	got := compose(in)

	if !strings.Contains(got.Next, "...") || strings.Count(got.Next, "é") != maxPromptRunes {
		t.Errorf("prompt not cut to %d runes", maxPromptRunes)
	}
	for name, section := range map[string]string{"progress": got.Progress, "next": got.Next, "decisions": got.Decisions, "commands": got.Commands} {
		if len(section) > maxSectionBytes+len("\n... truncated\n") {
			t.Errorf("%s is %d bytes", name, len(section))
		}
	}
	if !strings.Contains(got.Progress, "and 3 more)") || !strings.Contains(got.Progress, "and older commits, left out") {
		t.Errorf("progress does not say what it left out:\n%s", got.Progress)
	}
	in.Commits[0].Subject = strings.Repeat("s", 500)
	for i := range in.Commits {
		in.Commits[i].Files = []string{strings.Repeat("p/", 60)}
		in.Commits[i].Subject = strings.Repeat("s", 500)
	}
	if p := compose(in).Progress; !strings.HasSuffix(p, "... truncated\n") {
		t.Errorf("progress is not cut at the section limit: %d bytes", len(p))
	}
	if !strings.Contains(got.Decisions, "and 5 more") || strings.Contains(got.Decisions, "secret-20") {
		t.Errorf("decisions:\n%s", got.Decisions)
	}
	if !strings.Contains(got.Commands, "4 earlier events left out") || strings.Contains(got.Commands, ": e0\n") {
		t.Errorf("commands:\n%s", got.Commands)
	}
}

func TestLimitBytes_CutsAtARuneBoundary(t *testing.T) {
	got := limitBytes(strings.Repeat("é", maxSectionBytes))
	if !strings.HasSuffix(got, "... truncated\n") || strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("bad cut: %q", got[len(got)-30:])
	}
}
