package pool

import (
	"context"
	"log/slog"

	"github.com/Row-Labs/spareci/internal/gate"
)

// Gate decides whether the machine takes new jobs right now (laptop mode:
// on AC, user idle, inside a time window). *gate.Gate implements it.
type Gate interface {
	Check() gate.State
}

// admit is checked once per poll, before spawning. While the gate is closed
// the pool routes new jobs back to GitHub, cancels runners that haven't
// picked up a job, and starts none; running jobs carry on. When it opens
// again, routing comes back here. queued is how many matching jobs are
// waiting, for the log.
func (p *Pool) admit(ctx context.Context, queued int) bool {
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
	if !s.Open && queued != p.stranded {
		p.stranded = queued
		if queued > 0 {
			slog.Info("laptop mode: jobs queued for this machine before it paused will wait until it resumes", "jobs", queued)
		}
	}
	return s.Open
}

// syncRouting sets or clears the routing variable to match want. A failure
// is retried on the next poll.
func (p *Pool) syncRouting(ctx context.Context, want bool) {
	if p.cfg.FallbackVar == "" || p.cfg.DryRun || p.routed == want {
		return
	}
	var err error
	if want {
		err = p.gh.SetVariable(ctx, p.cfg.Repo, p.cfg.FallbackVar, p.cfg.RouteLabel)
	} else {
		err = p.gh.DeleteVariable(ctx, p.cfg.Repo, p.cfg.FallbackVar)
	}
	if err != nil {
		slog.Warn("laptop mode: could not update routing; retrying", "variable", p.cfg.FallbackVar, "on", want, "err", err)
		return
	}
	p.routed = want
	if want {
		slog.Info("routing on", "variable", p.cfg.FallbackVar, "value", p.cfg.RouteLabel)
	} else {
		slog.Info("routing off: new jobs go to GitHub-hosted runners", "variable", p.cfg.FallbackVar)
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
