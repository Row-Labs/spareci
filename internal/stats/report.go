package stats

import (
	"cmp"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/Row-Labs/spareci/internal/estimate"
)

// Totals is what a set of jobs would have cost on GitHub-hosted runners.
type Totals struct {
	Jobs    int     `json:"jobs"`
	Minutes int     `json:"minutes"` // billable: rounded up per job, as GitHub bills
	Saved   float64 `json:"saved"`   // USD at GitHub's list prices
}

func (t *Totals) add(p Priced) {
	t.Jobs++
	t.Minutes += p.Minutes
	t.Saved += p.Saved
}

// Period is the totals since a time.
type Period struct {
	Name string    `json:"name"`
	From time.Time `json:"from,omitzero"` // zero for all time
	Totals
}

// Line is one repo or one runner type.
type Line struct {
	Name string `json:"name"`
	SKU  string `json:"sku,omitempty"`
	Totals
	FreeMinutes int `json:"free_minutes,omitempty"` // of Minutes, free anyway because the repo is public
}

// Unpriced is a job whose GitHub-hosted runner isn't known, so it isn't in
// any total.
type Unpriced struct {
	Repo    string    `json:"repo"`
	Job     string    `json:"job"`
	URL     string    `json:"url,omitempty"`
	Ended   time.Time `json:"ended"`
	Minutes int       `json:"minutes"`
	Reason  string    `json:"reason"`
}

// Report is spareci stats.
type Report struct {
	Now      time.Time  `json:"now"`
	Since    time.Time  `json:"since,omitzero"` // the first recorded job
	Week     Period     `json:"this_week"`
	Month    Period     `json:"this_month"`
	AllTime  Period     `json:"all_time"`
	ByRepo   []Line     `json:"by_repo"`
	ByRunner []Line     `json:"by_runner"`
	Unpriced []Unpriced `json:"unpriced"`
	// Skipped is lines of the stats file that didn't parse.
	Skipped int `json:"skipped_lines,omitempty"`

	PricesChecked string `json:"prices_checked"`
	PricesSource  string `json:"prices_source"`
}

// Priced is one record at GitHub's list prices.
type Priced struct {
	Kind    estimate.Kind
	Runner  estimate.Runner
	Minutes int
	Saved   float64
	Free    bool   // a standard runner in a public repo: GitHub doesn't bill it
	Reason  string // why it's unpriced
}

// Price prices a record the way spareci estimate prices a job: billable
// minutes at the list price of the runner it would have run on.
func Price(r Record) Priced {
	p := Priced{Minutes: estimate.Minutes(r.Started, r.Ended)}
	if r.Fallback == nil {
		p.Kind, p.Reason = estimate.Unpriced, cmp.Or(r.FallbackError, "fallback runner unknown")
		return p
	}
	p.Kind, p.Runner = estimate.ForLabels(r.Fallback)
	switch p.Kind {
	case estimate.Unpriced:
		p.Reason = fmt.Sprintf("falls back to %s, which isn't a GitHub-hosted runner spareci can price", strings.Join(r.Fallback, ", "))
	case estimate.SelfHosted:
		p.Runner.SKU = "self-hosted"
	case estimate.Hosted:
		cost := float64(p.Minutes) * p.Runner.PerMinute
		if r.Public && !p.Runner.Larger {
			cost, p.Free = 0, true
		}
		p.Saved = cost - float64(p.Minutes)*estimate.SelfHostedPerMinute
	}
	return p
}

// Build adds up records. Weeks start on Monday and months on the 1st, in
// now's time zone.
func Build(recs []Record, skipped int, now time.Time) Report {
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	rep := Report{
		Now:           now,
		Week:          Period{Name: "This week", From: today.AddDate(0, 0, -(int(today.Weekday())+6)%7)},
		Month:         Period{Name: "This month", From: time.Date(y, m, 1, 0, 0, 0, 0, now.Location())},
		AllTime:       Period{Name: "All time"},
		Skipped:       skipped,
		Unpriced:      []Unpriced{},
		PricesChecked: estimate.PricesChecked,
		PricesSource:  estimate.PricesSource,
	}
	repos, runners := map[string]*Line{}, map[string]*Line{}
	for _, r := range recs {
		if rep.Since.IsZero() || r.Ended.Before(rep.Since) {
			rep.Since = r.Ended
		}
		p := Price(r)
		if p.Kind == estimate.Unpriced {
			rep.Unpriced = append(rep.Unpriced, Unpriced{Repo: r.Repo, Job: r.Job, URL: r.URL, Ended: r.Ended, Minutes: p.Minutes, Reason: p.Reason})
			continue
		}
		rep.AllTime.add(p)
		if !r.Ended.Before(rep.Month.From) {
			rep.Month.add(p)
		}
		if !r.Ended.Before(rep.Week.From) {
			rep.Week.add(p)
		}
		line(repos, r.Repo, r.Repo, "", p)
		line(runners, p.Runner.SKU, p.Runner.Name, p.Runner.SKU, p)
	}
	rep.ByRepo, rep.ByRunner = sorted(repos), sorted(runners)
	for _, per := range []*Period{&rep.Week, &rep.Month, &rep.AllTime} {
		per.Saved = cents4(per.Saved)
	}
	return rep
}

func line(m map[string]*Line, key, name, sku string, p Priced) {
	l := m[key]
	if l == nil {
		l = &Line{Name: name, SKU: sku}
		m[key] = l
	}
	l.add(p)
	if p.Free {
		l.FreeMinutes += p.Minutes
	}
}

func sorted(m map[string]*Line) []Line {
	out := make([]Line, 0, len(m))
	for _, l := range m {
		l.Saved = cents4(l.Saved)
		out = append(out, *l)
	}
	slices.SortFunc(out, func(a, b Line) int {
		return cmp.Or(cmp.Compare(b.Saved, a.Saved), cmp.Compare(b.Minutes, a.Minutes), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// cents4 drops float noise from dollar sums, as the estimate does.
func cents4(v float64) float64 { return math.Round(v*1e4) / 1e4 }

// Text writes the report for a terminal: short enough to screenshot.
func (r Report) Text(w io.Writer) {
	if r.AllTime.Jobs == 0 && len(r.Unpriced) == 0 {
		fmt.Fprintln(w, "No jobs recorded on this machine yet. They're counted from the first job `spareci up` runs.")
		return
	}
	fmt.Fprintf(w, "spareci saved you %s · %s jobs · %s min since %s\n\n",
		money(r.AllTime.Saved), estimate.FmtInt(r.AllTime.Jobs), estimate.FmtInt(r.AllTime.Minutes), r.Since.In(r.Now.Location()).Format("Jan 2, 2006"))

	width := 20
	for _, l := range append(append([]Line{}, r.ByRepo...), r.ByRunner...) {
		width = max(width, len([]rune(l.Name)))
	}
	row := func(cols ...string) {
		fmt.Fprintln(w, strings.TrimRight(fmt.Sprintf("  %-*s %6s %9s %10s", width, cols[0], cols[1], cols[2], cols[3]), " "))
	}
	totals := func(name string, t Totals, free bool) {
		saved := money(t.Saved)
		if free {
			saved = "free (public)"
		}
		row(name, estimate.FmtInt(t.Jobs), estimate.FmtInt(t.Minutes), saved)
	}
	row("", "JOBS", "MINUTES", "SAVED")
	for _, p := range []Period{r.Week, r.Month, r.AllTime} {
		totals(p.Name, p.Totals, false)
	}
	fmt.Fprintln(w)
	row("BY REPO", "", "", "")
	for _, l := range r.ByRepo {
		totals(l.Name, l.Totals, l.FreeMinutes == l.Minutes && l.Saved == 0 && l.Minutes > 0)
	}
	fmt.Fprintln(w)
	row("INSTEAD OF", "", "", "")
	for _, l := range r.ByRunner {
		totals(l.Name, l.Totals, l.FreeMinutes == l.Minutes && l.Minutes > 0)
	}
	fmt.Fprintln(w)
	if n := len(r.Unpriced); n > 0 {
		mins := 0
		for _, u := range r.Unpriced {
			mins += u.Minutes
		}
		fmt.Fprintf(w, "Not counted: %s, %s min, whose GitHub-hosted runner isn't known (--json says why).\n", plural(n, "job"), estimate.FmtInt(mins))
	}
	if r.Skipped > 0 {
		fmt.Fprintf(w, "Skipped %s of the stats file that didn't parse.\n", plural(r.Skipped, "line"))
	}
	fmt.Fprintf(w, "GitHub list prices, checked %s, as in spareci estimate. Your plan's included minutes may have covered some of this.\n", r.PricesChecked)
}

// Summary is the line logged when a job is recorded.
func Summary(rec Record, all Report) string {
	p := Price(rec)
	var this string
	switch {
	case p.Kind == estimate.Unpriced:
		this = fmt.Sprintf("job recorded, not priced (%s)", p.Reason)
	case p.Free:
		this = fmt.Sprintf("job recorded: %d min on %s, free for a public repo", p.Minutes, p.Runner.Name)
	default:
		this = fmt.Sprintf("saved %s: %d min on %s", money(p.Saved), p.Minutes, p.Runner.Name)
	}
	return fmt.Sprintf("%s; %s in all over %s", this, money(all.AllTime.Saved), plural(all.AllTime.Jobs, "job"))
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return estimate.FmtInt(n) + " " + noun + "s"
}

// money is estimate's dollar format, with a tenth of a cent under $1: a
// 1-minute Linux job saves $0.006, and three of them should add up to
// $0.018, not $0.01 each and $0.02 in all.
func money(v float64) string {
	if v > 0 && v < 1 {
		return fmt.Sprintf("$%.3f", v)
	}
	return estimate.Money(v)
}
