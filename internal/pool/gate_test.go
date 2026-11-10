package pool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Row-Labs/spareci/internal/gate"
	"github.com/Row-Labs/spareci/internal/gh"
)

// fakeGitHub serves one queued job for the spareci label and records the
// routing variable.
type fakeGitHub struct {
	mu       sync.Mutex
	variable string // "" when unset
	calls    []string
	failVar  bool
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/actions/runs") && r.URL.Query().Get("status") == "queued":
		w.Write([]byte(`{"workflow_runs":[{"id":7}]}`))
	case strings.HasSuffix(path, "/actions/runs"):
		w.Write([]byte(`{"workflow_runs":[]}`))
	case strings.HasSuffix(path, "/runs/7/jobs"):
		w.Write([]byte(`{"jobs":[{"id":1,"name":"test","status":"queued","labels":["spareci"]}]}`))
	case strings.Contains(path, "/actions/variables"):
		f.calls = append(f.calls, r.Method)
		if f.failVar {
			http.Error(w, "boom", 500)
			return
		}
		switch r.Method {
		case "PATCH", "POST":
			f.variable = "spareci"
		case "DELETE":
			f.variable = ""
		}
		w.WriteHeader(204)
	default:
		http.Error(w, "unexpected "+r.Method+" "+path, 500)
	}
}

func (f *fakeGitHub) state() (string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.variable, append([]string(nil), f.calls...)
}

type fakeGate struct{ s gate.State }

func (g *fakeGate) Check() gate.State { return g.s }

func newTestPool(t *testing.T, g Gate) (*Pool, *fakeGitHub) {
	t.Helper()
	fg := &fakeGitHub{}
	srv := httptest.NewServer(fg)
	t.Cleanup(srv.Close)
	p := New(Config{
		Repo:        "o/r",
		Labels:      []string{"self-hosted", "spareci"},
		Max:         0, // never spawn a real runner
		Interval:    time.Hour,
		FallbackVar: "SPARECI_RUNS_ON",
		RouteLabel:  "spareci",
		Gate:        g,
	}, gh.NewWithBase(srv.URL, "test"), nil)
	return p, fg
}

func TestGateRoutesOffAndBackOn(t *testing.T) {
	g := &fakeGate{gate.State{Open: true}}
	p, fg := newTestPool(t, g)
	ctx := context.Background()

	// Open at start: Run would set the variable; mimic that.
	p.routed = true
	fg.variable = "spareci"

	// An idle runner and a busy one.
	var idleCancelled, busyCancelled bool
	p.workers["idle"] = &worker{name: "idle", spawned: time.Now(), cancel: func() { idleCancelled = true }}
	p.workers["busy"] = &worker{name: "busy", spawned: time.Now(), started: time.Now(), job: "x", cancel: func() { busyCancelled = true }}

	// Unplug: routing cleared, idle runner stopped, busy one left alone.
	g.s = gate.State{Open: false, Reason: "on battery"}
	if err := p.tick(ctx, ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := fg.state(); v != "" {
		t.Errorf("variable = %q after pause, want cleared", v)
	}
	if !idleCancelled || busyCancelled {
		t.Errorf("idle cancelled = %v (want true), busy cancelled = %v (want false)", idleCancelled, busyCancelled)
	}
	if !p.paused || p.stranded != 1 {
		t.Errorf("paused=%v stranded=%d", p.paused, p.stranded)
	}

	// Still closed: no more API writes.
	_, before := fg.state()
	p.tick(ctx, ctx)
	if _, after := fg.state(); len(after) != len(before) {
		t.Errorf("extra variable calls while paused: %v", after[len(before):])
	}

	// Replug: routing back on.
	g.s = gate.State{Open: true}
	p.tick(ctx, ctx)
	if v, _ := fg.state(); v != "spareci" {
		t.Errorf("variable = %q after resume, want spareci", v)
	}
	if p.paused {
		t.Error("still paused")
	}
}

func TestGateClosedAtStartClearsLeftoverRouting(t *testing.T) {
	g := &fakeGate{gate.State{Open: false, Reason: "outside 18:00-08:00"}}
	p, fg := newTestPool(t, g)
	fg.variable = "spareci" // left over from a crash

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- p.Run(ctx, context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if v, _ := fg.state(); v == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("variable not cleared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, c := range func() []string { _, c := fg.state(); return c }() {
		if c != "DELETE" {
			t.Errorf("Run set routing while the gate was closed: %v", c)
		}
	}
}

func TestGateRoutingFailureRetries(t *testing.T) {
	g := &fakeGate{gate.State{Open: false, Reason: "on battery"}}
	p, fg := newTestPool(t, g)
	p.routed = true
	fg.variable = "spareci"
	fg.failVar = true
	ctx := context.Background()

	p.tick(ctx, ctx)
	if !p.routed {
		t.Fatal("routed flipped despite the API failing")
	}
	fg.mu.Lock()
	fg.failVar = false
	fg.mu.Unlock()
	p.tick(ctx, ctx)
	if v, _ := fg.state(); v != "" || p.routed {
		t.Errorf("retry didn't clear routing: variable=%q routed=%v", v, p.routed)
	}
}

func TestNoGateAlwaysAdmits(t *testing.T) {
	p, fg := newTestPool(t, nil)
	if !p.admit(context.Background(), 3) {
		t.Fatal("nil gate closed")
	}
	if _, calls := fg.state(); len(calls) != 0 {
		t.Errorf("nil gate touched routing: %v", calls)
	}
}
