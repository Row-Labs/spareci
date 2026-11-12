// Package route owns the repo variables that send jobs to this machine, and
// sends jobs back to GitHub-hosted runners when this machine can't take them.
//
// Routing works through `runs-on: ${{ vars.SPARECI_RUNS_ON_MACOS || 'macos-15' }}`,
// one variable per kind of job (see names.go).
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

// Route is one routing variable and the label it's set to while up.
type Route struct {
	Var   string // e.g. SPARECI_RUNS_ON_LINUX
	Label string // e.g. spareci-linux
}

type Router struct {
	API          API
	Repo         string
	Routes       []Route // every variable this machine sets; none disables routing
	HeartbeatVar string  // e.g. SPARECI_HEARTBEAT; "" disables the heartbeat

	Poll       time.Duration // how often a rescue checks progress; default 3s
	CancelWait time.Duration // how long a cancel may take before force-cancelling; default 1m
	RerunWait  time.Duration // how long to retry a re-run GitHub refuses as "already running"; default 3m

	mu       sync.Mutex // held while the routing variable is being changed
	on       bool       // routing is wanted (between Start and Stop)
	inflight map[int64]bool
}

// Start turns routing on and writes the first heartbeat.
func (r *Router) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on = true
	if len(r.Routes) == 0 {
		return nil
	}
	if err := r.setLocked(ctx); err != nil {
		return err
	}
	return r.beatLocked(ctx, time.Now())
}

// Enabled reports whether routing is configured at all.
func (r *Router) Enabled() bool { return len(r.Routes) > 0 }

// Vars names the routing variables, for logs.
func (r *Router) Vars() []string {
	var v []string
	for _, rt := range r.Routes {
		v = append(v, rt.Var)
	}
	return v
}

// Labels are the routing labels. A job carrying one was routed here.
func (r *Router) Labels() []string {
	var l []string
	for _, rt := range r.Routes {
		l = append(l, rt.Label)
	}
	return l
}

// Routed reports whether a job's runs-on came from one of our variables.
func (r *Router) Routed(j gh.Job) bool {
	for _, l := range j.Labels {
		for _, rt := range r.Routes {
			if strings.EqualFold(l, rt.Label) {
				return true
			}
		}
	}
	return false
}

func (r *Router) setLocked(ctx context.Context) error {
	for _, rt := range r.Routes {
		if err := r.API.SetVariable(ctx, r.Repo, rt.Var, rt.Label); err != nil {
			return fmt.Errorf("set routing variable %s: %w", rt.Var, err)
		}
	}
	return nil
}

func (r *Router) clearLocked(ctx context.Context) error {
	var err error
	for _, rt := range r.Routes {
		if e := r.API.DeleteVariable(ctx, r.Repo, rt.Var); e != nil {
			err = errors.Join(err, fmt.Errorf("clear routing variable %s: %w", rt.Var, e))
		}
	}
	return err
}

// Beat refreshes the heartbeat until ctx ends. It also re-asserts the routing
// variable, which the watchdog clears if this machine looked dead (say, while
// a laptop slept).
func (r *Router) Beat(ctx context.Context) {
	if len(r.Routes) == 0 {
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
				err := r.setLocked(ctx)
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
	if len(r.Routes) == 0 {
		return nil
	}
	err := r.clearLocked(ctx)
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
	if err := r.cancel(ctx, runID); err != nil {
		return err
	}

	// The re-run must be created while every routing variable is cleared: it
	// reads all of them again, and a run can hold jobs of several kinds. Hold
	// the lock so the heartbeat doesn't set them back in between. Runs created
	// in this short window also go to GitHub, which is the safe direction.
	r.mu.Lock()
	defer r.mu.Unlock()
	cleared := false
	if r.on && len(r.Routes) > 0 {
		cleared = true
		if err := r.clearLocked(ctx); err != nil {
			if serr := r.setLocked(context.WithoutCancel(ctx)); serr != nil {
				err = errors.Join(err, fmt.Errorf("restore routing: %w", serr))
			}
			return fmt.Errorf("clear routing for re-run: %w", err)
		}
	}
	if whole {
		err = r.rerun(ctx, runID, r.API.RerunRun)
	} else {
		err = r.rerun(ctx, runID, r.API.RerunFailedJobs)
	}
	var next gh.Run
	if err == nil {
		next, err = r.waitAttempt(ctx, runID, run.RunAttempt)
	}
	if err == nil && !whole {
		// GitHub has left a cancelled job out of a failed-jobs re-run (runs
		// 42889578581 and 42891078116: of two queued jobs cancelled together,
		// only one was re-run), which ends the run as cancelled. If that
		// happens, cancel the new attempt and re-run everything.
		if left := r.leftBehind(ctx, runID, next.RunStartedAt); left != "" {
			slog.Warn("re-run skipped a cancelled job; re-running the whole run", "run", runID, "job", left)
			err = r.cancel(ctx, runID)
			if err == nil {
				err = r.rerun(ctx, runID, r.API.RerunRun)
			}
			if err == nil {
				_, err = r.waitAttempt(ctx, runID, next.RunAttempt)
			}
		}
	}
	if cleared {
		if serr := r.setLocked(context.WithoutCancel(ctx)); serr != nil {
			err = errors.Join(err, fmt.Errorf("restore routing: %w", serr))
		}
	}
	if err != nil {
		return fmt.Errorf("re-run: %w", err)
	}
	return nil
}

// rerun calls a re-run endpoint. GitHub refuses a re-run with "This
// workflow is already running" (403) until every job of the cancelled
// attempt has completed, which can lag the run's own status, and for longer
// when a job's runner went away. So on that refusal it waits for the run to
// complete and tries again, for up to RerunWait.
func (r *Router) rerun(ctx context.Context, runID int64, fn func(context.Context, string, int64) error) error {
	limit := r.RerunWait
	if limit == 0 {
		limit = 3 * time.Minute
	}
	deadline := time.Now().Add(limit)
	for {
		err := fn(ctx, r.Repo, runID)
		if !AlreadyRunning(err) || time.Now().After(deadline) {
			return err
		}
		slog.Debug("re-run refused while the run is still running; waiting", "run", runID)
		if werr := r.waitCompleted(ctx, runID, time.Until(deadline)); werr != nil {
			return errors.Join(err, werr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.poll()):
		}
	}
}

// AlreadyRunning reports whether GitHub refused a re-run because the run
// hasn't finished.
func AlreadyRunning(err error) bool {
	var ae *gh.APIError
	return errors.As(err, &ae) && ae.Status == 403 && strings.Contains(strings.ToLower(ae.Body), "already running")
}

// ranHere reports whether a job was routed to this machine.
func (r *Router) ranHere(j gh.Job) bool { return r.Routed(j) }

// cancel cancels a run and waits for it to complete, force-cancelling it if
// a normal cancel doesn't finish in time.
func (r *Router) cancel(ctx context.Context, runID int64) error {
	run, err := r.API.GetRun(ctx, r.Repo, runID)
	if err != nil {
		return err
	}
	if run.Status == "completed" {
		return nil
	}
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
		// That can take minutes to settle too (Row-Labs/relay, 2026-11-11).
		return r.waitCompleted(ctx, runID, 3*time.Minute)
	}
	return nil
}

// leftBehind names a cancelled job that the attempt starting at started
// didn't re-run. GitHub lists such a job under the new attempt number, but
// it finished before the attempt began.
func (r *Router) leftBehind(ctx context.Context, runID int64, started time.Time) string {
	if started.IsZero() {
		return ""
	}
	left := ""
	for range 3 {
		jobs, err := r.API.RunJobs(ctx, r.Repo, runID)
		if err != nil {
			return ""
		}
		left = ""
		for _, j := range jobs {
			if j.Status == "completed" && j.Conclusion == "cancelled" && j.CompletedAt.Before(started) {
				left = j.Name
			}
		}
		if left == "" {
			return ""
		}
		select {
		case <-ctx.Done():
			return left
		case <-time.After(r.poll()):
		}
	}
	return left
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

// waitAttempt returns the run once the re-run's attempt exists, which is
// when GitHub has read the repo variables for it.
func (r *Router) waitAttempt(ctx context.Context, runID int64, prev int) (gh.Run, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		run, err := r.API.GetRun(ctx, r.Repo, runID)
		if err == nil && run.RunAttempt > prev {
			return run, nil
		}
		if time.Now().After(deadline) {
			return gh.Run{}, fmt.Errorf("run %d: no new attempt after re-run", runID)
		}
		select {
		case <-ctx.Done():
			return gh.Run{}, ctx.Err()
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
