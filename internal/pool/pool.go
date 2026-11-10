// Package pool watches a repo for queued jobs this machine can take and keeps
// enough single-use runners alive to drain them.
package pool

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Row-Labs/spareci/internal/cache"
	"github.com/Row-Labs/spareci/internal/gh"
	"github.com/Row-Labs/spareci/internal/runner"
)

type Config struct {
	Repo        string
	Labels      []string // every label the runners carry; a job matches if all its labels are here
	Max         int      // concurrent runners
	Interval    time.Duration
	FallbackVar string // repo variable set to Labels' routing label while up; "" disables
	RouteLabel  string // value written to FallbackVar
	DryRun      bool
	Cache       *cache.Server // nil: jobs use GitHub's cache service
}

type Pool struct {
	cfg  Config
	gh   *gh.Client
	host *runner.Host

	mu      sync.Mutex
	workers map[string]*worker
	seq     int
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
// Runner label matching on GitHub is case-insensitive.
func (p *Pool) Matches(j gh.Job) bool {
	if len(j.Labels) == 0 {
		return false
	}
	have := map[string]bool{}
	for _, l := range p.cfg.Labels {
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
	if p.cfg.FallbackVar != "" && !p.cfg.DryRun {
		if err := p.gh.SetVariable(hardCtx, p.cfg.Repo, p.cfg.FallbackVar, p.cfg.RouteLabel); err != nil {
			return fmt.Errorf("set fallback variable %s: %w", p.cfg.FallbackVar, err)
		}
		slog.Info("routing on", "variable", p.cfg.FallbackVar, "value", p.cfg.RouteLabel)
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
	return nil
}

func (p *Pool) spawn(ctx, hardCtx context.Context) error {
	host, _ := os.Hostname()
	host = strings.Split(host, ".")[0]
	p.mu.Lock()
	p.seq++
	name := fmt.Sprintf("spareci-%s-%d-%d", host, time.Now().Unix(), p.seq)
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

// drain turns routing off so new jobs go to GitHub-hosted runners, cancels
// runners that have no job, and waits for the rest to finish.
func (p *Pool) drain(hardCtx context.Context) error {
	if p.cfg.FallbackVar != "" && !p.cfg.DryRun {
		if err := p.gh.DeleteVariable(context.Background(), p.cfg.Repo, p.cfg.FallbackVar); err != nil {
			slog.Warn("could not clear fallback variable; jobs will queue for this machine", "variable", p.cfg.FallbackVar, "err", err)
		} else {
			slog.Info("routing off: new jobs go to GitHub-hosted runners", "variable", p.cfg.FallbackVar)
		}
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
