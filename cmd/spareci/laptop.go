package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Row-Labs/spareci/internal/gate"
	"github.com/Row-Labs/spareci/internal/pool"
	"github.com/Row-Labs/spareci/internal/runner"
)

// Laptop mode: conditions under which `spareci up` takes jobs.

type laptopOpts struct {
	onlyOnAC bool
	idle     time.Duration
	hours    string
}

func laptopFlags(fs *flag.FlagSet) *laptopOpts {
	o := &laptopOpts{}
	fs.BoolVar(&o.onlyOnAC, "only-on-ac", false, "take no new jobs while on battery")
	fs.DurationVar(&o.idle, "idle", 0, "take jobs only after no keyboard or mouse input for this long, e.g. 10m")
	fs.StringVar(&o.hours, "hours", "", "take jobs only in this local-time window, e.g. 18:00-08:00")
	return o
}

// config checks the flags.
func (o *laptopOpts) config() (gate.Config, error) {
	cfg := gate.Config{OnlyOnAC: o.onlyOnAC, Idle: o.idle}
	if o.idle < 0 {
		return cfg, fmt.Errorf("--idle must be positive")
	}
	if o.hours != "" {
		w, err := gate.ParseWindow(o.hours)
		if err != nil {
			return cfg, err
		}
		cfg.Hours = &w
	}
	return cfg, nil
}

// gate builds the gate, or returns nil when no condition is set. It fails
// when a condition can't be measured on this machine.
func (o *laptopOpts) gate(repo string) (*gate.Gate, error) {
	cfg, err := o.config()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled() {
		return nil, nil
	}
	g := gate.New(cfg, gate.SystemProbes())
	if err := g.Verify(); err != nil {
		return nil, err
	}
	g.OnChange = func(s gate.State) { writeGateState(repo, cfg, s) }
	return g, nil
}

// poolGate avoids handing the pool a non-nil interface holding a nil *gate.Gate.
func poolGate(g *gate.Gate) pool.Gate {
	if g == nil {
		return nil
	}
	return g
}

// The up process writes its gate state here so `spareci status` can show it.
type gateFile struct {
	Repo       string    `json:"repo"`
	PID        int       `json:"pid"`
	Conditions string    `json:"conditions"`
	Open       bool      `json:"open"`
	Reason     string    `json:"reason,omitempty"`
	Since      time.Time `json:"since"`
}

func gateStatePath(repo string) string {
	return filepath.Join(runner.DefaultRoot(), "state", strings.ReplaceAll(repo, "/", "_")+".gate.json")
}

func writeGateState(repo string, cfg gate.Config, s gate.State) {
	b, _ := json.Marshal(gateFile{Repo: repo, PID: os.Getpid(), Conditions: cfg.String(), Open: s.Open, Reason: s.Reason, Since: s.Since})
	path := gateStatePath(repo)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		err = os.WriteFile(path, b, 0o644)
		if err != nil {
			slog.Debug("could not write gate state", "err", err)
		}
	}
}

func removeGateState(repo string) { _ = os.Remove(gateStatePath(repo)) }

// printGateState prints laptop mode's state from a running `spareci up`, if any.
func printGateState(repo string) {
	b, err := os.ReadFile(gateStatePath(repo))
	if err != nil {
		return
	}
	var f gateFile
	if json.Unmarshal(b, &f) != nil || f.Repo != repo || syscall.Kill(f.PID, 0) != nil {
		return // stale: that process is gone
	}
	since := f.Since.Local().Format("15:04")
	if f.Open {
		fmt.Printf("laptop mode: taking jobs since %s (when %s)\n", since, f.Conditions)
	} else {
		fmt.Printf("laptop mode: paused since %s: %s (takes jobs when %s)\n", since, f.Reason, f.Conditions)
	}
}
