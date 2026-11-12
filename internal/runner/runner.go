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

	"github.com/Row-Labs/spareci/internal/gitmirror"
)

type Host struct {
	Root    string // cache root: runner installs, job dirs, logs, shared tool cache
	Repo    string // owner/name; job dirs are kept per repo (see owner.go)
	Version string
	// JobHome is the HOME jobs run with (see ResolveJobHome and JobEnv).
	// Empty: the user's own home and environment.
	JobHome string
	// Git, if set, is a local mirror of the job's repository that checkouts
	// borrow objects from. Run refreshes it in the background before each job.
	Git *gitmirror.Mirror

	source     string // install to clone per job; set by PrepareCacheVariant
	shimDir    string // docker shim, first on jobs' PATH; set by PrepareDocker
	dockerHost string // DOCKER_HOST for jobs with a separate home; set by PrepareDocker
}

// PrepareDocker sets up what jobs need to use the host's docker (see
// docker.go). Without docker it does nothing.
func (h *Host) PrepareDocker() error {
	userHome, _ := os.UserHomeDir()
	env := JobEnv(os.Environ(), userHome, h.JobHome)
	var path string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	dir, err := WriteDockerShim(filepath.Join(h.Root, "shims"), path)
	if err != nil || dir == "" {
		return err
	}
	h.shimDir = dir
	if h.JobHome != "" {
		h.dockerHost = DockerHost(os.Environ())
	}
	return nil
}

func DefaultRoot() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "spareci")
}

func (h *Host) installDir() string { return filepath.Join(h.Root, "runner", h.Version) }
func (h *Host) jobsDir() string    { return JobsDir(h.Root, h.Repo) }
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
func DefaultLabels() []string { return SelfHostedLabels(HostOS(), HostArch()) }

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

// JobsDir holds a repo's runner copies and container env files, one per job.
func JobsDir(root, repo string) string {
	if repo == "" {
		return filepath.Join(root, "jobs")
	}
	return filepath.Join(root, "jobs", RepoKey(repo))
}

// Prune removes job directories left behind by a crash: this repo's, and
// those of repos no spareci is running for. Job directories directly in
// <root>/jobs are from a spareci before 0.2, which may still be running
// jobs there, so they're left alone.
func (h *Host) Prune() {
	if h.Repo == "" {
		entries, _ := os.ReadDir(h.jobsDir())
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(h.jobsDir(), e.Name()))
		}
		return
	}
	all := filepath.Join(h.Root, "jobs")
	entries, _ := os.ReadDir(all)
	for _, e := range entries {
		key := e.Name()
		if !e.IsDir() || strings.HasPrefix(key, "spareci-") {
			continue // a job of a spareci before 0.2
		}
		if key == RepoKey(h.Repo) || !live(h.Root, key) {
			_ = os.RemoveAll(filepath.Join(all, key))
		}
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
func (h *Host) Run(ctx context.Context, name, jitConfig string, extraEnv []string, onStart func(job string)) (Result, error) {
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
	if h.JobHome != "" {
		if err := prepareHome(h.JobHome); err != nil {
			return res, err
		}
	}
	defer os.RemoveAll(dir)
	if h.Git != nil {
		// The runner takes several seconds to start and fetch its actions,
		// so the mirror usually has the job's commit before checkout asks.
		extraEnv = append(extraEnv, h.Git.Env()...)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if err := h.Git.Update(ctx); err != nil {
				slog.Warn("git mirror not updated; checkouts fetch more from GitHub", "err", err)
			}
		}()
	}
	src := h.source
	if src == "" {
		src = h.installDir()
	}
	if err := clone(src, dir); err != nil {
		return res, fmt.Errorf("clone runner: %w", err)
	}
	userHome, _ := os.UserHomeDir()
	base := JobEnv(os.Environ(), userHome, h.JobHome)
	if h.JobHome != "" {
		if err := LinkDockerPlugins(userHome, os.Getenv("DOCKER_CONFIG"), h.JobHome); err != nil {
			slog.Warn("docker CLI plugins not linked into the job home", "err", err)
		}
		if h.dockerHost != "" {
			base = append(base, "DOCKER_HOST="+h.dockerHost)
		}
	}
	if h.shimDir != "" {
		base = prependPath(base, h.shimDir)
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
	cmd.Env = append(base, env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return res, err
	}

	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		watchRunner(pr, logf, name, &res, onStart)
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

// watchRunner copies a runner's output to log until r ends, noting in res the
// job it took and how that ended, and calling onStart when the job starts.
func watchRunner(r io.Reader, log io.Writer, name string, res *Result, onStart func(job string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		fmt.Fprintln(log, line)
		// Runner stdout: "2026-11-10 18:00:00Z: Running job: checks"
		if _, rest, ok := strings.Cut(line, "Running job: "); ok {
			res.Job = rest
			slog.Info("job started", "runner", name, "job", rest)
			if onStart != nil {
				onStart(rest)
			}
		}
		if _, rest, ok := strings.Cut(line, "completed with result: "); ok {
			res.Conclusion = rest
		}
	}
	// A line too long for the scanner ends the scan; keep draining so the
	// runner never blocks on a full pipe.
	_, _ = io.Copy(log, r)
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
