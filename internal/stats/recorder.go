package stats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// Finished is what the runner saw of a job on this machine.
type Finished struct {
	Runner     string // runner name, which GitHub reports on the job
	Backend    string // which of the pool's backends ran it, e.g. linux-arm64
	Job        string // job name the runner printed
	Conclusion string // the runner's: Succeeded, Failed, Canceled
	Started    time.Time
	Ended      time.Time
}

// Recorder writes a Record for each job a repo's runners finish.
type Recorder struct {
	API  API
	Repo string
	Var  string // the routing variable in runs-on; default SPARECI_RUNS_ON
	Path string // jobs.jsonl

	Retry time.Duration // between lookups while GitHub catches up; default 3s
	Tries int           // lookups before giving up; default 10

	mu      sync.Mutex
	pending map[string]*lookup // runner name → job lookup started when the job did
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
// record it wrote. A job GitHub can't tell us about is still recorded, with
// this machine's times and no fallback, so it isn't priced.
func (r *Recorder) Done(ctx context.Context, f Finished) (Record, error) {
	r.Started(f.Runner, f.Started) // no-op if it was called
	r.mu.Lock()
	l := r.pending[f.Runner]
	r.mu.Unlock()
	select {
	case <-l.done:
	case <-ctx.Done():
		return Record{}, ctx.Err()
	}
	r.mu.Lock()
	delete(r.pending, f.Runner)
	r.mu.Unlock()

	rec := Record{Repo: r.Repo, Runner: f.Runner, Backend: f.Backend, Job: f.Job, Started: f.Started, Ended: f.Ended, Conclusion: f.Conclusion}
	rec.Public = r.isPublic(ctx)
	job, err := l.job, l.err
	if err != nil {
		// The run may have finished since; look once more among recent runs.
		job, err = r.find(ctx, f.Runner, f.Started, 2)
	}
	if err != nil {
		rec.FallbackError = err.Error()
	} else {
		r.fill(ctx, &rec, job)
	}
	if err := Append(r.Path, rec); err != nil {
		return rec, err
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
