package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDocker writes a "docker" into a new directory that prints the
// ACTIONS_RESULTS_URL it was run with and its arguments.
func fakeDocker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"$ACTIONS_RESULTS_URL $*\"\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWriteDockerShimNoDocker(t *testing.T) {
	dir, err := WriteDockerShim(filepath.Join(t.TempDir(), "shims"), t.TempDir())
	if err != nil || dir != "" {
		t.Fatalf("WriteDockerShim without docker = %q, %v; want no shim", dir, err)
	}
}

func TestDockerShim(t *testing.T) {
	real := fakeDocker(t)
	shims := filepath.Join(t.TempDir(), "shims")
	// The shim directory itself on PATH must not be picked as the real docker.
	dir, err := WriteDockerShim(shims, shims+string(filepath.ListSeparator)+real)
	if err != nil || dir != shims {
		t.Fatalf("WriteDockerShim = %q, %v", dir, err)
	}
	const local = "http://127.0.0.1:5000/"
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"buildkit url", []string{"ACTIONS_RESULTS_URL=" + local, "SPARECI_RESULTS_URL=" + local, "SPARECI_BUILDKIT_RESULTS_URL=http://host.docker.internal:5000/", "ACTIONS_RESULTS_ORL=https://results.example/"}, "http://host.docker.internal:5000/"},
		{"github fallback", []string{"ACTIONS_RESULTS_URL=" + local, "SPARECI_RESULTS_URL=" + local, "ACTIONS_RESULTS_ORL=https://results.example/"}, "https://results.example/"},
		{"step changed the url", []string{"ACTIONS_RESULTS_URL=http://elsewhere/", "SPARECI_RESULTS_URL=" + local, "SPARECI_BUILDKIT_RESULTS_URL=http://host.docker.internal:5000/"}, "http://elsewhere/"},
		{"no local cache", []string{"ACTIONS_RESULTS_URL=https://results.example/"}, "https://results.example/"},
	} {
		cmd := exec.Command(filepath.Join(shims, "docker"), "buildx", "build", "a b")
		cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, tc.env...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := strings.TrimSpace(string(out)); got != tc.want+" buildx build a b" {
			t.Errorf("%s: docker ran with %q, want %q", tc.name, got, tc.want+" buildx build a b")
		}
	}
}

func TestPrependPath(t *testing.T) {
	got := envMap(prependPath([]string{"HOME=/h", "PATH=/usr/bin:/bin"}, "/shims"))
	if got["PATH"] != "/shims:/usr/bin:/bin" || got["HOME"] != "/h" {
		t.Errorf("prependPath = %v", got)
	}
	if got := envMap(prependPath([]string{"HOME=/h"}, "/shims")); !strings.HasPrefix(got["PATH"], "/shims:") {
		t.Errorf("prependPath without PATH = %q", got["PATH"])
	}
}

func TestLinkDockerPlugins(t *testing.T) {
	user := t.TempDir()
	app := t.TempDir() // where an app bundle keeps the real plugin
	brew := t.TempDir()
	for _, p := range []string{filepath.Join(app, "docker-buildx"), filepath.Join(brew, "docker-compose")} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plugins := filepath.Join(user, ".docker", "cli-plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink(filepath.Join(app, "docker-buildx"), filepath.Join(plugins, "docker-buildx")))
	must(os.Symlink(filepath.Join(app, "gone"), filepath.Join(plugins, "docker-scan"))) // uninstalled
	must(os.WriteFile(filepath.Join(user, ".docker", "config.json"), []byte(`{"cliPluginsExtraDirs":["`+brew+`"],"auths":{"x":{}}}`), 0o600))

	job := t.TempDir()
	// A plugin the job installed itself stays.
	jobPlugins := filepath.Join(job, ".docker", "cli-plugins")
	must(os.MkdirAll(jobPlugins, 0o700))
	must(os.WriteFile(filepath.Join(jobPlugins, "docker-compose"), []byte("mine"), 0o755))

	for range 2 { // idempotent
		if err := LinkDockerPlugins(user, "", job); err != nil {
			t.Fatal(err)
		}
	}
	// Linked to the real file, not to the user's link (TempDir may itself be
	// behind a symlink, as /var is on macOS).
	want, _ := filepath.EvalSymlinks(filepath.Join(app, "docker-buildx"))
	if got, err := os.Readlink(filepath.Join(jobPlugins, "docker-buildx")); err != nil || got != want {
		t.Errorf("docker-buildx link = %q, %v; want %q", got, err, want)
	}
	if b, _ := os.ReadFile(filepath.Join(jobPlugins, "docker-compose")); string(b) != "mine" {
		t.Errorf("job's own docker-compose replaced: %q", b)
	}
	if _, err := os.Lstat(filepath.Join(jobPlugins, "docker-scan")); !os.IsNotExist(err) {
		t.Errorf("dangling plugin linked (err=%v)", err)
	}
	// The user's credentials don't come along.
	if _, err := os.Stat(filepath.Join(job, ".docker", "config.json")); !os.IsNotExist(err) {
		t.Errorf("config.json copied into the job home (err=%v)", err)
	}
}
