// Package gate decides whether this machine should take new jobs right now,
// for machines people also work on: only on mains power, only once the user
// has been idle for a while, only inside a time window.
//
// The gate only answers "open or closed". What happens on a transition
// (routing jobs back to GitHub, letting running jobs finish) is the pool's job.
package gate

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Config holds the conditions. The zero value has none, and the gate is
// always open.
type Config struct {
	OnlyOnAC bool          // closed while running on battery
	Idle     time.Duration // closed until the user has been idle this long; 0 disables
	Hours    *Window       // closed outside this local-time window; nil disables
}

func (c Config) Enabled() bool { return c.OnlyOnAC || c.Idle > 0 || c.Hours != nil }

// String describes the conditions, e.g. "on AC power, idle 10m, 18:00-08:00".
func (c Config) String() string {
	var parts []string
	if c.OnlyOnAC {
		parts = append(parts, "on AC power")
	}
	if c.Idle > 0 {
		parts = append(parts, "idle "+shortDuration(c.Idle))
	}
	if c.Hours != nil {
		parts = append(parts, c.Hours.String())
	}
	if len(parts) == 0 {
		return "always"
	}
	return strings.Join(parts, ", ")
}

// Probes read the machine's state. Tests replace them; SystemProbes returns
// the real ones for this OS.
type Probes struct {
	OnAC func() (bool, error)          // true on mains power, or when there is no battery
	Idle func() (time.Duration, error) // time since the last keyboard or mouse input
	Now  func() time.Time
}

// State is the result of a check.
type State struct {
	Open   bool
	Reason string    // why it's closed, e.g. "on battery"; "" when open
	Since  time.Time // when it last changed
}

func (s State) String() string {
	if s.Open {
		return "open"
	}
	return "closed: " + s.Reason
}

type Gate struct {
	cfg    Config
	probes Probes

	// OnChange, if set, is called after a check whose result differs from
	// the previous one (including the first check).
	OnChange func(State)

	mu      sync.Mutex
	checked bool
	state   State
	onAC    bool          // last good reading
	idle    time.Duration // last good reading
	warned  map[string]bool
}

func New(cfg Config, p Probes) *Gate {
	if p.Now == nil {
		p.Now = time.Now
	}
	return &Gate{cfg: cfg, probes: p, onAC: true, warned: map[string]bool{}}
}

func (g *Gate) Config() Config { return g.cfg }

// Verify reads every probe the conditions need once, so that `spareci up`
// can refuse a condition this machine can't measure instead of silently
// ignoring it or never taking a job.
func (g *Gate) Verify() error {
	if g.cfg.OnlyOnAC {
		if g.probes.OnAC == nil {
			return fmt.Errorf("--only-on-ac: power source can't be read on this OS")
		}
		if _, err := g.probes.OnAC(); err != nil {
			return fmt.Errorf("--only-on-ac: can't read the power source: %w", err)
		}
	}
	if g.cfg.Idle > 0 {
		if g.probes.Idle == nil {
			return fmt.Errorf("--idle: idle time can't be read on this OS")
		}
		if _, err := g.probes.Idle(); err != nil {
			return fmt.Errorf("--idle: can't read idle time: %w", err)
		}
	}
	return nil
}

// Check evaluates the conditions now. A probe that fails keeps its last good
// reading (and warns once), so a hiccup doesn't flip the gate.
func (g *Gate) Check() State {
	g.mu.Lock()
	var reasons []string
	if g.cfg.OnlyOnAC && g.probes.OnAC != nil {
		if ac, err := g.probes.OnAC(); err != nil {
			g.warnOnce("power", err)
		} else {
			g.onAC = ac
		}
		if !g.onAC {
			reasons = append(reasons, "on battery")
		}
	}
	if g.cfg.Idle > 0 && g.probes.Idle != nil {
		if d, err := g.probes.Idle(); err != nil {
			g.warnOnce("idle", err)
		} else {
			g.idle = d
		}
		if g.idle < g.cfg.Idle {
			reasons = append(reasons, fmt.Sprintf("user active (idle %s of %s)", shortDuration(g.idle), shortDuration(g.cfg.Idle)))
		}
	}
	now := g.probes.Now()
	if g.cfg.Hours != nil && !g.cfg.Hours.Contains(now) {
		reasons = append(reasons, "outside "+g.cfg.Hours.String())
	}

	next := State{Open: len(reasons) == 0, Reason: strings.Join(reasons, ", ")}
	changed := !g.checked || next.Open != g.state.Open
	if changed {
		next.Since = now
	} else {
		next.Since = g.state.Since
	}
	g.checked, g.state = true, next
	onChange := g.OnChange
	g.mu.Unlock()

	if changed && onChange != nil {
		onChange(next)
	}
	return next
}

// State returns the result of the last check.
func (g *Gate) State() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

func (g *Gate) warnOnce(what string, err error) {
	if g.warned[what] {
		return
	}
	g.warned[what] = true
	slog.Warn("laptop mode: probe failed; using the last reading", "probe", what, "err", err)
}

// Window is a daily local-time range. End before start crosses midnight.
type Window struct {
	Start, End int // minutes after midnight
}

// ParseWindow parses "HH:MM-HH:MM", e.g. "18:00-08:00".
func ParseWindow(s string) (Window, error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return Window{}, fmt.Errorf("hours %q: want HH:MM-HH:MM, e.g. 18:00-08:00", s)
	}
	start, err := parseClock(a)
	if err != nil {
		return Window{}, fmt.Errorf("hours %q: %w", s, err)
	}
	end, err := parseClock(b)
	if err != nil {
		return Window{}, fmt.Errorf("hours %q: %w", s, err)
	}
	if start == end {
		return Window{}, fmt.Errorf("hours %q: start and end are the same; leave --hours off to run at any time", s)
	}
	return Window{Start: start, End: end}, nil
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		// Allow "24:00" as an end of day.
		if strings.TrimSpace(s) == "24:00" {
			return 24 * 60, nil
		}
		return 0, fmt.Errorf("%q is not a 24-hour HH:MM time", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Contains reports whether t's local wall-clock time is inside the window.
// The start is inclusive and the end exclusive.
func (w Window) Contains(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	if w.Start < w.End {
		return m >= w.Start && m < w.End
	}
	return m >= w.Start || m < w.End
}

func (w Window) String() string {
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.Start/60, w.Start%60, w.End/60, w.End%60)
}

// shortDuration prints 10m instead of 10m0s.
func shortDuration(d time.Duration) string {
	d = d.Round(time.Second)
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
