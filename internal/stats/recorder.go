package stats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/Row-Labs/spareci/internal/gh"
	"github.com/Row-Labs/spareci/internal/workflows"
)

// API is the slice of GitHub the recorder reads. *gh.Client has it.
type API interface {
	ListRuns(ctx context.Context, repo, status string) ([]gh.Run, error)
	RunJobs(ctx context.Context, repo string, runID int64) ([]gh.Job, error)
	GetJob(ctx context.Context, repo string, jobID int64) (gh.RunJob, error)
	GetRun(ctx context.Context, repo string, runID int64) (gh.Run, error)
	FileAt(ctx context.Context, repo, path, ref string) ([]byte, error)
	Repo(ctx context.Context, repo string) (gh.Repo, error)
}

// Finished is what the runner saw of a job on this machine. It's also what
// the pending file holds for a job whose record isn't written yet.
type Finished struct {
	Runner     string    `json:"runner"`            // runner name, which GitHub reports on the job
	Backend    string    `json:"backend,omitempty"` // which of the pool's backends ran it, e.g. linux-arm64
	Job        string    `json:"job"`               // job name the runner printed
	Conclusion string    `json:"conclusion,omitempty"`
	Started    time.Time `json:"started"`
	Ended      time.Time `json:"ended"`
}

// Recorder writes a Record for each job a repo's runners finish.
type Recorder struct {
	API  API
	Repo string
	Var  string // the routing variable in runs-on; default SPARECI_RUNS_ON
	Path string // jobs.jsonl

	Retry time.Duration // first wait between lookups while GitHub catches up, doubling up to MaxRetry; default 3s
	Tries int           // lookups while the job runs, and reads of its final times; default 10
	// MaxRetry caps the wait between lookups; default 1m.
	MaxRetry time.Duration
	// Window is how long after a job ends (or after a restart, for a job
	// left pending) its lookup keeps going. GitHub's jobs API has lagged
	// by minutes. Default 10m.
	Window time.Duration

	mu      sync.Mutex
	pending map[string]*lookup  // runner name → job lookup started when the job did
	unsaved map[string]Finished // runner name → finished job not yet in the stats file
	fileMu  sync.Mutex          // serialises writes of the pending file
	files   map[string]*workflows.File
	public  *bool
	now     func() time.Time
}

type lookup struct {
	done chan struct{}
	job  gh.Job
	err  error
}

func (r *Recorder) retry() time.Duration {
	if r.Retry > 0 {
		return r.Retry
	}
	return 3 * time.Second
}

func (r *Recorder) window() time.Duration {
	if r.Window > 0 {
		return r.Window
	}
	return 10 * time.Minute
}

// backoff is the wait before lookup n+1: Retry, doubling, at most MaxRetry.
func (r *Recorder) backoff(n int) time.Duration {
	limit := r.MaxRetry
	if limit <= 0 {
		limit = time.Minute
	}
	d := r.retry()
	for range n {
		if d >= limit {
			break
		}
		d *= 2
	}
	return min(d, limit)
}

func (r *Recorder) tries() int {
	if r.Tries > 0 {
		return r.Tries
	}
	return 10
}

func (r *Recorder) varName() string {
	if r.Var != "" {
		return r.Var
	}
	return workflows.DefaultVar
}

// Started looks the job up on GitHub while it runs, when its run is surely
// still in progress and easy to find.
func (r *Recorder) Started(runner string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending = map[string]*lookup{}
	}
	if _, ok := r.pending[runner]; ok {
		return
	}
	l := &lookup{done: make(chan struct{})}
	r.pending[runner] = l
	go func() {
		defer close(l.done)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		l.job, l.err = r.find(ctx, runner, at, r.tries())
	}()
}

// Done records a finished job and logs the running total. It returns the
// record it wrote. GitHub's jobs API can lag by minutes, so the lookup is
// retried with backoff for Window after the job ended. A job GitHub still
// can't tell us about is recorded with this machine's times and no fallback,
// so it isn't priced.
//
// The job is in the pending file (PendingFile) until its record is written.
// If ctx ends first (spareci is stopping), Done returns ctx.Err() and leaves
// it there for Resume on the next start.
func (r *Recorder) Done(ctx context.Context, f Finished) (Record, error) {
	if err := r.addPending(f); err != nil {
		slog.Warn("could not save pending job stats; they're lost if spareci stops first", "err", err)
	}
	return r.done(ctx, f, f.Ended.Add(r.window()))
}

// Resume finishes the jobs a previous run left pending, each with a fresh
// Window. It returns how many there were; their records are written in the
// background, tracked by wg.
func (r *Recorder) Resume(ctx context.Context, wg *sync.WaitGroup) (int, error) {
	left, err := r.loadPending()
	if err != nil {
		return 0, err
	}
	// A crash between writing a record and updating the pending file
	// leaves a job in both; it's already recorded.
	if recs, _, err := Load(r.Path); err == nil && len(left) > 0 {
		for _, rec := range recs {
			left = slices.DeleteFunc(left, func(f Finished) bool { return rec.Repo == r.Repo && f.Runner == rec.Runner })
		}
		_ = r.savePendingList(left)
	}
	r.mu.Lock()
	if r.unsaved == nil {
		r.unsaved = map[string]Finished{}
	}
	for _, f := range left {
		r.unsaved[f.Runner] = f
	}
	r.mu.Unlock()
	deadline := r.clock().Add(r.window())
	for _, f := range left {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.done(ctx, f, deadline); err != nil && ctx.Err() == nil {
				slog.Warn("could not record job stats", "job", f.Job, "err", err)
			}
		}()
	}
	return len(left), nil
}

func (r *Recorder) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *Recorder) done(ctx context.Context, f Finished, deadline time.Time) (Record, error) {
	r.mu.Lock()
	l := r.pending[f.Runner]
	delete(r.pending, f.Runner)
	r.mu.Unlock()

	var job gh.Job
	err := errNotFound
	if l != nil {
		select {
		case <-l.done:
			job, err = l.job, l.err
		case <-ctx.Done():
			return Record{}, ctx.Err()
		}
	}
	// The run may have finished since, or GitHub may not show the runner
	// on the job yet: keep looking among recent runs until the deadline.
	for n := 0; err != nil; n++ {
		if job, err = r.findOnce(ctx, f.Runner, f.Started); err == nil {
			break
		}
		if ctx.Err() != nil {
			return Record{}, ctx.Err()
		}
		wait := r.backoff(n)
		if r.clock().Add(wait).After(deadline) {
			break
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return Record{}, ctx.Err()
		}
	}

	rec := Record{Repo: r.Repo, Runner: f.Runner, Backend: f.Backend, Job: f.Job, Started: f.Started, Ended: f.Ended, Conclusion: f.Conclusion}
	rec.Public = r.isPublic(ctx)
	if err != nil {
		rec.FallbackError = err.Error()
	} else {
		r.fill(ctx, &rec, job)
	}
	if ctx.Err() != nil {
		return Record{}, ctx.Err() // half-filled; leave it pending
	}
	if err := Append(r.Path, rec); err != nil {
		return rec, err
	}
	if err := r.removePending(f.Runner); err != nil {
		slog.Warn("could not update pending job stats; the job may be recorded twice", "err", err)
	}
	r.logTotal(rec)
	return rec, nil
}

// find is the job a runner took, by the runner name GitHub reports on it.
// It searches in-progress runs and runs updated since the job started.
func (r *Recorder) find(ctx context.Context, runner string, since time.Time, tries int) (gh.Job, error) {
	var lastErr error
	for try := range tries {
		if try > 0 && !r.sleep(ctx) {
			return gh.Job{}, ctx.Err()
		}
		job, err := r.findOnce(ctx, runner, since)
		if err == nil {
			return job, nil
		}
		lastErr = err
	}
	return gh.Job{}, lastErr
}

// sleep waits one retry interval; false if ctx ended first.
func (r *Recorder) sleep(ctx context.Context) bool {
	select {
	case <-time.After(r.retry()):
		return true
	case <-ctx.Done():
		return false
	}
}

var errNotFound = errors.New("not found")

func (r *Recorder) findOnce(ctx context.Context, runner string, since time.Time) (gh.Job, error) {
	seen := map[int64]bool{}
	for _, status := range []string{"in_progress", ""} {
		runs, err := r.API.ListRuns(ctx, r.Repo, status)
		if err != nil {
			return gh.Job{}, err
		}
		for _, run := range runs {
			if seen[run.ID] || (run.Status == "completed" && run.UpdatedAt.Before(since.Add(-time.Minute))) {
				continue
			}
			seen[run.ID] = true
			jobs, err := r.API.RunJobs(ctx, r.Repo, run.ID)
			if err != nil {
				return gh.Job{}, err
			}
			for _, j := range jobs {
				if j.RunnerName == runner {
					return j, nil
				}
			}
		}
	}
	return gh.Job{}, fmt.Errorf("no job on GitHub ran on runner %s: %w", runner, errNotFound)
}

// fill completes rec from GitHub: the job's billed times and conclusion, and
// the runs-on its workflow falls back to.
func (r *Recorder) fill(ctx context.Context, rec *Record, j gh.Job) {
	rec.JobID, rec.RunID, rec.RunAttempt, rec.Job, rec.URL = j.ID, j.RunID, j.RunAttempt, j.Name, j.HTMLURL

	// GitHub sets completed_at a moment after the runner exits.
	for try := 0; try < r.tries(); try++ {
		if try > 0 && !r.sleep(ctx) {
			break
		}
		full, err := r.API.GetJob(ctx, r.Repo, j.ID)
		if err != nil {
			continue
		}
		rec.HeadSHA, rec.Workflow = full.HeadSHA, full.WorkflowName
		if full.Status == "completed" && full.CompletedAt != nil && !full.StartedAt.IsZero() {
			rec.Started, rec.Ended, rec.Conclusion = full.StartedAt, *full.CompletedAt, full.Conclusion
			break
		}
	}

	run, err := r.API.GetRun(ctx, r.Repo, j.RunID)
	if err != nil {
		rec.FallbackError = fmt.Sprintf("read run %d: %v", j.RunID, err)
		return
	}
	rec.WorkflowPath = run.Path
	if rec.HeadSHA == "" {
		rec.HeadSHA = run.HeadSHA
	}
	if rec.Workflow == "" {
		rec.Workflow = run.Name
	}
	rec.Fallback, rec.FallbackError = r.fallback(ctx, run.Path, rec.HeadSHA, rec.Job)
}

// fallback reads the workflow at sha and returns the runs-on the job falls
// back to, or why it can't be known.
func (r *Recorder) fallback(ctx context.Context, path, sha, job string) ([]string, string) {
	if path == "" || sha == "" {
		return nil, "the run doesn't say which workflow file it ran"
	}
	f, err := r.workflow(ctx, path, sha)
	if err != nil {
		return nil, err.Error()
	}
	wj, err := f.FindJob(job)
	if err != nil {
		return nil, err.Error()
	}
	if wj.Fallback == nil {
		return nil, fmt.Sprintf("%s: %s", wj.Name(), wj.FallbackErr)
	}
	return wj.Fallback, ""
}

// workflow is a workflow file at a commit. Files at a commit never change,
// so each (path, sha) is fetched once per process.
func (r *Recorder) workflow(ctx context.Context, path, sha string) (*workflows.File, error) {
	key := path + "@" + sha
	r.mu.Lock()
	f := r.files[key]
	r.mu.Unlock()
	if f != nil {
		return f, nil
	}
	src, err := r.API.FileAt(ctx, r.Repo, path, sha)
	if err != nil {
		return nil, fmt.Errorf("read %s at %.7s: %w", path, sha, err)
	}
	f, err = workflows.Parse(path, src, r.varName())
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.files == nil || len(r.files) >= 200 {
		r.files = map[string]*workflows.File{}
	}
	r.files[key] = f
	r.mu.Unlock()
	return f, nil
}

func (r *Recorder) isPublic(ctx context.Context) bool {
	r.mu.Lock()
	p := r.public
	r.mu.Unlock()
	if p != nil {
		return *p
	}
	repo, err := r.API.Repo(ctx, r.Repo)
	if err != nil {
		return false // priced as private, as if the repo were billed; retried next job
	}
	pub := !repo.Private
	r.mu.Lock()
	r.public = &pub
	r.mu.Unlock()
	return pub
}

func (r *Recorder) logTotal(rec Record) {
	recs, _, err := Load(r.Path)
	if err != nil {
		slog.Warn("could not read job stats", "err", err)
		return
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	slog.Info(Summary(rec, Build(recs, 0, now)), "job", rec.Job, "url", rec.URL)
}
