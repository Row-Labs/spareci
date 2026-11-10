package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Docker on the host needs three things from spareci that a hosted runner
// gets for free:
//
//   - The CLI plugins (buildx, compose) live in the user's ~/.docker, which a
//     separate job home hides. LinkDockerPlugins links them into the job's.
//   - The daemon may only be reachable through the user's docker context
//     (Colima, for example, has no /var/run/docker.sock), which a separate job
//     home also hides. DockerHost finds its endpoint for DOCKER_HOST.
//   - BuildKit runs in a container (or the Docker VM on macOS), so its
//     type=gha cache can't reach the local cache on 127.0.0.1, and a failed
//     cache export fails the build. The docker shim (WriteDockerShim) gives
//     docker commands a URL BuildKit can reach instead.

// dockerShim wraps the real docker CLI. It only changes ACTIONS_RESULTS_URL,
// and only while it is spareci's local cache.
const dockerShim = `#!/bin/sh
# Written by spareci (internal/runner/docker.go).
# BuildKit runs inside a container or VM, where spareci's local cache on
# 127.0.0.1 is out of reach. Point docker (and so buildx's type=gha cache) at
# a URL it can reach: the same cache via host.docker.internal on macOS, else
# GitHub's own cache service, as without spareci.
if [ -n "$SPARECI_RESULTS_URL" ] && [ "$ACTIONS_RESULTS_URL" = "$SPARECI_RESULTS_URL" ]; then
  if [ -n "$SPARECI_BUILDKIT_RESULTS_URL" ]; then
    ACTIONS_RESULTS_URL=$SPARECI_BUILDKIT_RESULTS_URL
  elif [ -n "$ACTIONS_RESULTS_ORL" ]; then
    ACTIONS_RESULTS_URL=$ACTIONS_RESULTS_ORL
  fi
  export ACTIONS_RESULTS_URL
fi
exec '@DOCKER@' "$@"
`

// WriteDockerShim writes a docker wrapper into dir for the docker found on
// path and returns dir, or "" if there is no docker to wrap (a shim would
// make jobs think docker exists).
func WriteDockerShim(dir, path string) (string, error) {
	real := lookPath("docker", path, dir)
	if real == "" {
		return "", nil
	}
	if strings.ContainsAny(real, "'\n") {
		return "", fmt.Errorf("docker path %q can't be quoted in a shell script", real)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "docker")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.ReplaceAll(dockerShim, "@DOCKER@", real)), 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", err
	}
	return dir, os.Rename(tmp, p)
}

// lookPath finds an executable named name on path, skipping the directory
// skip (the shim's own).
func lookPath(name, path, skip string) string {
	for _, d := range filepath.SplitList(path) {
		if d == "" || filepath.Clean(d) == filepath.Clean(skip) {
			continue
		}
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// prependPath puts dir first on env's PATH.
func prependPath(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	found := false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			kv = "PATH=" + dir + string(filepath.ListSeparator) + v
			found = true
		}
		out = append(out, kv)
	}
	if !found {
		out = append(out, "PATH="+dir+string(filepath.ListSeparator)+"/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin")
	}
	return out
}

// LinkDockerPlugins links the user's docker CLI plugins (buildx, compose,
// ...) into jobHome/.docker/cli-plugins, so `docker buildx` works with a
// separate job home. It looks where the docker CLI would for the user:
// $DOCKER_CONFIG or ~/.docker, plus cliPluginsExtraDirs from its
// config.json (where Homebrew's buildx is registered). A plugin the job home
// already has as a regular file (one a job installed) is left alone.
func LinkDockerPlugins(userHome, dockerConfig, jobHome string) error {
	if dockerConfig == "" {
		dockerConfig = filepath.Join(userHome, ".docker")
	}
	dirs := []string{filepath.Join(dockerConfig, "cli-plugins")}
	if b, err := os.ReadFile(filepath.Join(dockerConfig, "config.json")); err == nil {
		var cfg struct {
			Extra []string `json:"cliPluginsExtraDirs"`
		}
		if json.Unmarshal(b, &cfg) == nil {
			dirs = append(dirs, cfg.Extra...)
		}
	}
	dst := filepath.Join(jobHome, ".docker", "cli-plugins")
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "docker-") {
				continue
			}
			target, err := filepath.EvalSymlinks(filepath.Join(d, e.Name()))
			if err != nil {
				continue // dangling link: an uninstalled plugin
			}
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return err
			}
			link := filepath.Join(dst, e.Name())
			if fi, err := os.Lstat(link); err == nil {
				if fi.Mode()&os.ModeSymlink == 0 {
					continue
				}
				if cur, _ := os.Readlink(link); cur == target {
					continue
				}
				_ = os.Remove(link)
			}
			if err := os.Symlink(target, link); err != nil {
				return err
			}
		}
	}
	return nil
}

// DockerHost returns the endpoint of the user's current docker context when
// jobs, which don't see the user's ~/.docker, would otherwise miss it: "" if
// there is no docker, DOCKER_HOST is already set (it is passed to jobs), or
// the context is the default socket anyway.
func DockerHost(env []string) string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "DOCKER_HOST=") {
			return ""
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	host := strings.TrimSpace(string(out))
	if host == "" || host == "unix:///var/run/docker.sock" {
		return ""
	}
	return host
}
