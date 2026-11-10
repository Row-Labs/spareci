package gitmirror

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// run runs git with only the given extra environment on top of an isolated
// config, so the developer's own hooksPath or URL rewrites can't interfere.
func run(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR=",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// upstream makes a repository with a few commits of content and returns its
// file:// URL and work tree.
func upstream(t *testing.T) (url, work string) {
	t.Helper()
	work = filepath.Join(t.TempDir(), "up")
	run(t, "", nil, "init", "-q", "-b", "main", work)
	for i := range 3 {
		if err := os.WriteFile(filepath.Join(work, "f"+string(rune('a'+i))), []byte(strings.Repeat("x", 1000*(i+1))), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, work, nil, "add", ".")
		run(t, work, nil, "commit", "-q", "-m", "c")
	}
	run(t, work, nil, "tag", "v1")
	return "file://" + work, work
}

// checkout does what actions/checkout does: init, add the remote, shallow
// fetch of one commit, check it out.
func checkout(t *testing.T, env []string, url, sha string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ws")
	run(t, "", env, "init", "-q", dir)
	run(t, dir, env, "remote", "add", "origin", url)
	run(t, dir, env, "-c", "protocol.version=2", "fetch", "-q", "--no-tags", "--depth=1", "origin", "+"+sha+":refs/remotes/origin/main")
	run(t, dir, env, "checkout", "-q", "--force", "-B", "main", "refs/remotes/origin/main")
	return dir
}

func TestMirrorCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	url, work := upstream(t)
	m := &Mirror{Dir: filepath.Join(t.TempDir(), "git", "acme", "app.git"), URL: url}
	if env := m.Env(); env != nil {
		t.Fatalf("Env before the mirror exists = %v, want nothing", env)
	}
	ctx := context.Background()
	if err := m.Update(ctx); err != nil {
		t.Fatal(err)
	}
	env := m.Env()
	if len(env) != 1 || !strings.HasPrefix(env[0], "GIT_TEMPLATE_DIR=") {
		t.Fatalf("Env = %v", env)
	}
	if got := run(t, m.Dir, nil, "tag"); got != "v1" {
		t.Errorf("mirror tags = %q", got)
	}
	if got := run(t, m.Dir, nil, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
		t.Errorf("mirror HEAD = %q, want the upstream default branch", got)
	}

	// A new commit upstream reaches the mirror on the next update.
	if err := os.WriteFile(filepath.Join(work, "fd"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, nil, "add", ".")
	run(t, work, nil, "commit", "-q", "-m", "new")
	head := run(t, work, nil, "rev-parse", "HEAD")
	if err := m.Update(ctx); err != nil {
		t.Fatal(err)
	}
	if got := run(t, m.Dir, nil, "rev-parse", "refs/heads/main"); got != head {
		t.Fatalf("mirror main = %s, want %s", got, head)
	}

	dir := checkout(t, env, url, head)
	// The hook detached the repository: no alternates, every object local.
	if _, err := os.Stat(filepath.Join(dir, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Errorf("alternates still present after checkout (err=%v)", err)
	}
	run(t, dir, nil, "fsck", "--connectivity-only")
	if out, err := exec.Command("git", "-C", dir, "config", "--local", "core.alternateRefsPrefixes").Output(); err == nil {
		t.Errorf("core.alternateRefsPrefixes left behind: %s", out)
	}
	if got := run(t, dir, nil, "show", "HEAD:fd"); got != "new" {
		t.Errorf("checked-out content = %q", got)
	}
	// Still a normal clone of the original remote: pushes go where they did.
	if got := run(t, dir, nil, "remote", "get-url", "--push", "origin"); got != url {
		t.Errorf("push url = %q, want %q", got, url)
	}
}

func TestMirrorFetchUsesMirrorObjects(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	url, work := upstream(t)
	m := &Mirror{Dir: filepath.Join(t.TempDir(), "app.git"), URL: url}
	if err := m.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	head := run(t, work, nil, "rev-parse", "HEAD")
	// Fetch without checking out: everything should come from the mirror,
	// so nothing lands in the new repository's own object store.
	dir := filepath.Join(t.TempDir(), "ws")
	run(t, "", m.Env(), "init", "-q", dir)
	run(t, dir, nil, "remote", "add", "origin", url)
	run(t, dir, nil, "fetch", "-q", "--no-tags", "--depth=1", "origin", "+"+head+":refs/remotes/origin/main")
	if got := run(t, dir, nil, "count-objects", "-v"); !strings.Contains(got, "count: 0") || !strings.Contains(got, "in-pack: 0") {
		t.Errorf("fetch transferred objects the mirror had:\n%s", got)
	}
}

func TestMirrorUpdateFailureKeepsOldMirror(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	url, work := upstream(t)
	m := &Mirror{Dir: filepath.Join(t.TempDir(), "app.git"), URL: url}
	if err := m.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(work); err != nil {
		t.Fatal(err)
	}
	m2 := &Mirror{Dir: m.Dir, URL: url} // a restarted spareci
	if err := m2.Update(context.Background()); err == nil {
		t.Fatal("update from a missing remote succeeded")
	}
	if m2.Env() == nil {
		t.Error("existing mirror not offered after a failed fetch")
	}

	bad := &Mirror{Dir: filepath.Join(t.TempDir(), "new.git"), URL: url}
	if err := bad.Update(context.Background()); err == nil {
		t.Fatal("clone from a missing remote succeeded")
	}
	if bad.Env() != nil {
		t.Error("Env offered a mirror that was never cloned")
	}
	if _, err := os.Stat(bad.Dir + ".partial"); !os.IsNotExist(err) {
		t.Errorf("partial clone left behind (err=%v)", err)
	}
}

func TestEnvNilMirror(t *testing.T) {
	var m *Mirror
	if m.Env() != nil {
		t.Fatal("nil mirror has env")
	}
}
