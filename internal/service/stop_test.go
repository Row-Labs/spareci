package service

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeProc is spareci running as the service: it logs, and exits once it
// has had the signals it needs (exitAfter), or never (0).
type fakeProc struct {
	mu        sync.Mutex
	sigs      []time.Time
	exitAfter int
	log       *os.File
}

func (p *fakeProc) Signal(sig syscall.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sig != syscall.SIGTERM {
		panic(sig)
	}
	p.sigs = append(p.sigs, time.Now())
	_, _ = p.log.WriteString("INFO shutting down\nINFO waiting for running jobs to finish jobs=1\n")
	return nil
}

func (p *fakeProc) Alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitAfter == 0 || len(p.sigs) < p.exitAfter
}

func newFakeProc(t *testing.T, exitAfter int) (*fakeProc, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("old line, from before the stop\n")
	t.Cleanup(func() { f.Close() })
	return &fakeProc{exitAfter: exitAfter, log: f}, path
}

func TestStopDrainsThenStopsJobsAtTheTimeout(t *testing.T) {
	p, log := newFakeProc(t, 2) // exits after the second SIGTERM
	var out strings.Builder
	start := time.Now()
	if err := stop(p, 50*time.Millisecond, time.Second, log, &out, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if len(p.sigs) != 2 {
		t.Fatalf("%d signals, want 2", len(p.sigs))
	}
	if d := p.sigs[1].Sub(start); d < 50*time.Millisecond {
		t.Errorf("second SIGTERM after %v, before the drain timeout", d)
	}
	o := out.String()
	if strings.Contains(o, "old line") || !strings.Contains(o, "  INFO waiting for running jobs") || !strings.Contains(o, "drain timeout reached") {
		t.Errorf("output:\n%s", o)
	}
}

func TestStopReturnsWhenJobsFinishFirst(t *testing.T) {
	p, log := newFakeProc(t, 1)
	var out strings.Builder
	if err := stop(p, time.Hour, time.Second, log, &out, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if len(p.sigs) != 1 {
		t.Errorf("%d signals, want 1", len(p.sigs))
	}
}

func TestStopGivesUpAfterStopAfter(t *testing.T) {
	p, log := newFakeProc(t, 0) // never exits
	var out strings.Builder
	done := make(chan error)
	go func() { done <- stop(p, 10*time.Millisecond, 20*time.Millisecond, log, &out, time.Millisecond) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop didn't give up")
	}
	if !strings.Contains(out.String(), "leaving it to the service manager") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestLaunchdPID(t *testing.T) {
	out := "gui/501/dev.spareci.o-r = {\n\tactive count = 1\n\tstate = running\n\tpid = 10622\n\tendpoints = {\n\t}\n}\n"
	m := launchdPID.FindStringSubmatch(out)
	if m == nil || m[1] != "10622" {
		t.Errorf("pid %v", m)
	}
}
