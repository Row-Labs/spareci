package cache

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestActionsCacheCompat runs the real @actions/cache npm client against a
// job listener. It needs node and `npm ci --prefix test/compat`; without them
// it is skipped (go test -short skips it too).
func TestActionsCacheCompat(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	script, _ := filepath.Abs("../../test/compat/compat.mjs")
	if _, err := os.Stat(filepath.Join(filepath.Dir(script), "node_modules", "@actions", "cache")); err != nil {
		t.Skip("run `npm ci --prefix test/compat` to enable")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}

	s := newServer(t, "")
	jl := listen(t, s, "o/r")
	// Real path: on macOS TempDir is under /var, a symlink to /private/var,
	// and the cache client's tar paths must agree with the cwd.
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), jl.Env()...)
	cmd.Env = append(cmd.Env,
		"ACTIONS_RUNTIME_TOKEN="+mainToken[len("Bearer "):],
		"GITHUB_WORKSPACE="+work,
		"RUNNER_TEMP="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("compat script failed: %v", err)
	}
}
