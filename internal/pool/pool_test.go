package pool

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Row-Labs/spareci/internal/gh"
	"github.com/Row-Labs/spareci/internal/runner"
	"github.com/Row-Labs/spareci/internal/stats"
)

// fakeBackend stands in for the native host or a Linux container backend.
type fakeBackend struct{ os, arch string }

func (f *fakeBackend) OS() string       { return f.os }
func (f *fakeBackend) Arch() string     { return f.arch }
func (f *fakeBackend) Labels() []string { return runner.SelfHostedLabels(f.os, f.arch) }
func (f *fakeBackend) Run(ctx context.Context, _, _ string, _ []string, _ func(string)) (runner.Result, error) {
	<-ctx.Done()
	return runner.Result{}, ctx.Err()
}

var _ runner.Backend = (*fakeBackend)(nil)

// macPool is a Mac with macOS natively and Linux arm64 in containers, which
// also takes Linux x64 jobs.
func macPool(max, macMax, linuxMax int) (*Pool, *Backend, *Backend) {
	mac := &Backend{Backend: &fakeBackend{"macos", "arm64"}, Routing: []string{"spareci-macos", "spareci"}, Max: macMax}
	linux := &Backend{Backend: &fakeBackend{"linux", "arm64"}, Routing: []string{"spareci-linux", "spareci-linux-arm"}, Max: linuxMax}
	return New(Config{Backends: []*Backend{mac, linux}, Max: max}, nil), mac, linux
}

func TestMatches(t *testing.T) {
	p, mac, linux := macPool(2, 0, 0)
	cases := []struct {
		labels []string
		want   *Backend
	}{
		{[]string{"spareci"}, mac},
		{[]string{"spareci-macos"}, mac},
		{[]string{"SPARECI-LINUX"}, linux}, // case-insensitive, like GitHub
		{[]string{"spareci-linux-arm"}, linux},
		{[]string{"self-hosted", "macos"}, mac},
		{[]string{"self-hosted", "Linux", "ARM64"}, linux},
		{[]string{"self-hosted", "Linux", "X64"}, nil}, // no x64 backend
		{[]string{"self-hosted", "macOS", "ARM64", "spareci"}, mac},
		{[]string{"macos-15"}, nil}, // GitHub-hosted label
		{[]string{"ubuntu-24.04"}, nil},
		{[]string{"spareci-linux", "macOS"}, nil}, // no one backend has both
		{[]string{"spareci", "gpu"}, nil},
		{nil, nil},
	}
	for _, c := range cases {
		j := gh.Job{Labels: c.labels}
		got := p.candidates(j)
		if (c.want == nil) != (len(got) == 0) || (c.want != nil && got[0] != c.want) || p.Matches(j) != (c.want != nil) {
			t.Errorf("%v: candidates %v, want %v", c.labels, got, c.want)
		}
	}
	// A job both fit goes to the first.
	if got := p.candidates(gh.Job{Labels: []string{"self-hosted", "ARM64"}}); len(got) != 2 || got[0] != mac {
		t.Errorf("self-hosted ARM64: %v", got)
	}
}

func job(name string, age time.Duration, run int64, labels ...string) gh.Job {
	return gh.Job{Name: name, RunID: run, Labels: labels, CreatedAt: time.Now().Add(-age)}
}

func count(bs []*Backend, b *Backend) int {
	n := 0
	for _, x := range bs {
		if x == b {
			n++
		}
	}
	return n
}

func TestPlanSpawnsOnTheMatchingBackend(t *testing.T) {
	p, mac, linux := macPool(4, 0, 0)
	pl := p.plan([]gh.Job{
		job("mac", time.Minute, 1, "spareci-macos"),
		job("legacy", time.Minute, 1, "spareci"),
		job("linux", time.Minute, 2, "spareci-linux"),
		job("hosted", time.Minute, 3, "ubuntu-24.04"),
	}, time.Now())
	if count(pl.spawn, mac) != 2 || count(pl.spawn, linux) != 1 || len(pl.stuck) != 0 {
		t.Errorf("spawn mac=%d linux=%d stuck=%d; want 2, 1, 0", count(pl.spawn, mac), count(pl.spawn, linux), len(pl.stuck))
	}
}

func TestPlanRespectsOverallAndPerBackendMax(t *testing.T) {
	p, mac, linux := macPool(3, 0, 1)
	jobs := []gh.Job{
		job("l1", 20*time.Minute, 1, "spareci-linux"),
		job("l2", 15*time.Minute, 2, "spareci-linux"),
		job("m1", 15*time.Minute, 3, "spareci-macos"),
		job("m2", 14*time.Minute, 4, "spareci-macos"),
		job("m3", 13*time.Minute, 5, "spareci-macos"),
	}
	pl := p.plan(jobs, time.Now())
	if count(pl.spawn, linux) != 1 || count(pl.spawn, mac) != 2 {
		t.Fatalf("spawn linux=%d mac=%d; want 1 (its max), 2 (the rest of --max)", count(pl.spawn, linux), count(pl.spawn, mac))
	}
	// Both full, so every job of either kind waits; max-wait applies to all.
	if len(pl.stuck) != 5 {
		t.Errorf("stuck %d, want 5", len(pl.stuck))
	}
	if got := Overdue(pl.stuck, 14*time.Minute+30*time.Second, time.Now()); len(got) != 3 {
		t.Errorf("overdue runs %v, want 1, 2, 3", got)
	}
}

func TestPlanStuckIsPerBackend(t *testing.T) {
	p, _, linux := macPool(5, 0, 1)
	jobs := []gh.Job{
		job("l1", 20*time.Minute, 1, "spareci-linux"),
		job("l2", 20*time.Minute, 2, "spareci-linux"),
		job("m1", 20*time.Minute, 3, "spareci-macos"),
	}
	pl := p.plan(jobs, time.Now())
	if count(pl.spawn, linux) != 1 || len(pl.spawn) != 2 {
		t.Fatalf("spawn %v", pl.spawn)
	}
	for _, j := range pl.stuck {
		if j.Name == "m1" {
			t.Error("a full Linux backend made a macOS job wait for max-wait")
		}
	}
	if len(pl.stuck) != 2 {
		t.Errorf("stuck %d, want both Linux jobs", len(pl.stuck))
	}
}

func TestPlanIdleRunnerCoversOnlyItsBackend(t *testing.T) {
	p, mac, linux := macPool(4, 0, 0)
	now := time.Now()
	p.workers["m"] = &worker{name: "m", backend: mac, spawned: now}
	p.workers["l"] = &worker{name: "l", backend: linux, spawned: now, started: now.Add(-5 * time.Second), job: "linux"}
	pl := p.plan([]gh.Job{
		job("mac", time.Minute, 1, "spareci-macos"),
		job("linux", time.Minute, 2, "spareci-linux"),  // just taken by l
		job("linux2", time.Minute, 2, "spareci-linux"), // needs a new one
		job("legacy", time.Minute, 3, "spareci"),       // needs a new mac one
	}, now)
	if count(pl.spawn, linux) != 1 || count(pl.spawn, mac) != 1 {
		t.Errorf("spawn linux=%d mac=%d; want 1, 1", count(pl.spawn, linux), count(pl.spawn, mac))
	}
}

func TestPlanReapsIdleRunnerWithNothingForItsBackend(t *testing.T) {
	p, mac, linux := macPool(4, 0, 0)
	now := time.Now()
	var macReaped, linuxReaped bool
	p.workers["m"] = &worker{name: "m", backend: mac, spawned: now.Add(-4 * time.Minute), cancel: func() { macReaped = true }}
	p.workers["l"] = &worker{name: "l", backend: linux, spawned: now.Add(-4 * time.Minute), cancel: func() { linuxReaped = true }}
	p.plan([]gh.Job{job("linux", time.Minute, 2, "spareci-linux")}, now)
	if !macReaped || linuxReaped {
		t.Errorf("reaped mac=%v (want true) linux=%v (want false)", macReaped, linuxReaped)
	}
}

func TestOverdue(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	jobs := []gh.Job{
		{RunID: 1, CreatedAt: now.Add(-11 * time.Minute)},
		{RunID: 1, CreatedAt: now.Add(-12 * time.Minute)}, // same run: listed once
		{RunID: 2, CreatedAt: now.Add(-9 * time.Minute)},  // not yet
		{RunID: 3}, // no timestamp: never
	}
	got := Overdue(jobs, 10*time.Minute, now)
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("Overdue = %v, want [1]", got)
	}
	if got := Overdue(jobs, 0, now); got != nil {
		t.Errorf("max-wait 0 should disable, got %v", got)
	}
}

func TestAssign(t *testing.T) {
	host := &fakeBackend{runner.HostOS(), runner.HostArch()}
	arm := &fakeBackend{"linux", "arm64"}
	x64 := &fakeBackend{"linux", "x64"}
	routing := func(bs []*Backend) map[runner.Backend][]string {
		m := map[runner.Backend][]string{}
		for _, b := range bs {
			m[b.Backend] = b.Routing
		}
		return m
	}

	if runner.HostOS() == "macos" && runner.HostArch() == "arm64" {
		// The usual Mac: macOS natively, Linux arm64 in containers taking x64 jobs too.
		bs, targets := Assign([]runner.Backend{host, arm}, Policy{Prefix: "spareci", X64OnARM: true, Legacy: true, Max: map[string]int{"linux": 2}})
		got := routing(bs)
		if strings.Join(got[host], ",") != "spareci-macos,spareci" || strings.Join(got[arm], ",") != "spareci-linux-arm,spareci-linux" {
			t.Errorf("routing %v", got)
		}
		if len(targets) != 3 || bs[1].Max != 2 || bs[0].Max != 0 {
			t.Errorf("targets %v, max %d %d", targets, bs[0].Max, bs[1].Max)
		}
	}

	// Without X64OnARM, x64 jobs need an x64 backend.
	bs, targets := Assign([]runner.Backend{arm}, Policy{Prefix: "spareci"})
	if got := routing(bs); strings.Join(got[arm], ",") != "spareci-linux-arm" || len(targets) != 1 {
		t.Errorf("arm only: %v %v", got, targets)
	}
	// An x64 backend beats X64OnARM.
	bs, _ = Assign([]runner.Backend{arm, x64}, Policy{Prefix: "spareci", X64OnARM: true})
	if got := routing(bs); strings.Join(got[arm], ",") != "spareci-linux-arm" || strings.Join(got[x64], ",") != "spareci-linux" {
		t.Errorf("arm and x64: %v", got)
	}
	// A second backend for a kind already taken gets nothing.
	if bs, _ := Assign([]runner.Backend{arm, &fakeBackend{"linux", "arm64"}}, Policy{Prefix: "spareci"}); len(bs) != 1 {
		t.Errorf("duplicate backend kept: %d", len(bs))
	}
}

// GitHub's jobs API kept listing finished jobs as queued for minutes (run
// 43099220289), and the pool started a runner for them. A job a runner here
// started is remembered by ID and no longer counts as queued.
func TestRanJobIsNotQueuedAgain(t *testing.T) {
	p, mac, _ := macPool(2, 0, 0)
	now := time.Now()
	build := gh.Job{ID: 11, RunID: 1, Name: "build", Labels: []string{"spareci"}, CreatedAt: now}
	test := gh.Job{ID: 12, RunID: 1, Name: "test", Labels: []string{"spareci"}, CreatedAt: now}
	p.queued = []gh.Job{build, test}
	p.claim(&worker{backend: mac}, "build", now)

	// Minutes later: the runner is gone, but GitHub still lists build.
	later := now.Add(3 * time.Minute)
	mine := p.notRan([]gh.Job{build, test}, later)
	if len(mine) != 1 || mine[0].ID != 12 {
		t.Fatalf("still queued: %v", mine)
	}
	if pl := p.plan(p.notRan([]gh.Job{build}, later), later); len(pl.spawn) != 0 {
		t.Errorf("started %d runners for a job that already ran", len(pl.spawn))
	}

	// Forgotten after ranFor.
	if mine := p.notRan([]gh.Job{build}, now.Add(ranFor+time.Minute)); len(mine) != 1 {
		t.Errorf("job still ignored after %v", ranFor)
	}
}

func TestClaimOnlyWhenUnambiguous(t *testing.T) {
	p, mac, linux := macPool(2, 0, 0)
	now := time.Now()
	// The same job queued in two runs: which one the runner took is unknown,
	// so neither is ignored.
	p.queued = []gh.Job{
		{ID: 21, RunID: 1, Name: "build", Labels: []string{"spareci"}},
		{ID: 22, RunID: 2, Name: "build", Labels: []string{"spareci"}},
		{ID: 23, RunID: 3, Name: "lint", Labels: []string{"spareci-linux"}},
		{ID: 24, RunID: 3, Name: "lint", Labels: []string{"spareci-macos"}},
	}
	p.claim(&worker{backend: mac}, "build", now)
	if len(p.ran) != 0 {
		t.Errorf("claimed %v from two candidates", p.ran)
	}
	// Two jobs named lint, but only one fits the Linux backend.
	w := &worker{backend: linux}
	p.claim(w, "lint", now)
	if len(p.ran) != 1 || p.ran[23].IsZero() || w.runID != 3 {
		t.Errorf("ran = %v, run %d; want only 23, in run 3", p.ran, w.runID)
	}
	// A second runner taking another "lint" can't claim 23 again; 24 doesn't
	// fit Linux, so nothing.
	p.claim(&worker{backend: linux}, "lint", now)
	if len(p.ran) != 1 {
		t.Errorf("ran = %v", p.ran)
	}
}

// Stopping never waits on GitHub for stats: a job it hasn't caught up on
// stays pending for the next start.
func TestStopLeavesStatsPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"workflow_runs":[],"jobs":[],"private":true}`) // GitHub hasn't caught up
	}))
	t.Cleanup(srv.Close)
	rec := &stats.Recorder{API: gh.NewWithBase(srv.URL, "test"), Repo: "o/r", Path: filepath.Join(t.TempDir(), "jobs.jsonl"),
		Retry: time.Millisecond, MaxRetry: 10 * time.Millisecond, Window: time.Hour}
	p, _, _ := macPool(1, 0, 0)
	p.cfg.Stats = rec
	now := time.Now()
	p.record(stats.Finished{Runner: "spareci-x-1", Job: "build", Started: now, Ended: now})
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	p.stopRecording()
	if took := time.Since(start); took > time.Second {
		t.Errorf("stopping waited %v on GitHub", took)
	}
	if n := rec.Pending(); n != 1 {
		t.Errorf("pending = %d, want 1", n)
	}
	if _, err := os.Stat(rec.PendingFile()); err != nil {
		t.Errorf("pending file: %v", err)
	}
}
