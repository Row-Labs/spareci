package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Row-Labs/spareci/internal/gate"
	"github.com/Row-Labs/spareci/internal/gh"
	"github.com/Row-Labs/spareci/internal/route"
)

// fakeGitHub has one run (7) with a job queued for the spareci label, and
// optionally a second job in progress. It records the repo variables and
// what a re-run's new attempt would route to.
type fakeGitHub struct {
	mu        sync.Mutex
	vars      map[string]string
	calls     []string
	failVar   bool
	busy      bool   // run 7 has a job in progress
	status    string // run 7's status
	attempt   int
	queuedFor string // label run 7's queued job waits for
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{vars: map[string]string{}, status: "queued", attempt: 1, queuedFor: "spareci"}
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case strings.HasSuffix(path, "/actions/runs") && r.URL.Query().Get("status") == "queued":
		reply(map[string]any{"workflow_runs": []any{map[string]any{"id": 7}}})
	case strings.HasSuffix(path, "/actions/runs"):
		reply(map[string]any{"workflow_runs": []any{}})
	case strings.HasSuffix(path, "/runs/7/jobs"):
		jobs := []gh.Job{{ID: 1, RunID: 7, Name: "test", Status: "queued", Labels: []string{f.queuedFor}}}
		if f.status == "completed" {
			jobs[0].Status, jobs[0].Conclusion = "completed", "cancelled"
		}
		if f.busy {
			jobs = append(jobs, gh.Job{ID: 2, RunID: 7, Name: "build", Status: "in_progress", Labels: []string{"ubuntu-24.04"}})
		}
		reply(map[string]any{"jobs": jobs})
	case strings.HasSuffix(path, "/runs/7") && r.Method == "GET":
		reply(gh.Run{ID: 7, Status: f.status, RunAttempt: f.attempt})
	case strings.HasSuffix(path, "/runs/7/cancel"):
		f.calls = append(f.calls, "cancel")
		f.status = "completed"
		w.WriteHeader(202)
	case strings.HasSuffix(path, "/runs/7/rerun-failed-jobs"), strings.HasSuffix(path, "/runs/7/rerun"):
		f.calls = append(f.calls, "rerun")
		f.status, f.attempt = "queued", f.attempt+1
		// The new attempt reads the routing variable now.
		if v := f.vars["SPARECI_RUNS_ON"]; v != "" {
			f.queuedFor = v
		} else {
			f.queuedFor = "ubuntu-24.04"
		}
		w.WriteHeader(201)
	case strings.Contains(path, "/actions/variables"):
		name := path[strings.LastIndex(path, "/")+1:]
		f.calls = append(f.calls, r.Method+" "+name)
		if f.failVar {
			http.Error(w, "boom", 500)
			return
		}
		switch r.Method {
		case "PATCH":
			var body struct{ Name, Value string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := f.vars[name]; !ok {
				http.Error(w, "not found", 404)
				return
			}
			f.vars[name] = body.Value
		case "POST":
			var body struct{ Name, Value string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.vars[body.Name] = body.Value
		case "DELETE":
			delete(f.vars, name)
		}
		w.WriteHeader(204)
	default:
		http.Error(w, "unexpected "+r.Method+" "+path, 500)
	}
}

func (f *fakeGitHub) get(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vars[name]
}

func (f *fakeGitHub) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeGitHub) set(fn func(f *fakeGitHub)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

type fakeGate struct {
	mu sync.Mutex
	s  gate.State
}

func (g *fakeGate) Check() gate.State { g.mu.Lock(); defer g.mu.Unlock(); return g.s }
func (g *fakeGate) set(open bool, why string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.s = gate.State{Open: open, Reason: why}
}

func newTestPool(t *testing.T, g Gate) (*Pool, *fakeGitHub) {
	t.Helper()
	fg := newFakeGitHub()
	srv := httptest.NewServer(fg)
	t.Cleanup(srv.Close)
	client := gh.NewWithBase(srv.URL, "test")
	p := New(Config{
		Repo:     "o/r",
		Backends: []*Backend{{Backend: &fakeBackend{os: "macos", arch: "arm64"}, Routing: []string{"spareci"}}},
		Max:      0, // never spawn a real runner
		Interval: time.Hour,
		Router: &route.Router{API: client, Repo: "o/r", Machine: "mac",
			Groups: []route.Group{{Heartbeat: "SPARECI_HEARTBEAT", Routes: []route.Route{{Var: "SPARECI_RUNS_ON", Label: "spareci"}}}},
			Poll:   time.Millisecond},
		Gate: g,
	}, client)
	return p, fg
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGatePausesAndResumes(t *testing.T) {
	g := &fakeGate{s: gate.State{Open: true}}
	p, fg := newTestPool(t, g)
	ctx := context.Background()

	// Open at start: routing and heartbeat on.
	if err := p.cfg.Router.Start(ctx); err != nil {
		t.Fatal(err)
	}
	p.routed = true

	// An idle runner and a busy one.
	var idleCancelled, busyCancelled bool
	b := p.cfg.Backends[0]
	p.workers["idle"] = &worker{name: "idle", backend: b, spawned: time.Now(), cancel: func() { idleCancelled = true }}
	p.workers["busy"] = &worker{name: "busy", backend: b, spawned: time.Now(), started: time.Now(), job: "x", cancel: func() { busyCancelled = true }}

	// Unplug: routing and heartbeat cleared, idle runner stopped, busy one
	// left alone, and the job queued here is re-run on GitHub.
	g.set(false, "on battery")
	if err := p.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if v := fg.get("SPARECI_RUNS_ON"); v != "" {
		t.Errorf("SPARECI_RUNS_ON = %q after pause, want cleared", v)
	}
	if v := fg.get("SPARECI_HEARTBEAT"); v != "" {
		t.Errorf("heartbeat still set while paused (would keep the watchdog quiet): %q", v)
	}
	if !idleCancelled || busyCancelled {
		t.Errorf("idle cancelled = %v (want true), busy cancelled = %v (want false)", idleCancelled, busyCancelled)
	}
	waitFor(t, "queued job sent to GitHub", func() bool {
		var q string
		fg.set(func(f *fakeGitHub) { q = f.queuedFor })
		return q == "ubuntu-24.04"
	})
	// Replug: routing and heartbeat back on.
	g.set(true, "")
	p.tick(ctx)
	if v := fg.get("SPARECI_RUNS_ON"); v != "spareci" {
		t.Errorf("SPARECI_RUNS_ON = %q after resume, want spareci", v)
	}
	if fg.get("SPARECI_HEARTBEAT") == "" {
		t.Error("heartbeat not restored on resume")
	}
	if p.paused {
		t.Error("still paused")
	}
}

func TestGatePauseLeavesBusyRunAlone(t *testing.T) {
	g := &fakeGate{s: gate.State{Open: false, Reason: "outside 18:00-08:00"}}
	p, fg := newTestPool(t, g)
	fg.set(func(f *fakeGitHub) { f.busy = true })
	ctx := context.Background()

	p.routed = true
	p.tick(ctx)
	time.Sleep(100 * time.Millisecond) // let the background rescue run
	for _, c := range fg.log() {
		if c == "cancel" || c == "rerun" {
			t.Fatalf("paused pool cancelled a run with a job in progress: %v", fg.log())
		}
	}
	// Once that job ends, the next poll sends the queued one back.
	fg.set(func(f *fakeGitHub) { f.busy = false })
	p.tick(ctx)
	waitFor(t, "rescue after busy job ended", func() bool {
		var q string
		fg.set(func(f *fakeGitHub) { q = f.queuedFor })
		return q == "ubuntu-24.04"
	})
}

func TestGateClosedAtStartClearsLeftoverRouting(t *testing.T) {
	g := &fakeGate{s: gate.State{Open: false, Reason: "outside 18:00-08:00"}}
	p, fg := newTestPool(t, g)
	fg.set(func(f *fakeGitHub) {
		f.vars["SPARECI_RUNS_ON"] = "spareci" // left over from a crash
		f.vars["SPARECI_HEARTBEAT"] = "1|"
		f.busy = true // keep the rescue out of this test
	})

	ctx, cancel := context.WithCancel(context.Background())
	// The busy run would hold the drain until the drain timeout.
	hard, hardCancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- p.Run(ctx, hard) }()
	waitFor(t, "variable cleared", func() bool { return fg.get("SPARECI_RUNS_ON") == "" && fg.get("SPARECI_HEARTBEAT") == "" })
	cancel()
	hardCancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, c := range fg.log() {
		if strings.HasPrefix(c, "PATCH") || strings.HasPrefix(c, "POST") {
			t.Errorf("Run set routing while the gate was closed: %v", fg.log())
		}
	}
}

func TestGateRoutingFailureRetries(t *testing.T) {
	g := &fakeGate{s: gate.State{Open: false, Reason: "on battery"}}
	p, fg := newTestPool(t, g)
	fg.set(func(f *fakeGitHub) {
		f.vars["SPARECI_RUNS_ON"] = "spareci"
		f.failVar = true
	})
	p.routed = true
	ctx := context.Background()

	p.tick(ctx)
	if !p.routed {
		t.Fatal("routed flipped despite the API failing")
	}
	for _, c := range fg.log() {
		if c == "cancel" {
			t.Fatal("rescued a run while routing was still on")
		}
	}
	fg.set(func(f *fakeGitHub) { f.failVar = false })
	p.tick(ctx)
	if v := fg.get("SPARECI_RUNS_ON"); v != "" || p.routed {
		t.Errorf("retry didn't clear routing: variable=%q routed=%v", v, p.routed)
	}
}

func TestNoGateAlwaysAdmits(t *testing.T) {
	p, fg := newTestPool(t, nil)
	if !p.admit(context.Background(), []gh.Job{{RunID: 7}}) {
		t.Fatal("nil gate closed")
	}
	if calls := fg.log(); len(calls) != 0 {
		t.Errorf("nil gate touched GitHub: %v", calls)
	}
}
