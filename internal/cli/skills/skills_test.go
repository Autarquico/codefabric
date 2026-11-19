package skills

import (
	"strings"
	"testing"
)

func TestHandtoInRunListsTheWorkersThroughTheSocketOfTheRun(t *testing.T) {
	got, err := HandtoInRun("v0.4.0-beta.1")
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{"name: handto", "<!-- cflk-version: v0.4.0-beta.1 -->", "!`cflk handoff --list`", "Bash(cflk handoff --list*)", "cflk handoff --session ${CLAUDE_SESSION_ID} --to $worker"} {
		if !strings.Contains(s, want) {
			t.Errorf("the skill lacks %q", want)
		}
	}
	for _, not := range []string{"cflk worker list", "{{cflk-version}}"} {
		if strings.Contains(s, not) {
			t.Errorf("the skill still has %q", not)
		}
	}
}

func TestHandtoInRunRefusesAVersionItCannotWrite(t *testing.T) {
	if _, err := HandtoInRun("v1 --> x"); err == nil {
		t.Error("a version with spaces was written into the skill")
	}
}
