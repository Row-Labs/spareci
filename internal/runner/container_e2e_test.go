package runner

import (
	"context"
	"os"
	"os/exec"
	"strings"
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
			start := time.Now()
			var log strings.Builder
			if err := c.Prepare(ctx, &log); err != nil {
				t.Fatalf("prepare: %v\n%s", err, log.String())
			}
			t.Logf("prepare took %s (image %s)", time.Since(start).Round(time.Second), c.Image())

			// The tools the Dockerfile promises, as the runner user, with the
			// docker socket passed through as Run does.
			check := `set -e
for t in git git-lfs curl wget jq unzip zip xz zstd make gcc g++ python3 pip3 sudo docker shasum rsync ssh tini; do command -v $t >/dev/null || { echo "missing $t"; exit 1; }; done
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
