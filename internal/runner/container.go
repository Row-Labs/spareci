package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Row-Labs/spareci/internal/gitmirror"
)

// Linux jobs in containers: one fresh container per job, from an image built
// locally from image/Dockerfile, through any Docker-API runtime the docker CLI
// reaches (DOCKER_HOST or the current docker context: OrbStack, Docker
// Desktop, Colima, Docker Engine). See docs/linux-containers.md.

//go:embed image/Dockerfile image/entrypoint.sh image/dockerproxy/main.go
var imageFS embed.FS

// imageFiles are the build context, as paths under image/.
var imageFiles = []string{"Dockerfile", "entrypoint.sh", "dockerproxy/main.go"}

// Paths in the job's container (see image/entrypoint.sh).
const (
	// jobHome is HOME, and the mount point of the job's work volume in
	// socket mode, so the workspace and HOME share a filesystem as on a
	// hosted runner (pnpm puts its store at the root of the project's
	// filesystem otherwise).
	jobHome = "/home/runner"
	// daemonSocketMount is where the runtime's socket is mounted. Jobs talk
	// to the proxy in front of it at /var/run/docker.sock.
	daemonSocketMount = "/run/spareci/docker.sock"
)

// Docker access for jobs in containers (Container.DockerMode).
const (
	// DockerSocket mounts the runtime's socket into the job's container, so
	// docker build, container: jobs and services: work. A job can then
	// control the runtime, and through it the machine: no isolation.
	DockerSocket = "socket"
	// DockerNone gives jobs no docker daemon. Steps that use docker fail.
	DockerNone = "none"
)

// ErrNoRuntime means there is no container runtime to run Linux jobs in.
var ErrNoRuntime = errors.New("no container runtime")

// Docker runs docker CLI commands. DockerCLI is the real one; tests fake it.
type Docker interface {
	// Output runs a command to completion and returns its stdout.
	Output(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
	// Stream runs a command with its stdout and stderr going to w, and
	// returns when it exits. Stopping what it started is the caller's job
	// (docker stop), so it has no context.
	Stream(w io.Writer, args ...string) error
}

// DockerCLI runs the docker CLI with spareci's own environment, so the user's
// DOCKER_HOST or current docker context applies.
type DockerCLI struct{ Bin string }

func (d DockerCLI) bin() string {
	if d.Bin != "" {
		return d.Bin
	}
	return "docker"
}

func (d DockerCLI) Output(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, d.bin(), args...)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return out, fmt.Errorf("docker %s: %w: %s", args[0], err, msg)
	}
	return out, nil
}

func (d DockerCLI) Stream(w io.Writer, args ...string) error {
	cmd := exec.Command(d.bin(), args...)
	// Its own process group: ^C in spareci's terminal is for spareci, which
	// drains, and must not reach a job's container through the CLI.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

// Container runs Linux jobs, each in a fresh container that is removed
// afterwards. One Container serves one architecture; on Apple Silicon, x64
// runs under Rosetta (see ContainerArches).
type Container struct {
	Root    string // spareci's cache root: logs and per-job env files
	Version string // actions/runner version baked into the image
	Target  string // ArchARM or ArchX64
	Repo    string // owner/name: names the per-repo tool cache volume
	// Resource limits per job, as docker run takes them ("4", "8g"). Empty:
	// no limit beyond the runtime's own (OrbStack and Docker Desktop cap
	// their VM).
	CPUs, Memory string
	// DockerMode is DockerSocket (default) or DockerNone.
	DockerMode string
	// Git, if set, is the repo's local mirror; it is mounted read-only at
	// its own path so checkouts borrow from it, as on the host.
	Git    *gitmirror.Mirror
	Docker Docker // nil: DockerCLI

	testMounts []string // extra -v mounts, for tests

	image      string // tag of the built image; set by Prepare
	dockerRoot string // the daemon's data directory; set by Prepare
	socket     string // the daemon's socket, as a path on the daemon's host; set by Prepare
}

var _ Backend = (*Container)(nil)

func (c *Container) OS() string       { return OSLinux }
func (c *Container) Arch() string     { return c.Target }
func (c *Container) Labels() []string { return SelfHostedLabels(OSLinux, c.Target) }

// Image is the local tag of the job image, once Prepare has made it.
func (c *Container) Image() string { return c.image }

func (c *Container) docker() Docker {
	if c.Docker != nil {
		return c.Docker
	}
	return DockerCLI{}
}

// ContainerArches lists the Linux architectures this machine can run job
// containers for, native first. Apple Silicon also runs x64 under Rosetta
// (OrbStack and Docker Desktop turn it on by default). An x64 machine isn't
// offered arm64: QEMU emulation is too slow for CI.
func ContainerArches() []string {
	switch HostArch() {
	case ArchARM:
		if runtime.GOOS == "darwin" {
			return []string{ArchARM, ArchX64}
		}
		return []string{ArchARM}
	case ArchX64:
		return []string{ArchX64}
	}
	return nil
}

func platform(arch string) string {
	if arch == ArchX64 {
		return "linux/amd64"
	}
	return "linux/arm64"
}

// imageContext is the build context: the Dockerfile, the entrypoint and the
// docker proxy's source.
func imageContext() ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range imageFiles {
		b, err := imageFS.ReadFile("image/" + name)
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), ModTime: time.Unix(0, 0)}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(b); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), tw.Close()
}

// ImageTag names the image for a runner version and architecture. It changes
// whenever the Dockerfile, entrypoint or proxy source does, which is what triggers a
// rebuild.
func ImageTag(version, arch string) string {
	h := sha256.New()
	for _, name := range imageFiles {
		b, _ := imageFS.ReadFile("image/" + name)
		fmt.Fprintf(h, "%s\x00%d\x00", name, len(b))
		h.Write(b)
	}
	return fmt.Sprintf("spareci-runner:%s-%s-%s", version, arch, hex.EncodeToString(h.Sum(nil))[:12])
}

// CheckRuntime reports whether a Docker-API runtime that runs Linux
// containers answers, as an error wrapping ErrNoRuntime if not. It is the
// cheap first step of Prepare, for callers that only need to know.
func CheckRuntime(ctx context.Context, d Docker) error {
	if d == nil {
		d = DockerCLI{}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := d.Output(ctx, nil, "version", "--format", "{{.Server.Os}}")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("%w: docker CLI not found", ErrNoRuntime)
		}
		return fmt.Errorf("%w: %v", ErrNoRuntime, err)
	}
	if osName := strings.TrimSpace(string(out)); osName != "linux" {
		return fmt.Errorf("%w: the docker daemon runs %q containers, not linux", ErrNoRuntime, osName)
	}
	return nil
}

// Prepare checks the runtime and builds the image if this version of it isn't
// there yet. An error wrapping ErrNoRuntime means there is no usable runtime:
// the machine should take no Linux jobs, rather than fail them. buildLog
// receives the build's output (it takes minutes the first time).
func (c *Container) Prepare(ctx context.Context, buildLog io.Writer) error {
	if c.Target != ArchARM && c.Target != ArchX64 {
		return fmt.Errorf("container arch %q: want %s or %s", c.Target, ArchARM, ArchX64)
	}
	if c.Version == "" {
		return errors.New("container: no runner version")
	}
	switch c.DockerMode {
	case "":
		c.DockerMode = DockerSocket
	case DockerSocket, DockerNone:
	default:
		return fmt.Errorf("container docker mode %q: want %q or %q", c.DockerMode, DockerSocket, DockerNone)
	}
	d := c.docker()
	if err := CheckRuntime(ctx, d); err != nil {
		return err
	}
	if c.DockerMode == DockerSocket {
		c.socket = daemonSocket(runtime.GOOS, os.Getenv("DOCKER_HOST"), func() string { return DockerHost(os.Environ()) })
		if c.socket == "" {
			slog.Warn("the docker daemon isn't on a local unix socket; Linux jobs get no docker", "DOCKER_HOST", os.Getenv("DOCKER_HOST"))
			c.DockerMode = DockerNone
		}
	}
	if c.DockerMode == DockerSocket {
		out, err := d.Output(ctx, nil, "info", "--format", "{{.DockerRootDir}}")
		if err != nil {
			return fmt.Errorf("%w: %v", ErrNoRuntime, err)
		}
		c.dockerRoot = strings.TrimSpace(string(out))
		if !path.IsAbs(c.dockerRoot) || strings.ContainsAny(c.dockerRoot, " \t=") {
			return fmt.Errorf("docker info: unexpected data directory %q", c.dockerRoot)
		}
	}

	tag := ImageTag(c.Version, c.Target)
	if _, err := d.Output(ctx, nil, "image", "inspect", "--format", "{{.Id}}", tag); err != nil {
		slog.Info("building the Linux job image; the first build takes a few minutes", "image", tag, "platform", platform(c.Target))
		bctx, err := imageContext()
		if err != nil {
			return err
		}
		start := time.Now()
		out, err := d.Output(ctx, bytes.NewReader(bctx), "build",
			"--platform", platform(c.Target),
			"--build-arg", "RUNNER_VERSION="+c.Version,
			"--label", "dev.spareci.image=runner",
			"--label", "dev.spareci.arch="+c.Target,
			"--progress", "plain",
			"-t", tag, "-")
		if buildLog != nil {
			_, _ = buildLog.Write(out)
		}
		if err != nil {
			return fmt.Errorf("build %s: %w", tag, err)
		}
		slog.Info("Linux job image built", "image", tag, "took", time.Since(start).Round(time.Second))
		c.pruneImages(ctx, tag)
	}
	// Runs the runner's own binary, which proves the platform works (Rosetta
	// or QEMU for x64 on Apple Silicon) before any job is taken.
	if _, err := d.Output(ctx, nil, "run", "--rm", "--pull", "never", "--platform", platform(c.Target),
		"--entrypoint", "/opt/runner/bin/Runner.Listener", tag, "--version"); err != nil {
		return fmt.Errorf("%w: %s containers don't start: %v", ErrNoRuntime, platform(c.Target), err)
	}
	c.image = tag
	return nil
}

// pruneImages removes older spareci images for this arch, once a new one
// exists.
func (c *Container) pruneImages(ctx context.Context, keep string) {
	out, err := c.docker().Output(ctx, nil, "image", "ls",
		"--filter", "label=dev.spareci.image=runner", "--filter", "label=dev.spareci.arch="+c.Target,
		"--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return
	}
	for _, ref := range strings.Fields(string(out)) {
		if ref != keep && strings.HasPrefix(ref, "spareci-runner:") {
			if _, err := c.docker().Output(ctx, nil, "image", "rm", ref); err == nil {
				slog.Info("removed old Linux job image", "image", ref)
			}
		}
	}
}

// Prune removes containers and work volumes a crash left behind.
func (c *Container) Prune(ctx context.Context) {
	d := c.docker()
	filter := "label=dev.spareci.root=" + c.Root
	if out, err := d.Output(ctx, nil, "ps", "-aq", "--filter", filter); err == nil {
		if ids := strings.Fields(string(out)); len(ids) > 0 {
			_, _ = d.Output(ctx, nil, append([]string{"rm", "-f", "-v"}, ids...)...)
		}
	}
	if out, err := d.Output(ctx, nil, "volume", "ls", "-q", "--filter", filter, "--filter", "label=dev.spareci.job"); err == nil {
		if names := strings.Fields(string(out)); len(names) > 0 {
			_, _ = d.Output(ctx, nil, append([]string{"volume", "rm", "-f"}, names...)...)
		}
	}
}

// daemonSocket is the path of the docker daemon's socket on the daemon's own
// host, which is what a container mounts. On macOS the daemon runs in a VM
// (OrbStack, Docker Desktop, Colima) at the standard path there, whatever
// socket the Mac side uses. On Linux the daemon is local, so it is the socket
// the CLI talks to: DOCKER_HOST, else the current context's (rootless Docker's
// is under $XDG_RUNTIME_DIR). "" if that isn't a unix socket.
func daemonSocket(goos, dockerHost string, contextHost func() string) string {
	const std = "/var/run/docker.sock"
	if goos == "darwin" {
		return std
	}
	host := dockerHost
	if host == "" {
		host = contextHost()
	}
	if host == "" {
		return std
	}
	if p, ok := strings.CutPrefix(host, "unix://"); ok && path.IsAbs(p) {
		return p
	}
	return ""
}

// volumeName makes a docker volume name from parts.
func volumeName(parts ...string) string {
	n := strings.Join(parts, "-")
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			return r
		}
		return '_'
	}, n)
}

// ToolCacheVolume is the per-repo, per-arch volume mounted at
// /opt/hostedtoolcache, so setup-* actions download a version once.
func (c *Container) ToolCacheVolume() string {
	return volumeName("spareci-toolcache", c.Repo, c.Target)
}

// jobEnv turns the env the pool gives a job into what the container gets. The
// local cache listens on the host's loopback: Docker on macOS reaches that as
// host.docker.internal (OrbStack and Docker Desktop forward it), so its URLs
// are rewritten. Elsewhere the container can't reach it and the job uses
// GitHub's cache, as without spareci (the cache would have to listen on the
// docker bridge).
func containerEnv(env []string, goos string) []string {
	cacheVars := map[string]bool{"ACTIONS_RESULTS_URL": true, "SPARECI_RESULTS_URL": true, "ACTIONS_CACHE_SERVICE_V2": true}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case k == "SPARECI_BUILDKIT_RESULTS_URL":
			continue // the docker shim's; containers have no shim and need none
		case cacheVars[k] && goos != "darwin":
			continue
		case cacheVars[k]:
			v = strings.Replace(v, "//127.0.0.1:", "//host.docker.internal:", 1)
		}
		out = append(out, k+"="+v)
	}
	return out
}

// runArgs are the docker run arguments for one job.
func (c *Container) runArgs(name, envFile, workVol string) []string {
	args := []string{"run", "--name", name,
		"--label", "dev.spareci.job=" + name, "--label", "dev.spareci.root=" + c.Root,
		"--platform", platform(c.Target), "--pull", "never",
		// The runner treats SIGINT as "finish up and deregister". Only
		// docker stop sends it: a signal to the CLI isn't passed on.
		"--stop-signal", "SIGINT", "--stop-timeout", "30", "--sig-proxy=false",
		"--env-file", envFile,
		"-v", c.ToolCacheVolume() + ":/opt/hostedtoolcache",
	}
	if runtime.GOOS != "darwin" {
		// Docker Engine on Linux doesn't define it; macOS runtimes do, and
		// there it must stay theirs (it reaches the Mac, not the VM).
		args = append(args, "--add-host", "host.docker.internal:host-gateway")
	}
	if c.CPUs != "" {
		args = append(args, "--cpus", c.CPUs)
	}
	if c.Memory != "" {
		args = append(args, "--memory", c.Memory)
	}
	if c.Git != nil {
		for _, kv := range c.Git.Env() {
			if v, ok := strings.CutPrefix(kv, "GIT_TEMPLATE_DIR="); ok {
				args = append(args, "-v", c.Git.Dir+":"+c.Git.Dir+":ro", "-v", v+":"+v+":ro")
			}
		}
	}
	if c.DockerMode == DockerSocket {
		// HOME, the runner and its workspace live in the job's own volume,
		// mounted at the same path in every job, so paths a job sees (and
		// caches keyed on them) don't change from job to job. The daemon
		// resolves bind mounts on its own host, where the volume is
		// somewhere else: the proxy in front of the socket rewrites them.
		args = append(args,
			"-v", c.socket+":"+daemonSocketMount,
			"-v", workVol+":"+jobHome,
			"-e", "SPARECI_DOCKER_MAP="+strings.Join(c.dockerMaps(workVol), " "))
	}
	for _, m := range c.testMounts {
		args = append(args, "-v", m)
	}
	return append(args, c.image)
}

// dockerMaps are the proxy's path rewrites for a job: paths in its container
// that are bind-mountable, to where the daemon sees them.
func (c *Container) dockerMaps(workVol string) []string {
	maps := []string{
		jobHome + "=" + c.volumePath(workVol),
		// The runner mounts the tool cache into container: jobs (/__t).
		"/opt/hostedtoolcache=" + c.volumePath(c.ToolCacheVolume()),
	}
	if c.socket != "/var/run/docker.sock" {
		// The runner mounts the socket into container actions by this
		// path; on the daemon's host it is elsewhere (rootless Docker).
		maps = append(maps, "/var/run/docker.sock="+c.socket)
	}
	return maps
}

// volumePath is where the daemon stores a volume's data.
func (c *Container) volumePath(vol string) string {
	return path.Join(c.dockerRoot, "volumes", vol, "_data")
}

// Run starts a container for one job with the given JIT config, streams the
// runner's output to its log, and removes the container (and its work
// volume) when the runner exits. Cancelling ctx stops the runner gracefully
// (SIGINT), then forcibly after 30 seconds.
func (c *Container) Run(ctx context.Context, name, jitConfig string, env []string, onStart func(job string)) (Result, error) {
	res := Result{Log: filepath.Join(c.Root, "logs", name+".log")}
	if c.image == "" {
		return res, errors.New("container backend not prepared")
	}
	d := c.docker()
	jobsDir := filepath.Join(c.Root, "jobs")
	for _, dir := range []string{jobsDir, filepath.Dir(res.Log)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return res, err
		}
	}
	if c.Git != nil {
		env = append(env, c.Git.Env()...)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if err := c.Git.Update(ctx); err != nil {
				slog.Warn("git mirror not updated; checkouts fetch more from GitHub", "err", err)
			}
		}()
	}

	// Secrets go in an env file, not on docker's command line.
	envFile := filepath.Join(jobsDir, name+".env")
	lines := append([]string{"SPARECI_JIT_CONFIG=" + jitConfig}, containerEnv(env, runtime.GOOS)...)
	for _, l := range lines {
		if strings.ContainsAny(l, "\n\r") {
			return res, fmt.Errorf("container env: %q has a newline", strings.SplitN(l, "=", 2)[0])
		}
	}
	if err := os.WriteFile(envFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return res, err
	}
	defer os.Remove(envFile)

	workVol := ""
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = d.Output(ctx, nil, "rm", "-f", "-v", name)
		if workVol != "" {
			_, _ = d.Output(ctx, nil, "volume", "rm", "-f", workVol)
		}
	}
	defer cleanup()
	if c.DockerMode == DockerSocket {
		workVol = volumeName("spareci-job", name)
		if _, err := d.Output(ctx, nil, "volume", "create",
			"--label", "dev.spareci.job="+name, "--label", "dev.spareci.root="+c.Root, workVol); err != nil {
			return res, err
		}
	}

	logf, err := os.Create(res.Log)
	if err != nil {
		return res, err
	}
	defer logf.Close()

	pr, pw := io.Pipe()
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		watchRunner(pr, logf, name, &res, onStart)
	}()
	waitErr := make(chan error, 1)
	go func() {
		waitErr <- d.Stream(pw, c.runArgs(name, envFile, workVol)...)
		pw.Close()
	}()

	select {
	case err = <-waitErr:
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		// --stop-timeout: SIGINT, then SIGKILL after 30s.
		if _, serr := d.Output(sctx, nil, "stop", name); serr != nil {
			_, _ = d.Output(sctx, nil, "kill", name)
		}
		cancel()
		err = <-waitErr
	}
	<-scanDone
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, err
}
