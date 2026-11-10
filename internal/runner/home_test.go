package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestResolveJobHome(t *testing.T) {
	root := "/cache/spareci"
	for _, tc := range []struct {
		mode, want string
		err        bool
	}{
		{"", "/cache/spareci/home/acme/app", false},
		{"spareci", "/cache/spareci/home/acme/app", false},
		{"user", "", false},
		{"/srv/ci-home/", "/srv/ci-home", false},
		{"relative/dir", "", true},
		{"usr", "", true},
	} {
		got, err := ResolveJobHome(tc.mode, root, "acme/app")
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("ResolveJobHome(%q) = %q, %v; want %q, err=%v", tc.mode, got, err, tc.want, tc.err)
		}
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestJobEnv(t *testing.T) {
	base := []string{
		"HOME=/Users/me",
		"PATH=/Users/me/.cargo/bin:/opt/homebrew/bin:/Users/me:.::/usr/bin:/Users/meow/bin:/bin",
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
		"TMPDIR=/var/folders/xy/T/",
		"HTTPS_PROXY=http://proxy:3128",
		"GOPATH=/Users/me/go",
		"GOCACHE=/Users/me/Library/Caches/go-build",
		"CARGO_HOME=/Users/me/.cargo",
		"RUSTUP_HOME=/Users/me/.rustup",
		"NVM_DIR=/Users/me/.nvm",
		"XDG_CACHE_HOME=/Users/me/.cache",
		"XDG_CONFIG_HOME=/Users/me/.config",
		"GH_TOKEN=secret",
		"SSH_AUTH_SOCK=/private/tmp/agent.sock",
		"NPM_CONFIG_USERCONFIG=/Users/me/.npmrc",
	}

	if got := JobEnv(base, "/Users/me", ""); !slices.Equal(got, base) {
		t.Fatalf("user mode must pass the environment through unchanged, got %v", got)
	}

	m := envMap(JobEnv(base, "/Users/me", "/jobs/home"))
	want := map[string]string{
		"HOME":                "/jobs/home",
		"PATH":                "/opt/homebrew/bin:/usr/bin:/Users/meow/bin:/bin",
		"LANG":                "en_US.UTF-8",
		"LC_ALL":              "en_US.UTF-8",
		"TMPDIR":              "/var/folders/xy/T/",
		"HTTPS_PROXY":         "http://proxy:3128",
		"GIT_CONFIG_NOSYSTEM": "1",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	for _, k := range []string{"GOPATH", "GOCACHE", "CARGO_HOME", "RUSTUP_HOME", "NVM_DIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "GH_TOKEN", "SSH_AUTH_SOCK", "NPM_CONFIG_USERCONFIG"} {
		if v, ok := m[k]; ok {
			t.Errorf("%s leaked into the job: %q", k, v)
		}
	}
	if len(m) != len(want) {
		t.Errorf("unexpected variables: %v", m)
	}
}

func TestStripHomeFallsBackToSystemPath(t *testing.T) {
	if got := stripHome("/Users/me/bin:/Users/me/.local/bin", "/Users/me"); got != "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Fatalf("got %q", got)
	}
}

// A job's tools must resolve their state inside the job home, not the user's.
// This runs the real Go toolchain under JobEnv.
func TestJobEnvMovesGoState(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	jobHome := t.TempDir()
	env := JobEnv(os.Environ(), "/nonexistent-user-home", jobHome)
	// The go binary may itself live under the user's home; keep it reachable.
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = kv + string(filepath.ListSeparator) + filepath.Dir(gobin)
		}
	}
	cmd := exec.Command(gobin, "env", "GOPATH", "GOMODCACHE", "GOCACHE")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	cacheBase := filepath.Join(jobHome, ".cache")
	if runtime.GOOS == "darwin" {
		cacheBase = filepath.Join(jobHome, "Library", "Caches")
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	want := []string{filepath.Join(jobHome, "go"), filepath.Join(jobHome, "go", "pkg", "mod"), filepath.Join(cacheBase, "go-build")}
	if !slices.Equal(lines, want) {
		t.Fatalf("go env under job home = %q, want %q", lines, want)
	}
}

func TestPrepareHome(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home", "acme", "app")
	if err := prepareHome(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("job home mode = %v, %v; want 0700", fi.Mode().Perm(), err)
	}
}
