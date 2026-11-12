package route

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A heartbeat is the watchdog's lease followed by the name of the machine
// that wrote it: "<unix seconds>|<cron>|...|<machine>". spareci 0.2 wrote
// the lease alone, which reads as a heartbeat with no machine name.

// Heartbeat is the heartbeat value machine writes at now.
func Heartbeat(now time.Time, machine string) string { return Lease(now) + CleanMachine(machine) }

// CleanMachine makes a machine name safe to put in a heartbeat: no "|",
// which separates its fields, and nothing a workflow log would mangle.
func CleanMachine(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, name)
}

// ParseHeartbeat reads a heartbeat value. ok is false if it has no time.
func ParseHeartbeat(v string) (at time.Time, machine string, ok bool) {
	ts, rest, _ := strings.Cut(v, "|")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return time.Time{}, "", false
	}
	if i := strings.LastIndex(rest, "|"); i >= 0 {
		machine = rest[i+1:]
	}
	return time.Unix(sec, 0), machine, true
}

// Owner is who serves a group right now, by its heartbeat.
type Owner struct {
	Group   Group
	Set     []string      // the group's routing variables that are set
	Machine string        // from the heartbeat; "" from spareci 0.2 or without one
	Age     time.Duration // since the heartbeat was written
	Beating bool          // there is a heartbeat
}

// Fresh reports whether the machine that wrote the heartbeat is up: the
// watchdog acts on a heartbeat once it's StaleAfter old.
func (o Owner) Fresh() bool { return o.Beating && o.Age < StaleAfter }

// Who names the owner for messages.
func (o Owner) Who() string {
	if o.Machine == "" {
		return "a spareci older than 0.3"
	}
	return o.Machine
}

// Getter reads a repo variable; a variable that doesn't exist reads as "".
type Getter interface {
	GetVariable(ctx context.Context, repo, name string) (string, error)
}

// ReadOwner reads a group's heartbeat and variables.
func ReadOwner(ctx context.Context, api Getter, repo string, g Group, now time.Time) (Owner, error) {
	o := Owner{Group: g}
	if g.Heartbeat != "" {
		v, err := api.GetVariable(ctx, repo, g.Heartbeat)
		if err != nil {
			return o, fmt.Errorf("read %s: %w", g.Heartbeat, err)
		}
		if at, m, ok := ParseHeartbeat(v); ok {
			o.Beating, o.Machine, o.Age = true, m, now.Sub(at)
		}
	}
	for _, rt := range g.Routes {
		v, err := api.GetVariable(ctx, repo, rt.Var)
		if err != nil {
			return o, fmt.Errorf("read %s: %w", rt.Var, err)
		}
		if v != "" {
			o.Set = append(o.Set, rt.Var)
		}
	}
	return o, nil
}

// ConflictError means another machine serves a group this machine wants.
type ConflictError struct {
	Repo  string
	Owner Owner
}

func (e *ConflictError) Error() string {
	o := e.Owner
	if o.Group.Legacy() || o.Machine == "" {
		return fmt.Sprintf("%s is serving %s, for every OS it runs (heartbeat %s ago in %s)",
			o.Who(), e.Repo, o.Age.Round(time.Second), o.Group.Heartbeat)
	}
	return fmt.Sprintf("%s is serving %s jobs for %s (heartbeat %s ago in %s)",
		o.Who(), o.Group.Name(), e.Repo, o.Age.Round(time.Second), o.Group.Heartbeat)
}

// heldBy reports whether a fresh heartbeat says another machine than
// machine serves the group. A heartbeat without a name in the old shared
// variable's group counts as this machine's own when this machine owns that
// group (an upgrade from 0.2 in place); anywhere else it's another machine.
func heldBy(o Owner, machine string, ownsLegacy bool) bool {
	if !o.Fresh() || o.Machine == machine {
		return false
	}
	return !(o.Machine == "" && o.Group.Legacy() && ownsLegacy)
}
