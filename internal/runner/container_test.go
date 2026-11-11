package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRuntime records commands and answers them from a script.
type fakeRuntime struct {
	mu      sync.Mutex
	calls   [][]string
	images  map[string]bool
	down    bool          // no daemon
	osName  string        // docker version's Server.Os
	stream  string        // what docker run prints
	block   chan struct{} // if set, docker run waits for it (or a stop)
	stopped chan struct{}
	envFile string // contents of --env-file at run time
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{images: map[string]bool{}, osName: "linux", stopped: make(chan struct{})}
}

func (f *fakeRuntime) record(args []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
}

func (f *fakeRuntime) called(sub ...string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if len(c) >= len(sub) && slices.Equal(c[:len(sub)], sub) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeRuntime) Output(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	f.record(args)
	if f.down {
		return nil, errors.New("Cannot connect to the Docker daemon")
	}
	switch args[0] {
	case "version":
		return []byte(f.osName + "\n"), nil
	case "info":
		return []byte("/var/lib/docker\n"), nil
	case "image":
		if args[1] == "inspect" {
			if f.images[args[len(args)-1]] {
				return []byte("sha256:x\n"), nil
			}
			return nil, errors.New("No such image")
		}
		if args[1] == "ls" {
			return []byte("spareci-runner:old-arm64-0000\n"), nil
		}
	case "build":
		if stdin == nil {
			return nil, errors.New("no build context")
		}
		b, _ := io.ReadAll(stdin)
		if !strings.Contains(string(b), "FROM ubuntu:24.04") {
			return nil, errors.New("context has no Dockerfile")
		}
		for i, a := range args {
			if a == "-t" {
				f.images[args[i+1]] = true
			}
		}
	case "stop":
		close(f.stopped)
	}
	return nil, nil
}

func (f *fakeRuntime) Stream(w io.Writer, args ...string) error {
	f.record(args)
	for i, a := range args {
		if a == "--env-file" {
			b, _ := os.ReadFile(args[i+1])
			f.envFile = string(b)
		}
	}
	io.WriteString(w, f.stream)
	if f.block != nil {
		select {
		case <-f.block:
		case <-f.stopped:
			io.WriteString(w, "Runner listener exit because of stop\n")
		}
	}
	return nil
}

func prepared(t *testing.T, f *fakeRuntime, c *Container) *Container {
	t.Helper()
	if c == nil {
		c = &Container{}
	}
	c.Root, c.Version, c.Repo, c.Docker = t.TempDir(), "2.338.2", "Row-Labs/spareci", f
	if c.Target == "" {
		c.Target = ArchARM
	}
	if err := c.Prepare(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestContainerIsLinuxBackend(t *testing.T) {
	var b Backend = &Container{Target: ArchX64}
	if b.OS() != OSLinux || b.Arch() != ArchX64 {
		t.Errorf("OS, Arch = %s, %s", b.OS(), b.Arch())
	}
	if got := strings.Join(b.Labels(), ","); got != "self-hosted,Linux,X64" {
		t.Errorf("Labels = %s", got)
	}
}

func TestPrepareNoRuntime(t *testing.T) {
	f := newFakeRuntime()
	f.down = true
	c := &Container{Root: t.TempDir(), Version: "2.338.2", Target: ArchARM, Docker: f}
	err := c.Prepare(context.Background(), nil)
	if !errors.Is(err, ErrNoRuntime) {
		t.Fatalf("err = %v, want ErrNoRuntime", err)
	}
	if len(f.called("build")) != 0 {
		t.Error("built an image without a runtime")
	}
	if _, err := c.Run(context.Background(), "r", "jit", nil, nil); err == nil {
		t.Error("Run without Prepare succeeded")
	}
}

func TestPrepareMissingDockerCLI(t *testing.T) {
	c := &Container{Root: t.TempDir(), Version: "2.338.2", Target: ArchARM, Docker: DockerCLI{Bin: filepath.Join(t.TempDir(), "no-docker")}}
	if err := c.Prepare(context.Background(), nil); !errors.Is(err, ErrNoRuntime) {
		t.Fatalf("err = %v, want ErrNoRuntime", err)
	}
}

func TestPrepareWindowsDaemon(t *testing.T) {
	f := newFakeRuntime()
	f.osName = "windows"
	c := &Container{Root: t.TempDir(), Version: "2.338.2", Target: ArchARM, Docker: f}
	if err := c.Prepare(context.Background(), nil); !errors.Is(err, ErrNoRuntime) {
		t.Fatalf("err = %v, want ErrNoRuntime", err)
	}
}

func TestPrepareBuildsOnceAndPrunes(t *testing.T) {
	f := newFakeRuntime()
	c := prepared(t, f, &Container{Target: ArchX64})
	builds := f.called("build")
	if len(builds) != 1 {
		t.Fatalf("builds = %d, want 1", len(builds))
	}
	b := strings.Join(builds[0], " ")
	for _, want := range []string{"--platform linux/amd64", "RUNNER_VERSION=2.338.2", "-t " + ImageTag("2.338.2", ArchX64)} {
		if !strings.Contains(b, want) {
			t.Errorf("build %q lacks %q", b, want)
		}
	}
	if len(f.called("image", "rm", "spareci-runner:old-arm64-0000")) != 1 {
		t.Error("old image not removed")
	}
	if err := c.Prepare(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if n := len(f.called("build")); n != 1 {
		t.Errorf("rebuilt an existing image: %d builds", n)
	}
}

func TestImageTagTracksVersionAndArch(t *testing.T) {
	a, b, x := ImageTag("2.338.2", ArchARM), ImageTag("2.339.0", ArchARM), ImageTag("2.338.2", ArchX64)
	if a == b || a == x || !strings.HasPrefix(a, "spareci-runner:2.338.2-arm64-") {
		t.Errorf("tags %s %s %s", a, b, x)
	}
}

func TestContainerEnv(t *testing.T) {
	in := []string{
		"ACTIONS_RESULTS_URL=http://127.0.0.1:5000/",
		"SPARECI_RESULTS_URL=http://127.0.0.1:5000/",
		"ACTIONS_CACHE_SERVICE_V2=true",
		"SPARECI_BUILDKIT_RESULTS_URL=http://host.docker.internal:5000/",
		"GIT_TEMPLATE_DIR=/x/repo.template",
	}
	mac := strings.Join(containerEnv(in, "darwin"), " ")
	if want := "ACTIONS_RESULTS_URL=http://host.docker.internal:5000/ SPARECI_RESULTS_URL=http://host.docker.internal:5000/ ACTIONS_CACHE_SERVICE_V2=true GIT_TEMPLATE_DIR=/x/repo.template"; mac != want {
		t.Errorf("darwin:\n got %s\nwant %s", mac, want)
	}
	if linux := strings.Join(containerEnv(in, "linux"), " "); linux != "GIT_TEMPLATE_DIR=/x/repo.template" {
		t.Errorf("linux: %s", linux)
	}
}

func TestContainerRun(t *testing.T) {
	f := newFakeRuntime()
	f.stream = "√ Connected to GitHub\n2026-11-11 10:00:00Z: Running job: build\n2026-11-11 10:01:00Z: Job build completed with result: Succeeded\n"
	c := prepared(t, f, &Container{CPUs: "4", Memory: "8g"})
	var started string
	res, err := c.Run(context.Background(), "spareci-mac-1-1", "JITSECRET", []string{"ACTIONS_RESULTS_URL=http://127.0.0.1:5000/"}, func(j string) { started = j })
	if err != nil {
		t.Fatal(err)
	}
	if res.Job != "build" || res.Conclusion != "Succeeded" || started != "build" {
		t.Errorf("res = %+v, started %q", res, started)
	}
	runs := f.called("run", "--name")
	if len(runs) != 1 {
		t.Fatalf("docker runs = %v", runs)
	}
	args := strings.Join(runs[0], " ")
	if strings.Contains(args, "JITSECRET") {
		t.Error("JIT config on docker's command line")
	}
	vol := "spareci-job-spareci-mac-1-1"
	for _, want := range []string{
		"--platform linux/arm64", "--stop-signal SIGINT", "--cpus 4", "--memory 8g",
		"-v spareci-toolcache-Row-Labs_spareci-arm64:/opt/hostedtoolcache",
		"-v /var/run/docker.sock:/var/run/docker.sock",
		"-v " + vol + ":/var/lib/docker/volumes/" + vol + "/_data",
		"SPARECI_RUNNER_ROOT=/var/lib/docker/volumes/" + vol + "/_data",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("docker run lacks %q:\n%s", want, args)
		}
	}
	if !strings.HasSuffix(args, " "+c.Image()) {
		t.Errorf("image is not last: %s", args)
	}
	if !strings.Contains(f.envFile, "SPARECI_JIT_CONFIG=JITSECRET\n") {
		t.Errorf("env file = %q", f.envFile)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(f.envFile, "ACTIONS_RESULTS_URL=http://host.docker.internal:5000/") {
		t.Errorf("cache URL not rewritten: %q", f.envFile)
	}
	if len(f.called("rm", "-f", "-v", "spareci-mac-1-1")) != 1 || len(f.called("volume", "rm", "-f", vol)) != 1 {
		t.Error("container or work volume not removed")
	}
	if _, err := os.Stat(filepath.Join(c.Root, "jobs", "spareci-mac-1-1.env")); !os.IsNotExist(err) {
		t.Error("env file left behind")
	}
	log, _ := os.ReadFile(res.Log)
	if !strings.Contains(string(log), "Running job: build") {
		t.Errorf("log = %q", log)
	}
}

func TestContainerRunNoDocker(t *testing.T) {
	f := newFakeRuntime()
	c := prepared(t, f, &Container{DockerMode: DockerNone})
	if _, err := c.Run(context.Background(), "r1", "jit", nil, nil); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(f.called("run", "--name")[0], " ")
	if strings.Contains(args, "docker.sock") || strings.Contains(args, "SPARECI_RUNNER_ROOT") {
		t.Errorf("docker passed through with DockerNone: %s", args)
	}
	if len(f.called("volume", "create")) != 0 {
		t.Error("work volume created without docker passthrough")
	}
	if len(f.called("info")) != 0 {
		t.Error("asked for the daemon's data dir without needing it")
	}
}

func TestContainerRunCancel(t *testing.T) {
	f := newFakeRuntime()
	f.block = make(chan struct{})
	c := prepared(t, f, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Run(ctx, "r2", "jit", nil, nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return after cancel")
	}
	if len(f.called("stop", "r2")) != 1 {
		t.Error("container not stopped")
	}
	if len(f.called("rm", "-f", "-v", "r2")) != 1 {
		t.Error("container not removed")
	}
}

func TestContainerPrune(t *testing.T) {
	f := newFakeRuntime()
	c := &Container{Root: "/cache/spareci", Docker: f}
	c.Prune(context.Background())
	if len(f.called("ps", "-aq", "--filter", "label=dev.spareci.root=/cache/spareci")) != 1 {
		t.Errorf("calls = %v", f.calls)
	}
}

func TestContainerArches(t *testing.T) {
	got := ContainerArches()
	if HostArch() != "" && (len(got) == 0 || got[0] != HostArch()) {
		t.Errorf("ContainerArches() = %v, want native %s first", got, HostArch())
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" && fmt.Sprint(got) != "[arm64 x64]" {
		t.Errorf("Apple Silicon: %v", got)
	}
}

func TestDaemonSocket(t *testing.T) {
	none := func() string { return "" }
	for _, tc := range []struct {
		goos, dockerHost, context, want string
	}{
		{"darwin", "unix:///Users/me/.orbstack/run/docker.sock", "", "/var/run/docker.sock"},
		{"linux", "", "", "/var/run/docker.sock"},
		{"linux", "unix:///run/user/1000/docker.sock", "", "/run/user/1000/docker.sock"},
		{"linux", "", "unix:///run/user/1000/docker.sock", "/run/user/1000/docker.sock"},
		{"linux", "tcp://10.0.0.2:2376", "", ""},
	} {
		ctx := none
		if tc.context != "" {
			v := tc.context
			ctx = func() string { return v }
		}
		if got := daemonSocket(tc.goos, tc.dockerHost, ctx); got != tc.want {
			t.Errorf("daemonSocket(%s, %q, %q) = %q, want %q", tc.goos, tc.dockerHost, tc.context, got, tc.want)
		}
	}
}
