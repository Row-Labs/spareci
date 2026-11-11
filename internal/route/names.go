package route

import (
	"regexp"
	"strconv"
	"strings"
)

// A job is routed by the OS and architecture of the GitHub-hosted runner it
// falls back to, each with its own repo variable and runner label:
//
//	runs-on: ${{ vars.SPARECI_RUNS_ON_MACOS || 'macos-15' }}             → spareci-macos
//	runs-on: ${{ vars.SPARECI_RUNS_ON_LINUX || 'ubuntu-24.04' }}         → spareci-linux
//	runs-on: ${{ vars.SPARECI_RUNS_ON_LINUX_ARM || 'ubuntu-24.04-arm' }} → spareci-linux-arm
//
// A machine sets a variable only while it has a backend for that kind of job,
// so a Linux job never lands on a Mac that can't run Linux, and an arm64 job
// never lands on an x64-only Linux host. The Linux variables follow GitHub's
// own naming: ubuntu-24.04 is x64 and ubuntu-24.04-arm is arm64.
//
// The variable spareci 0.1 used, SPARECI_RUNS_ON (label spareci), is still
// set for the host's native OS and architecture so existing workflows keep
// working; spareci init upgrades those lines.

// OS and architecture names, as runner.Backend reports them.
const (
	MacOS = "macos"
	Linux = "linux"
	ARM64 = "arm64"
	X64   = "x64"
)

// Target is a kind of job: the OS and architecture it asked for. Arch is
// empty for macOS, which has one routing variable whatever the CPU.
type Target struct{ OS, Arch string }

var (
	TargetMacOS    = Target{MacOS, ""}
	TargetLinux    = Target{Linux, X64}
	TargetLinuxARM = Target{Linux, ARM64}
	// Targets is every kind of job spareci routes, in a stable order.
	Targets = []Target{TargetMacOS, TargetLinux, TargetLinuxARM}
)

// DefaultBase and DefaultPrefix name the variables and labels.
const (
	DefaultBase   = "SPARECI_RUNS_ON"
	DefaultPrefix = "spareci"
)

func (t Target) suffix() string {
	switch t {
	case TargetMacOS:
		return "macos"
	case TargetLinux:
		return "linux"
	case TargetLinuxARM:
		return "linux-arm"
	}
	return ""
}

// Var is the repo variable for t: base_MACOS, base_LINUX or base_LINUX_ARM.
func (t Target) Var(base string) string {
	return base + "_" + strings.ToUpper(strings.ReplaceAll(t.suffix(), "-", "_"))
}

// Label is the runner label the variable is set to: prefix-macos, ...
func (t Target) Label(prefix string) string { return prefix + "-" + t.suffix() }

func (t Target) String() string {
	if t.Arch == "" {
		return "macOS"
	}
	return map[string]string{Linux: "Linux"}[t.OS] + " " + t.Arch
}

// Vars is every routing variable for base, the old one last.
func Vars(base string) []string {
	var out []string
	for _, t := range Targets {
		out = append(out, t.Var(base))
	}
	return append(out, base)
}

// Routes is the variable and label for each target, plus the old variable
// with the plain prefix as its label when legacy is set. With an empty base
// there is no routing.
func Routes(base, prefix string, targets []Target, legacy bool) []Route {
	if base == "" {
		return nil
	}
	var out []Route
	for _, t := range targets {
		out = append(out, Route{Var: t.Var(base), Label: t.Label(prefix)})
	}
	if legacy {
		out = append(out, Route{Var: base, Label: prefix})
	}
	return out
}

var macosVersion = regexp.MustCompile(`^macos-(\d+)`)

// HostedPlatform maps a GitHub-hosted label to its OS and architecture.
// ok is false for labels spareci doesn't know (larger runners with custom
// names, for example). OS is macos, linux or windows.
func HostedPlatform(label string) (os, arch string, ok bool) {
	l := strings.ToLower(label)
	switch {
	case strings.HasPrefix(l, "ubuntu-"):
		if strings.Contains(l, "-arm") {
			return Linux, ARM64, true
		}
		return Linux, X64, true
	case strings.HasPrefix(l, "windows-"):
		if strings.Contains(l, "-arm") {
			return "windows", ARM64, true
		}
		return "windows", X64, true
	case strings.HasPrefix(l, "macos-"):
		switch {
		case strings.HasSuffix(l, "-xlarge"):
			return MacOS, ARM64, true
		case strings.HasSuffix(l, "-large"), strings.HasSuffix(l, "-intel"):
			return MacOS, X64, true
		case l == "macos-latest":
			return MacOS, ARM64, true
		}
		if mm := macosVersion.FindStringSubmatch(l); mm != nil {
			if v, _ := strconv.Atoi(mm[1]); v >= 14 {
				return MacOS, ARM64, true
			}
			return MacOS, X64, true
		}
	}
	return "", "", false
}

// TargetFor is the kind of job a runs-on (its labels, without spareci's
// variable) asks for: from a GitHub-hosted label, or a self-hosted list's
// OS and architecture labels. ok is false when the labels don't say.
func TargetFor(labels []string) (Target, bool) {
	var osName, arch string
	for _, l := range labels {
		if o, a, ok := HostedPlatform(l); ok {
			osName, arch = o, a
			break
		}
		switch strings.ToLower(l) {
		case "macos":
			osName = MacOS
		case "linux":
			osName = Linux
		case "arm64":
			arch = ARM64
		case "x64":
			arch = X64
		}
	}
	switch osName {
	case MacOS:
		return TargetMacOS, true
	case Linux:
		if arch == ARM64 {
			return TargetLinuxARM, true
		}
		return TargetLinux, true
	}
	return Target{}, false
}
