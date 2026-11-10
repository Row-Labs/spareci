// Package estimate works out what a repo's GitHub Actions jobs cost on
// GitHub-hosted runners, and how much of that a self-hosted machine could take.
package estimate

import (
	"regexp"
	"strconv"
	"strings"
)

// GitHub's list prices per minute, in USD.
//
// Source: https://docs.github.com/en/billing/reference/actions-runner-pricing
// (checked 2026-11-10). These are the prices from January 1, 2026
// (https://github.com/resources/insights/2026-pricing-changes-for-github-actions).
// The same page says each job is rounded up to the whole minute, that standard
// runners are free in public repos, and that larger runners are billed even in
// public repos.
//
// Self-hosted runners are free. GitHub announced a $0.002/min platform charge
// for them from March 1, 2026, then postponed it on December 15, 2025 with no
// new date. If it comes back, set SelfHostedPerMinute.
const (
	PricesChecked = "2026-11-10"
	PricesSource  = "https://docs.github.com/en/billing/reference/actions-runner-pricing"

	SelfHostedPerMinute = 0.0
)

// Runner is one priced GitHub-hosted runner type.
type Runner struct {
	SKU       string  `json:"sku"`
	Name      string  `json:"name"`
	OS        string  `json:"os"`   // Linux, Windows, macOS
	Arch      string  `json:"arch"` // X64, ARM64
	Larger    bool    `json:"larger"`
	PerMinute float64 `json:"per_minute"`
}

var prices = map[string]Runner{}

func price(sku, name, os, arch string, larger bool, perMin float64) {
	prices[sku] = Runner{SKU: sku, Name: name, OS: os, Arch: arch, Larger: larger, PerMinute: perMin}
}

func init() {
	// Standard runners.
	price("actions_linux_slim", "Linux 1-core", "Linux", "X64", false, 0.002)
	price("actions_linux", "Linux 2-core", "Linux", "X64", false, 0.006)
	price("actions_linux_arm", "Linux 2-core arm64", "Linux", "ARM64", false, 0.005)
	price("actions_windows", "Windows 2-core", "Windows", "X64", false, 0.010)
	price("actions_windows_arm", "Windows 2-core arm64", "Windows", "ARM64", false, 0.010)
	price("actions_macos", "macOS standard", "macOS", "ARM64", false, 0.062)
	price("actions_macos_intel", "macOS standard Intel", "macOS", "X64", false, 0.062)

	// Larger runners.
	price("macos_l", "macOS 12-core Intel", "macOS", "X64", true, 0.077)
	price("macos_xl", "macOS 5-core M2 Pro", "macOS", "ARM64", true, 0.102)
	for cores, p := range map[int]float64{2: 0.006, 4: 0.012, 8: 0.022, 16: 0.042, 32: 0.082, 64: 0.162, 96: 0.252} {
		price(larger("linux", cores, ""), "Linux "+strconv.Itoa(cores)+"-core", "Linux", "X64", true, p)
	}
	for cores, p := range map[int]float64{4: 0.022, 8: 0.042, 16: 0.082, 32: 0.162, 64: 0.322, 96: 0.552} {
		price(larger("windows", cores, ""), "Windows "+strconv.Itoa(cores)+"-core", "Windows", "X64", true, p)
	}
	for cores, p := range map[int]float64{2: 0.005, 4: 0.008, 8: 0.014, 16: 0.026, 32: 0.050, 64: 0.098} {
		price(larger("linux", cores, "arm"), "Linux "+strconv.Itoa(cores)+"-core arm64", "Linux", "ARM64", true, p)
	}
	for cores, p := range map[int]float64{2: 0.008, 4: 0.014, 8: 0.026, 16: 0.050, 32: 0.098, 64: 0.194} {
		price(larger("windows", cores, "arm"), "Windows "+strconv.Itoa(cores)+"-core arm64", "Windows", "ARM64", true, p)
	}
	price("linux_4_core_gpu", "Linux 4-core GPU", "Linux", "X64", true, 0.052)
	price("windows_4_core_gpu", "Windows 4-core GPU", "Windows", "X64", true, 0.102)
}

func larger(os string, cores int, variant string) string {
	sku := os + "_" + strconv.Itoa(cores) + "_core"
	if variant != "" {
		sku += "_" + variant
	}
	return sku
}

// Price looks up a runner by SKU.
func Price(sku string) (Runner, bool) {
	r, ok := prices[sku]
	return r, ok
}

var coresRe = regexp.MustCompile(`(\d+)[-_ ]?(?:cores?|vcpus?|cpus?)\b`)

// standardSKU maps a GitHub-hosted image label (ubuntu-24.04, macos-15,
// windows-11-arm, ...) to its standard runner. ok is false for labels that
// are not GitHub images.
func standardSKU(label string) (string, bool) {
	l := strings.ToLower(label)
	switch {
	case l == "ubuntu-slim":
		return "actions_linux_slim", true
	case strings.HasPrefix(l, "ubuntu-"):
		if strings.HasSuffix(l, "-arm") {
			return "actions_linux_arm", true
		}
		return "actions_linux", true
	case strings.HasPrefix(l, "windows-"):
		if strings.HasSuffix(l, "-arm") {
			return "actions_windows_arm", true
		}
		return "actions_windows", true
	case strings.HasPrefix(l, "macos-"):
		switch {
		case strings.HasSuffix(l, "-xlarge"):
			return "macos_xl", true
		case strings.HasSuffix(l, "-large"):
			return "macos_l", true
		case strings.HasSuffix(l, "-intel"), l == "macos-13", l == "macos-12", l == "macos-11":
			return "actions_macos_intel", true
		}
		return "actions_macos", true
	}
	return "", false
}

// largerSKU guesses a larger runner's SKU from the label an org gave it, such
// as "ubuntu-4-core" or "windows-arm-16core". GitHub's API does not report a
// larger runner's size on the job, so this only works when the name says it.
func largerSKU(labels []string) (string, bool) {
	for _, label := range labels {
		l := strings.ToLower(label)
		m := coresRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		os := "linux"
		if strings.Contains(l, "windows") || strings.Contains(l, "win") {
			os = "windows"
		}
		variant := ""
		switch {
		case strings.Contains(l, "gpu"):
			variant = "gpu"
		case strings.Contains(l, "arm"):
			variant = "arm"
		}
		cores, _ := strconv.Atoi(m[1])
		sku := larger(os, cores, variant)
		if _, ok := prices[sku]; ok {
			return sku, true
		}
	}
	return "", false
}
