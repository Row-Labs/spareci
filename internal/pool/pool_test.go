package pool

import (
	"testing"

	"github.com/Row-Labs/spareci/internal/gh"
)

func TestMatches(t *testing.T) {
	p := New(Config{Labels: []string{"self-hosted", "macOS", "ARM64", "spareci"}}, nil, nil)
	cases := []struct {
		labels []string
		want   bool
	}{
		{[]string{"spareci"}, true},
		{[]string{"self-hosted", "macos"}, true}, // case-insensitive, like GitHub
		{[]string{"self-hosted", "macOS", "ARM64", "spareci"}, true},
		{[]string{"macos-15"}, false}, // GitHub-hosted label
		{[]string{"spareci", "gpu"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := p.Matches(gh.Job{Labels: c.labels}); got != c.want {
			t.Errorf("Matches(%v) = %v, want %v", c.labels, got, c.want)
		}
	}
}
