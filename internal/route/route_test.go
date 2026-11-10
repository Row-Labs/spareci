package route

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Row-Labs/spareci/internal/gh"
)

// fake is an in-memory repo: variables plus one run.
type fake struct {
	mu      sync.Mutex
	vars    map[string]string
	run     gh.Run
	jobs    []gh.Job
	calls   []string
	stuck   bool   // ignore normal cancel, like a run held by a dead runner
	skip    string // a job a failed-jobs re-run leaves out, as GitHub once did
	routing string // value of SPARECI_RUNS_ON when the re-run was created
}

func (f *fake) cancelJobs() {
	for i := range f.jobs {
		if f.jobs[i].Status != "completed" {
			f.jobs[i].Status, f.jobs[i].Conclusion = "completed", "cancelled"
		}
	}
}

func newFake() *fake {
	return &fake{vars: map[string]string{}, run: gh.Run{ID: 7, Status: "queued", RunAttempt: 1}}
}

func (f *fake) log(s string) { f.calls = append(f.calls, s) }

func (f *fake) SetVariable(_ context.Context, _, name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vars[name] = value
	f.log("set " + name)
	return nil
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
	if f.run.Status != "completed" {
		return errors.New("run not completed")
	}
	f.log(kind)
	f.routing = f.vars["SPARECI_RUNS_ON"]
	f.run.RunAttempt++
	f.run.Status = "queued"
	for i, j := range f.jobs {
		again := kind == "rerun-all" || (j.Conclusion != "success" && j.Name != f.skip)
		if again {
			f.jobs[i] = gh.Job{Name: j.Name, Status: "queued", RunAttempt: f.run.RunAttempt, Labels: []string{"ubuntu-24.04"}}
		}
	}
	return nil
}

func (f *fake) RerunFailedJobs(context.Context, string, int64) error { return f.rerun("rerun-failed") }
func (f *fake) RerunRun(context.Context, string, int64) error        { return f.rerun("rerun-all") }

func router(f *fake) *Router {
	return &Router{API: f, Repo: "o/r", Var: "SPARECI_RUNS_ON", Value: "spareci", HeartbeatVar: "SPARECI_HEARTBEAT", Poll: time.Millisecond}
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
		if j.RunAttempt != 3 {
			t.Errorf("job %s left at attempt %d", j.Name, j.RunAttempt)
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

func TestWatchdogWorkflow(t *testing.T) {
	w := Watchdog("SPARECI_RUNS_ON", "SPARECI_HEARTBEAT")
	for _, m := range Slots() {
		if !strings.Contains(w, fmt.Sprintf("- cron: '%s'", Cron(m))) {
			t.Errorf("missing cron for minute %d", m)
		}
	}
	for _, want := range []string{
		"if: vars.SPARECI_RUNS_ON && !contains(vars.SPARECI_HEARTBEAT, format('|{0}|', github.event.schedule))",
		"secrets.SPARECI_TOKEN",
		"actions/variables/SPARECI_RUNS_ON",
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
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
