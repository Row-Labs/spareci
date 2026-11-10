package estimate

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Text writes the report for a terminal: short enough to screenshot.
func (r Report) Text(w io.Writer) {
	scope := strings.Join(r.Repos, ", ")
	if len(r.Repos) > 3 {
		scope = fmt.Sprintf("%d repos", len(r.Repos))
	}
	jobs := r.SelfHosted.Jobs + r.Unpriced.Jobs
	for _, l := range r.Hosted {
		jobs += l.Jobs
	}
	fmt.Fprintf(w, "%s · %s – %s (%s days) · %s runs, %s jobs\n\n", scope,
		r.From.Format("Jan 2"), r.To.Format("Jan 2"), strconv.FormatFloat(r.Days, 'f', -1, 64),
		fmtInt(r.Runs), fmtInt(jobs))

	row := func(cols ...any) {
		fmt.Fprintln(w, strings.TrimRight(fmt.Sprintf("  %-22s %6s %9s %14s  %s", cols...), " "))
	}
	row("RUNNER", "JOBS", "MINUTES", "COST", "THIS MACHINE")
	for _, l := range r.Hosted {
		cost := money(l.Cost)
		if l.FreeMinutes == l.Minutes {
			cost = "free (public)"
		}
		fits := "no"
		if l.Fits {
			fits = "yes"
		}
		row(l.Name, fmtInt(l.Jobs), fmtInt(l.Minutes), cost, fits)
	}
	if r.Unpriced.Jobs > 0 {
		row(r.Unpriced.Name, fmtInt(r.Unpriced.Jobs), fmtInt(r.Unpriced.Minutes), "?", "?")
	}
	if r.SelfHosted.Jobs > 0 {
		row(r.SelfHosted.Name, fmtInt(r.SelfHosted.Jobs), fmtInt(r.SelfHosted.Minutes), money(r.SelfHosted.Cost), "already")
	}
	row("Total, GitHub-hosted", "", fmtInt(r.Minutes), money(r.Cost), "")
	fmt.Fprintln(w)
	if len(r.Hosted) == 0 {
		fmt.Fprintln(w, "No jobs ran on GitHub-hosted runners in this window.")
	} else {
		fmt.Fprintf(w, "≈ %s/month on GitHub-hosted runners.\n", money(r.MonthlyCost))
		fmt.Fprintf(w, "This machine (%s %s) could take %s of those minutes: ≈ %s/month saved.\n",
			r.Host.OS, r.Host.Arch, fmtInt(r.FitMinutes), money(r.MonthlySaving))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "GitHub list prices, checked %s: %s\n", r.PricesChecked, r.PricesSource)
	for _, c := range r.Caveats {
		fmt.Fprintf(w, "- %s\n", c)
	}
}

func money(v float64) string {
	if v >= 100 {
		return "$" + fmtInt(int(v+0.5))
	}
	return fmt.Sprintf("$%.2f", v)
}

func fmtInt(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + fmtInt(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
