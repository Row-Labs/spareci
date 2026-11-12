// Package pool watches a repo for queued jobs this machine can take and keeps
// enough single-use runners alive to drain them. It holds one backend per
// kind of runner the machine offers (on a Mac: macOS natively, Linux in
// containers) and starts each job's runner on a backend whose labels fit it.
package pool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Row-Labs/spareci/internal/cache"
	"github.com/Row-Labs/spareci/internal/gh"
	"github.com/Row-Labs/spareci/internal/route"
	"github.com/Row-Labs/spareci/internal/runner"
	"github.com/Row-Labs/spareci/internal/stats"
)

// Backend is a runner.Backend with what the pool adds to it.
type Backend struct {
	runner.Backend
	// Routing are the routing labels its runners also carry (spareci-linux,
	// ...), so jobs routed by those variables come to it.
	Routing []string
	Max     int // concurrent runners on this backend; 0: only Config.Max limits it
}

// AllLabels are every label its runners register with. A job matches the
// backend if all its runs-on labels are here.
func (b *Backend) AllLabels() []string {
	return append(append([]string{}, b.Labels()...), b.Routing...)
}

// Name says which backend ran a job, for logs and stats: linux-arm64, or
// the backend's own Name if it has one.
func (b *Backend) Name() string {
	if n, ok := b.Backend.(interface{ Name() string }); ok {
		return n.Name()
	}
	return b.OS() + "-" + b.Arch()
}

func (b *Backend) fits(j gh.Job) bool { return Matches(b.AllLabels(), j) }

type Config struct {
	Repo     string
	Backends []*Backend // in order of preference when a job fits several
	Max      int        // concurrent runners across all backends
	Interval time.Duration
	Router   *route.Router // routing variable and heartbeat; nil disables routing
	MaxWait  time.Duration // a job queued here longer than this goes to GitHub-hosted runners; 0 disables
	DryRun   bool
	Cache    *cache.Server   // nil: jobs use GitHub's cache service
	Gate     Gate            // nil: always take jobs; see gate.go
	Stats    *stats.Recorder // nil: don't record jobs
	Machine  string          // this machine's name, which starts its runners' names; "" for the host name
}

type Pool struct {
	cfg Config
	gh  *gh.Client

	mu      sync.Mutex
	workers map[string]*worker
	seq     int
	// queued is the last poll's queued jobs for this machine, and ran the
	// IDs of jobs a runner here took, when that's known (see claim).
	queued []gh.Job
	ran    map[int64]time.Time

	recording   sync.WaitGroup // jobs being written to the stats file
	running     sync.WaitGroup // runners, until they have exited and been cleaned up
	rescuing    sync.WaitGroup // runs being sent to GitHub-hosted runners
	statsCtx    context.Context
	stopRecords context.CancelFunc

	// Laptop mode (gate.go); only touched by the poll loop.
	paused, routed bool
	stranded       int
}

type worker struct {
	name     string
	spawned  time.Time
	started  time.Time // zero until the runner picks up a job
	job      string    // name of the job it took
	backend  *Backend
	cancel   context.CancelFunc
	runnerID int64
	runID    int64 // run of the job it took, once known (claim, findRuns)
}

func New(cfg Config, client *gh.Client) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	return &Pool{cfg: cfg, gh: client, workers: map[string]*worker{}, ran: map[int64]time.Time{}, statsCtx: ctx, stopRecords: cancel}
}

// ranFor is how long a job this machine ran is ignored if GitHub still lists
// it as queued. GitHub's jobs API has done that for minutes after the job
// finished (run 43099220289), which would start a runner for nothing.
const ranFor = time.Hour

// claim notes the ID of the job a runner on b just started, so that later
// polls don't count it as queued. The runner only prints the job's name,
// so the ID is taken from the last poll's queued jobs, and only when exactly
// one there has that name and fits b. When several do (the same job queued
// in two runs), nothing is claimed rather than risk ignoring a job that is
// really waiting. p.mu must be held.
func (p *Pool) claim(w *worker, job string, now time.Time) {
	var found []gh.Job
	for _, j := range p.queued {
		if j.Name == job && w.backend.fits(j) && p.ran[j.ID].IsZero() {
			found = append(found, j)
		}
	}
	if len(found) == 1 {
		p.ran[found[0].ID] = now
		w.runID = found[0].RunID
	}
}

// notRan drops jobs this machine already ran, and forgets old ones.
// p.mu must be held.
func (p *Pool) notRan(jobs []gh.Job, now time.Time) []gh.Job {
	for id, at := range p.ran {
		if now.Sub(at) > ranFor {
			delete(p.ran, id)
		}
	}
	var out []gh.Job
	for _, j := range jobs {
		if p.ran[j.ID].IsZero() {
			out = append(out, j)
		}
	}
	return out
}

// Matches reports whether some backend can take the job.
func (p *Pool) Matches(j gh.Job) bool { return len(p.candidates(j)) > 0 }

// candidates are the backends that can take j, in order of preference.
func (p *Pool) candidates(j gh.Job) []*Backend {
	var out []*Backend
	for _, b := range p.cfg.Backends {
		if b.fits(j) {
			out = append(out, b)
		}
	}
	return out
}

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
// and then until every job is done, or hardCtx is cancelled and the jobs
// still running are stopped (see drain).
func (p *Pool) Run(ctx, hardCtx context.Context) error {
	if r := p.cfg.Router; r != nil && !p.cfg.DryRun {
		if p.cfg.Gate != nil && !p.cfg.Gate.Check().Open {
			p.routed = true // may be left over from a crash; the first poll clears it
		} else {
			if err := r.Start(hardCtx); err != nil {
				return err
			}
			p.routed = true
			slog.Info("routing on", "variables", r.Vars(), "heartbeats", r.Heartbeats())
		}
		go r.Beat(ctx)
	}
	if p.cfg.Stats != nil && !p.cfg.DryRun {
		// Jobs whose stats a previous run couldn't finish before stopping.
		if n, err := p.cfg.Stats.Resume(p.statsCtx, &p.recording); err != nil {
			slog.Warn("could not read pending job stats", "err", err)
		} else if n > 0 {
			slog.Info("recording job stats left pending last time", "jobs", n)
		}
	}

	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	for {
		if err := p.tick(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("poll failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return p.drain(hardCtx)
		case <-t.C:
		}
	}
}

func (p *Pool) tick(ctx context.Context) error {
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
	mine = p.notRan(mine, time.Now())
	p.queued = mine
	p.mu.Unlock()
	if !p.admit(ctx, mine) {
		return nil
	}

	p.mu.Lock()
	plan := p.plan(mine, time.Now())
	p.mu.Unlock()

	if len(mine) > 0 {
		names := make([]string, len(mine))
		for i, j := range mine {
			names[i] = j.Name
		}
		slog.Debug("queued jobs for us", "count", len(mine), "jobs", names, "active", plan.active, "spawning", len(plan.spawn))
	}
	if p.cfg.DryRun {
		for _, j := range mine {
			var on []string
			for _, b := range p.candidates(j) {
				on = append(on, b.Name())
			}
			slog.Info("dry run: would take", "job", j.Name, "labels", j.Labels, "backends", on, "url", j.HTMLURL)
		}
		return nil
	}
	for _, b := range plan.spawn {
		if err := p.spawn(ctx, b); err != nil {
			return err
		}
	}
	// Jobs left over after this tick's spawns wait for a free slot. Past
	// MaxWait, send them to GitHub-hosted runners instead.
	if len(plan.stuck) > 0 {
		p.rescue(ctx, Overdue(plan.stuck, p.cfg.MaxWait, time.Now()), false, "waited longer than --max-wait")
	}
	return nil
}

// plan is what one poll does about the queued jobs.
type plan struct {
	spawn  []*Backend // start one runner on each
	stuck  []gh.Job   // jobs that wait for a free slot on a full backend
	active int
}

// plan matches queued jobs, oldest first, to runners: a runner that hasn't
// started a job yet covers one job its backend fits; otherwise a new runner
// starts on the first fitting backend with room under its own Max and the
// pool's. Jobs that get neither are stuck, and so is every job sharing a
// full backend with them, since GitHub may give any of them the next free
// runner. p.mu must be held.
func (p *Pool) plan(mine []gh.Job, now time.Time) plan {
	queue := append([]gh.Job(nil), mine...)
	sort.SliceStable(queue, func(a, b int) bool { return queue[a].CreatedAt.Before(queue[b].CreatedAt) })

	idle := map[*Backend]int{}
	active := map[*Backend]int{}
	for _, w := range p.workers {
		active[w.backend]++
		switch {
		case w.started.IsZero():
			idle[w.backend]++
		case now.Sub(w.started) < 20*time.Second:
			// After a runner starts a job, the API can keep reporting that job
			// as queued for a few seconds; it's covered.
			for i, j := range queue {
				if j.Name == w.job && w.backend.fits(j) {
					queue = append(queue[:i], queue[i+1:]...)
					break
				}
			}
		}
	}
	// Reap a runner that never got work (e.g. the job was cancelled).
	for _, w := range p.workers {
		if !w.started.IsZero() || now.Sub(w.spawned) <= 3*time.Minute {
			continue
		}
		wanted := false
		for _, j := range mine {
			wanted = wanted || w.backend.fits(j)
		}
		if !wanted {
			slog.Info("reaping idle runner", "runner", w.name)
			w.cancel()
		}
	}

	total := len(p.workers)
	pl := plan{active: total}
	full := map[*Backend]bool{}
	var left []gh.Job
	for _, j := range queue {
		cands := p.candidates(j)
		done := false
		for _, b := range cands {
			if idle[b] > 0 {
				idle[b]--
				done = true
				break
			}
		}
		for _, b := range cands {
			if done {
				break
			}
			if total < p.cfg.Max && (b.Max <= 0 || active[b] < b.Max) {
				pl.spawn = append(pl.spawn, b)
				active[b]++
				total++
				done = true
			}
		}
		if !done {
			left = append(left, j)
			for _, b := range cands {
				full[b] = true
			}
		}
	}
	if len(left) > 0 {
		for _, j := range mine {
			for _, b := range p.candidates(j) {
				if full[b] {
					pl.stuck = append(pl.stuck, j)
					break
				}
			}
		}
	}
	return pl
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
// A rescue isn't stopped by ctx: one cut off between cancelling a run and
// re-running it would leave the run cancelled. drain waits for them.
func (p *Pool) rescue(ctx context.Context, runs []int64, force bool, why string) {
	r := p.cfg.Router
	// Without the routing variable a re-run would come straight back here.
	if r == nil || !r.Enabled() || p.cfg.DryRun {
		return
	}
	for _, id := range runs {
		p.rescuing.Add(1)
		go func() {
			defer p.rescuing.Done()
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rescueTimeout)
			defer cancel()
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

// RunnerPrefix starts the name of every runner a machine registers.
func RunnerPrefix(machine string) string {
	if machine == "" {
		host, _ := os.Hostname()
		machine = strings.Split(host, ".")[0]
	}
	return "spareci-" + machine + "-"
}

func (p *Pool) spawn(ctx context.Context, b *Backend) error {
	p.mu.Lock()
	p.seq++
	name := fmt.Sprintf("%s%d-%d", RunnerPrefix(p.cfg.Machine), time.Now().Unix(), p.seq)
	p.mu.Unlock()

	jit, err := p.gh.GenerateJIT(ctx, p.cfg.Repo, name, b.AllLabels())
	if err != nil {
		return fmt.Errorf("generate jit runner: %w", err)
	}
	// The runner outlives ctx: in-flight jobs finish on the first ^C, and
	// drain stops them itself when it has to (abort).
	var env []string
	var jl *cache.JobListener
	if p.cfg.Cache != nil {
		if jl, err = p.cfg.Cache.Register(p.cfg.Repo); err != nil {
			slog.Warn("local cache unavailable for this job; it will use GitHub's", "err", err)
		} else {
			env = jl.Env()
		}
	}
	runCtx, cancel := context.WithCancel(context.Background())
	w := &worker{name: name, spawned: time.Now(), cancel: cancel, runnerID: jit.ID, backend: b}
	p.mu.Lock()
	p.workers[name] = w
	p.running.Add(1)
	p.mu.Unlock()
	slog.Info("runner up", "runner", name, "backend", b.Name())

	go func() {
		defer p.running.Done()
		defer cancel()
		if jl != nil {
			defer jl.Close()
		}
		res, err := b.Run(runCtx, name, jit.Config, env, func(job string) {
			now := time.Now()
			p.mu.Lock()
			w.started, w.job = now, job
			p.claim(w, job, now)
			p.mu.Unlock()
			if p.cfg.Stats != nil {
				p.cfg.Stats.Started(name, now)
			}
		})

		p.mu.Lock()
		delete(p.workers, name)
		started := w.started
		p.mu.Unlock()
		if res.Job != "" && p.cfg.Stats != nil {
			p.record(stats.Finished{Runner: name, Backend: b.Name(), Job: res.Job, Conclusion: res.Conclusion, Started: started, Ended: time.Now()})
		}

		// A JIT runner that never took a job stays registered; clean it up.
		if res.Job == "" {
			_ = p.gh.DeleteRunner(context.Background(), p.cfg.Repo, jit.ID)
		}
		attrs := []any{"runner", name, "backend", b.Name(), "job", res.Job, "result", res.Conclusion, "took", time.Since(w.spawned).Round(time.Second), "log", res.Log}
		if err != nil && runCtx.Err() == nil {
			slog.Warn("runner exited with error", append(attrs, "err", err)...)
		} else {
			slog.Info("runner done", attrs...)
		}
	}()
	return nil
}

// record adds a finished job to the stats file in the background. It asks
// GitHub about the job, which can take minutes after the job ends.
func (p *Pool) record(f stats.Finished) {
	p.recording.Add(1)
	go func() {
		defer p.recording.Done()
		if _, err := p.cfg.Stats.Done(p.statsCtx, f); err != nil && p.statsCtx.Err() == nil {
			slog.Warn("could not record job stats", "job", f.Job, "err", err)
		}
	}()
}

// stopRecording stops looking jobs up. Those not recorded yet stay in the
// stats pending file, and the next `spareci up` finishes them, so stopping
// never waits on GitHub.
func (p *Pool) stopRecording() {
	p.stopRecords()
	done := make(chan struct{})
	go func() { p.recording.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	if p.cfg.Stats != nil {
		if n := p.cfg.Stats.Pending(); n > 0 {
			slog.Info("job stats left pending; the next spareci up records them", "jobs", n)
		}
	}
}

// Stopping, after the drain timeout or a second ^C, takes at most
// StopAfterDrain: up to StopRunners for runners to stop and report their jobs
// to GitHub, then the rest for sending those jobs' runs, and jobs still queued
// here, to GitHub-hosted runners. A service manager's stop timeout is the
// drain timeout plus StopAfterDrain, so it never kills spareci mid-way.
const (
	StopAfterDrain = 5 * time.Minute
	StopRunners    = 75 * time.Second // a container gets 30s after SIGINT (docker stop --time), plus docker's own
	rescueTimeout  = 3*time.Minute + 15*time.Second
)

// drainPoll is how often drain checks on running jobs; a variable for tests.
var drainPoll = 5 * time.Second

// rescuable reports whether a queued job is one this machine should send
// back to GitHub: routed here, and not one a runner here already ran (GitHub
// can list a finished job as queued for minutes, and re-running its run
// would run its jobs again).
func (p *Pool) rescuable(j gh.Job) bool {
	p.mu.Lock()
	ran := !p.ran[j.ID].IsZero()
	p.mu.Unlock()
	return !ran && p.Matches(j) && p.cfg.Router.Routed(j)
}

// rescueQueued sends every job still queued for this machine to
// GitHub-hosted runners, since no runner here will take it.
func (p *Pool) rescueQueued(ctx context.Context) {
	r := p.cfg.Router
	if !r.Enabled() {
		return
	}
	n, err := RescueQueued(ctx, p.gh, r, p.rescuable)
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
	return RescueRuns(ctx, r, runIDs(mine))
}

// RescueRuns force-rescues runs in parallel (route.Router.Rescue).
func RescueRuns(ctx context.Context, r *route.Router, runs []int64) (int, error) {
	var wg sync.WaitGroup
	errs := make([]error, len(runs))
	for i, id := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// ErrRescuing: a rescue started earlier is finishing it.
			if err := r.Rescue(ctx, id, true); err != nil && !errors.Is(err, route.ErrRescuing) {
				errs[i] = fmt.Errorf("run %d: %w", id, err)
			}
		}()
	}
	wg.Wait()
	return len(runs), errors.Join(errs...)
}

// drain stops spareci without leaving a job stranded on GitHub:
//
//  1. Routing goes off, so new runs go to GitHub-hosted runners, and runners
//     that haven't taken a job are stopped.
//  2. Runs with a job queued here are sent back to GitHub straight away,
//     unless a job in the run is in progress: cancelling would throw that
//     work away, so they're retried each poll and sent back when it ends.
//  3. Running jobs finish, until hardCtx is cancelled (the drain timeout or
//     a second ^C). Then abort stops their runners gracefully, which reports
//     each job to GitHub as finished at once; killed runners would leave
//     their jobs in progress for minutes.
//  4. The runs of aborted jobs, and jobs still queued here, are re-run on
//     GitHub-hosted runners.
func (p *Pool) drain(hardCtx context.Context) error {
	defer p.stopRecording()
	r := p.cfg.Router
	routed := r != nil && r.Enabled() && !p.cfg.DryRun
	if r != nil && !p.cfg.DryRun {
		if err := r.Stop(context.Background()); err != nil {
			slog.Warn("could not clear routing; jobs will queue for this machine until `spareci down`", "variables", r.Vars(), "err", err)
		} else {
			slog.Info("routing off: new jobs go to GitHub-hosted runners", "variables", r.Vars())
		}
	}
	var aborted []int64
	waiting := 0
	for {
		busy, left := p.stopIdle()
		elsewhere := 0
		if routed {
			elsewhere = p.sendBack()
		}
		if left == 0 && elsewhere == 0 {
			break
		}
		if hardCtx.Err() != nil {
			aborted = p.abort()
			break
		}
		if busy > 0 {
			slog.Info("waiting for running jobs to finish (^C again to stop them)", "jobs", busy)
		} else if elsewhere != waiting {
			// Cancelling is per run: sending a queued job back now would
			// cancel a job running on another machine or on GitHub.
			slog.Info("waiting for jobs running elsewhere in runs with a job queued here; those go back to GitHub when they finish (^C again to cancel them now)", "runs", elsewhere)
		}
		waiting = elsewhere
		select {
		case <-hardCtx.Done():
		case <-time.After(drainPoll):
		}
	}

	if !routed {
		p.waitRescues(time.Minute)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), rescueTimeout)
	defer cancel()
	if len(aborted) > 0 {
		n, err := RescueRuns(ctx, r, aborted)
		if err != nil {
			slog.Warn("could not re-run every stopped job's run on GitHub-hosted runners", "runs", n, "err", err)
		} else {
			slog.Info("re-ran stopped jobs' runs on GitHub-hosted runners", "runs", n)
		}
	}
	p.waitRescues(time.Until(deadline(ctx)))
	p.rescueQueued(ctx)
	return nil
}

func deadline(ctx context.Context) time.Time {
	d, _ := ctx.Deadline()
	return d
}

// stopIdle stops runners that haven't taken a job, and counts the runners
// left and those of them running a job.
func (p *Pool) stopIdle() (busy, left int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, w := range p.workers {
		if w.started.IsZero() {
			w.cancel()
		} else {
			busy++
		}
	}
	return busy, len(p.workers)
}

// sendBack starts sending runs with a job queued here to GitHub-hosted
// runners, leaving runs with a job in progress until it ends. It returns
// how many runs it left for a job in progress on a runner that isn't this
// pool's: another machine's, or GitHub's.
func (p *Pool) sendBack() int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	jobs, err := p.gh.QueuedJobs(ctx, p.cfg.Repo)
	if err != nil {
		slog.Debug("could not list queued jobs", "err", err)
		return 0
	}
	var mine []gh.Job
	for _, j := range jobs {
		if p.rescuable(j) {
			mine = append(mine, j)
		}
	}
	runs := runIDs(mine)
	elsewhere := 0
	for _, id := range runs {
		if p.busyElsewhere(ctx, id) {
			elsewhere++
		}
	}
	p.rescue(ctx, runs, false, "spareci is stopping")
	return elsewhere
}

// busyElsewhere reports whether a run has a job in progress on a runner
// that isn't one of this pool's.
func (p *Pool) busyElsewhere(ctx context.Context, runID int64) bool {
	jobs, err := p.gh.RunJobs(ctx, p.cfg.Repo, runID)
	if err != nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, j := range jobs {
		if j.Status == "in_progress" && p.workers[j.RunnerName] == nil {
			return true
		}
	}
	return false
}

func (p *Pool) waitRescues(limit time.Duration) {
	done := make(chan struct{})
	go func() { p.rescuing.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
		slog.Warn("stopping with runs still being sent to GitHub-hosted runners; `spareci down` finishes them")
	}
}

// abort stops every runner and waits up to StopRunners for them to exit. A
// runner stopped with SIGINT cancels its job and reports it before exiting,
// so GitHub marks the job completed at once. It returns the runs of the
// jobs it stopped.
func (p *Pool) abort() []int64 {
	p.mu.Lock()
	var busy []*worker
	for _, w := range p.workers {
		if !w.started.IsZero() {
			busy = append(busy, w)
		}
	}
	p.mu.Unlock()
	slog.Info("stopping running jobs; their runs will be re-run on GitHub-hosted runners", "jobs", len(busy))
	p.findRuns(busy)

	p.mu.Lock()
	var runs []int64
	seen := map[int64]bool{}
	for _, w := range busy {
		if w.runID != 0 && !seen[w.runID] {
			seen[w.runID] = true
			runs = append(runs, w.runID)
		}
	}
	for _, w := range p.workers {
		w.cancel()
	}
	p.mu.Unlock()

	done := make(chan struct{})
	go func() { p.running.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(StopRunners):
		slog.Warn("runners still running after being stopped", "waited", StopRunners)
	}
	return runs
}

// findRuns looks up the run of each busy worker whose run isn't known yet,
// by the runner name GitHub lists on its job.
func (p *Pool) findRuns(busy []*worker) {
	want := map[string]*worker{}
	p.mu.Lock()
	for _, w := range busy {
		if w.runID == 0 {
			want[w.name] = w
		}
	}
	p.mu.Unlock()
	if len(want) == 0 || p.gh == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runs, err := p.gh.ListRuns(ctx, p.cfg.Repo, "in_progress")
	if err != nil {
		slog.Warn("could not find the runs of running jobs", "err", err)
		return
	}
	for _, run := range runs {
		jobs, err := p.gh.RunJobs(ctx, p.cfg.Repo, run.ID)
		if err != nil {
			slog.Warn("could not find the runs of running jobs", "err", err)
			return
		}
		p.mu.Lock()
		for _, j := range jobs {
			if w := want[j.RunnerName]; w != nil {
				w.runID = j.RunID
				delete(want, j.RunnerName)
			}
		}
		n := len(want)
		p.mu.Unlock()
		if n == 0 {
			return
		}
	}
}
