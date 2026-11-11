package main

import (
	"context"
	"os/exec"
	"runtime"
	"time"

	"github.com/Row-Labs/spareci/internal/route"
	"github.com/Row-Labs/spareci/internal/runner"
)

// linuxBackends are the container backends that run Linux jobs here, in
// order of preference. None until the container backend lands
// (linux-containers); until then a Mac takes only macOS jobs and never sets
// the Linux routing variables.
func linuxBackends(ctx context.Context, host *runner.Host, dry bool) []runner.Backend {
	return nil
}

// containerLinux lists the Linux architectures (route.ARM64, route.X64) a
// Mac's container runtime can run jobs for, or nil without one. A Linux
// host runs Linux jobs natively, so this is only asked on a Mac.
//
// Stand-in until the container backend's own detection lands: any Docker
// API runtime that answers, running the Mac's own architecture natively and,
// on Apple Silicon, x64 through Rosetta.
func containerLinux() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").Run() != nil {
		return nil
	}
	if runner.HostArch() == runner.ArchARM {
		return []string{route.ARM64, route.X64}
	}
	return []string{route.X64}
}
