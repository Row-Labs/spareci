package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Row-Labs/spareci/internal/gh"
	"github.com/Row-Labs/spareci/internal/route"
	"github.com/Row-Labs/spareci/internal/runner"
)

// repoFake is a repo on GitHub with several runs, enough for drain: queued
// jobs, jobs a runner here takes, cancel and re-run.
type repoFake struct {
	mu    sync.Mutex
	runs  map[int64]*gh.Run
	jobs  map[int64][]gh.Job
	vars  map[string]string
	calls []string
	seq   int64
}

func newRepoFake() *repoFake {
	return &repoFake{runs: map[int64]*gh.Run{}, jobs: map[int64][]gh.Job{}, vars: map[string]string{}}
}

// add queues a run with one job per label.
func (f *repoFake) add(run int64, jobs ...gh.Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[run] = &gh.Run{ID: run, Status: "queued", RunAttempt: 1}
	for i := range jobs {
		f.seq++
		jobs[i].ID, jobs[i].RunID, jobs[i].Status, jobs[i].CreatedAt = run*100+f.seq, run, "queued", time.Now()
	}
	f.jobs[run] = jobs
}

func (f *repoFake) log(s string) { f.calls = append(f.calls, s) }

func (f *repoFake) called(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.calls, s)
}

func (f *repoFake) settle(run int64) {
	done := true
	for _, j := range f.jobs[run] {
		if j.Status != "completed" {
			done = false
		}
		if j.Status == "in_progress" {
			f.runs[run].Status = "in_progress"
		}
	}
	if done {
		f.runs[run].Status = "completed"
	}
}

// take gives a runner here the first queued job routed to label, as GitHub
// would.
func (f *repoFake) take(runnerName, label string) (gh.Job, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, run := range sortedRuns(f.runs) {
		for i, j := range f.jobs[run] {
			if j.Status == "queued" && slices.Contains(j.Labels, label) {
				f.jobs[run][i].Status, f.jobs[run][i].RunnerName = "in_progress", runnerName
				f.settle(run)
				return f.jobs[run][i], true
			}
		}
	}
	return gh.Job{}, false
}

// finish ends the job a runner here holds.
func (f *repoFake) finish(runnerName, conclusion string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for run, jobs := range f.jobs {
		for i, j := range jobs {
			if j.RunnerName == runnerName && j.Status == "in_progress" {
				f.jobs[run][i].Status, f.jobs[run][i].Conclusion, f.jobs[run][i].CompletedAt = "completed", conclusion, time.Now()
				f.settle(run)
			}
		}
	}
}

func (f *repoFake) status(run int64) (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[run].Status, f.runs[run].RunAttempt
}

func sortedRuns(m map[int64]*gh.Run) []int64 {
	var ids []int64
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

var runPath = regexp.MustCompile(`/actions/runs/(\d+)(/.*)?$`)

func (f *repoFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	m := runPath.FindStringSubmatch(path)
	var id int64
	var run *gh.Run
	if m != nil {
		id, _ = strconv.ParseInt(m[1], 10, 64)
		if run = f.runs[id]; run == nil {
			http.Error(w, "no run", 404)
			return
		}
	}
	switch {
	case strings.HasSuffix(path, "/actions/runs"):
		var out []gh.Run
		for _, rid := range sortedRuns(f.runs) {
			if f.runs[rid].Status == r.URL.Query().Get("status") {
				out = append(out, *f.runs[rid])
			}
		}
		reply(map[string]any{"workflow_runs": out})
	case m != nil && m[2] == "/jobs":
		reply(map[string]any{"jobs": f.jobs[id]})
	case m != nil && m[2] == "" && r.Method == "GET":
		reply(*run)
	case m != nil && m[2] == "/cancel":
		f.log(fmt.Sprintf("cancel %d", id))
		// A job on a runner here is cancelled by that runner; the rest now.
		for i, j := range f.jobs[id] {
			if j.Status == "queued" {
				f.jobs[id][i].Status, f.jobs[id][i].Conclusion = "completed", "cancelled"
			}
		}
		f.settle(id)
		w.WriteHeader(202)
	case m != nil && m[2] == "/force-cancel":
		f.log(fmt.Sprintf("force-cancel %d", id))
		for i, j := range f.jobs[id] {
			if j.Status != "completed" {
				f.jobs[id][i].Status, f.jobs[id][i].Conclusion = "completed", "cancelled"
			}
		}
		f.settle(id)
		w.WriteHeader(202)
	case m != nil && (m[2] == "/rerun-failed-jobs" || m[2] == "/rerun"):
		if run.Status != "completed" {
			http.Error(w, `{"message":"This workflow is already running"}`, 403)
			return
		}
		f.log(fmt.Sprintf("rerun %d", id))
		label := "ubuntu-24.04"
		if v := f.vars["SPARECI_RUNS_ON"]; v != "" {
			label = v
		}
		run.RunAttempt++
		run.Status = "queued"
		for i, j := range f.jobs[id] {
			if j.Conclusion != "success" || m[2] == "/rerun" {
				f.jobs[id][i] = gh.Job{ID: j.ID, RunID: id, Name: j.Name, Status: "queued", Labels: []string{label}, RunAttempt: run.RunAttempt}
			}
		}
	case strings.HasSuffix(path, "/generate-jitconfig"):
		f.seq++
		reply(map[string]any{"runner": map[string]any{"id": f.seq}, "encoded_jit_config": "jit"})
	case strings.Contains(path, "/actions/runners/"):
		w.WriteHeader(204)
	case strings.Contains(path, "/actions/variables"):
		name := path[strings.LastIndex(path, "/")+1:]
		var body struct{ Name, Value string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.Method {
		case "PATCH":
			if _, ok := f.vars[name]; !ok {
				http.Error(w, "not found", 404)
				return
			}
			f.vars[name] = body.Value
		case "POST":
			f.vars[body.Name] = body.Value
		case "DELETE":
			delete(f.vars, name)
		}
		w.WriteHeader(204)
	default:
		http.Error(w, "unexpected "+r.Method+" "+path, 500)
	}
}

// jobBackend runs a runner that takes the next job routed to "spareci" from
// the fake and holds it until the test ends it, or until it's stopped: then
// it reports the job cancelled, as actions/runner does on SIGINT.
type jobBackend struct {
	fake   *repoFake
	finish chan string // runner names to finish successfully
}

func (b *jobBackend) OS() string       { return runner.OSMacOS }
func (b *jobBackend) Arch() string     { return runner.ArchARM }
func (b *jobBackend) Labels() []string { return runner.SelfHostedLabels(b.OS(), b.Arch()) }
func (b *jobBackend) Run(ctx context.Context, name, _ string, _ []string, onStart func(string)) (runner.Result, error) {
	var j gh.Job
	for {
		var ok bool
		if j, ok = b.fake.take(name, "spareci"); ok {
			break
		}
		select {
		case <-ctx.Done():
			return runner.Result{}, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	onStart(j.Name)
	for {
		select {
		case <-ctx.Done():
			b.fake.finish(name, "cancelled")
			return runner.Result{Job: j.Name, Conclusion: "Canceled"}, ctx.Err()
		case n := <-b.finish:
			if n != name {
				go func() { b.finish <- n }()
				time.Sleep(time.Millisecond)
				continue
			}
			b.fake.finish(name, "success")
			return runner.Result{Job: j.Name, Conclusion: "Succeeded"}, nil
		}
	}
}

func drainPool(t *testing.T, max int) (*Pool, *repoFake, *jobBackend) {
	t.Helper()
	old := drainPoll
	drainPoll = 5 * time.Millisecond
	t.Cleanup(func() { drainPoll = old })
	f := newRepoFake()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client := gh.NewWithBase(srv.URL, "test")
	b := &jobBackend{fake: f, finish: make(chan string, 10)}
	p := New(Config{
		Repo:     "o/r",
		Backends: []*Backend{{Backend: b, Routing: []string{"spareci"}}},
		Max:      max,
		Interval: 5 * time.Millisecond,
		Router: &route.Router{API: client, Repo: "o/r", Routes: []route.Route{{Var: "SPARECI_RUNS_ON", Label: "spareci"}},
			Poll: time.Millisecond, CancelWait: 50 * time.Millisecond, RerunWait: time.Second},
	}, client)
	return p, f, b
}

func (p *Pool) busyRunners() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var names []string
	for _, w := range p.workers {
		if !w.started.IsZero() {
			names = append(names, w.name)
		}
	}
	slices.Sort(names)
	return names
}

// The drain timeout passes with jobs still running (the service manager is
// stopping spareci): their runners are stopped so GitHub marks the jobs
// completed, and their runs are re-run on GitHub-hosted runners, so nothing
// is left in progress.
func TestDrainTimeoutStopsRunningJobsAndReRunsThem(t *testing.T) {
	p, f, _ := drainPool(t, 2)
	// Two runs with a job of the same name: the runner only prints the name,
	// so the runs are found by runner name on GitHub.
	f.add(1, gh.Job{Name: "build", Labels: []string{"spareci"}})
	f.add(2, gh.Job{Name: "build", Labels: []string{"spareci"}})
	soft, softCancel := context.WithCancel(context.Background())
	hard, hardCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(soft, hard) }()
	waitFor(t, "both jobs running here", func() bool { return len(p.busyRunners()) == 2 })

	softCancel()
	waitFor(t, "routing off", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.vars["SPARECI_RUNS_ON"] == "" })
	time.Sleep(30 * time.Millisecond) // draining: the jobs keep running
	if s, _ := f.status(1); s != "in_progress" {
		t.Fatalf("run 1 %s while draining", s)
	}
	start := time.Now()
	hardCancel() // the drain timeout
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("drain didn't return after the timeout")
	}
	if took := time.Since(start); took > StopAfterDrain {
		t.Errorf("stopping took %v, more than StopAfterDrain", took)
	}
	for _, run := range []int64{1, 2} {
		s, attempt := f.status(run)
		if s == "in_progress" || attempt != 2 {
			t.Errorf("run %d: %s, attempt %d; want re-run (attempt 2)", run, s, attempt)
		}
		f.mu.Lock()
		if l := f.jobs[run][0].Labels[0]; l != "ubuntu-24.04" {
			t.Errorf("run %d re-ran on %s, want GitHub-hosted", run, l)
		}
		f.mu.Unlock()
	}
}

// Jobs that finish within the drain timeout aren't touched.
func TestDrainLetsRunningJobsFinish(t *testing.T) {
	p, f, b := drainPool(t, 1)
	f.add(1, gh.Job{Name: "build", Labels: []string{"spareci"}})
	soft, softCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(soft, context.Background()) }()
	waitFor(t, "job running here", func() bool { return len(p.busyRunners()) == 1 })
	name := p.busyRunners()[0]
	softCancel()
	time.Sleep(30 * time.Millisecond)
	b.finish <- name
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain didn't return once the job finished")
	}
	if f.called("cancel 1") || f.called("rerun 1") {
		t.Errorf("finished run was cancelled or re-run: %v", f.calls)
	}
	if s, _ := f.status(1); s != "completed" {
		t.Errorf("run 1 %s", s)
	}
}

// A run with a job queued here and nothing in progress goes back to GitHub
// as soon as draining starts, not after running jobs finish. One whose other
// job is running here waits for it.
func TestDrainSendsQueuedJobsBackFirst(t *testing.T) {
	p, f, b := drainPool(t, 1)
	f.add(1, gh.Job{Name: "build", Labels: []string{"spareci"}}, gh.Job{Name: "lint", Labels: []string{"spareci"}})
	soft, softCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(soft, context.Background()) }()
	waitFor(t, "job running here", func() bool { return len(p.busyRunners()) == 1 })
	name := p.busyRunners()[0]
	f.add(2, gh.Job{Name: "test", Labels: []string{"spareci"}}) // waits: --max 1
	softCancel()

	waitFor(t, "run 2 sent back", func() bool { return f.called("rerun 2") })
	if f.called("cancel 1") {
		t.Fatal("run 1 cancelled while a job in it was running here")
	}
	b.finish <- name
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain didn't return")
	}
	// lint, still queued here, goes back once build has finished.
	if !f.called("rerun 1") {
		t.Errorf("run 1's queued job wasn't sent back: %v", f.calls)
	}
}

// Everything after the drain timeout fits in StopAfterDrain, which the
// service manager adds to its stop timeout.
func TestStopBudgetFits(t *testing.T) {
	findRuns := 15 * time.Second
	if StopRunners+findRuns+rescueTimeout >= StopAfterDrain {
		t.Errorf("stopping can take %v, more than StopAfterDrain (%v)", StopRunners+findRuns+rescueTimeout, StopAfterDrain)
	}
}
