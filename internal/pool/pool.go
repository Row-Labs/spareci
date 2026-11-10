// Package pool watches a repo for queued jobs this machine can take and keeps
// enough single-use runners alive to drain them.
package pool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Row-Labs/spareci/internal/cache"
	"github.com/Row-Labs/spareci/internal/gh"
	"github.com/Row-Labs/spareci/internal/route"
	"github.com/Row-Labs/spareci/internal/runner"
)

type Config struct {
	Repo     string
	Labels   []string // every label the runners carry; a job matches if all its labels are here
	Max      int      // concurrent runners
	Interval time.Duration
	Router   *route.Router // routing variable and heartbeat; nil disables routing
	MaxWait  time.Duration // a job queued here longer than this goes to GitHub-hosted runners; 0 disables
	DryRun   bool
	Cache    *cache.Server // nil: jobs use GitHub's cache service
	Gate     Gate          // nil: always take jobs; see gate.go
}

type Pool struct {
	cfg  Config
	gh   *gh.Client
	host *runner.Host

	mu      sync.Mutex
	workers map[string]*worker
	seq     int

	// Laptop mode (gate.go); only touched by the poll loop.
	paused, routed bool
	stranded       int
}

type worker struct {
	name     string
	spawned  time.Time
	started  time.Time // zero until the runner picks up a job
	job      string    // name of the job it took
	cancel   context.CancelFunc
	runnerID int64
}

func New(cfg Config, client *gh.Client, host *runner.Host) *Pool {
	return &Pool{cfg: cfg, gh: client, host: host, workers: map[string]*worker{}}
}

// Matches reports whether a job's runs-on labels are all ones we carry.
func (p *Pool) Matches(j gh.Job) bool { return Matches(p.cfg.Labels, j) }

// Matches reports whether a job's runs-on labels are all in labels.
// Runner label matching on GitHub is case-insensitive.
func Matches(labels []string, j gh.Job) bool {
	if len(j.Labels) == 0 {
		return false
	}
	have := map[string]bool{}
	for _, l := range labels {
		have[strings.ToLower(l)] = true
	}
	for _, l := range j.Labels {
		if !have[strings.ToLower(l)] {
			return false
		}
	}
	return true
}

// Run polls until ctx is cancelled (stop taking work; finish running jobs)
// and then until hardCtx is cancelled or every job is done.
func (p *Pool) Run(ctx, hardCtx context.Context) error {
	if r := p.cfg.Router; r != nil && !p.cfg.DryRun {
		if p.cfg.Gate != nil && !p.cfg.Gate.Check().Open {
			p.routed = true // may be left over from a crash; the first poll clears it
		} else {
			if err := r.Start(hardCtx); err != nil {
				return err
			}
			p.routed = true
			slog.Info("routing on", "variable", r.Var, "value", r.Value, "heartbeat", r.HeartbeatVar)
		}
		go r.Beat(ctx)
	}

	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	for {
		if err := p.tick(ctx, hardCtx); err != nil && ctx.Err() == nil {
			slog.Warn("poll failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return p.drain(hardCtx)
		case <-t.C:
		}
	}
}

func (p *Pool) tick(ctx, hardCtx context.Context) error {
	jobs, err := p.gh.QueuedJobs(ctx, p.cfg.Repo)
	if err != nil {
		return err
	}
	var mine []gh.Job
	for _, j := range jobs {
		if p.Matches(j) {
			mine = append(mine, j)
		}
	}
	if !p.admit(ctx, mine) {
		return nil
	}

	p.mu.Lock()
	active := len(p.workers)
	// A runner covers one queued job until it starts one. After that, the API
	// can keep reporting the job it took as queued for a few seconds, so for a
	// short grace period a queued job with that name stays covered too.
	queuedNames := map[string]int{}
	for _, j := range mine {
		queuedNames[j.Name]++
	}
	waiting := 0
	now := time.Now()
	for _, w := range p.workers {
		switch {
		case w.started.IsZero():
			waiting++
		case now.Sub(w.started) < 20*time.Second && queuedNames[w.job] > 0:
			queuedNames[w.job]--
			waiting++
		}
		// Reap a runner that never got work (e.g. the job was cancelled).
		if w.started.IsZero() && len(mine) == 0 && now.Sub(w.spawned) > 3*time.Minute {
			slog.Info("reaping idle runner", "runner", w.name)
			w.cancel()
		}
	}
	p.mu.Unlock()

	want := len(mine) - waiting
	room := p.cfg.Max - active
	n := min(want, room)

	if len(mine) > 0 {
		names := make([]string, len(mine))
		for i, j := range mine {
			names[i] = j.Name
		}
		slog.Debug("queued jobs for us", "count", len(mine), "jobs", names, "active", active, "spawning", max(n, 0))
	}
	if p.cfg.DryRun {
		for _, j := range mine {
			slog.Info("dry run: would take", "job", j.Name, "labels", j.Labels, "url", j.HTMLURL)
		}
		return nil
	}
	for i := 0; i < n; i++ {
		if err := p.spawn(ctx, hardCtx); err != nil {
			return err
		}
	}
	// Jobs left over after this tick's spawns wait for a free slot. Past
	// MaxWait, send them to GitHub-hosted runners instead.
	if len(mine) > waiting+max(n, 0) {
		p.rescue(ctx, Overdue(mine, p.cfg.MaxWait, now), false, "waited longer than --max-wait")
	}
	return nil
}

// Overdue is the runs with a job that has been queued longer than maxWait.
func Overdue(jobs []gh.Job, maxWait time.Duration, now time.Time) []int64 {
	if maxWait <= 0 {
		return nil
	}
	var late []gh.Job
	for _, j := range jobs {
		if !j.CreatedAt.IsZero() && now.Sub(j.CreatedAt) > maxWait {
			late = append(late, j)
		}
	}
	return runIDs(late)
}

func runIDs(jobs []gh.Job) []int64 {
	var runs []int64
	seen := map[int64]bool{}
	for _, j := range jobs {
		if !seen[j.RunID] {
			seen[j.RunID] = true
			runs = append(runs, j.RunID)
		}
	}
	return runs
}

// rescue sends runs' queued jobs to GitHub-hosted runners in the background.
func (p *Pool) rescue(ctx context.Context, runs []int64, force bool, why string) {
	r := p.cfg.Router
	// Without the routing variable a re-run would come straight back here.
	if r == nil || r.Var == "" || p.cfg.DryRun {
		return
	}
	for _, id := range runs {
		go func() {
			err := r.Rescue(ctx, id, force)
			switch {
			case errors.Is(err, route.ErrRescuing):
			case errors.Is(err, route.ErrBusy):
				slog.Debug("not rescuing run yet: a job in it is running", "run", id)
			case err != nil:
				slog.Warn("could not send run to GitHub-hosted runners", "run", id, "err", err)
			default:
				slog.Info("sent queued jobs to GitHub-hosted runners", "run", id, "why", why)
			}
		}()
	}
}

// RunnerPrefix starts the name of every runner this machine registers.
func RunnerPrefix() string {
	host, _ := os.Hostname()
	return "spareci-" + strings.Split(host, ".")[0] + "-"
}

func (p *Pool) spawn(ctx, hardCtx context.Context) error {
	p.mu.Lock()
	p.seq++
	name := fmt.Sprintf("%s%d-%d", RunnerPrefix(), time.Now().Unix(), p.seq)
	p.mu.Unlock()

	jit, err := p.gh.GenerateJIT(ctx, p.cfg.Repo, name, p.cfg.Labels)
	if err != nil {
		return fmt.Errorf("generate jit runner: %w", err)
	}
	// The runner outlives the soft ctx (in-flight jobs finish on first ^C)
	// but not the hard one.
	var env []string
	var jl *cache.JobListener
	if p.cfg.Cache != nil {
		if jl, err = p.cfg.Cache.Register(p.cfg.Repo); err != nil {
			slog.Warn("local cache unavailable for this job; it will use GitHub's", "err", err)
		} else {
			env = jl.Env()
		}
	}
	runCtx, cancel := context.WithCancel(hardCtx)
	w := &worker{name: name, spawned: time.Now(), cancel: cancel, runnerID: jit.ID}
	p.mu.Lock()
	p.workers[name] = w
	p.mu.Unlock()
	slog.Info("runner up", "runner", name)

	go func() {
		defer cancel()
		if jl != nil {
			defer jl.Close()
		}
		res, err := p.host.Run(runCtx, name, jit.Config, env, func(job string) {
			p.mu.Lock()
			w.started, w.job = time.Now(), job
			p.mu.Unlock()
		})

		p.mu.Lock()
		delete(p.workers, name)
		p.mu.Unlock()

		// A JIT runner that never took a job stays registered; clean it up.
		if res.Job == "" {
			_ = p.gh.DeleteRunner(context.Background(), p.cfg.Repo, jit.ID)
		}
		attrs := []any{"runner", name, "job", res.Job, "result", res.Conclusion, "took", time.Since(w.spawned).Round(time.Second), "log", res.Log}
		if err != nil && runCtx.Err() == nil {
			slog.Warn("runner exited with error", append(attrs, "err", err)...)
		} else {
			slog.Info("runner done", attrs...)
		}
	}()
	return nil
}

// rescueQueued sends every job still queued for this machine to
// GitHub-hosted runners, since no runner here will take it.
func (p *Pool) rescueQueued() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if p.cfg.Router.Var == "" {
		return
	}
	n, err := RescueQueued(ctx, p.gh, p.cfg.Router, p.Matches)
	if n > 0 || err != nil {
		slog.Info("sent queued jobs to GitHub-hosted runners", "runs", n, "err", err)
	}
}

// RescueQueued cancels and re-runs every run with a queued job that match
// accepts, so those jobs go to GitHub-hosted runners. Routing must be off.
func RescueQueued(ctx context.Context, client *gh.Client, r *route.Router, match func(gh.Job) bool) (int, error) {
	jobs, err := client.QueuedJobs(ctx, r.Repo)
	if err != nil {
		return 0, err
	}
	var mine []gh.Job
	for _, j := range jobs {
		if match(j) {
			mine = append(mine, j)
		}
	}
	runs := runIDs(mine)
	var wg sync.WaitGroup
	errs := make([]error, len(runs))
	for i, id := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = r.Rescue(ctx, id, true)
		}()
	}
	wg.Wait()
	return len(runs), errors.Join(errs...)
}

// drain turns routing off so new jobs go to GitHub-hosted runners, cancels
// runners that have no job, waits for the rest to finish, and then sends jobs
// still queued here to GitHub-hosted runners.
func (p *Pool) drain(hardCtx context.Context) error {
	if r := p.cfg.Router; r != nil && !p.cfg.DryRun {
		if err := r.Stop(context.Background()); err != nil {
			slog.Warn("could not clear routing; jobs will queue for this machine until `spareci down`", "variable", r.Var, "err", err)
		} else {
			slog.Info("routing off: new jobs go to GitHub-hosted runners", "variable", r.Var)
		}
		defer p.rescueQueued()
	}
	for {
		p.mu.Lock()
		busy := 0
		for _, w := range p.workers {
			if w.started.IsZero() {
				w.cancel()
			} else {
				busy++
			}
		}
		left := len(p.workers)
		p.mu.Unlock()
		if left == 0 {
			return nil
		}
		if busy > 0 {
			slog.Info("waiting for running jobs to finish (^C again to abort them)", "jobs", busy)
		}
		select {
		case <-hardCtx.Done():
			time.Sleep(2 * time.Second) // let runners deregister
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}
