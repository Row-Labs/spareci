package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Row-Labs/spareci/internal/route"
	"github.com/Row-Labs/spareci/internal/runner"
)

// linuxOpts are up's flags for Linux jobs in containers.
type linuxOpts struct {
	cpus, memory, docker *string
}

// linuxBackends are the container backends that run Linux jobs on a Mac, in
// order of preference: the Mac's own architecture first. With x64OnARM, x64
// jobs go to the arm64 backend (pool.Policy), so no x64 one is made and its
// image isn't built. No container runtime means no Linux backends: the Mac
// takes only macOS jobs and Linux ones stay with GitHub.
func linuxBackends(ctx context.Context, host *runner.Host, repo string, dry, x64OnARM bool, o linuxOpts) []runner.Backend {
	if runtime.GOOS != "darwin" {
		return nil // a Linux host runs its Linux jobs natively
	}
	var out []runner.Backend
	for _, arch := range runner.ContainerArches() {
		if x64OnARM && arch == runner.ArchX64 && runner.HostArch() == runner.ArchARM {
			continue
		}
		c := &runner.Container{
			Root: host.Root, Version: host.Version, Target: arch, Repo: repo,
			CPUs: *o.cpus, Memory: *o.memory, DockerMode: *o.docker, Git: host.Git,
		}
		if dry {
			if err := runner.CheckRuntime(ctx, nil); err != nil {
				slog.Info("no Linux jobs here", "reason", err)
				return nil
			}
			out = append(out, c)
			continue
		}
		if err := prepareContainer(ctx, c); err != nil {
			if errors.Is(err, runner.ErrNoRuntime) {
				slog.Info("no Linux jobs here", "arch", arch, "reason", err)
			} else {
				slog.Warn("Linux container backend unavailable; its jobs stay with GitHub", "arch", arch, "err", err)
			}
			continue
		}
		out = append(out, c)
	}
	return out
}

// prepareContainer builds the job image if needed, logging the build to a
// file, and removes what a crash left behind.
func prepareContainer(ctx context.Context, c *runner.Container) error {
	logPath := filepath.Join(c.Root, "logs", "image-"+c.Target+".log")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	f, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := c.Prepare(ctx, f); err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	c.Prune(pctx)
	slog.Info("Linux jobs in containers", "arch", c.Target, "image", c.Image(), "docker", c.DockerMode)
	return nil
}

// containerLinux lists the Linux architectures (route.ARM64, route.X64) a
// Mac's container runtime can run jobs for, or nil without one. A Linux
// host runs Linux jobs natively, so this is only asked on a Mac.
func containerLinux() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	if runner.CheckRuntime(context.Background(), nil) != nil {
		return nil
	}
	var out []string
	for _, a := range runner.ContainerArches() {
		out = append(out, map[string]string{runner.ArchARM: route.ARM64, runner.ArchX64: route.X64}[a])
	}
	return out
}
