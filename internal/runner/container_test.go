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
	ps      string // docker ps output
	volumes string // docker volume ls output
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
	case "ps":
		return []byte(f.ps), nil
	case "volume":
		if args[1] == "ls" {
			return []byte(f.volumes), nil
		}
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
		"-v /var/run/docker.sock:/run/spareci/docker.sock",
		"-v " + vol + ":/home/runner ",
		"SPARECI_DOCKER_MAP=/home/runner=/var/lib/docker/volumes/" + vol + "/_data" +
			" /opt/hostedtoolcache=/var/lib/docker/volumes/spareci-toolcache-Row-Labs_spareci-arm64/_data ",
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

// Every job of a repo sees the same paths: its work volume is mounted at the
// same place whatever the volume is called, and only the daemon-side paths
// the proxy maps to differ. Caches keyed on paths (setup-node's pnpm store)
// then match from job to job, as on GitHub.
func TestContainerJobsSeeTheSamePaths(t *testing.T) {
	f := newFakeRuntime()
	c := prepared(t, f, &Container{})
	for _, name := range []string{"spareci-mac-1-1", "spareci-mac-2-7"} {
		if _, err := c.Run(context.Background(), name, "jit", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	runs := f.called("run", "--name")
	if len(runs) != 2 {
		t.Fatalf("runs = %v", runs)
	}
	var mounts [2][]string
	for i, r := range runs {
		for j := 0; j+1 < len(r); j++ {
			if r[j] == "-v" {
				_, target, _ := strings.Cut(r[j+1], ":")
				mounts[i] = append(mounts[i], target)
			}
		}
	}
	if strings.Join(mounts[0], " ") != strings.Join(mounts[1], " ") {
		t.Errorf("jobs mount different paths:\n%v\n%v", mounts[0], mounts[1])
	}
	a, b := strings.Join(runs[0], " "), strings.Join(runs[1], " ")
	if !strings.Contains(a, "spareci-job-spareci-mac-1-1:/home/runner") || !strings.Contains(b, "spareci-job-spareci-mac-2-7:/home/runner") {
		t.Errorf("jobs don't each get their own volume at /home/runner:\n%s\n%s", a, b)
	}
}

func TestContainerRunNoDocker(t *testing.T) {
	f := newFakeRuntime()
	c := prepared(t, f, &Container{DockerMode: DockerNone})
	if _, err := c.Run(context.Background(), "r1", "jit", nil, nil); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(f.called("run", "--name")[0], " ")
	if strings.Contains(args, "docker.sock") || strings.Contains(args, "SPARECI_DOCKER_MAP") || strings.Contains(args, "/home/runner") {
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

// Two repos' spareci share a cache root. Starting one removes its own
// leftovers and those of a repo no spareci is running for, but never the
// other running spareci's jobs, nor a running job of a spareci before 0.2.
func TestContainerPruneLeavesAnotherRunningReposJobs(t *testing.T) {
	root := t.TempDir()
	release, err := Claim(root, "o/live") // this process stands in for o/live's spareci
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	dead := filepath.Join(root, "state", RepoKey("o/dead")+".pid")
	if err := os.WriteFile(dead, []byte("999999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFakeRuntime()
	f.ps = strings.Join([]string{
		"mine\to/me\trunning",
		"live\to/live\trunning",
		"dead\to/dead\trunning",
		"never\to/never\texited",
		"oldrun\t\trunning",
		"oldexit\t\texited",
	}, "\n") + "\n"
	f.volumes = "vmine\to/me\nvlive\to/live\nvdead\to/dead\nvold\t\n"
	c := &Container{Root: root, Repo: "o/me", Docker: f}
	c.Prune(context.Background())
	rm := f.called("rm", "-f", "-v")
	if len(rm) != 1 || strings.Join(rm[0][3:], " ") != "mine dead never oldexit" {
		t.Errorf("removed containers %v", rm)
	}
	vrm := f.called("volume", "rm", "-f")
	if len(vrm) != 1 || strings.Join(vrm[0][3:], " ") != "vmine vdead vold" {
		t.Errorf("removed volumes %v", vrm)
	}
}

func TestHostPruneLeavesAnotherRunningReposJobs(t *testing.T) {
	root := t.TempDir()
	release, err := Claim(root, "o/live")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	jobs := filepath.Join(root, "jobs")
	for _, d := range []string{"o_me/spareci-x-1-1", "o_live/spareci-x-1-2", "o_dead/spareci-x-1-3", "spareci-x-1-4"} {
		if err := os.MkdirAll(filepath.Join(jobs, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	(&Host{Root: root, Repo: "o/me"}).Prune()
	var left []string
	entries, _ := os.ReadDir(jobs)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if got := strings.Join(left, " "); got != "o_live spareci-x-1-4" {
		t.Errorf("left %s; want o_live (running) and spareci-x-1-4 (a spareci before 0.2)", got)
	}
}

func TestClaimRelease(t *testing.T) {
	root := t.TempDir()
	release, err := Claim(root, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if !live(root, RepoKey("o/r")) {
		t.Fatal("claimed repo not live")
	}
	release()
	if live(root, RepoKey("o/r")) {
		t.Error("released repo still live")
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
