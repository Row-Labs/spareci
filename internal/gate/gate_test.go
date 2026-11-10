package gate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fake is a controllable machine: power, idle time and clock.
type fake struct {
	ac      bool
	acErr   error
	idle    time.Duration
	idleErr error
	now     time.Time
}

func (f *fake) probes() Probes {
	return Probes{
		OnAC: func() (bool, error) { return f.ac, f.acErr },
		Idle: func() (time.Duration, error) { return f.idle, f.idleErr },
		Now:  func() time.Time { return f.now },
	}
}

func at(hhmm string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", "2026-11-10 "+hhmm, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func mustWindow(t *testing.T, s string) *Window {
	t.Helper()
	w, err := ParseWindow(s)
	if err != nil {
		t.Fatal(err)
	}
	return &w
}

func TestNoConditionsAlwaysOpen(t *testing.T) {
	f := &fake{ac: false, idle: 0, now: at("12:00")}
	g := New(Config{}, f.probes())
	if s := g.Check(); !s.Open {
		t.Fatalf("got %v, want open", s)
	}
	if (Config{}).Enabled() {
		t.Error("zero Config reports Enabled")
	}
}

func TestOnlyOnAC(t *testing.T) {
	f := &fake{ac: true, now: at("12:00")}
	g := New(Config{OnlyOnAC: true}, f.probes())
	if s := g.Check(); !s.Open {
		t.Fatalf("on AC: got %v, want open", s)
	}
	f.ac = false
	if s := g.Check(); s.Open || s.Reason != "on battery" {
		t.Fatalf("on battery: got %v", s)
	}
	f.ac = true
	if s := g.Check(); !s.Open {
		t.Fatalf("replugged: got %v, want open", s)
	}
}

func TestIdle(t *testing.T) {
	f := &fake{idle: 30 * time.Second, now: at("12:00")}
	g := New(Config{Idle: 10 * time.Minute}, f.probes())
	s := g.Check()
	if s.Open {
		t.Fatalf("active user: got open")
	}
	if !strings.Contains(s.Reason, "user active") || !strings.Contains(s.Reason, "10m") {
		t.Errorf("reason = %q", s.Reason)
	}
	f.idle = 10 * time.Minute // exactly the threshold counts as idle
	if s := g.Check(); !s.Open {
		t.Fatalf("idle 10m: got %v", s)
	}
	f.idle = 2 * time.Second // user is back
	if s := g.Check(); s.Open {
		t.Fatal("user back: got open")
	}
}

func TestHours(t *testing.T) {
	f := &fake{}
	g := New(Config{Hours: mustWindow(t, "18:00-08:00")}, f.probes())
	cases := map[string]bool{
		"17:59": false,
		"18:00": true, // start inclusive
		"23:59": true,
		"00:00": true, // crosses midnight
		"07:59": true,
		"08:00": false, // end exclusive
		"12:00": false,
	}
	for hhmm, want := range cases {
		f.now = at(hhmm)
		s := g.Check()
		if s.Open != want {
			t.Errorf("%s: open = %v, want %v (%s)", hhmm, s.Open, want, s.Reason)
		}
		if !want && s.Reason != "outside 18:00-08:00" {
			t.Errorf("%s: reason = %q", hhmm, s.Reason)
		}
	}
}

func TestDaytimeWindow(t *testing.T) {
	w := mustWindow(t, "09:30-17:00")
	for hhmm, want := range map[string]bool{"09:29": false, "09:30": true, "16:59": true, "17:00": false, "02:00": false} {
		if got := w.Contains(at(hhmm)); got != want {
			t.Errorf("%s: %v, want %v", hhmm, got, want)
		}
	}
	if w := mustWindow(t, "22:00-24:00"); !w.Contains(at("23:59")) || w.Contains(at("00:00")) {
		t.Error("22:00-24:00 wrong")
	}
}

func TestParseWindowErrors(t *testing.T) {
	for _, s := range []string{"", "18:00", "6pm-8am", "25:00-08:00", "18:00-18:00", "18:00-08:60"} {
		if _, err := ParseWindow(s); err == nil {
			t.Errorf("ParseWindow(%q) succeeded", s)
		}
	}
	if w := mustWindow(t, " 8:05 - 9:00 "); w.String() != "08:05-09:00" {
		t.Errorf("got %s", w)
	}
}

func TestAllConditionsReasons(t *testing.T) {
	f := &fake{ac: false, idle: time.Minute, now: at("12:00")}
	cfg := Config{OnlyOnAC: true, Idle: 5 * time.Minute, Hours: mustWindow(t, "18:00-08:00")}
	g := New(cfg, f.probes())
	s := g.Check()
	for _, want := range []string{"on battery", "user active", "outside 18:00-08:00"} {
		if !strings.Contains(s.Reason, want) {
			t.Errorf("reason %q missing %q", s.Reason, want)
		}
	}
	f.ac, f.idle, f.now = true, time.Hour, at("19:00")
	if s := g.Check(); !s.Open {
		t.Fatalf("all met: got %v", s)
	}
	if got := cfg.String(); got != "on AC power, idle 5m, 18:00-08:00" {
		t.Errorf("Config.String() = %q", got)
	}
}

func TestOnChangeAndSince(t *testing.T) {
	f := &fake{ac: true, now: at("12:00")}
	g := New(Config{OnlyOnAC: true}, f.probes())
	var changes []State
	g.OnChange = func(s State) { changes = append(changes, s) }

	g.Check() // first check always reports
	f.now = at("12:01")
	g.Check() // no change
	f.ac, f.now = false, at("12:02")
	g.Check()
	f.now = at("12:03")
	s := g.Check()

	if len(changes) != 2 || !changes[0].Open || changes[1].Open {
		t.Fatalf("changes = %+v", changes)
	}
	if !s.Since.Equal(at("12:02")) {
		t.Errorf("Since = %v, want 12:02", s.Since)
	}
	if g.State() != s {
		t.Error("State() differs from last Check()")
	}
}

func TestProbeErrorKeepsLastReading(t *testing.T) {
	f := &fake{ac: false, now: at("12:00")}
	g := New(Config{OnlyOnAC: true, Idle: time.Minute}, f.probes())
	f.idle = time.Hour
	if g.Check().Open {
		t.Fatal("on battery: open")
	}
	f.acErr, f.idleErr = errors.New("pmset hiccup"), errors.New("ioreg hiccup")
	f.ac, f.idle = true, 0 // values are ignored when the probe errors
	if s := g.Check(); s.Open || s.Reason != "on battery" {
		t.Fatalf("probe error should keep last reading; got %v", s)
	}
}

func TestVerify(t *testing.T) {
	f := &fake{ac: true}
	if err := New(Config{OnlyOnAC: true, Idle: time.Minute}, f.probes()).Verify(); err != nil {
		t.Fatal(err)
	}
	f.idleErr = errors.New("no idle source")
	if err := New(Config{Idle: time.Minute}, f.probes()).Verify(); err == nil || !strings.Contains(err.Error(), "--idle") {
		t.Errorf("Verify with failing idle probe = %v", err)
	}
	if err := New(Config{OnlyOnAC: true}, Probes{}).Verify(); err == nil {
		t.Error("Verify with no power probe succeeded")
	}
	// Hours needs no probe.
	if err := New(Config{Hours: mustWindow(t, "18:00-08:00")}, Probes{}).Verify(); err != nil {
		t.Error(err)
	}
}

func TestParsePmset(t *testing.T) {
	cases := map[string]bool{
		"Now drawing from 'AC Power'\n -InternalBattery-0 (id=1)\t100%; charged; 0:00 remaining present: true\n": true,
		"Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1)\t87%; discharging; 4:12 remaining\n":       false,
		"Now drawing from 'UPS Power'\n": false,
		"Now drawing from 'AC Power'\n":  true, // desktop Mac: no battery line
	}
	for in, want := range cases {
		got, err := parsePmsetBatt(in)
		if err != nil || got != want {
			t.Errorf("parsePmsetBatt(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parsePmsetBatt("garbage"); err == nil {
		t.Error("garbage parsed")
	}
}

func TestParseHIDIdleTime(t *testing.T) {
	out := "    | |   \"HIDIdleTime\" = 61348916\n    | |   \"HIDIdleTime\" = 5\n"
	d, err := parseHIDIdleTime(out)
	if err != nil || d != 61348916*time.Nanosecond {
		t.Errorf("got %v, %v", d, err)
	}
	if _, err := parseHIDIdleTime("nothing here"); err == nil {
		t.Error("missing key parsed")
	}
}

func TestParseLinuxIdle(t *testing.T) {
	if d, err := parseMutterIdle("(uint64 125000,)\n"); err != nil || d != 125*time.Second {
		t.Errorf("mutter: %v, %v", d, err)
	}
	if d, err := parseMillis("4200\n"); err != nil || d != 4200*time.Millisecond {
		t.Errorf("xprintidle: %v, %v", d, err)
	}
	now := time.UnixMicro(1_000_000_000_000)
	if d, err := parseLogindIdle("IdleHint=yes\nIdleSinceHint=999940000000\n", now); err != nil || d != time.Minute {
		t.Errorf("logind idle: %v, %v", d, err)
	}
	if d, err := parseLogindIdle("IdleHint=no\nIdleSinceHint=0\n", now); err != nil || d != 0 {
		t.Errorf("logind active: %v, %v", d, err)
	}
	if _, err := parseLogindIdle("", now); err == nil {
		t.Error("empty logind output parsed")
	}
}

func TestLinuxOnAC(t *testing.T) {
	type supply map[string]string
	mk := func(t *testing.T, supplies map[string]supply) string {
		root := t.TempDir()
		for name, files := range supplies {
			dir := filepath.Join(root, name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for f, v := range files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte(v+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		return root
	}
	cases := []struct {
		name     string
		supplies map[string]supply
		want     bool
	}{
		{"desktop, no supplies", nil, true},
		{"laptop plugged in", map[string]supply{"AC": {"type": "Mains", "online": "1"}, "BAT0": {"type": "Battery", "status": "Charging"}}, true},
		{"laptop unplugged", map[string]supply{"AC": {"type": "Mains", "online": "0"}, "BAT0": {"type": "Battery", "status": "Discharging"}}, false},
		{"laptop unplugged, full", map[string]supply{"ADP1": {"type": "Mains", "online": "0"}, "BAT0": {"type": "Battery", "status": "Full"}}, false},
		{"USB-C charger", map[string]supply{"ucsi": {"type": "USB", "online": "1"}, "BAT0": {"type": "Battery", "status": "Not charging"}}, true},
		{"battery only, discharging", map[string]supply{"BAT0": {"type": "Battery", "status": "Discharging"}}, false},
		{"desktop with wireless mouse", map[string]supply{"hidpp_battery_0": {"type": "Battery", "scope": "Device", "status": "Discharging"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := mk(t, c.supplies)
			if c.supplies == nil {
				root = filepath.Join(root, "missing")
			}
			got, err := linuxOnAC(root)
			if err != nil || got != c.want {
				t.Errorf("got %v, %v; want %v", got, err, c.want)
			}
		})
	}
}
