// Package gitmirror keeps a bare mirror of a job's repository on this machine
// so that actions/checkout transfers only what the mirror doesn't have yet.
//
// Jobs get GIT_TEMPLATE_DIR pointing at a template with two files:
//
//   - objects/info/alternates names the mirror's object directory, so every
//     repository a job creates with git init or git clone can read the
//     mirror's objects. Fetches still go to GitHub with the job's own token;
//     the mirror only lets git tell GitHub "I already have these", so GitHub
//     sends the delta.
//   - config limits the mirror refs git considers to the default branch.
//   - hooks/post-index-change runs once git has written an index (the
//     checkout after the fetch). It copies the objects the repository uses
//     out of the mirror (git repack -a -d, local and fast) and removes the
//     alternates line, so the repository is self-contained again: copying it
//     into a container or a cache works as on a hosted runner.
//
// Nothing in a workflow changes, the remote URL stays github.com (pushes,
// auth, submodules and LFS behave as before), and if the mirror isn't ready
// jobs simply don't get the template.
package gitmirror

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Mirror is one repository's bare mirror and the git template that points
// jobs at it.
type Mirror struct {
	Dir   string // bare repository, e.g. <cache root>/git/owner/repo.git
	URL   string // https://github.com/owner/repo.git
	Token string // read access to URL; sent as an HTTP header, never in argv

	mu      sync.Mutex // one update at a time
	readyMu sync.Mutex
	ready   bool
}

// New returns the mirror for repo (owner/name) under root.
func New(root, repo, token string) *Mirror {
	return &Mirror{
		Dir:   filepath.Join(root, "git", filepath.FromSlash(repo)+".git"),
		URL:   "https://github.com/" + repo + ".git",
		Token: token,
	}
}

func (m *Mirror) templateDir() string { return strings.TrimSuffix(m.Dir, ".git") + ".template" }
func (m *Mirror) objectsDir() string  { return filepath.Join(m.Dir, "objects") }

// Env is what a job needs to use the mirror: GIT_TEMPLATE_DIR once the mirror
// and its template exist, nothing before.
func (m *Mirror) Env() []string {
	if m == nil {
		return nil
	}
	m.readyMu.Lock()
	ready := m.ready
	m.readyMu.Unlock()
	if !ready {
		return nil
	}
	return []string{"GIT_TEMPLATE_DIR=" + m.templateDir()}
}

// Update clones the mirror if it doesn't exist and fetches it otherwise. If
// another update is running it returns at once: the mirror is about as fresh
// as it's going to get. A failure leaves an existing mirror usable.
func (m *Mirror) Update(ctx context.Context) error {
	if !m.mu.TryLock() {
		return nil
	}
	defer m.mu.Unlock()
	start := time.Now()
	fresh := false
	if _, err := os.Stat(filepath.Join(m.Dir, "HEAD")); err != nil {
		if err := m.clone(ctx); err != nil {
			return err
		}
		fresh = true
	} else if err := m.git(ctx, m.Dir, "fetch", "--quiet", "--prune", "--no-write-fetch-head", "origin"); err != nil {
		// Keep serving the mirror we have: alternates only make fetches
		// smaller, never wrong.
		m.markReady()
		return fmt.Errorf("fetch git mirror: %w", err)
	}
	head, err := m.output(ctx, m.Dir, "symbolic-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("git mirror: %w", err)
	}
	if err := writeTemplate(m.templateDir(), m.objectsDir(), head); err != nil {
		return err
	}
	m.markReady()
	if fresh {
		slog.Info("git mirror ready; checkouts fetch only what it lacks", "dir", m.Dir, "took", time.Since(start).Round(time.Second))
	} else {
		slog.Debug("git mirror updated", "dir", m.Dir, "took", time.Since(start).Round(time.Millisecond))
	}
	return nil
}

func (m *Mirror) markReady() {
	if _, err := os.Stat(filepath.Join(m.templateDir(), "objects", "info", "alternates")); err != nil {
		return
	}
	m.readyMu.Lock()
	m.ready = true
	m.readyMu.Unlock()
}

// clone makes the mirror in a temporary directory and renames it into place,
// so a half-finished clone is never used.
func (m *Mirror) clone(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(m.Dir), 0o700); err != nil {
		return err
	}
	tmp := m.Dir + ".partial"
	_ = os.RemoveAll(tmp)
	slog.Info("creating git mirror for faster checkouts", "url", m.URL, "dir", m.Dir)
	if err := m.git(ctx, "", "init", "--quiet", "--bare", tmp); err != nil {
		return fmt.Errorf("git mirror: %w", err)
	}
	// Branches and tags only: refs/pull/* can run to thousands of refs, and
	// a pull request's merge commit shares nearly everything with its base.
	for _, kv := range [][2]string{
		{"remote.origin.url", m.URL},
		{"remote.origin.fetch", "+refs/heads/*:refs/heads/*"},
	} {
		if err := m.git(ctx, tmp, "config", kv[0], kv[1]); err != nil {
			return fmt.Errorf("git mirror: %w", err)
		}
	}
	if err := m.git(ctx, tmp, "config", "--add", "remote.origin.fetch", "+refs/tags/*:refs/tags/*"); err != nil {
		return fmt.Errorf("git mirror: %w", err)
	}
	if err := m.git(ctx, tmp, "fetch", "--quiet", "--no-write-fetch-head", "origin"); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("clone git mirror: %w", err)
	}
	// HEAD names the default branch, which the template offers to fetches.
	out, err := m.output(ctx, tmp, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("git mirror: %w", err)
	}
	head, _, _ := strings.Cut(strings.TrimPrefix(out, "ref: "), "\t")
	if !strings.HasPrefix(head, "refs/heads/") {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("git mirror: no default branch in %q", out)
	}
	if err := m.git(ctx, tmp, "symbolic-ref", "HEAD", head); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("git mirror: %w", err)
	}
	return os.Rename(tmp, m.Dir)
}

// git runs git in dir with the token as an extra header (through the
// environment, so it isn't visible in ps) and without the user's or the
// system's git config, which may rewrite github.com URLs to SSH.
func (m *Mirror) git(ctx context.Context, dir string, args ...string) error {
	_, err := m.output(ctx, dir, args...)
	return err
}

func (m *Mirror) output(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_TEMPLATE_DIR=", // not our own template
	)
	if m.Token != "" {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + m.Token))
		cmd.Env = append(cmd.Env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
			"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+auth,
		)
	}
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, stderr)
	}
	return strings.TrimSpace(string(out)), nil
}

// writeTemplate (re)writes the git template that points new repositories at
// objects and detaches them again after their first checkout.
//
// The template's config limits the mirror refs git uses to head, the default
// branch. Without it, git offers every branch and tag of the mirror and its
// connectivity check after the fetch walks from all of them: on
// microsoft/vscode (6,400 refs) that check alone took 6s, more than the
// download it saved.
func writeTemplate(dir, objects, head string) error {
	if strings.ContainsAny(objects, "'\n") {
		return fmt.Errorf("git mirror path %q can't be quoted in a hook", objects)
	}
	files := map[string]struct {
		body string
		mode os.FileMode
	}{
		"objects/info/alternates": {objects + "\n", 0o644},
		"config":                  {"[core]\n\talternateRefsPrefixes = " + head + "\n", 0o644},
		"hooks/post-index-change": {strings.ReplaceAll(hook, "@OBJECTS@", objects), 0o755},
	}
	for name, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, []byte(f.body), f.mode); err != nil {
			return err
		}
		if err := os.Chmod(tmp, f.mode); err != nil {
			return err
		}
		if err := os.Rename(tmp, p); err != nil {
			return err
		}
	}
	return nil
}

// hook detaches a repository from the mirror: repack copies in every object
// its refs reach, borrowed ones included, then the mirror's alternates line
// goes. If repack fails the repository keeps borrowing, which still works on
// this machine. It runs after every index write, so it must be cheap when
// there's nothing to do.
const hook = `#!/bin/sh
# Written by spareci (internal/gitmirror). Copies the objects this repository
# borrowed from spareci's local git mirror into it, once, after the checkout.
alt=$(git rev-parse --git-path objects/info/alternates 2>/dev/null) || exit 0
grep -qxF '@OBJECTS@' "$alt" 2>/dev/null || exit 0
git repack -a -d -q >/dev/null 2>&1 || exit 0
grep -vxF '@OBJECTS@' "$alt" > "$alt.spareci" || true
if [ -s "$alt.spareci" ]; then mv -f "$alt.spareci" "$alt"; else rm -f "$alt" "$alt.spareci"; fi
git config --local --unset core.alternateRefsPrefixes 2>/dev/null
exit 0
`
