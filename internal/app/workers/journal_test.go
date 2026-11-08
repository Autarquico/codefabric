package workers_test

import (
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func withJournal(j v1.JournalReport) func(*v1.HeartbeatRequest) {
	return func(r *v1.HeartbeatRequest) { r.Journal = &j }
}

func truncation(id uint64, at time.Time) v1.JournalTruncation {
	return v1.JournalTruncation{ID: id, At: at, LogsFromSeq: 10, LogsToSeq: 40, LogLines: 31, OutputBytes: 2048, RunIDs: []string{"run_1", "run_2"}}
}

func TestJournal_ATruncationBecomesOneEventWithItsRangeHoweverManyTimesItIsSent(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	epoch := newEpoch()
	report := v1.JournalReport{BacklogBytes: 4096, Truncations: []v1.JournalTruncation{truncation(1, e.clock.Now())}}

	out, err := e.svc.Heartbeat(t.Context(), w, request(epoch, withJournal(report)))
	must(t, err)
	if out.JournalAck != 1 {
		t.Fatalf("ack = %d, want 1", out.JournalAck)
	}
	// The Worker did not see the ack: it sends it again.
	out, err = e.svc.Heartbeat(t.Context(), w, request(epoch, withJournal(report)))
	must(t, err)
	if out.JournalAck != 1 {
		t.Fatalf("ack after a resend = %d, want 1", out.JournalAck)
	}

	events := e.events(domain.EventJournalTruncated)
	if len(events) != 1 {
		t.Fatalf("%d JournalTruncated events, want 1", len(events))
	}
	d := events[0].Data
	if d["logs_from_seq"] != float64(10) || d["logs_to_seq"] != float64(40) || d["log_lines"] != float64(31) || d["output_bytes"] != float64(2048) {
		t.Errorf("range = %+v", d)
	}
	if want := "the Worker discarded 31 log lines (seq 10-40) and 2.0 KiB of output (output of run_1, run_2)"; events[0].Message != want || events[0].Level != domain.LevelWarn {
		t.Errorf("message %q at level %q, want %q at warn", events[0].Message, events[0].Level, want)
	}
	if events[0].WorkerID != domain.ID(e.worker("box").ID) {
		t.Errorf("event of %s, want the Worker", events[0].WorkerID)
	}
}

func TestJournal_TheBacklogIsKeptAndShownOnlyWhileItIsNotZero(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	epoch := newEpoch()

	e.beat(w, request(epoch, withJournal(v1.JournalReport{BacklogBytes: 5000})))
	got, err := e.svc.WithJournalBacklog(t.Context())
	must(t, err)
	if len(got) != 1 || got[0].Name != "box" || got[0].JournalBacklogBytes != 5000 || got[0].JournalTruncatedAt != nil {
		t.Fatalf("backlogs = %+v", got)
	}
	if v := e.worker("box"); v.JournalBacklogBytes != 5000 {
		t.Errorf("view backlog = %d", v.JournalBacklogBytes)
	}

	e.beat(w, request(epoch, withJournal(v1.JournalReport{})))
	if got, err = e.svc.WithJournalBacklog(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("backlogs after it drained = %+v, %v", got, err)
	}
}

func TestJournal_TheTimeOfATruncationIsTheControllersNotTheWorkers(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	liar := e.clock.Now().Add(-100 * 24 * time.Hour)
	e.beat(w, request(newEpoch(), withJournal(v1.JournalReport{BacklogBytes: 1, Truncations: []v1.JournalTruncation{truncation(1, liar)}})))

	got, err := e.svc.WithJournalBacklog(t.Context())
	must(t, err)
	if len(got) != 1 || got[0].JournalTruncatedAt == nil || !got[0].JournalTruncatedAt.Equal(e.clock.Now()) {
		t.Fatalf("backlogs = %+v, want truncated at %s", got, e.clock.Now())
	}
}

func TestJournal_AWorkerCannotMakeMoreEventsThanAFewPerHeartbeat(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	epoch := newEpoch()
	var all []v1.JournalTruncation
	for id := uint64(1); id <= v1.MaxJournalTruncations; id++ {
		all = append(all, truncation(id, e.clock.Now()))
	}
	report := v1.JournalReport{Truncations: all}

	out, err := e.svc.Heartbeat(t.Context(), w, request(epoch, withJournal(report)))
	must(t, err)
	if out.JournalAck == 0 || out.JournalAck >= v1.MaxJournalTruncations {
		t.Fatalf("ack = %d: some, not all, truncations are taken in one heartbeat", out.JournalAck)
	}
	if got := len(e.events(domain.EventJournalTruncated)); uint64(got) != out.JournalAck {
		t.Fatalf("%d events for an ack of %d", got, out.JournalAck)
	}
	// What was not taken is sent again and taken in the beats that follow, once each.
	for range v1.MaxJournalTruncations {
		out, err = e.svc.Heartbeat(t.Context(), w, request(epoch, withJournal(report)))
		must(t, err)
	}
	if got := len(e.events(domain.EventJournalTruncated)); out.JournalAck != v1.MaxJournalTruncations || got != v1.MaxJournalTruncations {
		t.Fatalf("ack %d with %d events, want %d of each", out.JournalAck, got, v1.MaxJournalTruncations)
	}
}

func TestJournal_ADaemonWithoutAJournalReportLeavesWhatIsKnown(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	epoch := newEpoch()
	e.beat(w, request(epoch, withJournal(v1.JournalReport{BacklogBytes: 7, Truncations: []v1.JournalTruncation{truncation(3, e.clock.Now())}})))

	out, err := e.svc.Heartbeat(t.Context(), w, request(epoch))
	must(t, err)
	if out.JournalAck != 3 || e.worker("box").JournalBacklogBytes != 7 {
		t.Fatalf("ack %d backlog %d", out.JournalAck, e.worker("box").JournalBacklogBytes)
	}
}

func TestJournal_ADiscardKeepsTheWorkerInTheStatusAfterTheBacklogDrains(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	epoch := newEpoch()
	e.beat(w, request(epoch, withJournal(v1.JournalReport{BacklogBytes: 0, Truncations: []v1.JournalTruncation{truncation(1, e.clock.Now())}})))

	got, err := e.svc.WithJournalBacklog(t.Context())
	must(t, err)
	if len(got) != 1 || got[0].Name != "box" || got[0].JournalBacklogBytes != 0 || got[0].JournalTruncatedAt == nil {
		t.Fatalf("status workers = %+v, want the Worker with its discard and no backlog", got)
	}
	e.clock.Advance(25 * time.Hour)
	if got, err = e.svc.WithJournalBacklog(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("status workers a day later = %+v (%v), want none", got, err)
	}
}

func TestJournal_AReportThatIsNotWellFormedIsRefused(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	with := func(mutate func(*v1.JournalTruncation)) v1.JournalReport {
		tr := truncation(1, now)
		mutate(&tr)
		return v1.JournalReport{Truncations: []v1.JournalTruncation{tr}}
	}
	tooMany := v1.JournalReport{}
	for id := uint64(1); id <= v1.MaxJournalTruncations+1; id++ {
		tooMany.Truncations = append(tooMany.Truncations, truncation(id, now))
	}
	cases := map[string]v1.JournalReport{
		"backlog out of range": {BacklogBytes: 1 << 63},
		"too many":             tooMany,
		"id zero":              with(func(tr *v1.JournalTruncation) { tr.ID = 0 }),
		"ids that do not grow": {Truncations: []v1.JournalTruncation{truncation(2, now), truncation(2, now)}},
		"range backwards":      with(func(tr *v1.JournalTruncation) { tr.LogsFromSeq, tr.LogsToSeq = 9, 3 }),
		"nothing discarded":    with(func(tr *v1.JournalTruncation) { tr.LogLines, tr.OutputBytes = 0, 0 }),
		"values out of range":  with(func(tr *v1.JournalTruncation) { tr.OutputBytes = 1 << 63 }),
		"too many runs":        with(func(tr *v1.JournalTruncation) { tr.RunIDs = make([]string, v1.MaxTruncationRuns+1) }),
		"no time":              with(func(tr *v1.JournalTruncation) { tr.At = time.Time{} }),
	}
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	for name, report := range cases {
		if _, err := e.svc.Heartbeat(t.Context(), w, request(newEpoch(), withJournal(report))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if got := len(e.events(domain.EventJournalTruncated)); got != 0 {
		t.Errorf("%d events from refused heartbeats", got)
	}
}
