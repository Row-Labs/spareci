package pool

import (
	"github.com/Row-Labs/spareci/internal/route"
	"github.com/Row-Labs/spareci/internal/runner"
)

// Policy says how backends take kinds of jobs.
type Policy struct {
	Prefix string // routing label prefix (spareci)
	// X64OnARM lets an arm64 Linux backend take Linux x64 jobs (fallback
	// ubuntu-24.04) when there's no x64 one. Most jobs don't care about the
	// CPU, and arm64 containers run natively on Apple Silicon.
	X64OnARM bool
	// Legacy also routes the old shared variable (SPARECI_RUNS_ON, label
	// Prefix) to the backend that runs the host's own OS and architecture.
	Legacy bool
	// Max caps each OS's backends (runner.OSMacOS, runner.OSLinux); 0 or
	// missing: only the pool's --max.
	Max map[string]int
}

// Assign gives each backend the kinds of jobs it takes, in order of
// preference: a kind already taken by an earlier backend isn't given to a
// later one. It returns the pool's backends (those that take something) and
// the kinds of jobs routed here, for the router.
func Assign(backends []runner.Backend, p Policy) ([]*Backend, []route.Target) {
	var out []*Backend
	var targets []route.Target
	taken := map[route.Target]bool{}
	legacy := !p.Legacy
	give := func(b *Backend, t route.Target) {
		if !taken[t] {
			taken[t] = true
			targets = append(targets, t)
			b.Routing = append(b.Routing, t.Label(p.Prefix))
		}
	}
	for _, rb := range backends {
		b := &Backend{Backend: rb, Max: p.Max[rb.OS()]}
		switch {
		case rb.OS() == runner.OSMacOS:
			give(b, route.TargetMacOS)
		case rb.OS() == runner.OSLinux && rb.Arch() == runner.ArchARM:
			give(b, route.TargetLinuxARM)
		case rb.OS() == runner.OSLinux && rb.Arch() == runner.ArchX64:
			give(b, route.TargetLinux)
		}
		if !legacy && rb.OS() == runner.HostOS() && rb.Arch() == runner.HostArch() {
			legacy = true
			b.Routing = append(b.Routing, p.Prefix)
		}
		if len(b.Routing) > 0 {
			out = append(out, b)
		}
	}
	if p.X64OnARM && !taken[route.TargetLinux] {
		for _, b := range out {
			if b.OS() == runner.OSLinux && b.Arch() == runner.ArchARM {
				give(b, route.TargetLinux)
				break
			}
		}
	}
	return out, targets
}
