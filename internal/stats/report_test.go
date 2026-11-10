package stats

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Row-Labs/spareci/internal/estimate"
	"github.com/Row-Labs/spareci/internal/gh"
)

var update = flag.Bool("update", false, "rewrite testdata/stats.txt")

// now is a Tuesday, so this week started Monday Nov 9.
var now = time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)

func rec(repo, job string, ended time.Time, d time.Duration, fallback ...string) Record {
	return Record{Repo: repo, Job: job, Runner: "spareci-test-1", Started: ended.Add(-d), Ended: ended, Conclusion: "success", Fallback: fallback}
}

func fixtureRecords() []Record {
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 12, 0, 0, 0, time.UTC) }
	pub := rec("acme/site", "build", day(11, 10), 3*time.Minute, "ubuntu-24.04")
	pub.Public = true
	unknown := rec("acme/app", "matrix (macos-15)", day(11, 10), 2*time.Minute)
	unknown.FallbackError = "ci:matrix: fallback matrix.os is an expression, not a label"
	return []Record{
		rec("acme/app", "test", day(10, 20), 6*time.Minute+10*time.Second, "macos-15"), // 7 min, last month
		rec("acme/app", "test", day(11, 3), 5*time.Minute, "macos-15"),                 // this month, last week
		rec("acme/app", "build", day(11, 9), 30*time.Second, "ubuntu-24.04"),           // this week: 1 min
		rec("acme/app", "big", day(11, 10), 10*time.Minute, "macos-15-xlarge"),         // larger runner
		rec("acme/api", "lint", day(11, 10), 2*time.Minute, "self-hosted", "Linux"),    // already free
		rec("acme/api", "odd", day(11, 10), 4*time.Minute, "my-custom-runner"),         // can't be priced
		pub,     // free: standard runner, public repo
		unknown, // no fallback
	}
}

func TestBuild(t *testing.T) {
	r := Build(fixtureRecords(), 1, now)

	if !r.Week.From.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)) || !r.Month.From.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("week from %v, month from %v", r.Week.From, r.Month.From)
	}
	// macOS standard $0.062, Linux 2-core $0.006, macOS 5-core M2 Pro $0.102.
	want := map[string]Totals{
		"all":   {Jobs: 6, Minutes: 7 + 5 + 1 + 10 + 2 + 3, Saved: 7*0.062 + 5*0.062 + 1*0.006 + 10*0.102},
		"month": {Jobs: 5, Minutes: 5 + 1 + 10 + 2 + 3, Saved: 5*0.062 + 0.006 + 1.02},
		"week":  {Jobs: 4, Minutes: 1 + 10 + 2 + 3, Saved: 0.006 + 1.02},
	}
	for name, got := range map[string]Totals{"all": r.AllTime.Totals, "month": r.Month.Totals, "week": r.Week.Totals} {
		w := want[name]
		w.Saved = cents4(w.Saved)
		if got != w {
			t.Errorf("%s = %+v, want %+v", name, got, w)
		}
	}
	if len(r.Unpriced) != 2 || r.Unpriced[0].Job != "odd" || !strings.Contains(r.Unpriced[1].Reason, "matrix.os") {
		t.Errorf("unpriced = %+v", r.Unpriced)
	}
	if r.ByRunner[0].Name != "macOS 5-core M2 Pro" || r.ByRunner[1].Name != "macOS standard" || r.ByRunner[1].Minutes != 12 {
		t.Errorf("by runner = %+v", r.ByRunner)
	}
	for _, l := range r.ByRunner {
		if l.SKU == "actions_linux" && (l.Minutes != 4 || l.FreeMinutes != 3) {
			t.Errorf("Linux line = %+v, want 4 min of which 3 free", l)
		}
	}
	if !r.Since.Equal(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)) || r.Skipped != 1 {
		t.Errorf("since %v, skipped %d", r.Since, r.Skipped)
	}
}

// stats and estimate must agree: the same job costs the same in both.
func TestPriceMatchesEstimate(t *testing.T) {
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		label string
		d     time.Duration
	}{{"macos-15", 61 * time.Second}, {"ubuntu-24.04", 4*time.Minute + 1*time.Second}, {"ubuntu-8-core", 90 * time.Second}, {"windows-11-arm", 0}} {
		end := start.Add(c.d)
		group := int64(0)
		job := estimate.Job{RunJob: gh.RunJob{Status: "completed", StartedAt: start, CompletedAt: &end, Labels: []string{c.label}, RunnerName: "GitHub Actions 1", RunnerGroupID: &group}, Repo: "acme/app"}
		est := estimate.Build([]estimate.Job{job}, estimate.Host{OS: "macOS", Arch: "ARM64"}, []string{"acme/app"}, 1, start, end.Add(time.Hour))
		p := Price(Record{Started: start, Ended: end, Fallback: []string{c.label}})
		if len(est.Hosted) != 1 || est.Hosted[0].Minutes != p.Minutes || est.Hosted[0].Cost != cents4(p.Saved) || est.Hosted[0].SKU != p.Runner.SKU {
			t.Errorf("%s for %v: estimate %+v, stats %+v", c.label, c.d, est.Hosted, p)
		}
	}
}

func TestText(t *testing.T) {
	var buf bytes.Buffer
	Build(fixtureRecords(), 0, now).Text(&buf)
	golden := filepath.Join("testdata", "stats.txt")
	if *update {
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if buf.String() != string(want) {
		t.Errorf("text report changed (go test -run TestText -update to accept):\n%s", buf.String())
	}

	buf.Reset()
	Build(nil, 0, now).Text(&buf)
	if !strings.Contains(buf.String(), "No jobs recorded") {
		t.Errorf("empty report: %q", buf.String())
	}
}

func TestJSON(t *testing.T) {
	b, err := json.Marshal(Build(fixtureRecords(), 0, now))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"this_week", "this_month", "all_time", "by_repo", "by_runner", "unpriced", "prices_checked", "since"} {
		if _, ok := m[k]; !ok {
			t.Errorf("JSON has no %q: %s", k, b)
		}
	}
	// No jobs is an empty list, not null, for scripts.
	b, _ = json.Marshal(Build(nil, 0, now))
	if !strings.Contains(string(b), `"unpriced":[]`) || !strings.Contains(string(b), `"by_repo":[]`) {
		t.Errorf("empty JSON: %s", b)
	}
}

func TestSummary(t *testing.T) {
	recs := fixtureRecords()
	all := Build(recs, 0, now)
	got := Summary(recs[0], all)
	if got != "saved $0.434: 7 min on macOS standard; $1.77 in all over 6 jobs" {
		t.Errorf("Summary = %q", got)
	}
	if got := Summary(recs[7], all); !strings.HasPrefix(got, "job recorded, not priced (ci:matrix:") {
		t.Errorf("Summary of an unknown fallback = %q", got)
	}
}

func TestAppendLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats", "jobs.jsonl")
	if recs, bad, err := Load(path); err != nil || recs != nil || bad != 0 {
		t.Fatalf("Load of a missing file = %v, %d, %v", recs, bad, err)
	}
	for _, r := range fixtureRecords()[:2] {
		if err := Append(path, r); err != nil {
			t.Fatal(err)
		}
	}
	// A line cut short by a crash.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"repo":"acme/app","job":"te` + "\n")
	f.Close()
	if err := Append(path, fixtureRecords()[2]); err != nil {
		t.Fatal(err)
	}
	recs, bad, err := Load(path)
	if err != nil || len(recs) != 3 || bad != 1 || recs[2].Job != "build" || !recs[0].Ended.Equal(fixtureRecords()[0].Ended) {
		t.Errorf("Load = %+v, %d, %v", recs, bad, err)
	}
}
