package main

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// SIGTERM from launchd or systemd drains: soft stop at once, running jobs
// stopped at the drain timeout, well inside the service manager's own
// timeout (the drain timeout plus pool.StopAfterDrain).
func TestStopOnDrainsThenStopsJobsAtTheTimeout(t *testing.T) {
	sigs := make(chan os.Signal, 3)
	quit := make(chan struct{})
	soft, hard := stopOn(sigs, 50*time.Millisecond, func() { close(quit) })
	start := time.Now()
	sigs <- syscall.SIGTERM
	select {
	case <-soft.Done():
	case <-time.After(time.Second):
		t.Fatal("first SIGTERM didn't start the drain")
	}
	if hard.Err() != nil {
		t.Fatal("running jobs stopped at once")
	}
	select {
	case <-hard.Done():
		if d := time.Since(start); d < 50*time.Millisecond {
			t.Errorf("jobs stopped after %v, before the drain timeout", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain timeout didn't stop running jobs")
	}
	sigs <- syscall.SIGTERM
	select {
	case <-quit:
	case <-time.After(time.Second):
		t.Fatal("a signal after the drain timeout didn't quit")
	}
}

func TestStopOnSecondSignalStopsJobsAndThirdQuits(t *testing.T) {
	sigs := make(chan os.Signal, 3)
	quit := make(chan struct{})
	soft, hard := stopOn(sigs, 0, func() { close(quit) }) // no drain limit
	sigs <- os.Interrupt
	<-soft.Done()
	time.Sleep(20 * time.Millisecond)
	if hard.Err() != nil {
		t.Fatal("no drain limit, but running jobs were stopped")
	}
	sigs <- os.Interrupt
	select {
	case <-hard.Done():
	case <-time.After(time.Second):
		t.Fatal("second ^C didn't stop running jobs")
	}
	sigs <- os.Interrupt
	select {
	case <-quit:
	case <-time.After(time.Second):
		t.Fatal("third ^C didn't quit")
	}
}
