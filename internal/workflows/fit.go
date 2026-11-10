package workflows

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Fit is how well this machine suits a job.
type Fit int

const (
	// FitGood: same OS and architecture, or a self-hosted job whose labels
	// this machine all carries. Recommended.
	FitGood Fit = iota
	// FitMaybe: might work (another architecture, a Linux job on a Mac, a
	// label spareci doesn't know). Only converted when asked for by name.
	FitMaybe
	// FitNo: can't run here (Windows, macOS on Linux, containers on macOS).
	FitNo
)

// Machine describes the host spareci would run jobs on.
type Machine struct {
	OS     string   // GOOS: darwin or linux
	Arch   string   // GOARCH: arm64 or amd64
	Labels []string // runner labels, e.g. self-hosted, macOS, ARM64, spareci
}

// Assess sets each job's Fit and Why for this machine.
func (m Machine) Assess(files []*File) {
	for _, f := range files {
		for _, j := range f.Jobs {
			if j.Skip == "" {
				j.Fit, j.Why = m.fit(j)
			}
		}
	}
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

// fitSelfHosted: a self-hosted job needs a runner with every label.
func (m Machine) fitSelfHosted(labels []string) (Fit, string) {
	var missing []string
	for _, l := range labels {
		found := false
		for _, have := range m.Labels {
			if strings.EqualFold(l, have) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, l)
		}
	}
	if len(missing) > 0 {
		return FitNo, fmt.Sprintf("self-hosted job needs %s, which this machine doesn't carry", strings.Join(missing, ", "))
	}
	return FitGood, ""
}

var macosVersion = regexp.MustCompile(`^macos-(\d+)`)

// hostedPlatform maps a GitHub-hosted label to its OS and architecture.
// ok is false for labels spareci doesn't know (larger runners with custom
// names, for example).
func hostedPlatform(label string) (goos, arch string, ok bool) {
	l := strings.ToLower(label)
	switch {
	case strings.HasPrefix(l, "ubuntu-"):
		if strings.Contains(l, "-arm") {
			return "linux", "arm64", true
		}
		return "linux", "amd64", true
	case strings.HasPrefix(l, "windows-"):
		if strings.Contains(l, "-arm") {
			return "windows", "arm64", true
		}
		return "windows", "amd64", true
	case strings.HasPrefix(l, "macos-"):
		switch {
		case strings.HasSuffix(l, "-xlarge"):
			return "darwin", "arm64", true
		case strings.HasSuffix(l, "-large"), strings.HasSuffix(l, "-intel"):
			return "darwin", "amd64", true
		case l == "macos-latest":
			return "darwin", "arm64", true
		}
		if mm := macosVersion.FindStringSubmatch(l); mm != nil {
			if v, _ := strconv.Atoi(mm[1]); v >= 14 {
				return "darwin", "arm64", true
			}
			return "darwin", "amd64", true
		}
	}
	return "", "", false
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
	case goos != m.OS:
		return FitMaybe, fmt.Sprintf("%s job on a %s machine", osName[goos], osName[m.OS])
	case arch != m.Arch:
		return FitMaybe, fmt.Sprintf("%s job on an %s machine", archName[arch], archName[m.Arch])
	}
	return FitGood, ""
}
