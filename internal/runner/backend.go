package runner

import (
	"context"
	"runtime"
)

// OS and Arch values a Backend reports, in GitHub's spelling for runner
// labels (lower case here; Labels gives the capitalised forms).
const (
	OSMacOS = "macos"
	OSLinux = "linux"
	ArchARM = "arm64"
	ArchX64 = "x64"
)

// Backend runs ephemeral runners of one OS and architecture. The pool keeps
// one or more of them and routes each queued job to the backend whose labels
// match it: on a Mac, the native Host takes macOS jobs and a Container takes
// Linux ones.
type Backend interface {
	OS() string       // OSMacOS or OSLinux
	Arch() string     // ArchARM or ArchX64
	Labels() []string // labels its runners register with, excluding the routing label (the pool adds it)
	// Run starts one runner with a JIT config and blocks until it exits.
	// onStart is called when it picks up a job; env (KEY=VALUE) reaches every
	// step. Cancelling ctx stops it gracefully, then forcibly.
	Run(ctx context.Context, name, jitConfig string, env []string, onStart func(job string)) (Result, error)
}

var _ Backend = (*Host)(nil)

// HostOS is this machine's OS as a Backend reports it ("" if unsupported).
func HostOS() string {
	return map[string]string{"darwin": OSMacOS, "linux": OSLinux}[runtime.GOOS]
}

// HostArch is this machine's CPU as a Backend reports it ("" if unsupported).
func HostArch() string {
	return map[string]string{"arm64": ArchARM, "amd64": ArchX64}[runtime.GOARCH]
}

// SelfHostedLabels are the labels GitHub gives a self-hosted runner of this
// OS and architecture: self-hosted, macOS|Linux, ARM64|X64.
func SelfHostedLabels(os, arch string) []string {
	osLabel := map[string]string{OSMacOS: "macOS", OSLinux: "Linux"}[os]
	archLabel := map[string]string{ArchARM: "ARM64", ArchX64: "X64"}[arch]
	return []string{"self-hosted", osLabel, archLabel}
}

// OS is the host's OS: the native backend runs jobs directly on it.
func (h *Host) OS() string { return HostOS() }

// Arch is the host's CPU architecture.
func (h *Host) Arch() string { return HostArch() }

// Labels are the self-hosted labels for this machine.
func (h *Host) Labels() []string { return SelfHostedLabels(h.OS(), h.Arch()) }
