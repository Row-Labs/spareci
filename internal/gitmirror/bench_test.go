package gitmirror

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCheckoutTiming measures actions/checkout's git commands against a real
// GitHub repository, with and without the mirror. It needs the network, so
// it only runs when asked:
//
//	SPARECI_BENCH_REPO=cli/cli go test ./internal/gitmirror -run TestCheckoutTiming -v
//
// SPARECI_BENCH_DEPTHS picks the fetch depths (default "1,0"; 0 is
// fetch-depth: 0, the full history) and SPARECI_BENCH_MIRROR reuses a mirror
// directory between runs.
func TestCheckoutTiming(t *testing.T) {
	repo := os.Getenv("SPARECI_BENCH_REPO")
	if repo == "" {
		t.Skip("set SPARECI_BENCH_REPO=owner/name to measure checkouts")
	}
	dir := os.Getenv("SPARECI_BENCH_MIRROR")
	if dir == "" {
		dir = t.TempDir()
	}
	m := New(dir, repo, os.Getenv("GITHUB_TOKEN"))
	start := time.Now()
	if err := m.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Logf("mirror update: %s", time.Since(start).Round(10*time.Millisecond))
	start = time.Now()
	if err := m.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Logf("mirror update with nothing new: %s", time.Since(start).Round(10*time.Millisecond))
	sha := run(t, m.Dir, nil, "rev-parse", "HEAD")
	url := "https://github.com/" + repo

	depths := strings.Split(os.Getenv("SPARECI_BENCH_DEPTHS"), ",")
	if depths[0] == "" {
		depths = []string{"1", "0"}
	}
	for _, depth := range depths {
		for round := range 2 {
			for _, env := range [][]string{nil, m.Env()} {
				ws := filepath.Join(t.TempDir(), "ws")
				run(t, "", env, "init", "-q", ws)
				run(t, ws, env, "remote", "add", "origin", url)
				t0 := time.Now()
				if depth == "0" {
					run(t, ws, env, "-c", "protocol.version=2", "fetch", "-q", "--prune", "origin", "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*")
				} else {
					run(t, ws, env, "-c", "protocol.version=2", "fetch", "-q", "--no-tags", "--depth="+depth, "origin", "+"+sha+":refs/remotes/origin/main")
				}
				t1 := time.Now()
				run(t, ws, env, "checkout", "-q", "--force", "-B", "main", sha)
				t2 := time.Now()
				_, alt := os.Stat(filepath.Join(ws, ".git", "objects", "info", "alternates"))
				t.Logf("depth=%s round=%d mirror=%-5v fetch=%-6s checkout=%-6s total=%-6s self-contained=%v",
					depth, round+1, env != nil, t1.Sub(t0).Round(10*time.Millisecond), t2.Sub(t1).Round(10*time.Millisecond),
					t2.Sub(t0).Round(10*time.Millisecond), os.IsNotExist(alt))
			}
		}
	}
}
