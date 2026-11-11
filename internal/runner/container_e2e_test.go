package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestContainerE2E builds the job image for every arch this machine offers
// and checks it against a real runtime: the tools jobs expect, the runner user
// and docker passthrough, and that a container is cleaned up after a runner
// that can't register. Slow the first time (image builds).
//
//	SPARECI_TEST_CONTAINER=1 go test ./internal/runner -run TestContainerE2E -v -timeout 30m
func TestContainerE2E(t *testing.T) {
	if os.Getenv("SPARECI_TEST_CONTAINER") == "" {
		t.Skip("set SPARECI_TEST_CONTAINER=1 to test against a real container runtime")
	}
	version := os.Getenv("SPARECI_RUNNER_VERSION")
	if version == "" {
		version = "2.338.2"
	}
	for _, arch := range ContainerArches() {
		t.Run(arch, func(t *testing.T) {
			ctx := context.Background()
			c := &Container{Root: t.TempDir(), Version: version, Target: arch, Repo: "spareci/e2e-test"}
			t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", c.ToolCacheVolume()).Run() })
			start := time.Now()
			var log strings.Builder
			if err := c.Prepare(ctx, &log); err != nil {
				t.Fatalf("prepare: %v\n%s", err, log.String())
			}
			t.Logf("prepare took %s (image %s)", time.Since(start).Round(time.Second), c.Image())

			// The tools the Dockerfile promises, as the runner user, with the
			// docker socket passed through as Run does.
			check := `set -e
for t in git git-lfs curl wget jq unzip zip xz zstd make gcc g++ python3 pip3 sudo docker shasum rsync ssh tini gh; do command -v $t >/dev/null || { echo "missing $t"; exit 1; }; done
docker buildx version >/dev/null; docker compose version >/dev/null
test "$(id -u)" = 1001
sudo -n true
test "$(uname -m)" = "$EXPECT"
test -w /opt/hostedtoolcache
grep -q ACTIONS_RESULTS_ORL /opt/runner-cache/bin/Runner.Worker.dll 2>/dev/null || python3 -c 'import sys; d=open("/opt/runner-cache/bin/Runner.Worker.dll","rb").read(); sys.exit(0 if "ACTIONS_RESULTS_ORL".encode("utf-16-le") in d else 1)'
echo tools-ok`
			expect := map[string]string{ArchARM: "aarch64", ArchX64: "x86_64"}[arch]
			out, err := exec.Command("docker", "run", "--rm", "--platform", platform(arch), "-u", "runner",
				"-e", "EXPECT="+expect, "-v", c.ToolCacheVolume()+":/opt/hostedtoolcache",
				"--entrypoint", "bash", c.Image(), "-c", check).CombinedOutput()
			if err != nil || !strings.Contains(string(out), "tools-ok") {
				t.Fatalf("image check: %v\n%s", err, out)
			}

			// A JIT config GitHub won't accept: the runner starts, fails to
			// register and exits, and the container and volume must go.
			name := "spareci-e2e-" + arch + "-" + time.Now().Format("150405")
			res, err := c.Run(ctx, name, "bm90IGEgaml0IGNvbmZpZw==", nil, nil)
			if res.Job != "" {
				t.Errorf("ran a job with a bogus JIT config: %+v", res)
			}
			b, _ := os.ReadFile(res.Log)
			t.Logf("runner exit: %v; log tail:\n%s", err, tail(string(b), 8))
			if len(b) == 0 {
				t.Error("no runner output captured")
			}
			if out, _ := exec.Command("docker", "ps", "-aq", "--filter", "name="+name).Output(); len(strings.TrimSpace(string(out))) > 0 {
				t.Error("container left behind")
			}
			if out, _ := exec.Command("docker", "volume", "ls", "-q", "--filter", "name=spareci-job-"+name).Output(); len(strings.TrimSpace(string(out))) > 0 {
				t.Error("work volume left behind")
			}
		})
	}
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// fakeRunner stands in for actions/runner: the entrypoint copies it to the
// runner's root and runs it as the runner would, so this checks the real
// entrypoint, volume layout and docker proxy without GitHub. It reports the
// paths caches are keyed on, and uses docker the ways jobs do.
const fakeRunner = `#!/bin/bash
set -euo pipefail
me=$JOB
ws=$PWD/_work/repo/repo
mkdir -p "$ws" && cd "$ws"
export COREPACK_ENABLE_DOWNLOAD_PROMPT=0
echo '{"name":"x","packageManager":"pnpm@10.20.0"}' > package.json
if [ ! -x /opt/hostedtoolcache/go-e2e/bin/go ]; then
  mkdir -p /opt/hostedtoolcache/go-e2e.tmp
  curl -fsSL "https://go.dev/dl/go1.27.3.linux-$(dpkg --print-architecture).tar.gz" | tar xz -C /opt/hostedtoolcache/go-e2e.tmp --strip-components=1
  mv /opt/hostedtoolcache/go-e2e.tmp /opt/hostedtoolcache/go-e2e 2>/dev/null || rm -rf /opt/hostedtoolcache/go-e2e.tmp
fi
echo "path pwd=$(pwd)"
echo "path home=$HOME"
echo "path pnpm=$(corepack pnpm store path 2>/dev/null | tail -1)"
echo "path npm=$(npm config get cache)"
echo "path gomodcache=$(/opt/hostedtoolcache/go-e2e/bin/go env GOMODCACHE)"

# Isolation: another job running now must not see or change this workspace.
echo "$me" > marker
sleep 4
echo "check own=$(cat marker)"
echo "check files=$(ls | tr '\n' ,)"

# Bind mounts resolve to this job's volume on the daemon: docker run as a
# step would, the runner's container: mounts, compose, and a build.
img=$IMAGE
echo "check run=$(docker run --rm --pull never --entrypoint cat -v "$PWD:/w" "$img" /w/marker)"
echo "check work=$(docker run --rm --pull never --entrypoint cat -v "$HOME/actions-runner/_work:/__w" "$img" /__w/repo/repo/marker)"
echo "check toolcache=$(docker run --rm --pull never --entrypoint ls -v /opt/hostedtoolcache:/__t:ro "$img" /__t | grep -c go-e2e)"
printf 'services:\n  t:\n    image: %s\n    pull_policy: never\n    entrypoint: ["cat", "/w/marker"]\n    volumes: ["./:/w"]\n' "$img" > compose.yml
echo "check compose=$(docker compose -p "c$me" run --rm -T t 2>/dev/null | tr -d '\r')"
docker compose -p "c$me" down >/dev/null 2>&1 || true
printf 'FROM %s\nCOPY marker /m\n' "$img" > Dockerfile
echo "check build=$(docker build -q -t "spareci-e2e-build-$me" . >/dev/null && docker run --rm --entrypoint cat "spareci-e2e-build-$me" /m)"
docker image rm "spareci-e2e-build-$me" >/dev/null
echo "check stdin=$(echo piped | docker run --rm -i --entrypoint cat "$img")"
`

// TestContainerPathsE2E runs two jobs at once and checks that they see the
// same paths (so caches keyed on them hit from job to job, as on GitHub),
// don't share a workspace, and that bind mounts from them reach their own
// volume on the daemon.
//
//	SPARECI_TEST_CONTAINER=1 go test ./internal/runner -run TestContainerPathsE2E -v -timeout 30m
func TestContainerPathsE2E(t *testing.T) {
	if os.Getenv("SPARECI_TEST_CONTAINER") == "" {
		t.Skip("set SPARECI_TEST_CONTAINER=1 to test against a real container runtime")
	}
	version := os.Getenv("SPARECI_RUNNER_VERSION")
	if version == "" {
		version = "2.338.2"
	}
	ctx := context.Background()
	fake, err := os.MkdirTemp("", "spareci-fake-runner")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(fake) })
	if err := os.WriteFile(filepath.Join(fake, "run.sh"), []byte(fakeRunner), 0o755); err != nil {
		t.Fatal(err)
	}
	// Explicitly: the file mode is subject to the umask.
	for _, p := range []string{fake, filepath.Join(fake, "run.sh")} {
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := &Container{Root: t.TempDir(), Version: version, Target: ContainerArches()[0], Repo: "spareci/e2e-test",
		testMounts: []string{fake + ":/opt/runner:ro"}}
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", c.ToolCacheVolume()).Run() })
	var buildLog strings.Builder
	if err := c.Prepare(ctx, &buildLog); err != nil {
		t.Fatalf("prepare: %v\n%s", err, buildLog.String())
	}

	stamp := time.Now().Format("150405")
	jobs := []string{"spareci-e2e-a-" + stamp, "spareci-e2e-b-" + stamp}
	reports := make([]map[string]string, len(jobs))
	var wg sync.WaitGroup
	for i, name := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := c.Run(ctx, name, "unused", []string{"JOB=" + name, "IMAGE=" + c.Image()}, nil)
			b, _ := os.ReadFile(res.Log)
			if err != nil {
				t.Errorf("%s: %v\n%s", name, err, b)
			}
			reports[i] = map[string]string{}
			for _, l := range strings.Split(string(b), "\n") {
				if kind, kv, ok := strings.Cut(l, " "); ok && (kind == "path" || kind == "check") {
					k, v, _ := strings.Cut(kv, "=")
					reports[i][kind+" "+k] = v
				}
			}
			t.Logf("%s:\n%s", name, b)
		}()
	}
	wg.Wait()

	for _, k := range []string{"path pwd", "path home", "path pnpm", "path npm", "path gomodcache"} {
		a, b := reports[0][k], reports[1][k]
		if a == "" || a != b {
			t.Errorf("%s: %q and %q, want the same non-empty path", k, a, b)
		}
	}
	want := map[string]string{
		"path pwd":        "/home/runner/actions-runner/_work/repo/repo",
		"path home":       "/home/runner",
		"path pnpm":       "/home/runner/.local/share/pnpm/store/v10",
		"path npm":        "/home/runner/.npm",
		"path gomodcache": "/home/runner/go/pkg/mod",
	}
	for i, name := range jobs {
		r := reports[i]
		for k, v := range want {
			if r[k] != v {
				t.Errorf("%s: %s = %q, want %q", name, k, r[k], v)
			}
		}
		for _, k := range []string{"own", "run", "work", "compose", "build"} {
			if r["check "+k] != name {
				t.Errorf("%s: %s saw %q, want its own workspace", name, k, r["check "+k])
			}
		}
		if r["check files"] != "marker,package.json," {
			t.Errorf("%s: workspace holds %q", name, r["check files"])
		}
		if r["check toolcache"] != "1" {
			t.Errorf("%s: tool cache not mounted through: %q", name, r["check toolcache"])
		}
		if r["check stdin"] != "piped" {
			t.Errorf("%s: stdin through docker run -i: %q", name, r["check stdin"])
		}
		if out, _ := exec.Command("docker", "volume", "ls", "-q", "--filter", "name=spareci-job-"+name).Output(); len(strings.TrimSpace(string(out))) > 0 {
			t.Errorf("%s: work volume left behind", name)
		}
	}
}
