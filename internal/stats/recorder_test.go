package stats

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Row-Labs/spareci/internal/gh"
)

// fakeGitHub serves testdata, recorded from Row-Labs/spareci run 42904037716
// (three jobs spareci ran on 2026-11-10), as the REST API.
type fakeGitHub struct {
	mu        sync.Mutex
	hits      map[string]int
	noFile    bool // the contents API 404s
	jobChecks int  // GET of the job reports it in progress this many times first
	lagJobs   int  // the run's jobs list is empty this many times first, as when GitHub's API lags
}

func (f *fakeGitHub) lag(n int) { f.mu.Lock(); f.lagJobs = n; f.mu.Unlock() }

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits[r.URL.Path]++
	n := f.hits[r.URL.Path]
	f.mu.Unlock()
	const repo = "/repos/Row-Labs/spareci"
	serve := func(file string) {
		b, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Write(b)
	}
	switch p := r.URL.Path; {
	case p == repo:
		fmt.Fprint(w, `{"full_name":"Row-Labs/spareci","private":true}`)
	case p == repo+"/actions/runs":
		// The run as it looked while its jobs ran.
		fmt.Fprintf(w, `{"workflow_runs":[{"id":42904037716,"status":"in_progress","updated_at":%q}]}`, time.Now().UTC().Format(time.RFC3339))
	case p == repo+"/actions/runs/42904037716/jobs":
		f.mu.Lock()
		lagging := n <= f.lagJobs
		f.mu.Unlock()
		if lagging {
			fmt.Fprint(w, `{"total_count":0,"jobs":[]}`)
			return
		}
		serve("jobs-42904037716.json")
	case p == repo+"/actions/runs/42904037716":
		serve("run-42904037716.json")
	case strings.HasPrefix(p, repo+"/actions/jobs/"):
		if n <= f.jobChecks {
			fmt.Fprint(w, `{"id":130137251602,"status":"in_progress","started_at":"2026-11-10T21:20:30Z","completed_at":null}`)
			return
		}
		serve("job-" + strings.TrimPrefix(p, repo+"/actions/jobs/") + ".json")
	case p == repo+"/contents/.github/workflows/ci.yml" && r.URL.Query().Get("ref") == "0a593c585c1d327ae8e5bc2b0c6466d3f7467b6e" && !f.noFile:
		serve("contents-ci.json")
	default:
		http.NotFound(w, r)
	}
}

func newRecorder(t *testing.T, f *fakeGitHub) *Recorder {
	f.hits = map[string]int{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Recorder{
		API: gh.NewWith("token", srv.URL, nil), Repo: "Row-Labs/spareci",
		Path: filepath.Join(t.TempDir(), "jobs.jsonl"), Retry: time.Millisecond, Tries: 5,
	}
}

func TestRecorder(t *testing.T) {
	f := &fakeGitHub{jobChecks: 2}
	r := newRecorder(t, f)
	ctx := context.Background()
	local := time.Now()

	r.Started("spareci-Eventric-1794345622-2", local)
	rec, err := r.Done(ctx, Finished{Runner: "spareci-Eventric-1794345622-2", Backend: "linux-arm64", Job: "build", Conclusion: "Succeeded", Started: local, Ended: local.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if rec.JobID != 130137251602 || rec.RunID != 42904037716 || rec.WorkflowPath != ".github/workflows/ci.yml" || rec.Workflow != "CI" || rec.Backend != "linux-arm64" {
		t.Errorf("record ids = %+v", rec)
	}
	// GitHub's billed times, read once the job showed as completed.
	if rec.Started.Format(time.RFC3339) != "2026-11-10T21:20:30Z" || rec.Ended.Format(time.RFC3339) != "2026-11-10T21:20:47Z" || rec.Conclusion != "success" {
		t.Errorf("record times = %v – %v, %s", rec.Started, rec.Ended, rec.Conclusion)
	}
	if !slices.Equal(rec.Fallback, []string{"ubuntu-24.04"}) || rec.FallbackError != "" || rec.Public {
		t.Errorf("fallback = %q (%s), public %v", rec.Fallback, rec.FallbackError, rec.Public)
	}

	// A second job in the same run reuses the workflow file.
	r.Started("spareci-Eventric-1794345647-3", local)
	rec2, err := r.Done(ctx, Finished{Runner: "spareci-Eventric-1794345647-3", Job: "cache-restore", Started: local, Ended: local})
	if err != nil {
		t.Fatal(err)
	}
	if rec2.Job != "cache-restore" || !slices.Equal(rec2.Fallback, []string{"ubuntu-24.04"}) {
		t.Errorf("second record = %+v", rec2)
	}
	if n := f.hits["/repos/Row-Labs/spareci/contents/.github/workflows/ci.yml"]; n != 1 {
		t.Errorf("workflow file fetched %d times, want 1", n)
	}

	recs, bad, err := Load(r.Path)
	if err != nil || bad != 0 || len(recs) != 2 || recs[0].JobID != rec.JobID {
		t.Errorf("file = %+v, %d, %v", recs, bad, err)
	}
	p := Price(recs[0])
	if p.Minutes != 1 || p.Runner.Name != "Linux 2-core" || p.Saved != 0.006 {
		t.Errorf("priced = %+v", p)
	}
}

func TestRecorderUnknown(t *testing.T) {
	ctx := context.Background()
	local := time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC)

	// No job on GitHub ran on this runner: recorded with local times, unpriced.
	r := newRecorder(t, &fakeGitHub{})
	rec, err := r.Done(ctx, Finished{Runner: "spareci-nowhere-1", Job: "build", Conclusion: "Succeeded", Started: local, Ended: local.Add(90 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Fallback != nil || !strings.Contains(rec.FallbackError, "no job on GitHub ran on runner spareci-nowhere-1") || !rec.Ended.Equal(local.Add(90*time.Second)) || rec.Conclusion != "Succeeded" {
		t.Errorf("record = %+v", rec)
	}
	if p := Price(rec); p.Minutes != 2 || p.Saved != 0 || p.Reason == "" {
		t.Errorf("priced = %+v", p)
	}

	// The workflow file can't be read: say so, don't guess.
	r = newRecorder(t, &fakeGitHub{noFile: true})
	rec, err = r.Done(ctx, Finished{Runner: "spareci-Eventric-1794345622-1", Job: "cache-save", Started: local, Ended: local})
	if err != nil {
		t.Fatal(err)
	}
	if rec.JobID != 130137251106 || rec.Fallback != nil || !strings.Contains(rec.FallbackError, "read .github/workflows/ci.yml at 0a593c5") {
		t.Errorf("record = %+v", rec)
	}
}

const lateRunner = "spareci-Eventric-1794345622-2"

// GitHub's jobs API took minutes to show finished jobs on 2026-11-11 (run
// 43099220289). The lookup keeps going, with backoff, for Window.
func TestRecorderWaitsForLaggingAPI(t *testing.T) {
	f := &fakeGitHub{lagJobs: 30} // far more than Tries
	r := newRecorder(t, f)
	r.Window, r.MaxRetry = 10*time.Second, 5*time.Millisecond
	now := time.Now()
	rec, err := r.Done(context.Background(), Finished{Runner: lateRunner, Job: "build", Started: now, Ended: now})
	if err != nil {
		t.Fatal(err)
	}
	if rec.JobID != 130137251602 || rec.FallbackError != "" {
		t.Errorf("record = %+v", rec)
	}
	if _, err := os.Stat(r.PendingFile()); !os.IsNotExist(err) {
		t.Errorf("pending file left behind: %v", err)
	}
}

func TestRecorderGivesUpAfterWindow(t *testing.T) {
	f := &fakeGitHub{lagJobs: 1 << 30}
	r := newRecorder(t, f)
	r.Window, r.MaxRetry = 50*time.Millisecond, 10*time.Millisecond
	now := time.Now()
	start := time.Now()
	rec, err := r.Done(context.Background(), Finished{Runner: lateRunner, Job: "build", Started: now, Ended: now})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.FallbackError, "no job on GitHub ran on runner") {
		t.Errorf("record = %+v", rec)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v to give up", took)
	}
	if hits := f.hits["/repos/Row-Labs/spareci/actions/runs/42904037716/jobs"]; hits < 3 {
		t.Errorf("looked %d times; want retries", hits)
	}
}

func TestBackoff(t *testing.T) {
	r := &Recorder{Retry: time.Second, MaxRetry: 10 * time.Second}
	var got []time.Duration
	for n := range 6 {
		got = append(got, r.backoff(n))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	if !slices.Equal(got, want) {
		t.Errorf("backoff = %v, want %v", got, want)
	}
}

// Stopping spareci mid-lookup leaves the job pending; the next start
// records it.
func TestRecorderPendingSurvivesStop(t *testing.T) {
	f := &fakeGitHub{lagJobs: 1 << 30}
	r := newRecorder(t, f)
	r.Window, r.MaxRetry = time.Hour, 5*time.Millisecond
	now := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, err := r.Done(ctx, Finished{Runner: lateRunner, Backend: "linux-arm64", Job: "build", Conclusion: "Succeeded", Started: now, Ended: now})
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Done after stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Done didn't return when spareci stopped")
	}
	if recs, _, _ := Load(r.Path); len(recs) != 0 {
		t.Fatalf("recorded before GitHub knew the job: %+v", recs)
	}
	left, err := r.loadPending()
	if err != nil || len(left) != 1 || left[0].Runner != lateRunner || left[0].Backend != "linux-arm64" {
		t.Fatalf("pending = %+v, %v", left, err)
	}

	// Next start: GitHub has caught up.
	f.lag(0)
	r2 := &Recorder{API: r.API, Repo: r.Repo, Path: r.Path, Retry: time.Millisecond, Tries: 5}
	var wg sync.WaitGroup
	n, err := r2.Resume(context.Background(), &wg)
	if err != nil || n != 1 {
		t.Fatalf("Resume = %d, %v", n, err)
	}
	wg.Wait()
	recs, _, _ := Load(r.Path)
	if len(recs) != 1 || recs[0].JobID != 130137251602 || recs[0].Backend != "linux-arm64" {
		t.Errorf("records = %+v", recs)
	}
	if _, err := os.Stat(r.PendingFile()); !os.IsNotExist(err) {
		t.Errorf("pending file left behind: %v", err)
	}
	if n, _ := r2.Resume(context.Background(), &wg); n != 0 {
		t.Errorf("second Resume found %d", n)
	}
}

// A crash after writing the record but before updating the pending file
// doesn't record the job twice.
func TestResumeSkipsRecordedJobs(t *testing.T) {
	r := newRecorder(t, &fakeGitHub{})
	now := time.Now()
	if err := Append(r.Path, Record{Repo: r.Repo, Runner: lateRunner, Job: "build", Started: now, Ended: now}); err != nil {
		t.Fatal(err)
	}
	if err := r.addPending(Finished{Runner: lateRunner, Job: "build", Started: now, Ended: now}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	if n, err := (&Recorder{API: r.API, Repo: r.Repo, Path: r.Path}).Resume(context.Background(), &wg); n != 0 || err != nil {
		t.Errorf("Resume = %d, %v; want nothing to do", n, err)
	}
	if _, err := os.Stat(r.PendingFile()); !os.IsNotExist(err) {
		t.Errorf("pending file left behind: %v", err)
	}
}
