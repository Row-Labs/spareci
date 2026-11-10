// Package route owns the repo variables that send jobs to this machine, and
// sends jobs back to GitHub-hosted runners when this machine can't take them.
//
// Routing works through `runs-on: ${{ vars.SPARECI_RUNS_ON || 'macos-15' }}`.
// GitHub reads repo variables once per run attempt, when the attempt is
// created, so:
//   - Clearing the variable affects runs created afterwards, not jobs already
//     queued here.
//   - A queued job is sent back by cancelling its run and re-running the
//     failed (cancelled) jobs while the variable is cleared: the new attempt
//     reads the variable again and falls back to the hosted label.
package route

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Row-Labs/spareci/internal/gh"
)

// API is the slice of the GitHub client routing needs.
type API interface {
	SetVariable(ctx context.Context, repo, name, value string) error
	DeleteVariable(ctx context.Context, repo, name string) error
	RunJobs(ctx context.Context, repo string, runID int64) ([]gh.Job, error)
	GetRun(ctx context.Context, repo string, runID int64) (gh.Run, error)
	CancelRun(ctx context.Context, repo string, runID int64) error
	ForceCancelRun(ctx context.Context, repo string, runID int64) error
	RerunFailedJobs(ctx context.Context, repo string, runID int64) error
	RerunRun(ctx context.Context, repo string, runID int64) error
}

// HeartbeatEvery is how often a running daemon refreshes its heartbeat.
const HeartbeatEvery = time.Minute

type Router struct {
	API          API
	Repo         string
	Var          string // routing variable, e.g. SPARECI_RUNS_ON; "" disables routing
	Value        string // its value while up: the routing label
	HeartbeatVar string // e.g. SPARECI_HEARTBEAT; "" disables the heartbeat

	Poll       time.Duration // how often a rescue checks progress; default 3s
	CancelWait time.Duration // how long a cancel may take before force-cancelling; default 1m

	mu       sync.Mutex // held while the routing variable is being changed
	on       bool       // routing is wanted (between Start and Stop)
	inflight map[int64]bool
}

// Start turns routing on and writes the first heartbeat.
func (r *Router) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on = true
	if r.Var == "" {
		return nil
	}
	if err := r.API.SetVariable(ctx, r.Repo, r.Var, r.Value); err != nil {
		return fmt.Errorf("set routing variable %s: %w", r.Var, err)
	}
	return r.beatLocked(ctx, time.Now())
}

// Beat refreshes the heartbeat until ctx ends. It also re-asserts the routing
// variable, which the watchdog clears if this machine looked dead (say, while
// a laptop slept).
func (r *Router) Beat(ctx context.Context) {
	if r.Var == "" {
		return
	}
	t := time.NewTicker(HeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			r.mu.Lock()
			if r.on {
				err := r.API.SetVariable(ctx, r.Repo, r.Var, r.Value)
				if err == nil {
					err = r.beatLocked(ctx, now)
				}
				if err != nil && ctx.Err() == nil {
					slog.Warn("heartbeat failed", "err", err)
				}
			}
			r.mu.Unlock()
		}
	}
}

func (r *Router) beatLocked(ctx context.Context, now time.Time) error {
	if r.HeartbeatVar == "" {
		return nil
	}
	return r.API.SetVariable(ctx, r.Repo, r.HeartbeatVar, Lease(now))
}

// Stop turns routing off: runs created from now on go to GitHub-hosted runners.
func (r *Router) Stop(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on = false
	if r.Var == "" {
		return nil
	}
	err := r.API.DeleteVariable(ctx, r.Repo, r.Var)
	if r.HeartbeatVar != "" {
		err = errors.Join(err, r.API.DeleteVariable(ctx, r.Repo, r.HeartbeatVar))
	}
	return err
}

// ErrBusy means the run has a job in progress, so cancelling it would throw
// that job's work away. Try again later.
var ErrBusy = errors.New("run has a job in progress")

// ErrRescuing means another rescue of the run is under way.
var ErrRescuing = errors.New("run is already being rescued")

// Rescue sends a run's queued jobs to GitHub-hosted runners: it cancels the
// run and re-runs it with routing cleared, then restores routing if it was on.
//
// Usually only the failed and cancelled jobs are re-run. If a job in the run
// already finished on this machine, the whole run is re-run on GitHub-hosted
// runners instead: caches that job saved went to this machine's local cache,
// and the hosted re-run of a later job couldn't restore them.
//
// Unless force is set, a run with any job in progress is left alone
// (ErrBusy): GitHub can only cancel whole runs. A run already being rescued
// is skipped (ErrRescuing).
func (r *Router) Rescue(ctx context.Context, runID int64, force bool) error {
	r.mu.Lock()
	if r.inflight == nil {
		r.inflight = map[int64]bool{}
	}
	if r.inflight[runID] {
		r.mu.Unlock()
		return ErrRescuing
	}
	r.inflight[runID] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.inflight, runID)
		r.mu.Unlock()
	}()

	jobs, err := r.API.RunJobs(ctx, r.Repo, runID)
	if err != nil {
		return err
	}
	whole := false
	for _, j := range jobs {
		if j.Status == "in_progress" && !force {
			return ErrBusy
		}
		if j.Status == "completed" && j.Conclusion != "cancelled" && j.Conclusion != "skipped" && r.ranHere(j) {
			whole = true
		}
	}

	run, err := r.API.GetRun(ctx, r.Repo, runID)
	if err != nil {
		return err
	}
	if run.Status != "completed" {
		if err := r.API.CancelRun(ctx, r.Repo, runID); err != nil && !gh.IsStatus(err, 409) {
			return fmt.Errorf("cancel run: %w", err)
		}
		wait := r.CancelWait
		if wait == 0 {
			wait = time.Minute
		}
		if err := r.waitCompleted(ctx, runID, wait); err != nil {
			// A job held by a runner that went away can block a normal cancel.
			if err := r.API.ForceCancelRun(ctx, r.Repo, runID); err != nil && !gh.IsStatus(err, 409) {
				return fmt.Errorf("force-cancel run: %w", err)
			}
			if err := r.waitCompleted(ctx, runID, time.Minute); err != nil {
				return err
			}
		}
	}

	// The re-run must be created while the routing variable is cleared. Hold
	// the lock so the heartbeat doesn't set it back in between. Runs created
	// in this short window also go to GitHub, which is the safe direction.
	r.mu.Lock()
	defer r.mu.Unlock()
	cleared := false
	if r.on && r.Var != "" {
		if err := r.API.DeleteVariable(ctx, r.Repo, r.Var); err != nil {
			return fmt.Errorf("clear routing for re-run: %w", err)
		}
		cleared = true
	}
	if whole {
		err = r.API.RerunRun(ctx, r.Repo, runID)
	} else {
		err = r.API.RerunFailedJobs(ctx, r.Repo, runID)
	}
	if err == nil {
		err = r.waitAttempt(ctx, runID, run.RunAttempt)
	}
	if cleared {
		if serr := r.API.SetVariable(context.WithoutCancel(ctx), r.Repo, r.Var, r.Value); serr != nil {
			err = errors.Join(err, fmt.Errorf("restore routing: %w", serr))
		}
	}
	if err != nil {
		return fmt.Errorf("re-run: %w", err)
	}
	return nil
}

// ranHere reports whether a job was routed to this machine.
func (r *Router) ranHere(j gh.Job) bool {
	for _, l := range j.Labels {
		if r.Value != "" && strings.EqualFold(l, r.Value) {
			return true
		}
	}
	return false
}

func (r *Router) poll() time.Duration {
	if r.Poll > 0 {
		return r.Poll
	}
	return 3 * time.Second
}

func (r *Router) waitCompleted(ctx context.Context, runID int64, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		run, err := r.API.GetRun(ctx, r.Repo, runID)
		if err == nil && run.Status == "completed" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("run %d still %q after cancelling", runID, run.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.poll()):
		}
	}
}

// waitAttempt returns once the re-run's attempt exists, which is when GitHub
// has read the repo variables for it.
func (r *Router) waitAttempt(ctx context.Context, runID int64, prev int) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		run, err := r.API.GetRun(ctx, r.Repo, runID)
		if err == nil && run.RunAttempt > prev {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("run %d: no new attempt after re-run", runID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.poll()):
		}
	}
}

// The heartbeat doubles as a lease for the watchdog workflow (see Watchdog).
// The workflow fires on fixed minutes of the hour; the heartbeat lists the
// ones whose firing falls between LeaseBehind ago and LeaseAhead from now.
// A firing listed in the heartbeat is skipped, which costs nothing. While the
// daemon is alive, every firing is listed unless GitHub delays it by more than
// LeaseBehind. Once it dies, the list stops moving and a firing is unlisted at
// most LeaseAhead plus one slot later.
const (
	SlotEvery   = 5 // minutes between watchdog firings
	SlotOffset  = 2 // first firing minute; off the top of the hour, when GitHub's scheduler is busiest
	LeaseAhead  = 10 * time.Minute
	LeaseBehind = 30 * time.Minute
	// StaleAfter is how old a heartbeat must be before the watchdog acts.
	StaleAfter = 5 * time.Minute
)

// Slots are the watchdog's firing minutes.
func Slots() []int {
	var s []int
	for m := SlotOffset; m < 60; m += SlotEvery {
		s = append(s, m)
	}
	return s
}

// Cron is a slot's schedule, exactly as the workflow file writes it; GitHub
// passes this string back as github.event.schedule.
func Cron(minute int) string { return fmt.Sprintf("%d * * * *", minute) }

// Lease is the heartbeat value at now: "<unix seconds>|<cron>|<cron>|...|".
func Lease(now time.Time) string {
	now = now.UTC()
	var b strings.Builder
	b.WriteString(strconv.FormatInt(now.Unix(), 10))
	b.WriteString("|")
	hour := now.Truncate(time.Hour)
	for _, m := range Slots() {
		for _, h := range []time.Duration{-time.Hour, 0, time.Hour} {
			t := hour.Add(h + time.Duration(m)*time.Minute)
			if !t.Before(now.Add(-LeaseBehind)) && !t.After(now.Add(LeaseAhead)) {
				b.WriteString(Cron(m))
				b.WriteString("|")
				break
			}
		}
	}
	return b.String()
}
