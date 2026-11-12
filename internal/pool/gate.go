package pool

import (
	"context"
	"log/slog"

	"github.com/Row-Labs/spareci/internal/gate"
	"github.com/Row-Labs/spareci/internal/gh"
)

// Gate decides whether the machine takes new jobs right now (laptop mode:
// on AC, user idle, inside a time window). *gate.Gate implements it.
type Gate interface {
	Check() gate.State
}

// admit is checked once per poll, before spawning. mine is the jobs queued
// for this machine.
//
// While the gate is closed, the pool works as if it were shut down, except
// that it keeps polling:
//   - routing is off (route.Router.Stop), so new runs go to GitHub-hosted
//     runners, and the heartbeat is deleted with it, so the watchdog stays
//     out of the way exactly as it does after ^C;
//   - runners that haven't picked up a job are stopped, and none start;
//   - running jobs carry on;
//   - jobs already queued here are sent back like at shutdown, except that a
//     run with a job in progress is left until that job ends, so pausing
//     never throws away work.
//
// When the gate opens, routing and the heartbeat come back (Router.Start).
func (p *Pool) admit(ctx context.Context, mine []gh.Job) bool {
	if p.cfg.Gate == nil {
		return true
	}
	s := p.cfg.Gate.Check()
	if s.Open == p.paused {
		p.paused = !s.Open
		if s.Open {
			slog.Info("laptop mode: conditions met, taking jobs again")
		} else {
			slog.Info("laptop mode: paused; new jobs go to GitHub-hosted runners, running jobs will finish", "reason", s.Reason)
			p.cancelIdle()
		}
		p.stranded = 0
	}
	p.syncRouting(ctx, s.Open)
	if s.Open {
		return true
	}
	if len(mine) != p.stranded {
		p.stranded = len(mine)
		if len(mine) > 0 {
			slog.Info("laptop mode: sending jobs queued for this machine to GitHub-hosted runners", "jobs", len(mine))
		}
	}
	// Only once routing is really off: a re-run made while it's on would come
	// straight back here.
	if !p.routed && len(mine) > 0 {
		p.rescue(ctx, runIDs(mine), false, "laptop mode paused")
	}
	return false
}

// syncRouting turns routing on or off to match want. A failure is retried
// on the next poll.
func (p *Pool) syncRouting(ctx context.Context, want bool) {
	r := p.cfg.Router
	if r == nil || p.cfg.DryRun || p.routed == want {
		return
	}
	var err error
	if want {
		err = r.Start(ctx)
	} else {
		err = r.Stop(ctx)
	}
	if err != nil {
		slog.Warn("laptop mode: could not update routing; retrying", "variables", r.Vars(), "on", want, "err", err)
		return
	}
	p.routed = want
	if want {
		slog.Info("routing on", "variables", r.Vars(), "heartbeats", r.Heartbeats())
	} else {
		slog.Info("routing off: new jobs go to GitHub-hosted runners", "variables", r.Vars())
	}
}

// cancelIdle stops runners that are registered but haven't taken a job, so
// GitHub can't hand them one.
func (p *Pool) cancelIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, w := range p.workers {
		if w.started.IsZero() {
			slog.Info("laptop mode: stopping idle runner", "runner", w.name)
			w.cancel()
		}
	}
}
