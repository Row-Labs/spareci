package route

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Row-Labs/spareci/internal/gh"
	"go.yaml.in/yaml/v3"
)

const hb = "SPARECI_HEARTBEAT"

// machine is a router for one machine of a repo split by OS, as `spareci up
// --os ...` makes it: its OSes' groups, and the old variable only without
// --os (split false).
func machine(f *fake, name string, targets []Target, split bool) *Router {
	r := &Router{API: f, Repo: "o/r", Machine: name, Poll: time.Millisecond,
		Groups: Groups(DefaultBase, DefaultPrefix, hb, targets, !split)}
	if split {
		r.OldHeartbeat = hb
	}
	return r
}

func TestHeartbeatNamesTheMachine(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 3, 0, 0, time.UTC)
	v := Heartbeat(now, "Mac-mini")
	at, m, ok := ParseHeartbeat(v)
	if !ok || !at.Equal(now) || m != "Mac-mini" {
		t.Fatalf("ParseHeartbeat(%q) = %v %q %v", v, at, m, ok)
	}
	// Still a lease: the watchdog's contains(heartbeat, '|<cron>|') works.
	if !strings.Contains(v, "|"+Cron(7)+"|") {
		t.Errorf("heartbeat %q doesn't list the next firing", v)
	}
	// 0.2's heartbeat is the lease alone.
	if _, m, ok := ParseHeartbeat(Lease(now)); !ok || m != "" {
		t.Errorf("0.2 heartbeat: machine %q ok %v", m, ok)
	}
	if _, _, ok := ParseHeartbeat(""); ok {
		t.Error("empty heartbeat parsed")
	}
	if got := CleanMachine("a|b c"); got != "a-b-c" {
		t.Errorf("CleanMachine = %q", got)
	}
}

func TestGroupsSplitByOS(t *testing.T) {
	gs := Groups(DefaultBase, DefaultPrefix, hb, []Target{TargetLinux, TargetLinuxARM}, false)
	if len(gs) != 1 || gs[0].OS != Linux || gs[0].Heartbeat != "SPARECI_HEARTBEAT_LINUX" || len(gs[0].Routes) != 2 {
		t.Fatalf("Linux machine groups = %+v", gs)
	}
	gs = Groups(DefaultBase, DefaultPrefix, hb, []Target{TargetMacOS}, true)
	if len(gs) != 2 || gs[0].Heartbeat != "SPARECI_HEARTBEAT_MACOS" || !gs[1].Legacy() || gs[1].Heartbeat != hb || gs[1].Routes[0] != (Route{"SPARECI_RUNS_ON", "spareci"}) {
		t.Fatalf("single Mac groups = %+v", gs)
	}
	if gs := Groups(DefaultBase, DefaultPrefix, "", []Target{TargetMacOS}, false); gs[0].Heartbeat != "" {
		t.Errorf("no heartbeat base: %+v", gs)
	}
}

// A Mac serving macOS and a Linux box serving Linux share the repo: each
// sets only its own variables and heartbeat, and stopping one leaves the
// other's alone.
func TestTwoMachinesSplitByOS(t *testing.T) {
	f := newFake()
	mac := machine(f, "mini", []Target{TargetMacOS}, true)
	srv := machine(f, "server", []Target{TargetLinux, TargetLinuxARM}, true)
	ctx := context.Background()
	if err := mac.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("second machine with other OSes refused: %v", err)
	}
	for v, want := range map[string]string{
		"SPARECI_RUNS_ON_MACOS": "spareci-macos", "SPARECI_RUNS_ON_LINUX": "spareci-linux", "SPARECI_RUNS_ON_LINUX_ARM": "spareci-linux-arm",
	} {
		if f.vars[v] != want {
			t.Errorf("%s = %q, want %q", v, f.vars[v], want)
		}
	}
	if _, m, _ := ParseHeartbeat(f.vars["SPARECI_HEARTBEAT_MACOS"]); m != "mini" {
		t.Errorf("macOS heartbeat names %q", m)
	}
	if _, m, _ := ParseHeartbeat(f.vars["SPARECI_HEARTBEAT_LINUX"]); m != "server" {
		t.Errorf("Linux heartbeat names %q", m)
	}
	if _, ok := f.vars["SPARECI_RUNS_ON"]; ok {
		t.Error("a machine with --os set the old shared variable")
	}
	if _, ok := f.vars[hb]; ok {
		t.Error("a machine with --os wrote the old heartbeat")
	}

	if err := srv.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"SPARECI_RUNS_ON_LINUX", "SPARECI_RUNS_ON_LINUX_ARM", "SPARECI_HEARTBEAT_LINUX"} {
		if _, ok := f.vars[v]; ok {
			t.Errorf("%s still set after the Linux machine stopped", v)
		}
	}
	if f.vars["SPARECI_RUNS_ON_MACOS"] != "spareci-macos" || f.vars["SPARECI_HEARTBEAT_MACOS"] == "" {
		t.Errorf("stopping the Linux machine touched macOS routing: %v", f.vars)
	}
	if srv.Routed(gh.Job{Labels: []string{"spareci-macos"}}) || !mac.Routed(gh.Job{Labels: []string{"spareci-macos"}}) {
		t.Error("a machine counts another machine's label as its own")
	}
}

// A rescue clears only this machine's variables for the re-run, so a job of
// the same run routed to the other machine goes back to it.
func TestRescueClearsOnlyOwnVariables(t *testing.T) {
	f := newFake()
	mac := machine(f, "mini", []Target{TargetMacOS}, true)
	srv := machine(f, "server", []Target{TargetLinux}, true)
	_ = mac.Start(context.Background())
	_ = srv.Start(context.Background())
	f.jobs = []gh.Job{
		{Name: "mac", Status: "queued", Labels: []string{"spareci-macos"}},
		{Name: "linux", Status: "queued", Labels: []string{"spareci-linux"}},
	}
	if err := srv.Rescue(context.Background(), 7, false); err != nil {
		t.Fatal(err)
	}
	if f.routing != "spareci-macos" {
		t.Errorf("re-run created with routing %q; want only the Mac's variable set", f.routing)
	}
}

func TestSameOSOnTwoMachinesIsRefused(t *testing.T) {
	f := newFake()
	a := machine(f, "eventric", []Target{TargetLinux}, true)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := f.vars["SPARECI_HEARTBEAT_LINUX"]
	b := machine(f, "server", []Target{TargetLinux}, true)
	err := b.Start(context.Background())
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Owner.Machine != "eventric" || ce.Owner.Group.OS != Linux {
		t.Fatalf("err = %v, want a conflict naming eventric", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "eventric is serving Linux jobs for o/r") {
		t.Errorf("message %q", msg)
	}
	if f.vars["SPARECI_HEARTBEAT_LINUX"] != before {
		t.Error("the refused machine wrote the heartbeat")
	}

	// A machine without --os would take Linux too: also refused.
	c := machine(f, "laptop", []Target{TargetMacOS, TargetLinux}, false)
	if err := c.Start(context.Background()); !errors.As(err, &ce) {
		t.Errorf("all-OS machine over a Linux machine: err %v", err)
	}
}

func TestOwnOrStaleHeartbeatIsNoConflict(t *testing.T) {
	f := newFake()
	// The same machine restarting after a crash.
	f.vars["SPARECI_HEARTBEAT_LINUX"] = Heartbeat(time.Now().Add(-30*time.Second), "server")
	if err := machine(f, "server", []Target{TargetLinux}, true).Start(context.Background()); err != nil {
		t.Errorf("restart: %v", err)
	}
	// A machine that died: its heartbeat is as old as the watchdog's limit.
	f.vars["SPARECI_HEARTBEAT_MACOS"] = Heartbeat(time.Now().Add(-StaleAfter), "old-mac")
	if err := machine(f, "new-mac", []Target{TargetMacOS}, true).Start(context.Background()); err != nil {
		t.Errorf("stale heartbeat: %v", err)
	}
}

// Stop (and so spareci down) leaves a group another machine serves alone,
// and no longer counts its jobs as this machine's.
func TestStopLeavesAnotherMachinesGroup(t *testing.T) {
	f := newFake()
	srv := machine(f, "server", []Target{TargetLinux}, true)
	_ = srv.Start(context.Background())
	// spareci down on the Mac, without --os: every group.
	down := machine(f, "mini", Targets, false)
	if err := down.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.vars["SPARECI_RUNS_ON_LINUX"] != "spareci-linux" || f.vars["SPARECI_HEARTBEAT_LINUX"] == "" {
		t.Errorf("down cleared the Linux machine's routing: %v", f.vars)
	}
	if got := down.Held(); len(got) != 1 || got[0] != "Linux" {
		t.Errorf("Held = %v", got)
	}
	if down.Routed(gh.Job{Labels: []string{"spareci-linux"}}) {
		t.Error("down would rescue the Linux machine's jobs")
	}
	if !down.Routed(gh.Job{Labels: []string{"spareci-macos"}}) {
		t.Error("down no longer rescues its own macOS jobs")
	}
}

// spareci 0.2 wrote SPARECI_HEARTBEAT without a name. Upgrading a single
// machine in place keeps working, and keeps writing the old heartbeat that
// a 0.2 watchdog reads; a machine with --os refuses while a 0.2 is up.
func TestMigrationFrom02(t *testing.T) {
	f := newFake()
	f.vars[hb] = Lease(time.Now().Add(-20 * time.Second))
	f.vars["SPARECI_RUNS_ON"] = "spareci"
	f.vars["SPARECI_RUNS_ON_MACOS"] = "spareci-macos"

	split := machine(f, "server", []Target{TargetLinux}, true)
	err := split.Start(context.Background())
	var ce *ConflictError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "older than 0.3") {
		t.Fatalf("--os machine while 0.2 is up: %v", err)
	}

	single := machine(f, "mini", []Target{TargetMacOS}, false)
	if err := single.Start(context.Background()); err != nil {
		t.Fatalf("upgrade in place: %v", err)
	}
	at, m, ok := ParseHeartbeat(f.vars[hb])
	if !ok || m != "mini" || time.Since(at) > time.Minute {
		t.Errorf("old heartbeat not kept up: %q", f.vars[hb])
	}
	// Still the lease a 0.2 watchdog matches its firings against.
	if !strings.HasPrefix(f.vars[hb], Lease(at)) {
		t.Errorf("old heartbeat %q isn't a lease", f.vars[hb])
	}
	if f.vars["SPARECI_RUNS_ON"] != "spareci" || f.vars["SPARECI_HEARTBEAT_MACOS"] == "" {
		t.Errorf("after upgrade: %v", f.vars)
	}
	if err := single.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.vars) != 0 {
		t.Errorf("after stop: %v", f.vars)
	}
}

// A heartbeat another machine wrote since this one's last beat means both
// serve the same OS: it's logged as an error naming the other machine.
func TestBeatWarnsWhenAnotherMachineWritesTheHeartbeat(t *testing.T) {
	f := newFake()
	a := machine(f, "mini", []Target{TargetMacOS}, true)
	_ = a.Start(context.Background())
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	a.mu.Lock()
	a.warnLocked(context.Background(), time.Now())
	a.mu.Unlock()
	if buf.Len() > 0 {
		t.Fatalf("warned about its own heartbeat: %s", buf.String())
	}
	f.vars["SPARECI_HEARTBEAT_MACOS"] = Heartbeat(time.Now(), "other-mac")
	a.mu.Lock()
	a.warnLocked(context.Background(), time.Now())
	a.mu.Unlock()
	if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "other=other-mac") {
		t.Errorf("log %q", buf.String())
	}
}

// The watchdog's first step decides which OSes' machines are dead. Run it
// with one fresh and one stale heartbeat: only the stale one's group is
// marked, and the job's other steps clear only that group.
func TestWatchdogChecksEachOSOnItsOwn(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(Watchdog(allGroups())), &doc); err != nil {
		t.Fatal(err)
	}
	script := doc.Jobs["watchdog"].Steps[0].Run
	now := time.Now()
	for _, c := range []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"Linux machine dead, Mac alive", map[string]string{
			"SPARECI_RUNS_ON_MACOS": "spareci-macos", "SPARECI_HEARTBEAT_MACOS": Heartbeat(now.Add(-time.Minute), "mini"),
			"SPARECI_RUNS_ON_LINUX": "spareci-linux", "SPARECI_HEARTBEAT_LINUX": Heartbeat(now.Add(-20*time.Minute), "server"),
		}, []string{"linux=true", "stale=true"}},
		{"both alive", map[string]string{
			"SPARECI_RUNS_ON_MACOS": "spareci-macos", "SPARECI_HEARTBEAT_MACOS": Heartbeat(now, "mini"),
			"SPARECI_RUNS_ON_LINUX_ARM": "spareci-linux-arm", "SPARECI_HEARTBEAT_LINUX": Heartbeat(now, "server"),
		}, nil},
		{"0.2 machine alive: the OS groups read the old heartbeat", map[string]string{
			"SPARECI_RUNS_ON": "spareci", "SPARECI_RUNS_ON_MACOS": "spareci-macos", "SPARECI_RUNS_ON_LINUX_ARM": "spareci-linux-arm", "SPARECI_HEARTBEAT": Lease(now),
		}, nil},
		{"own heartbeat wins over the old one", map[string]string{
			"SPARECI_RUNS_ON_MACOS": "spareci-macos", "SPARECI_HEARTBEAT_MACOS": Heartbeat(now.Add(-time.Hour), "mini"), "SPARECI_HEARTBEAT": Heartbeat(now, "mini"),
		}, []string{"macos=true", "stale=true"}},
		{"0.2 machine dead", map[string]string{
			"SPARECI_RUNS_ON": "spareci", "SPARECI_RUNS_ON_MACOS": "spareci-macos", "SPARECI_HEARTBEAT": Lease(now.Add(-time.Hour)),
		}, []string{"macos=true", "stale=true", "legacy=true", "stale=true"}},
		{"routed with no heartbeat", map[string]string{"SPARECI_RUNS_ON_LINUX": "spareci-linux"}, []string{"linux=true", "stale=true"}},
		{"heartbeat without routing", map[string]string{"SPARECI_HEARTBEAT_MACOS": Heartbeat(now.Add(-time.Hour), "mini")}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")
			cmd := exec.Command("bash", "-e", "-c", script)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_OUTPUT=" + out}
			for k, v := range c.env {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
			log, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, log)
			}
			b, _ := os.ReadFile(out)
			got := strings.Fields(string(b))
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("outputs %v, want %v\n%s", got, c.want, log)
			}
			if c.name == "Linux machine dead, Mac alive" && (!strings.Contains(string(log), "no heartbeat from server") || !strings.Contains(string(log), "mini is up")) {
				t.Errorf("log doesn't name the machines:\n%s", log)
			}
		})
	}
}
