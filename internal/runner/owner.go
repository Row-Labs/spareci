package runner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Several `spareci up`s, one per repo, share a cache root, and each cleans up
// what a crash left behind when it starts. It removes only its own repo's
// leftovers, and another repo's when no spareci for that repo is running:
// never a live job of another repo's spareci. Liveness is a pid file per repo
// (Claim). Containers, volumes and runner directories carry their repo.

// RepoKey names a repo in file names: owner_name.
func RepoKey(repo string) string { return strings.ReplaceAll(repo, "/", "_") }

func pidPath(root, key string) string { return filepath.Join(root, "state", key+".pid") }

// Claim records that this process runs jobs for repo, until release.
func Claim(root, repo string) (release func(), err error) {
	path := pidPath(root, RepoKey(repo))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	pid := os.Getpid()
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return nil, err
	}
	return func() {
		// Only if it's still ours: a later spareci for the repo may own it.
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(pid) {
			_ = os.Remove(path)
		}
	}, nil
}

// live reports whether a spareci that claimed the repo with this key is
// still running. An unreadable pid file counts as not running.
func live(root, key string) bool {
	b, err := os.ReadFile(pidPath(root, key))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return true
	}
	err = syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// leftover reports whether startup cleanup for repo should remove something
// labelled with owner (its repo; "" from a spareci before 0.2, which didn't
// label it) that is running or not.
func leftover(root, repo, owner string, running bool) bool {
	switch {
	case owner == repo:
		return true
	case owner == "":
		// Unknown owner: only what isn't running, which no one is using.
		return !running
	default:
		return !live(root, RepoKey(owner))
	}
}
