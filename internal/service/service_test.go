package service

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"time"
)

var spec = Spec{
	Repo:  "Row-Labs/spareci",
	Exec:  "/Users/me/go/bin/spareci",
	Args:  []string{"up", "--repo", "Row-Labs/spareci", "--label", "a&b", "--max-wait", "15m"},
	Env:   map[string]string{"PATH": "/opt/homebrew/bin:/usr/bin", "B": "50% $off"},
	Log:   "/Users/me/Library/Caches/spareci/service/spareci-row-labs-spareci.log",
	Drain: 10 * time.Minute, StopAfter: 5 * time.Minute,
}

func TestName(t *testing.T) {
	if got := Name("Row-Labs/spare.ci"); got != "spareci-row-labs-spare-ci" {
		t.Errorf("Name = %q", got)
	}
	if got := LaunchdLabel("Row-Labs/spareci"); got != "dev.spareci.row-labs-spareci" {
		t.Errorf("LaunchdLabel = %q", got)
	}
}

func TestPlistIsValidXMLWithRestartOnFailure(t *testing.T) {
	p := Plist(spec)
	d := xml.NewDecoder(strings.NewReader(p))
	d.Strict = true
	var texts []string
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("invalid XML: %v\n%s", err, p)
		}
		if c, ok := tok.(xml.CharData); ok && strings.TrimSpace(string(c)) != "" {
			texts = append(texts, string(c))
		}
	}
	joined := strings.Join(texts, "\n")
	for _, want := range []string{
		"dev.spareci.row-labs-spareci", spec.Exec, "a&b", "--max-wait", spec.Log, "/opt/homebrew/bin:/usr/bin",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("plist missing %q", want)
		}
	}
	for _, want := range []string{
		"<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<string>a&amp;b</string>",
		// launchd waits out the drain before it SIGKILLs spareci.
		"<key>ExitTimeOut</key>\n\t<integer>900</integer>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q:\n%s", want, p)
		}
	}
}

func TestUnit(t *testing.T) {
	u := Unit(spec)
	for _, want := range []string{
		`ExecStart="/Users/me/go/bin/spareci" "up" "--repo" "Row-Labs/spareci" "--label" "a&b" "--max-wait" "15m"`,
		`Environment="B=50%% $$off"`,
		`Environment="PATH=/opt/homebrew/bin:/usr/bin"`,
		"Restart=on-failure",
		"KillMode=mixed", // SIGTERM to spareci only, so runners drain
		"TimeoutStopSec=900s",
		"StandardOutput=append:" + spec.Log,
		"WantedBy=default.target",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit missing %q:\n%s", want, u)
		}
	}
}

func TestSystemdQuote(t *testing.T) {
	cases := map[string]string{
		`plain`:      `"plain"`,
		`has space`:  `"has space"`,
		`q"uote`:     `"q\"uote"`,
		`back\slash`: `"back\\slash"`,
		`100%`:       `"100%%"`,
		`$HOME`:      `"$$HOME"`,
	}
	for in, want := range cases {
		if got := systemdQuote(in); got != want {
			t.Errorf("systemdQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestRepoArgReadsBackBothFormats(t *testing.T) {
	if got := repoArg(Plist(spec)); got != "Row-Labs/spareci" {
		t.Errorf("plist: %q", got)
	}
	if got := repoArg(Unit(spec)); got != "Row-Labs/spareci" {
		t.Errorf("unit: %q", got)
	}
}

func TestNoDrainLimitMeansNoStopTimeout(t *testing.T) {
	s := spec
	s.Drain = 0
	if !strings.Contains(Plist(s), "<key>ExitTimeOut</key>\n\t<integer>0</integer>") {
		t.Error("plist: want ExitTimeOut 0 (no limit)")
	}
	if !strings.Contains(Unit(s), "TimeoutStopSec=infinity") {
		t.Error("unit: want TimeoutStopSec=infinity")
	}
}
