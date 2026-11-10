// Package runner installs GitHub's own actions/runner once and runs throwaway
// copies of it, one per job. Using the real runner means logs, status checks,
// artifacts and caches all report to GitHub exactly as on a hosted runner.
package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type Host struct {
	Root    string // cache root: runner installs, job dirs, logs, shared tool cache
	Version string

	source string // install to clone per job; set by PrepareCacheVariant
}

func DefaultRoot() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "spareci")
}

func (h *Host) installDir() string { return filepath.Join(h.Root, "runner", h.Version) }
func (h *Host) jobsDir() string    { return filepath.Join(h.Root, "jobs") }
func (h *Host) LogsDir() string    { return filepath.Join(h.Root, "logs") }

// ToolCache persists across jobs, so setup-node, setup-python and friends
// download a version once instead of every run.
func (h *Host) ToolCache() string { return filepath.Join(h.Root, "toolcache") }

// Platform is the runner's tarball suffix for this machine, e.g. "osx-arm64".
func Platform() (string, error) {
	osName := map[string]string{"darwin": "osx", "linux": "linux"}[runtime.GOOS]
	arch := map[string]string{"arm64": "arm64", "amd64": "x64"}[runtime.GOARCH]
	if osName == "" || arch == "" {
		return "", fmt.Errorf("unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return osName + "-" + arch, nil
}

// DefaultLabels are the labels GitHub would give a self-hosted runner here.
func DefaultLabels() []string {
	osLabel := map[string]string{"darwin": "macOS", "linux": "Linux"}[runtime.GOOS]
	arch := map[string]string{"arm64": "ARM64", "amd64": "X64"}[runtime.GOARCH]
	return []string{"self-hosted", osLabel, arch}
}

// Install downloads and unpacks the runner if this version is not there yet.
func (h *Host) Install(ctx context.Context) error {
	dir := h.installDir()
	if _, err := os.Stat(filepath.Join(dir, "run.sh")); err == nil {
		return nil
	}
	plat, err := Platform()
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/actions-runner-%s-%s.tar.gz", h.Version, plat, h.Version)
	slog.Info("downloading actions/runner", "version", h.Version, "platform", plat)

	tmp := dir + ".partial"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	tar := exec.CommandContext(ctx, "tar", "xzf", "-", "-C", tmp)
	tar.Stdin = resp.Body
	if out, err := tar.CombinedOutput(); err != nil {
		return fmt.Errorf("extract runner: %v: %s", err, out)
	}
	return os.Rename(tmp, dir)
}

// Prune removes job directories left behind by a crash.
func (h *Host) Prune() {
	entries, _ := os.ReadDir(h.jobsDir())
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(h.jobsDir(), e.Name()))
	}
}

// Result is what one ephemeral runner did.
type Result struct {
	Job        string // job name as the runner printed it
	Conclusion string // Succeeded, Failed, Canceled; empty if no job ran
	Log        string
}

// Run clones the installed runner (copy-on-write where the filesystem allows),
// runs it for a single job with the given JIT config, and deletes the clone.
// Cancelling ctx stops the runner gracefully, then forcibly. onStart is called
// when the runner picks up its job. extraEnv (KEY=VALUE) reaches every step.
func (h *Host) Run(ctx context.Context, name, jitConfig string, extraEnv []string, onStart func()) (Result, error) {
	res := Result{Log: filepath.Join(h.LogsDir(), name+".log")}
	dir := filepath.Join(h.jobsDir(), name)
	if err := os.MkdirAll(h.jobsDir(), 0o755); err != nil {
		return res, err
	}
	if err := os.MkdirAll(h.LogsDir(), 0o755); err != nil {
		return res, err
	}
	if err := os.MkdirAll(h.ToolCache(), 0o755); err != nil {
		return res, err
	}
	defer os.RemoveAll(dir)
	src := h.source
	if src == "" {
		src = h.installDir()
	}
	if err := clone(src, dir); err != nil {
		return res, fmt.Errorf("clone runner: %w", err)
	}
	env := append([]string{"AGENT_TOOLSDIRECTORY=" + h.ToolCache(), "RUNNER_TOOL_CACHE=" + h.ToolCache()}, extraEnv...)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(strings.Join(env, "\n")+"\n"), 0o644); err != nil {
		return res, err
	}

	logf, err := os.Create(res.Log)
	if err != nil {
		return res, err
	}
	defer logf.Close()

	cmd := exec.Command(filepath.Join(dir, "run.sh"), "--jitconfig", jitConfig)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return res, err
	}

	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			line := sc.Text()
			fmt.Fprintln(logf, line)
			// Runner stdout: "2026-11-10 18:00:00Z: Running job: checks"
			if _, rest, ok := strings.Cut(line, "Running job: "); ok {
				res.Job = rest
				slog.Info("job started", "runner", name, "job", rest)
				if onStart != nil {
					onStart()
				}
			}
			if _, rest, ok := strings.Cut(line, "completed with result: "); ok {
				res.Conclusion = rest
			}
		}
	}()

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait(); pw.Close() }()

	select {
	case err = <-waitErr:
	case <-ctx.Done():
		// The runner treats SIGINT as "finish up and deregister".
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
		select {
		case err = <-waitErr:
		case <-time.After(30 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			err = <-waitErr
		}
	}
	<-scanDone
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, err
}

func clone(src, dst string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("cp", "-cR", src, dst) // APFS clonefile: instant, no extra disk
	} else {
		cmd = exec.Command("cp", "-a", "--reflink=auto", src, dst)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}
