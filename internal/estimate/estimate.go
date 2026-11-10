package estimate

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Row-Labs/spareci/internal/gh"
)

// Source is the slice of the GitHub API an estimate reads. *gh.Client has it.
type Source interface {
	Runs(ctx context.Context, repo string, from, to time.Time) ([]gh.Run, error)
	RunJobs(ctx context.Context, repo string, runID int64) ([]gh.RunJob, error)
}

// Job is a finished job and the repo it ran in.
type Job struct {
	gh.RunJob
	Repo   string
	Public bool
}

// Collect fetches every job of every run created in [from, to), across repos.
// budget, if set, sees the run count before any jobs are fetched (one request
// per run) and can refuse. progress, if set, is called after each run.
func Collect(ctx context.Context, src Source, repos []gh.Repo, from, to time.Time, budget func(runs int) error, progress func(done, total int)) (jobs []Job, runs int, err error) {
	type item struct {
		repo gh.Repo
		run  gh.Run
	}
	var items []item
	for _, r := range repos {
		rs, err := src.Runs(ctx, r.FullName, from, to)
		if err != nil {
			return nil, 0, err
		}
		for _, run := range rs {
			items = append(items, item{r, run})
		}
	}

	if budget != nil {
		if err := budget(len(items)); err != nil {
			return nil, 0, err
		}
	}

	// A few requests at a time: GitHub asks for serial requests to avoid its
	// secondary rate limit, and a handful in flight stays well inside it.
	const workers = 4
	var (
		mu       sync.Mutex
		done     int
		firstErr error
		wg       sync.WaitGroup
		next     = make(chan item)
	)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range next {
				js, err := runJobs(ctx, src, it.repo.FullName, it.run.ID)
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						cancel()
					}
				} else {
					for _, j := range js {
						jobs = append(jobs, Job{RunJob: j, Repo: it.repo.FullName, Public: !it.repo.Private})
					}
				}
				done++
				if progress != nil {
					progress(done, len(items))
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, it := range items {
		select {
		case next <- it:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	if firstErr != nil {
		return nil, 0, firstErr
	}
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].ID < jobs[b].ID })
	return jobs, len(items), nil
}

// runJobs retries GitHub's occasional 5xx, so one bad gateway doesn't sink
// a scan of hundreds of runs.
func runJobs(ctx context.Context, src Source, repo string, id int64) ([]gh.RunJob, error) {
	var err error
	for attempt := range 3 {
		var js []gh.RunJob
		if js, err = src.RunJobs(ctx, repo, id); err == nil {
			return js, nil
		}
		var ae *gh.APIError
		if !errors.As(err, &ae) || ae.Status < 500 {
			return nil, err
		}
		select {
		case <-time.After(time.Duration(attempt+1) * retryDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, err
}

var retryDelay = time.Second

// Host is the machine that would take the jobs.
type Host struct {
	OS   string `json:"os"`   // macOS, Linux
	Arch string `json:"arch"` // ARM64, X64
}

// Line is the usage of one runner type.
type Line struct {
	Runner
	Jobs        int     `json:"jobs"`
	Minutes     int     `json:"minutes"`      // billable minutes, rounded up per job
	FreeMinutes int     `json:"free_minutes"` // of Minutes, those free because the repo is public
	Cost        float64 `json:"cost"`
	Fits        bool    `json:"fits_this_machine"`
}

// Report is the result of an estimate.
type Report struct {
	Repos []string  `json:"repos"`
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
	Days  float64   `json:"days"`
	Host  Host      `json:"host"`
	Runs  int       `json:"runs"`

	Hosted     []Line  `json:"hosted"`       // GitHub-hosted, most expensive first
	SelfHosted Line    `json:"self_hosted"`  // already off GitHub's meter
	Unpriced   Line    `json:"unpriced"`     // larger runners whose size the API doesn't say
	Minutes    int     `json:"minutes"`      // hosted minutes
	Cost       float64 `json:"cost"`         // hosted cost in the window
	FitMinutes int     `json:"fits_minutes"` // hosted minutes this machine could take
	FitCost    float64 `json:"fits_cost"`

	MonthlyCost   float64 `json:"monthly_cost"`
	MonthlySaving float64 `json:"monthly_saving"`

	PricesChecked string   `json:"prices_checked"`
	PricesSource  string   `json:"prices_source"`
	Caveats       []string `json:"caveats"`
}

// BillableMinutes is a finished job's duration rounded up to the whole minute,
// as GitHub bills it. Jobs that never got a runner (skipped, cancelled while
// queued) cost nothing.
func BillableMinutes(j gh.RunJob) int {
	if j.Status != "completed" || j.CompletedAt == nil || j.StartedAt.IsZero() || j.RunnerName == "" {
		return 0
	}
	d := j.CompletedAt.Sub(j.StartedAt)
	if d <= 0 {
		return 1
	}
	return int(math.Ceil(d.Minutes()))
}

type kind int

const (
	hosted kind = iota
	selfHosted
	unpriced
)

// classify decides where a job ran. A self-hosted runner always carries the
// self-hosted label, but a job can ask for it by a custom label alone, so the
// runner decides: GitHub-hosted runners are named "GitHub Actions N" and
// standard ones sit in runner group 0. The API sometimes reports a null group
// for standard runners, so a GitHub image label (ubuntu-24.04, macos-15) on a
// hosted runner is enough.
func classify(j gh.RunJob) (kind, string) {
	for _, l := range j.Labels {
		if strings.EqualFold(l, "self-hosted") {
			return selfHosted, ""
		}
	}
	standardGroup := j.RunnerGroupID != nil && *j.RunnerGroupID == 0
	if !strings.HasPrefix(j.RunnerName, "GitHub Actions") && !standardGroup {
		return selfHosted, ""
	}
	// Sized names first: "ubuntu-8-core" is a larger runner, not ubuntu-*.
	if sku, ok := largerSKU(j.Labels); ok {
		return hosted, sku
	}
	for _, l := range j.Labels {
		if sku, ok := standardSKU(l); ok {
			return hosted, sku
		}
	}
	if standardGroup {
		return hosted, "actions_linux" // a standard image we don't know yet
	}
	return unpriced, ""
}

// Build prices jobs and works out what host could take.
func Build(jobs []Job, host Host, repos []string, runs int, from, to time.Time) Report {
	r := Report{
		Repos: repos, From: from, To: to, Host: host, Runs: runs,
		Days:          to.Sub(from).Hours() / 24,
		PricesChecked: PricesChecked, PricesSource: PricesSource,
		SelfHosted: Line{Runner: Runner{Name: "Self-hosted"}},
		Unpriced:   Line{Runner: Runner{Name: "Larger, size unknown"}},
	}
	lines := map[string]*Line{}
	for _, j := range jobs {
		mins := BillableMinutes(j.RunJob)
		if mins == 0 {
			continue
		}
		k, sku := classify(j.RunJob)
		switch k {
		case selfHosted:
			r.SelfHosted.Jobs++
			r.SelfHosted.Minutes += mins
			r.SelfHosted.Cost += float64(mins) * SelfHostedPerMinute
			continue
		case unpriced:
			r.Unpriced.Jobs++
			r.Unpriced.Minutes += mins
			continue
		}
		l := lines[sku]
		if l == nil {
			p, _ := Price(sku)
			l = &Line{Runner: p, Fits: p.OS == host.OS && p.Arch == host.Arch}
			lines[sku] = l
		}
		l.Jobs++
		l.Minutes += mins
		if j.Public && !l.Larger {
			l.FreeMinutes += mins
		} else {
			l.Cost += float64(mins) * l.PerMinute
		}
	}
	for _, l := range lines {
		r.Hosted = append(r.Hosted, *l)
		r.Minutes += l.Minutes
		r.Cost += l.Cost
		if l.Fits {
			r.FitMinutes += l.Minutes
			r.FitCost += l.Cost
		}
	}
	sort.Slice(r.Hosted, func(a, b int) bool {
		x, y := r.Hosted[a], r.Hosted[b]
		if x.Cost != y.Cost {
			return x.Cost > y.Cost
		}
		if x.Minutes != y.Minutes {
			return x.Minutes > y.Minutes
		}
		return x.SKU < y.SKU
	})
	for i := range r.Hosted {
		r.Hosted[i].Cost = cents4(r.Hosted[i].Cost)
	}
	r.Cost, r.FitCost = cents4(r.Cost), cents4(r.FitCost)
	if r.Days > 0 {
		scale := 30 / r.Days
		r.MonthlyCost = r.Cost * scale
		r.MonthlyCost = cents4(r.MonthlyCost)
		r.MonthlySaving = cents4((r.FitCost - float64(r.FitMinutes)*SelfHostedPerMinute) * scale)
	}
	r.Caveats = caveats(r)
	return r
}

// cents4 drops float noise (51.645999...) from dollar figures.
func cents4(v float64) float64 { return math.Round(v*1e4) / 1e4 }

func caveats(r Report) []string {
	c := []string{
		"List prices. Your plan's included usage ($12/mo Free, $18 Pro and Team, $300 Enterprise Cloud) comes off the bill first, so a small bill may already be $0.",
	}
	var otherArch, otherOS int
	for _, l := range r.Hosted {
		switch {
		case l.Fits:
		case l.OS == r.Host.OS:
			otherArch += l.Minutes
		default:
			otherOS += l.Minutes
		}
	}
	if otherArch > 0 {
		c = append(c, fmtInt(otherArch)+" min ran on "+r.Host.OS+" with another CPU architecture; they could move only if the jobs don't depend on it.")
	}
	if otherOS > 0 {
		c = append(c, fmtInt(otherOS)+" min need another OS (macOS jobs need a Mac; spareci doesn't run Windows jobs).")
	}
	if r.Unpriced.Minutes > 0 {
		c = append(c, fmtInt(r.Unpriced.Minutes)+" min ran on larger runners whose size their name doesn't give; they aren't priced.")
	}
	c = append(c, "One machine runs jobs one or a few at a time, so moving them can make CI slower when many run at once.")
	return c
}
