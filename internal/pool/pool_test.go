package pool

import (
	"testing"
	"time"

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

func TestOverdue(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	jobs := []gh.Job{
		{RunID: 1, CreatedAt: now.Add(-11 * time.Minute)},
		{RunID: 1, CreatedAt: now.Add(-12 * time.Minute)}, // same run: listed once
		{RunID: 2, CreatedAt: now.Add(-9 * time.Minute)},  // not yet
		{RunID: 3}, // no timestamp: never
	}
	got := Overdue(jobs, 10*time.Minute, now)
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("Overdue = %v, want [1]", got)
	}
	if got := Overdue(jobs, 0, now); got != nil {
		t.Errorf("max-wait 0 should disable, got %v", got)
	}
}
