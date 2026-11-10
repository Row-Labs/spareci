package runner

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Row-Labs/spareci/internal/cache"
)

// TestDockerBuildxGHACache does what setup-buildx-action and
// build-push-action do in a job, in the environment spareci gives jobs: a
// separate home, the docker shim on PATH and the local cache. It builds the
// same image twice with a fresh builder in between, and the second build must
// come from the local cache. It needs a running Docker, pulls images, and
// only runs when asked:
//
//	SPARECI_TEST_DOCKER=1 go test ./internal/runner -run TestDockerBuildxGHACache -v
func TestDockerBuildxGHACache(t *testing.T) {
	if os.Getenv("SPARECI_TEST_DOCKER") == "" {
		t.Skip("set SPARECI_TEST_DOCKER=1 to build with docker")
	}
	store, err := cache.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := cache.NewServer(store, cache.DefaultUpstream)
	if err != nil {
		t.Fatal(err)
	}
	jl, err := srv.Register("acme/app")
	if err != nil {
		t.Fatal(err)
	}
	defer jl.Close()

	userHome, _ := os.UserHomeDir()
	jobHome := t.TempDir()
	if err := LinkDockerPlugins(userHome, os.Getenv("DOCKER_CONFIG"), jobHome); err != nil {
		t.Fatal(err)
	}
	env := JobEnv(os.Environ(), userHome, jobHome)
	var path string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	shims, err := WriteDockerShim(filepath.Join(t.TempDir(), "shims"), path)
	if err != nil || shims == "" {
		t.Fatalf("no docker on the job's PATH (%v)", err)
	}
	env = prependPath(env, shims)
	if h := DockerHost(os.Environ()); h != "" {
		env = append(env, "DOCKER_HOST="+h)
	}
	env = append(env, jl.Env()...)
	env = append(env, "ACTIONS_RUNTIME_TOKEN="+runtimeToken(), "ACTIONS_RESULTS_ORL=https://results.invalid/")

	docker := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(filepath.Join(shims, "docker"), args...) // first on the job's PATH
		cmd.Env = env
		cmd.Dir = t.TempDir()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	builder := fmt.Sprintf("spareci-test-%d", time.Now().UnixNano())
	docker("buildx", "create", "--name", builder, "--driver", "docker-container")
	defer exec.Command("docker", "buildx", "rm", "-f", builder).Run()

	ctxDir := t.TempDir()
	dockerfile := fmt.Sprintf("FROM alpine:3.20\nRUN echo %d > /stamp && head -c 5000000 /dev/urandom > /blob\n", time.Now().UnixNano())
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	build := func() string {
		return docker("buildx", "build", "--builder", builder, "--progress", "plain",
			"--cache-to", "type=gha,mode=max,scope=spareci-test", "--cache-from", "type=gha,scope=spareci-test", ctxDir)
	}
	first := build()
	if len(store.List("acme/app")) == 0 {
		t.Fatalf("nothing saved to the local cache:\n%s", first)
	}
	docker("buildx", "prune", "--builder", builder, "-af")
	second := build()
	if !runCached(second) {
		t.Fatalf("second build did not come from the cache:\n%s", second)
	}
}

// runCached reports whether the Dockerfile's RUN step was a cache hit.
func runCached(out string) bool {
	var step string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && strings.HasPrefix(f[0], "#") && strings.Contains(line, "RUN echo") {
			step = f[0]
		}
		if step != "" && len(f) >= 2 && f[0] == step && f[1] == "CACHED" {
			return true
		}
	}
	return false
}

// runtimeToken is an unsigned ACTIONS_RUNTIME_TOKEN with the claims BuildKit
// checks (ac, exp, nbf).
func runtimeToken() string {
	ac, _ := json.Marshal([]cache.Scope{{Scope: "refs/heads/main", Permission: 3}})
	now := time.Now().Unix()
	claims, _ := json.Marshal(map[string]any{"ac": string(ac), "exp": now + 3600, "nbf": now - 60})
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc(claims) + "." + enc([]byte("sig"))
}
