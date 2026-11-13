package route

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Row-Labs/spareci/internal/gh"
	"go.yaml.in/yaml/v3"
)

// fake is an in-memory repo: variables plus one run.
type fake struct {
	mu      sync.Mutex
	vars    map[string]string
	run     gh.Run
	jobs    []gh.Job
	calls   []string
	stuck   bool   // ignore normal cancel, like a run held by a dead runner
	lag     int    // re-runs refused as "already running" after the run completed
	skip    string // a job a failed-jobs re-run leaves out, as GitHub once did
	routing string // routing variables set when the re-run was created
	clock   time.Time
}

func (f *fake) cancelJobs() {
	for i := range f.jobs {
		if f.jobs[i].Status != "completed" {
			f.jobs[i].Status, f.jobs[i].Conclusion, f.jobs[i].CompletedAt = "completed", "cancelled", f.tick()
		}
	}
}

func newFake() *fake {
	return &fake{vars: map[string]string{}, run: gh.Run{ID: 7, Status: "queued", RunAttempt: 1}, clock: time.Unix(1000, 0)}
}

// tick is the fake's clock: each GitHub action happens a second later.
func (f *fake) tick() time.Time { f.clock = f.clock.Add(time.Second); return f.clock }

func (f *fake) log(s string) { f.calls = append(f.calls, s) }

func (f *fake) SetVariable(_ context.Context, _, name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vars[name] = value
	f.log("set " + name)
	return nil
}

func (f *fake) GetVariable(_ context.Context, _, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vars[name], nil
}

func (f *fake) DeleteVariable(_ context.Context, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vars, name)
	f.log("delete " + name)
	return nil
}

func (f *fake) RunJobs(context.Context, string, int64) ([]gh.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobs, nil
}

func (f *fake) GetRun(context.Context, string, int64) (gh.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.run, nil
}

func (f *fake) CancelRun(context.Context, string, int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("cancel")
	if !f.stuck {
		f.run.Status = "completed"
		f.cancelJobs()
	}
	return nil
}

func (f *fake) ForceCancelRun(context.Context, string, int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("force-cancel")
	f.run.Status = "completed"
	f.cancelJobs()
	return nil
}

func (f *fake) rerun(kind string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.run.Status != "completed" || f.lag > 0 {
		f.lag--
		f.log(kind + " refused")
		return &gh.APIError{Status: 403, Body: `{"message":"This workflow is already running"}`}
	}
	f.log(kind)
	f.routing = ""
	for _, v := range Vars(DefaultBase) {
		f.routing += f.vars[v]
	}
	f.run.RunAttempt++
	f.run.Status = "queued"
	f.run.RunStartedAt = f.tick()
	for i, j := range f.jobs {
		again := kind == "rerun-all" || (j.Conclusion != "success" && j.Name != f.skip)
		if again {
			f.jobs[i] = gh.Job{Name: j.Name, Status: "queued", Labels: []string{"ubuntu-24.04"}}
		}
		// Like GitHub, list every job under the new attempt number, re-run or not.
		f.jobs[i].RunAttempt = f.run.RunAttempt
	}
	return nil
}

func (f *fake) RerunFailedJobs(context.Context, string, int64) error { return f.rerun("rerun-failed") }
func (f *fake) RerunRun(context.Context, string, int64) error        { return f.rerun("rerun-all") }

func router(f *fake) *Router {
	return &Router{API: f, Repo: "o/r", Machine: "mac", Poll: time.Millisecond,
		Groups: []Group{{Heartbeat: "SPARECI_HEARTBEAT", Routes: []Route{{"SPARECI_RUNS_ON", "spareci"}}}}}
}

func TestStartStop(t *testing.T) {
	f := newFake()
	r := router(f)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.vars["SPARECI_RUNS_ON"] != "spareci" || !strings.Contains(f.vars["SPARECI_HEARTBEAT"], "|") {
		t.Fatalf("after Start: %v", f.vars)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.vars) != 0 {
		t.Fatalf("after Stop: %v", f.vars)
	}
}

func TestRescueReRunsWithRoutingCleared(t *testing.T) {
	f := newFake()
	f.jobs = []gh.Job{{Name: "test", Status: "queued", Labels: []string{"spareci"}}}
	r := router(f)
	_ = r.Start(context.Background())
	if err := r.Rescue(context.Background(), 7, false); err != nil {
		t.Fatal(err)
	}
	if f.routing != "" {
		t.Errorf("re-run created with routing %q; want it cleared so the job goes to GitHub", f.routing)
	}
	if f.vars["SPARECI_RUNS_ON"] != "spareci" {
		t.Errorf("routing not restored after rescue: %v", f.vars)
	}
	if !contains(f.calls, "rerun-failed") {
		t.Errorf("calls %v: want a failed-jobs re-run", f.calls)
	}
}

func TestRescueReRunsEverythingIfGitHubSkipsAJob(t *testing.T) {
	f := newFake()
	f.skip = "build"
	f.jobs = []gh.Job{
		{Name: "build", Status: "queued", RunAttempt: 1, Labels: []string{"spareci"}},
		{Name: "cache-save", Status: "queued", RunAttempt: 1, Labels: []string{"spareci"}},
	}
	r := router(f)
	_ = r.Start(context.Background())
	if err := r.Rescue(context.Background(), 7, false); err != nil {
		t.Fatal(err)
	}
	want := []string{"cancel", "delete SPARECI_RUNS_ON", "rerun-failed", "cancel", "rerun-all", "set SPARECI_RUNS_ON"}
	got := f.calls[2:] // after Start's two sets
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("calls %v, want %v", got, want)
	}
	for _, j := range f.jobs {
		if j.Status != "queued" || j.Labels[0] != "ubuntu-24.04" {
			t.Errorf("job %s not re-run on GitHub: %+v", j.Name, j)
		}
	}
	if f.routing != "" {
		t.Errorf("whole re-run created with routing %q", f.routing)
	}
}

func TestRescueWholeRunWhenAJobRanHere(t *testing.T) {
	f := newFake()
	f.jobs = []gh.Job{
		{Name: "build", Status: "completed", Conclusion: "success", Labels: []string{"spareci"}},
		{Name: "test", Status: "queued", Labels: []string{"spareci"}},
	}
	r := router(f)
	if err := r.Rescue(context.Background(), 7, false); err != nil {
		t.Fatal(err)
	}
	if !contains(f.calls, "rerun-all") {
		t.Errorf("calls %v: want the whole run re-run, since build's caches are local", f.calls)
	}
}

func TestRescueOnlyFailedWhenEarlierJobsRanOnGitHub(t *testing.T) {
	f := newFake()
	f.jobs = []gh.Job{
		{Name: "lint", Status: "completed", Conclusion: "success", Labels: []string{"ubuntu-24.04"}},
		{Name: "test", Status: "queued", Labels: []string{"spareci"}},
	}
	if err := router(f).Rescue(context.Background(), 7, false); err != nil {
		t.Fatal(err)
	}
	if !contains(f.calls, "rerun-failed") {
		t.Errorf("calls %v", f.calls)
	}
}

func TestRescueLeavesBusyRunsAlone(t *testing.T) {
	f := newFake()
	f.jobs = []gh.Job{
		{Name: "a", Status: "in_progress", Labels: []string{"spareci"}},
		{Name: "b", Status: "queued", Labels: []string{"spareci"}},
	}
	r := router(f)
	if err := r.Rescue(context.Background(), 7, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("busy run was touched: %v", f.calls)
	}
	if err := r.Rescue(context.Background(), 7, true); err != nil {
		t.Fatalf("forced: %v", err)
	}
}

func TestRescueForceCancelsAStuckRun(t *testing.T) {
	f := newFake()
	f.stuck = true
	f.jobs = []gh.Job{{Name: "test", Status: "queued", Labels: []string{"spareci"}}}
	r := router(f)
	r.CancelWait = 10 * time.Millisecond
	if err := r.Rescue(context.Background(), 7, true); err != nil {
		t.Fatal(err)
	}
	if !contains(f.calls, "force-cancel") {
		t.Errorf("calls %v", f.calls)
	}
}

func TestLeaseCoversFiringsAroundNow(t *testing.T) {
	now := time.Date(2026, 11, 10, 19, 41, 30, 0, time.UTC)
	lease := Lease(now)
	if !strings.HasPrefix(lease, fmt.Sprint(now.Unix())+"|") {
		t.Fatalf("lease %q doesn't start with the timestamp", lease)
	}
	covered := func(m int) bool { return strings.Contains(lease, "|"+Cron(m)+"|") }
	// Window is 19:11:30 .. 19:51:30.
	for _, m := range []int{12, 17, 22, 27, 32, 37, 42, 47} {
		if !covered(m) {
			t.Errorf("minute %d should be covered: %q", m, lease)
		}
	}
	for _, m := range []int{52, 57, 2, 7} {
		if covered(m) {
			t.Errorf("minute %d should not be covered: %q", m, lease)
		}
	}
	// "2 * * * *" must not match inside "52 * * * *": the delimiters matter.
	if strings.Contains(lease, "|2 * * * *|") {
		t.Errorf("substring match: %q", lease)
	}
}

// A frozen lease (daemon dead) must stop covering firings soon: within
// LeaseAhead plus one slot, for any time of death.
func TestDeadDaemonIsNoticedWithinOneSlotAfterLease(t *testing.T) {
	start := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	for d := time.Duration(0); d < time.Hour; d += 37 * time.Second {
		death := start.Add(d)
		lease := Lease(death)
		found := false
		for t2 := death; t2.Before(death.Add(LeaseAhead + SlotEvery*time.Minute + time.Minute)); t2 = t2.Add(time.Minute) {
			if (t2.Minute()-SlotOffset)%SlotEvery == 0 && !strings.Contains(lease, "|"+Cron(t2.Minute())+"|") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("death at %s: no uncovered firing within %s (lease %q)", death.Format("15:04:05"), LeaseAhead+SlotEvery*time.Minute, lease)
		}
	}
}

// While the daemon is alive, a firing that starts up to LeaseBehind late is
// still covered and costs nothing.
func TestLateFiringIsCoveredWhileAlive(t *testing.T) {
	fire := time.Date(2026, 11, 10, 19, 2, 0, 0, time.UTC)
	for late := time.Duration(0); late <= LeaseBehind; late += time.Minute {
		if !strings.Contains(Lease(fire.Add(late)), "|"+Cron(2)+"|") {
			t.Errorf("firing %s late is not covered", late)
		}
	}
}

func allGroups() []Group { return AllGroups(DefaultBase, DefaultPrefix, "SPARECI_HEARTBEAT") }

func TestWatchdogWorkflow(t *testing.T) {
	w := Watchdog(allGroups())
	for _, m := range Slots() {
		if !strings.Contains(w, fmt.Sprintf("- cron: \"%s\"", Cron(m))) {
			t.Errorf("missing cron for minute %d", m)
		}
	}
	for _, want := range []string{
		// Skipped unless some OS is routed and its own heartbeat doesn't list the firing.
		"if: ((vars.SPARECI_RUNS_ON_MACOS) && !contains((vars.SPARECI_HEARTBEAT_MACOS || vars.SPARECI_HEARTBEAT), format('|{0}|', github.event.schedule))) || " +
			"((vars.SPARECI_RUNS_ON_LINUX || vars.SPARECI_RUNS_ON_LINUX_ARM) && !contains((vars.SPARECI_HEARTBEAT_LINUX || vars.SPARECI_HEARTBEAT), format('|{0}|', github.event.schedule))) || " +
			"((vars.SPARECI_RUNS_ON) && !contains(vars.SPARECI_HEARTBEAT, format('|{0}|', github.event.schedule)))\n",
		"SPARECI_RUNS_ON_LINUX_ARM: ${{ vars.SPARECI_RUNS_ON_LINUX_ARM }}",
		"SPARECI_HEARTBEAT_LINUX: ${{ vars.SPARECI_HEARTBEAT_LINUX }}",
		`check macOS macos "${SPARECI_HEARTBEAT_MACOS:-$SPARECI_HEARTBEAT}" "$SPARECI_RUNS_ON_MACOS"`,
		`check Linux linux "${SPARECI_HEARTBEAT_LINUX:-$SPARECI_HEARTBEAT}" "$SPARECI_RUNS_ON_LINUX$SPARECI_RUNS_ON_LINUX_ARM"`,
		`check SPARECI_RUNS_ON legacy "$SPARECI_HEARTBEAT" "$SPARECI_RUNS_ON"`,
		"STALE_LINUX: ${{ steps.beat.outputs.linux }}",
		`if [ "$STALE_LINUX" = true ]; then`,
		`unroute SPARECI_RUNS_ON_LINUX_ARM "$SPARECI_RUNS_ON_LINUX_ARM"`,
		`unroute SPARECI_RUNS_ON_MACOS "$SPARECI_RUNS_ON_MACOS"`,
		`actions/variables/SPARECI_HEARTBEAT_MACOS"`,
		"VAR_TOKEN: ${{ steps.app.outputs.token || secrets.SPARECI_TOKEN }}",
		"uses: actions/create-github-app-token@v3",
		"client-id: ${{ secrets.SPARECI_APP_CLIENT_ID }}",
		"private-key: ${{ secrets.SPARECI_APP_KEY }}",
		"if: steps.beat.outputs.stale == 'true' && env.HAS_APP == 'true'",
		"rerun-failed-jobs",
		"(( now - beat < 300 ))",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("workflow missing %q", want)
		}
	}
	if strings.Contains(w, "<%") || strings.Contains(w, "%>") {
		t.Error("unexpanded template action")
	}

	// It's valid YAML, the app step comes before the step that uses its token,
	// and a failed mint falls through to the SPARECI_TOKEN fallback.
	var doc struct {
		Jobs map[string]struct {
			Steps []map[string]any `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(w), &doc); err != nil {
		t.Fatalf("workflow isn't YAML: %v", err)
	}
	steps := doc.Jobs["watchdog"].Steps
	if len(steps) != 3 || steps[0]["id"] != "beat" || steps[1]["id"] != "app" || steps[1]["continue-on-error"] != true {
		t.Errorf("steps = %v", steps)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func multiRouter(f *fake) *Router {
	r := router(f)
	r.Groups = Groups(DefaultBase, DefaultPrefix, "SPARECI_HEARTBEAT", []Target{TargetMacOS, TargetLinux}, true)
	return r
}

func TestStartStopEveryRoute(t *testing.T) {
	f := newFake()
	r := multiRouter(f)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"SPARECI_RUNS_ON_MACOS": "spareci-macos", "SPARECI_RUNS_ON_LINUX": "spareci-linux", "SPARECI_RUNS_ON": "spareci"}
	for k, v := range want {
		if f.vars[k] != v {
			t.Errorf("%s = %q, want %q", k, f.vars[k], v)
		}
	}
	if _, ok := f.vars["SPARECI_RUNS_ON_LINUX_ARM"]; ok {
		t.Error("set the arm64 Linux variable without an arm64 Linux backend")
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.vars) != 0 {
		t.Fatalf("after Stop: %v", f.vars)
	}
}

func TestRescueClearsEveryRouteForTheReRun(t *testing.T) {
	f := newFake()
	f.jobs = []gh.Job{
		{Name: "mac", Status: "queued", Labels: []string{"spareci-macos"}},
		{Name: "linux", Status: "queued", Labels: []string{"spareci-linux"}},
	}
	r := multiRouter(f)
	_ = r.Start(context.Background())
	if err := r.Rescue(context.Background(), 7, false); err != nil {
		t.Fatal(err)
	}
	if f.routing != "" {
		t.Errorf("re-run created with routing %q set; want every variable cleared", f.routing)
	}
	if f.vars["SPARECI_RUNS_ON_LINUX"] != "spareci-linux" || f.vars["SPARECI_RUNS_ON_MACOS"] != "spareci-macos" {
		t.Errorf("routing not restored after rescue: %v", f.vars)
	}
}

func TestRescueWholeRunWhenALinuxJobRanHere(t *testing.T) {
	f := newFake()
	f.jobs = []gh.Job{
		{Name: "build", Status: "completed", Conclusion: "success", Labels: []string{"spareci-linux"}},
		{Name: "test", Status: "queued", Labels: []string{"spareci-macos"}},
	}
	r := multiRouter(f)
	_ = r.Start(context.Background())
	if err := r.Rescue(context.Background(), 7, false); err != nil {
		t.Fatal(err)
	}
	if !contains(f.calls, "rerun-all") {
		t.Errorf("calls %v: want the whole run re-run", f.calls)
	}
}

func TestNames(t *testing.T) {
	for _, c := range []struct {
		labels []string
		want   Target
		ok     bool
	}{
		{[]string{"macos-15"}, TargetMacOS, true},
		{[]string{"macos-13"}, TargetMacOS, true},
		{[]string{"ubuntu-24.04"}, TargetLinux, true},
		{[]string{"ubuntu-latest"}, TargetLinux, true},
		{[]string{"ubuntu-24.04-arm"}, TargetLinuxARM, true},
		{[]string{"self-hosted", "Linux", "ARM64"}, TargetLinuxARM, true},
		{[]string{"self-hosted", "linux"}, TargetLinux, true},
		{[]string{"self-hosted", "macOS"}, TargetMacOS, true},
		{[]string{"windows-latest"}, Target{}, false},
		{[]string{"my-big-runner"}, Target{}, false},
	} {
		got, ok := TargetFor(c.labels)
		if got != c.want || ok != c.ok {
			t.Errorf("TargetFor(%v) = %v, %v; want %v, %v", c.labels, got, ok, c.want, c.ok)
		}
	}
	if v, l := TargetLinuxARM.Var("SPARECI_RUNS_ON"), TargetLinuxARM.Label("spareci"); v != "SPARECI_RUNS_ON_LINUX_ARM" || l != "spareci-linux-arm" {
		t.Errorf("arm64 Linux: %s %s", v, l)
	}
	if got := Groups("", "spareci", "SPARECI_HEARTBEAT", Targets, true); got != nil {
		t.Errorf("empty base routes %v", got)
	}
}

// The generated file passes prettier --check with Prettier's defaults, so a
// repo that checks formatting accepts it as written. The golden file was
// checked with: npx prettier@3.6.2 --check internal/route/testdata/spareci-watchdog.yml
// Regenerate it with: go run ./cmd/spareci watchdog > internal/route/testdata/spareci-watchdog.yml
// and check it with Prettier again.
func TestWatchdogGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/spareci-watchdog.yml")
	if err != nil {
		t.Fatal(err)
	}
	if got := Watchdog(allGroups()); got != string(want) {
		t.Errorf("watchdog workflow differs from testdata/spareci-watchdog.yml; regenerate it and check it with Prettier")
	}
}

func TestRescueRetriesAReRunRefusedAsAlreadyRunning(t *testing.T) {
	f := newFake()
	f.jobs = []gh.Job{{Name: "test", Status: "queued", Labels: []string{"spareci"}}}
	f.lag = 2 // GitHub's jobs lag the run's completed status
	r := router(f)
	if err := r.Rescue(context.Background(), 7, false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls, ","); got != "cancel,rerun-failed refused,rerun-failed refused,rerun-failed" {
		t.Errorf("calls %s", got)
	}
}

func TestRescueGivesUpOnAReRunThatStaysRefused(t *testing.T) {
	f := newFake()
	f.jobs = []gh.Job{{Name: "test", Status: "queued", Labels: []string{"spareci"}}}
	f.lag = 1 << 30
	r := router(f)
	r.RerunWait = 20 * time.Millisecond
	err := r.Rescue(context.Background(), 7, false)
	if !AlreadyRunning(err) {
		t.Fatalf("err %v, want already running", err)
	}
}
