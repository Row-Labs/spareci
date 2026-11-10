package pool

import (
	"testing"

	"github.com/blaze-rowland/homerun/internal/gh"
)

func TestMatches(t *testing.T) {
	p := New(Config{Labels: []string{"self-hosted", "macOS", "ARM64", "homerun"}}, nil, nil)
	cases := []struct {
		labels []string
		want   bool
	}{
		{[]string{"homerun"}, true},
		{[]string{"self-hosted", "macos"}, true}, // case-insensitive, like GitHub
		{[]string{"self-hosted", "macOS", "ARM64", "homerun"}, true},
		{[]string{"macos-15"}, false}, // GitHub-hosted label
		{[]string{"homerun", "gpu"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := p.Matches(gh.Job{Labels: c.labels}); got != c.want {
			t.Errorf("Matches(%v) = %v, want %v", c.labels, got, c.want)
		}
	}
}
