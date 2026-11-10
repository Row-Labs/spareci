package estimate

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Row-Labs/spareci/internal/gh"
)

var update = flag.Bool("update", false, "rewrite testdata/report.txt")

var (
	from  = time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	to    = from.AddDate(0, 0, 30)
	mac   = Host{OS: "macOS", Arch: "ARM64"}
	repos = []gh.Repo{{FullName: "acme/app", Private: true}, {FullName: "acme/site"}}
)

var runsPath = regexp.MustCompile(`^/repos/acme/([a-z]+)/actions/runs$`)
var jobsPath = regexp.MustCompile(`^/repos/acme/[a-z]+/actions/runs/(\d+)/jobs$`)

// fixtureAPI serves testdata as the GitHub REST API. fail lets a test inject
// errors per request.
func fixtureAPI(t *testing.T, fail func(r *http.Request) int) *gh.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail != nil {
			if code := fail(r); code != 0 {
				http.Error(w, "boom", code)
				return
			}
		}
		var file string
		if m := runsPath.FindStringSubmatch(r.URL.Path); m != nil {
			file = "runs-acme-" + m[1] + ".json"
		} else if m := jobsPath.FindStringSubmatch(r.URL.Path); m != nil {
			file = "jobs-" + m[1] + ".json"
		}
		if r.URL.Query().Get("page") != "1" {
			fmt.Fprint(w, `{"total_count":0,"workflow_runs":[],"jobs":[]}`)
			return
		}
		data, err := os.ReadFile(filepath.Join("testdata", file))
		if file == "" || err != nil {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return gh.NewWith("test", srv.URL, srv.Client())
}

func fixtureReport(t *testing.T) Report {
	t.Helper()
	jobs, runs, err := Collect(context.Background(), fixtureAPI(t, nil), repos, from, to, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return Build(jobs, mac, []string{"acme/app", "acme/site"}, runs, from, to)
}

func TestBuild(t *testing.T) {
	r := fixtureReport(t)

	type want struct {
		jobs, minutes, free int
		cost                float64
		fits                bool
	}
	wants := map[string]want{
		"actions_macos":     {2, 7, 0, 7 * 0.062, true},   // 3m10s -> 4, rerun 3m00s -> 3
		"macos_xl":          {1, 10, 0, 10 * 0.102, true}, // exactly 10m -> 10
		"actions_linux":     {2, 8, 7, 1 * 0.006, false},  // private 59s -> 1; public 6m01s -> 7, free
		"actions_windows":   {1, 3, 0, 3 * 0.010, false},  // 2m01s -> 3
		"linux_8_core":      {1, 5, 0, 5 * 0.022, false},  // size from the label
		"linux_4_core":      {1, 2, 0, 2 * 0.012, false},  // larger: billed in a public repo
		"actions_linux_arm": {1, 1, 0, 1 * 0.005, false},
	}
	if len(r.Hosted) != len(wants) {
		t.Errorf("got %d hosted lines, want %d: %+v", len(r.Hosted), len(wants), r.Hosted)
	}
	for _, l := range r.Hosted {
		w, ok := wants[l.SKU]
		if !ok {
			t.Errorf("unexpected line %s", l.SKU)
			continue
		}
		got := want{l.Jobs, l.Minutes, l.FreeMinutes, l.Cost, l.Fits}
		if got.jobs != w.jobs || got.minutes != w.minutes || got.free != w.free || got.fits != w.fits || !near(got.cost, w.cost) {
			t.Errorf("%s = %+v, want %+v", l.SKU, got, w)
		}
	}
	if r.Hosted[0].SKU != "macos_xl" {
		t.Errorf("most expensive line first, got %s", r.Hosted[0].SKU)
	}

	// Skipped, never-assigned and still-running jobs cost nothing.
	if r.SelfHosted.Jobs != 1 || r.SelfHosted.Minutes != 8 || r.SelfHosted.Cost != 0 {
		t.Errorf("self-hosted = %+v", r.SelfHosted)
	}
	if r.Unpriced.Jobs != 1 || r.Unpriced.Minutes != 5 {
		t.Errorf("unpriced = %+v", r.Unpriced)
	}
	if r.Runs != 3 || r.Minutes != 36 {
		t.Errorf("runs, minutes = %d, %d; want 3, 36", r.Runs, r.Minutes)
	}
	if !near(r.Cost, 1.629) || !near(r.FitCost, 1.454) || r.FitMinutes != 17 {
		t.Errorf("cost %.4f fit %.4f (%d min)", r.Cost, r.FitCost, r.FitMinutes)
	}
	if !near(r.MonthlyCost, r.Cost) || !near(r.MonthlySaving, r.FitCost) {
		t.Errorf("30-day window should not be rescaled: %+v", r)
	}
}

func TestMonthlyScales(t *testing.T) {
	jobs, runs, err := Collect(context.Background(), fixtureAPI(t, nil), repos, from, to, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := Build(jobs, mac, nil, runs, to.AddDate(0, 0, -7), to)
	if math.Abs(r.MonthlyCost-r.Cost*30/7) > 1e-4 {
		t.Errorf("monthly %.4f, want %.4f", r.MonthlyCost, r.Cost*30/7)
	}
}

func TestLinuxHost(t *testing.T) {
	jobs, runs, _ := Collect(context.Background(), fixtureAPI(t, nil), repos, from, to, nil, nil)
	r := Build(jobs, Host{OS: "Linux", Arch: "X64"}, nil, runs, from, to)
	// Private Linux 2-core (1 min) and both larger x64 Linux runners.
	if r.FitMinutes != 8+5+2 || !near(r.FitCost, 0.006+0.11+0.024) {
		t.Errorf("fit = %d min $%.4f", r.FitMinutes, r.FitCost)
	}
	if !strings.Contains(strings.Join(r.Caveats, "\n"), "1 min ran on Linux with another CPU architecture") {
		t.Errorf("caveats miss the arm64 job: %q", r.Caveats)
	}
}

func TestText(t *testing.T) {
	var buf bytes.Buffer
	fixtureReport(t).Text(&buf)
	golden := filepath.Join("testdata", "report.txt")
	if *update {
		os.WriteFile(golden, buf.Bytes(), 0o644)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if buf.String() != string(want) {
		t.Errorf("report changed (go test -update to accept):\n%s", buf.String())
	}
}

func TestJSON(t *testing.T) {
	b, err := json.Marshal(fixtureReport(t))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(b, &out)
	for _, k := range []string{"hosted", "monthly_cost", "monthly_saving", "prices_checked", "caveats", "fits_minutes"} {
		if _, ok := out[k]; !ok {
			t.Errorf("JSON has no %q", k)
		}
	}
}

func TestRetries5xx(t *testing.T) {
	retryDelay = time.Millisecond
	var calls atomic.Int32
	api := fixtureAPI(t, func(r *http.Request) int {
		if strings.HasSuffix(r.URL.Path, "/runs/102/jobs") && calls.Add(1) <= 2 {
			return http.StatusBadGateway
		}
		return 0
	})
	jobs, _, err := Collect(context.Background(), api, repos, from, to, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 14 {
		t.Errorf("got %d jobs, want 14", len(jobs))
	}

	api = fixtureAPI(t, func(r *http.Request) int {
		if strings.HasSuffix(r.URL.Path, "/runs/102/jobs") {
			return http.StatusNotFound
		}
		return 0
	})
	if _, _, err := Collect(context.Background(), api, repos, from, to, nil, nil); !gh.IsStatus(err, 404) {
		t.Errorf("4xx should fail without retrying, got %v", err)
	}
}

func TestBudget(t *testing.T) {
	_, _, err := Collect(context.Background(), fixtureAPI(t, nil), repos, from, to, func(runs int) error {
		if runs != 3 {
			t.Errorf("budget saw %d runs, want 3", runs)
		}
		return fmt.Errorf("no budget")
	}, nil)
	if err == nil || err.Error() != "no budget" {
		t.Errorf("err = %v", err)
	}
}

// GitHub returns at most 1,000 runs for a filtered listing, so a busy window
// has to be split.
func TestRunsSplitsBusyWindow(t *testing.T) {
	var id atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Query().Get("created"), "..")
		a, _ := time.Parse(time.RFC3339, parts[0])
		b, _ := time.Parse(time.RFC3339, parts[1])
		total := int(b.Sub(a).Hours()) + 1 // one run an hour: 721 in 30 days
		total *= 2                         // 1,442: over the cap
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		n := min(100, total-(page-1)*100)
		var runs []string
		for range max(n, 0) {
			runs = append(runs, fmt.Sprintf(`{"id":%d}`, id.Add(1)))
		}
		fmt.Fprintf(w, `{"total_count":%d,"workflow_runs":[%s]}`, total, strings.Join(runs, ","))
	}))
	defer srv.Close()
	runs, err := gh.NewWith("t", srv.URL, srv.Client()).Runs(context.Background(), "acme/app", from, to)
	if err != nil {
		t.Fatal(err)
	}
	// Each half ends a second before its end, so holds 360 hourly buckets:
	// 720 runs, under the cap.
	if len(runs) != 2*720 {
		t.Errorf("got %d runs, want %d", len(runs), 2*720)
	}
}

func TestJobsPaginate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		n := min(100, 150-(page-1)*100)
		var jobs []string
		for i := range max(n, 0) {
			jobs = append(jobs, fmt.Sprintf(`{"id":%d}`, (page-1)*100+i))
		}
		fmt.Fprintf(w, `{"total_count":150,"jobs":[%s]}`, strings.Join(jobs, ","))
	}))
	defer srv.Close()
	jobs, err := gh.NewWith("t", srv.URL, srv.Client()).RunJobs(context.Background(), "acme/app", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 150 {
		t.Errorf("got %d jobs, want 150", len(jobs))
	}
}

func TestBillableMinutes(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		d    time.Duration
		want int
	}{{0, 1}, {time.Second, 1}, {time.Minute, 1}, {time.Minute + time.Second, 2}, {59*time.Minute + 59*time.Second, 60}} {
		end := start.Add(c.d)
		j := gh.RunJob{Status: "completed", StartedAt: start, CompletedAt: &end, RunnerName: "GitHub Actions 1"}
		if got := BillableMinutes(j); got != c.want {
			t.Errorf("%v -> %d min, want %d", c.d, got, c.want)
		}
	}
}

func TestSKUs(t *testing.T) {
	for label, want := range map[string]string{
		"ubuntu-latest":    "actions_linux",
		"ubuntu-slim":      "actions_linux_slim",
		"ubuntu-22.04-arm": "actions_linux_arm",
		"windows-2025":     "actions_windows",
		"windows-11-arm":   "actions_windows_arm",
		"macos-latest":     "actions_macos",
		"macos-15-intel":   "actions_macos_intel",
		"macos-14-large":   "macos_l",
		"macos-26-xlarge":  "macos_xl",
	} {
		if got, _ := standardSKU(label); got != want {
			t.Errorf("standardSKU(%q) = %q, want %q", label, got, want)
		}
	}
	for label, want := range map[string]string{
		"ubuntu-16-core":    "linux_16_core",
		"linux-arm64-8core": "linux_8_core_arm",
		"windows-4-cores":   "windows_4_core",
		"win-arm-2-core":    "windows_2_core_arm",
		"gpu-linux-4-core":  "linux_4_core_gpu",
		"ubuntu-3-core":     "", // no such size
		"my-fast-runner":    "",
	} {
		if got, _ := largerSKU([]string{label}); got != want {
			t.Errorf("largerSKU(%q) = %q, want %q", label, got, want)
		}
	}
	for sku, p := range prices {
		if p.PerMinute <= 0 || p.OS == "" || p.Arch == "" || p.Name == "" {
			t.Errorf("%s incomplete: %+v", sku, p)
		}
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
