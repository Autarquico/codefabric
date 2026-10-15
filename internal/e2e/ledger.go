//go:build e2e && (linux || darwin)

package e2e

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/procident"
)

// The ledger is the proof of what harnesses ran. Its script, the prompt of a
// Task, appends a line to <ledger>/<token>.log when it starts and when it ends,
// with its pid and its working directory: the worktree of one Run under one
// lease epoch on one Worker. That is the identity of an execution; the token
// only groups the executions of a Task. The ledger lives in the directory of
// the harness, outside every worktree, and nothing the cluster does removes it.

// ledgerHold bounds how long the script waits to be released, before scaling.
// It outlasts the longest wait of a test, so that only a forgotten harness hits it.
const ledgerHold = 180 * time.Second

// ledgerPoll is the pause of the script between two looks for its go file.
const ledgerPoll = 100 * time.Millisecond

// scriptFile is the file the harness command runs, in the run directory (the
// adapters/harness/command package's ScriptFile, which this suite may not import).
const scriptFile = "command.sh"

// Execution is one run of a harness: a Run, under one lease epoch, on one
// Worker (by name). Worker is "" for a directory that is no Worker's worktree.
type Execution struct {
	Run    string
	Epoch  uint64
	Worker string
}

func (e Execution) String() string {
	return fmt.Sprintf("%s on %s", domain.RunEpoch(e.Run, e.Epoch), e.Worker)
}

// Instance is what the ledger says of one harness process.
type Instance struct {
	Execution
	Token    string
	PID      int
	Dir      string // the working directory it wrote, symlinks resolved
	Started  time.Time
	Ended    bool // it wrote result.txt and its end
	TimedOut bool // nobody released it within the cap
	// Verified: started.json of the execution, written by its wrapper, names
	// this pid. It is looked at when the ledger first sees the instance.
	Verified bool
}

// Ledger reads the lines of the script and tells whether an instance lives.
type Ledger struct {
	t       *testing.T
	dir     string
	workers []ledgerWorker

	mu     sync.Mutex
	tokens int
	seen   map[instanceKey]*instanceState
}

type ledgerWorker struct {
	name     string
	paths    config.Paths // as the daemon sees them, for its run directories
	resolved config.Paths // with symlinks resolved, as the script's pwd -P is
}

type instanceKey struct {
	token string
	pid   int
	dir   string
}

// instanceState is what was learnt of an instance the first time it was seen:
// whether its wrapper names it and the procident token of its process, empty
// if it was not running the script by then.
type instanceState struct {
	verified bool
	ident    string
}

func newLedger(t *testing.T, dir string, workers []*Worker) *Ledger {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "go"), 0o700); err != nil {
		t.Fatal(err)
	}
	l := &Ledger{t: t, dir: dir, seen: map[instanceKey]*instanceState{}}
	for _, w := range workers {
		data, err := filepath.EvalSymlinks(w.Box.Paths.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		l.workers = append(l.workers, ledgerWorker{name: w.Name, paths: w.Box.Paths, resolved: config.Paths{DataDir: data}})
	}
	return l
}

// Token is a new token, for the executions of one Task.
func (l *Ledger) Token() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokens++
	return "t" + strconv.Itoa(l.tokens)
}

// Script is the prompt of a Task whose harness records itself under token,
// waits to be released, writes token to result.txt and records its end. It
// says on its output when it starts and when it ends. It is released by its
// token or by its own execution, the file of its worktree under go/x.
func (l *Ledger) Script(token string) string {
	polls := int(Scaled(ledgerHold) / ledgerPoll)
	return fmt.Sprintf(`L='%s'; T='%s'; D="$(pwd -P)"
printf 'start %%s %%s %%s\n' "$$" "$D" "$(date +%%s)" >> "$L/$T.log"
echo "e2e: started $T"
n=0
while [ ! -e "$L/go/$T" ] && [ ! -e "$L/go/x$D" ]; do
  n=$((n+1))
  if [ "$n" -gt %d ]; then
    printf 'timeout %%s %%s %%s\n' "$$" "$D" "$(date +%%s)" >> "$L/$T.log"
    exit 99
  fi
  sleep %s
done
printf '%%s\n' "$T" > result.txt
echo "e2e: finished $T"
printf 'end %%s %%s %%s\n' "$$" "$D" "$(date +%%s)" >> "$L/$T.log"
exit 0`, l.dir, token, polls, strconv.FormatFloat(ledgerPoll.Seconds(), 'f', -1, 64))
}

// Release lets every harness of token, now and later, write its result and end.
func (l *Ledger) Release(token string) {
	l.t.Helper()
	if err := os.WriteFile(filepath.Join(l.dir, "go", token), nil, 0o600); err != nil {
		l.t.Fatal(err)
	}
}

// ReleaseExecution lets the harness of one execution write its result and
// end, and no other harness of its Task: an orphan stays while its retry ends.
func (l *Ledger) ReleaseExecution(e Execution) {
	l.t.Helper()
	i := slices.IndexFunc(l.workers, func(w ledgerWorker) bool { return w.name == e.Worker })
	if i < 0 {
		l.t.Fatalf("e2e: release %v: no Worker %q", e, e.Worker)
	}
	file := filepath.Join(l.dir, "go", "x", l.workers[i].resolved.WorkerWorktreeDir(e.Run, e.Epoch))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		l.t.Fatal(err)
	}
}

// Instances are the harness processes of token, in the order they started.
func (l *Ledger) Instances(token string) []Instance {
	l.t.Helper()
	out, err := l.instances(token)
	if err != nil {
		l.t.Fatal(err)
	}
	return out
}

// All are the harness processes of every token.
func (l *Ledger) All() []Instance {
	l.t.Helper()
	out, err := l.all()
	if err != nil {
		l.t.Fatal(err)
	}
	return out
}

// instances is Instances that returns what it cannot read, for a goroutine
// that is not the test's.
func (l *Ledger) instances(token string) ([]Instance, error) {
	raw, err := os.ReadFile(filepath.Join(l.dir, token+".log")) //nolint:gosec // a file of the ledger
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l.parse(token, string(raw))
}

// all is All that returns what it cannot read.
func (l *Ledger) all() ([]Instance, error) {
	matches, err := filepath.Glob(filepath.Join(l.dir, "*.log"))
	if err != nil {
		return nil, err
	}
	var out []Instance
	for _, m := range matches {
		ins, err := l.instances(strings.TrimSuffix(filepath.Base(m), ".log"))
		if err != nil {
			return nil, err
		}
		out = append(out, ins...)
	}
	return out, nil
}

// parse reads the lines of a token. A line the script is still writing has
// no newline yet and waits for the next read.
func (l *Ledger) parse(token, text string) ([]Instance, error) {
	var out []Instance
	index := map[instanceKey]int{}
	sc := bufio.NewScanner(strings.NewReader(text[:strings.LastIndexByte(text, '\n')+1]))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 4 {
			return nil, fmt.Errorf("e2e: ledger %s: a line that is not <kind> <pid> <dir> <time>: %q", token, sc.Text())
		}
		pid, err1 := strconv.Atoi(f[1])
		at, err2 := strconv.ParseInt(f[3], 10, 64)
		if err := errors.Join(err1, err2); err != nil {
			return nil, fmt.Errorf("e2e: ledger %s: %q: %w", token, sc.Text(), err)
		}
		key := instanceKey{token: token, pid: pid, dir: f[2]}
		if f[0] == "start" {
			index[key] = len(out)
			out = append(out, Instance{Execution: l.execution(f[2]), Token: token, PID: pid, Dir: f[2], Started: time.Unix(at, 0)})
			continue
		}
		i, ok := index[key]
		if !ok {
			return nil, fmt.Errorf("e2e: ledger %s: %q has no start", token, sc.Text())
		}
		switch f[0] {
		case "end":
			out[i].Ended = true
		case "timeout":
			out[i].TimedOut = true
		default:
			return nil, fmt.Errorf("e2e: ledger %s: unknown line %q", token, sc.Text())
		}
	}
	for i := range out {
		out[i].Verified = l.state(out[i]).verified
	}
	return out, sc.Err()
}

// execution is the execution whose worktree dir is, by the layout of the Worker.
func (l *Ledger) execution(dir string) Execution {
	run, epoch, ok := domain.ParseRunEpoch(filepath.Base(dir))
	if !ok {
		return Execution{}
	}
	e := Execution{Run: run, Epoch: epoch}
	for _, w := range l.workers {
		if w.resolved.WorkerWorktreeDir(run, epoch) == dir {
			e.Worker = w.name
		}
	}
	return e
}

// state is what is known of an instance: whether the wrapper's started.json
// names its pid and, taken the first time it is seen, the procident token of
// its process if that is still the script of the execution.
func (l *Ledger) state(in Instance) *instanceState {
	key := instanceKey{token: in.Token, pid: in.PID, dir: in.Dir}
	l.mu.Lock()
	defer l.mu.Unlock()
	st, seen := l.seen[key]
	if !seen {
		st = &instanceState{}
		l.seen[key] = st
	}
	var w *ledgerWorker
	for i := range l.workers {
		if l.workers[i].name == in.Worker {
			w = &l.workers[i]
		}
	}
	if w == nil {
		return st
	}
	runDir := w.paths.WorkerRunDir(in.Run, in.Epoch)
	if !seen && !in.Ended && !in.TimedOut {
		st.ident = scriptIdentity(in.PID, filepath.Join(filepath.Base(runDir), scriptFile))
	}
	// The wrapper writes started.json once the script is spawned: a script may
	// write its start first, so a look that finds none is made again.
	if !st.verified {
		var started execwrap.Started
		found, err := execwrap.Read(runDir, execwrap.StartedFile, &started)
		st.verified = err == nil && found && started.PID == in.PID
	}
	return st
}

// scriptIdentity is the procident token of pid if, before and after it is
// taken, the process is the script whose path ends in script: a pid reused by
// another process gets none.
func scriptIdentity(pid int, script string) string {
	if !runsScript(pid, script) {
		return ""
	}
	ident, err := procident.StartOf(pid)
	if err != nil || !runsScript(pid, script) {
		return ""
	}
	return ident
}

func runsScript(pid int, script string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // ps of a pid of the ledger
	return err == nil && strings.HasSuffix(strings.TrimSpace(string(out)), string(filepath.Separator)+script)
}

// Live says whether the process of the instance is still the one that wrote
// its start: one the ledger never saw running is not.
func (l *Ledger) Live(in Instance) bool {
	st := l.state(in)
	return st.ident != "" && procident.Verify(in.PID, st.ident) == procident.Same
}
