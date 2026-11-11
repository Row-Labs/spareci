package runner

import (
	"reflect"
	"runtime"
	"testing"
)

func TestHostIsNativeBackend(t *testing.T) {
	var b Backend = &Host{}
	if b.OS() == "" || b.Arch() == "" {
		t.Skipf("unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if got, want := b.Labels(), DefaultLabels(); !reflect.DeepEqual(got, want) {
		t.Errorf("Host.Labels() = %v, DefaultLabels() = %v", got, want)
	}
}

func TestSelfHostedLabels(t *testing.T) {
	for _, tc := range []struct {
		os, arch string
		want     []string
	}{
		{OSMacOS, ArchARM, []string{"self-hosted", "macOS", "ARM64"}},
		{OSLinux, ArchARM, []string{"self-hosted", "Linux", "ARM64"}},
		{OSLinux, ArchX64, []string{"self-hosted", "Linux", "X64"}},
	} {
		if got := SelfHostedLabels(tc.os, tc.arch); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("SelfHostedLabels(%s, %s) = %v, want %v", tc.os, tc.arch, got, tc.want)
		}
	}
}
