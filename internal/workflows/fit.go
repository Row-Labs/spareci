package workflows

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Row-Labs/spareci/internal/route"
	"github.com/Row-Labs/spareci/internal/runner"
)

// Fit is how well this machine suits a job.
type Fit int

const (
	// FitGood: same OS and architecture, or a self-hosted job whose labels
	// this machine all carries. Recommended.
	FitGood Fit = iota
	// FitMaybe: might work (another architecture, a Linux job on a Mac
	// without a container runtime, a label spareci doesn't know). Only
	// converted when asked for by name.
	FitMaybe
	// FitNo: can't run here (Windows, macOS on Linux, containers on macOS).
	FitNo
)

// Machine describes the host spareci would run jobs on.
type Machine struct {
	OS     string   // GOOS: darwin or linux
	Arch   string   // GOARCH: arm64 or amd64
	Labels []string // native runner labels, e.g. self-hosted, macOS, ARM64, spareci
	// Linux lists the architectures (route.ARM64, route.X64) this Mac can
	// run Linux jobs for in containers; empty without a container runtime.
	Linux []string
}

// Assess sets each job's Fit and Why for this machine, and the routing
// variable of a job whose labels don't say which OS it needs: this
// machine's own.
func (m Machine) Assess(files []*File) {
	for _, f := range files {
		for _, j := range f.Jobs {
			if j.Skip == "" {
				j.Fit, j.Why = m.fit(j)
			}
			if j.Var == "" && j.edit != nil {
				j.Var = m.native().Var(j.base)
			}
		}
	}
}

// native is the kind of job this machine runs without containers.
func (m Machine) native() route.Target {
	if m.OS == "darwin" {
		return route.TargetMacOS
	}
	if m.Arch == "arm64" {
		return route.TargetLinuxARM
	}
	return route.TargetLinux
}

func (m Machine) fit(j *Job) (Fit, string) {
	if j.container && m.OS != "linux" {
		return FitNo, "uses container: or services:, which need a Linux runner"
	}
	for _, l := range j.Labels {
		if strings.EqualFold(l, "self-hosted") {
			return m.fitSelfHosted(j.Labels)
		}
	}
	if len(j.Labels) == 1 {
		return m.fitHosted(j.Labels[0])
	}
	// A list of GitHub-hosted labels is unusual (a runner must match all of
	// them), so don't recommend it, but say if one label rules it out.
	for _, l := range j.Labels {
		if f, why := m.fitHosted(l); f == FitNo {
			return f, why
		}
	}
	return FitMaybe, "several GitHub-hosted labels; check the job before routing it here"
}

// fitSelfHosted: a self-hosted job needs a runner with every label, native
// or in one of this Mac's Linux containers.
func (m Machine) fitSelfHosted(labels []string) (Fit, string) {
	missing := missingFrom(m.Labels, labels)
	if len(missing) == 0 {
		return FitGood, ""
	}
	for _, a := range m.Linux {
		if len(missingFrom(runner.SelfHostedLabels(route.Linux, a), labels)) == 0 {
			return FitGood, ""
		}
	}
	return FitNo, fmt.Sprintf("self-hosted job needs %s, which this machine doesn't carry", strings.Join(missing, ", "))
}

func missingFrom(have, want []string) []string {
	var missing []string
	for _, l := range want {
		if !slices.ContainsFunc(have, func(h string) bool { return strings.EqualFold(h, l) }) {
			missing = append(missing, l)
		}
	}
	return missing
}

// hostedPlatform maps a GitHub-hosted label to its GOOS and GOARCH.
// ok is false for labels spareci doesn't know (larger runners with custom
// names, for example).
func hostedPlatform(label string) (goos, arch string, ok bool) {
	o, a, ok := route.HostedPlatform(label)
	if !ok {
		return "", "", false
	}
	return map[string]string{route.MacOS: "darwin"}[o] + map[string]string{route.Linux: "linux", "windows": "windows"}[o],
		map[string]string{route.ARM64: "arm64", route.X64: "amd64"}[a], true
}

var osName = map[string]string{"darwin": "macOS", "linux": "Linux", "windows": "Windows"}
var archName = map[string]string{"arm64": "arm64", "amd64": "x64"}

func (m Machine) fitHosted(label string) (Fit, string) {
	goos, arch, ok := hostedPlatform(label)
	switch {
	case !ok:
		return FitMaybe, fmt.Sprintf("spareci doesn't know what %q runs on", label)
	case goos == "windows":
		return FitNo, "Windows job; spareci runs on macOS and Linux"
	case goos == "darwin" && m.OS != "darwin":
		return FitNo, "macOS job; needs a Mac"
	case goos == "linux" && m.OS == "darwin":
		want := archName[arch]
		switch {
		case len(m.Linux) == 0:
			return FitMaybe, "Linux job; this Mac takes it in a container once a container runtime (OrbStack, Docker Desktop, Colima) is running"
		case slices.Contains(m.Linux, want):
			return FitGood, ""
		default:
			return FitMaybe, fmt.Sprintf("%s Linux job; this Mac's containers run %s", want, strings.Join(m.Linux, ", "))
		}
	case goos != m.OS:
		return FitMaybe, fmt.Sprintf("%s job on a %s machine", osName[goos], osName[m.OS])
	case arch != m.Arch:
		return FitMaybe, fmt.Sprintf("%s job on an %s machine", archName[arch], archName[m.Arch])
	}
	return FitGood, ""
}
