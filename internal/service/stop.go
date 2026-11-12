package service

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Stop stops the running service the way ^C stops `spareci up`, rather than
// through the service manager, which kills spareci after its stop timeout:
//
//  1. SIGTERM: spareci turns routing off, sends queued jobs back to GitHub,
//     and lets running jobs finish.
//  2. After drain (if they haven't), a second SIGTERM: spareci stops its
//     runners, so GitHub marks their jobs completed, and re-runs their runs
//     on GitHub-hosted runners.
//  3. If spareci still hasn't exited after stopAfter more, it's left to the
//     service manager.
//
// spareci exits 0, so launchd and systemd don't restart it. The service's
// log lines written meanwhile are copied to out. drain 0 waits as long as
// the jobs take. Stop does nothing if the service isn't running.
func Stop(repo string, drain, stopAfter time.Duration, logPath string, out io.Writer) error {
	pid, err := MainPID(repo)
	if err != nil || pid == 0 {
		return err
	}
	return stop(osProcess(pid), drain, stopAfter, logPath, out, time.Second)
}

// process is the running service, for stop; a fake in tests.
type process interface {
	Signal(sig syscall.Signal) error
	Alive() bool
}

type osProcess int

func (p osProcess) Signal(sig syscall.Signal) error { return syscall.Kill(int(p), sig) }
func (p osProcess) Alive() bool                     { return syscall.Kill(int(p), 0) == nil }

func stop(p process, drain, stopAfter time.Duration, logPath string, out io.Writer, tick time.Duration) error {
	tail := newTail(logPath)
	defer tail.close()
	if err := p.Signal(syscall.SIGTERM); err != nil {
		if !p.Alive() {
			return nil
		}
		return fmt.Errorf("stop the service: %w", err)
	}
	if drain > 0 {
		fmt.Fprintf(out, "stopping: routing off, queued jobs go back to GitHub; running jobs get up to %v to finish\n", drain)
	} else {
		fmt.Fprintln(out, "stopping: routing off, queued jobs go back to GitHub; waiting for running jobs to finish")
	}
	start := time.Now()
	var aborted time.Time
	for p.Alive() {
		tail.copy(out)
		now := time.Now()
		if aborted.IsZero() && drain > 0 && now.Sub(start) >= drain {
			fmt.Fprintln(out, "drain timeout reached: stopping running jobs; they'll be re-run on GitHub-hosted runners")
			aborted = now
			_ = p.Signal(syscall.SIGTERM)
		}
		if !aborted.IsZero() && now.Sub(aborted) >= stopAfter {
			fmt.Fprintf(out, "spareci hasn't stopped %v after the drain timeout; leaving it to the service manager\n", stopAfter)
			return nil
		}
		time.Sleep(tick)
	}
	tail.copy(out)
	return nil
}

// tail copies lines appended to a log file after it was opened.
type tail struct {
	f   *os.File
	buf *bufio.Reader
}

func newTail(path string) *tail {
	f, err := os.Open(path)
	if err != nil {
		return &tail{}
	}
	_, _ = f.Seek(0, io.SeekEnd)
	return &tail{f: f, buf: bufio.NewReader(f)}
}

func (t *tail) copy(out io.Writer) {
	if t.f == nil {
		return
	}
	for {
		line, err := t.buf.ReadString('\n')
		if err != nil {
			// A partial line: read it again whole next time.
			if len(line) > 0 {
				_, _ = t.f.Seek(-int64(len(line)), io.SeekCurrent)
				t.buf.Reset(t.f)
			}
			return
		}
		fmt.Fprint(out, "  "+line)
	}
}

func (t *tail) close() {
	if t.f != nil {
		_ = t.f.Close()
	}
}

var launchdPID = regexp.MustCompile(`(?m)^\s*pid = (\d+)$`)

// MainPID is the running service's process ID, or 0 if it isn't running.
func MainPID(repo string) (int, error) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("launchctl", "print", launchdTarget(repo)).Output()
		if err != nil {
			return 0, nil // not loaded
		}
		if m := launchdPID.FindSubmatch(out); m != nil {
			return strconv.Atoi(string(m[1]))
		}
		return 0, nil
	case "linux":
		out, err := exec.Command("systemctl", "--user", "show", "-p", "MainPID", "--value", Name(repo)+".service").Output()
		if err != nil {
			return 0, nil
		}
		return strconv.Atoi(strings.TrimSpace(string(out)))
	}
	return 0, fmt.Errorf("background service not supported on %s", runtime.GOOS)
}
