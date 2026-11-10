package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/Row-Labs/spareci/internal/estimate"
	"github.com/Row-Labs/spareci/internal/gh"
)

type repoList []string

func (r *repoList) String() string     { return strings.Join(*r, ",") }
func (r *repoList) Set(v string) error { *r = append(*r, v); return nil }

// estimateCmd reads a repo's recent Actions history and prices it. It changes
// nothing on GitHub or on this machine.
func estimateCmd(args []string) error {
	fs := flag.NewFlagSet("estimate", flag.ExitOnError)
	var repos repoList
	fs.Var(&repos, "repo", "owner/name; repeat for more (default: the repo in the current directory)")
	org := fs.String("org", "", "every repo in this org the token can see")
	days := fs.Int("days", 30, "how many days of history to read")
	asJSON := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)
	if *days < 1 {
		return errors.New("--days must be at least 1")
	}

	client, err := gh.New()
	if err != nil {
		return err
	}
	ctx := context.Background()

	var targets []gh.Repo
	if *org != "" {
		all, err := client.OrgRepos(ctx, *org)
		if err != nil {
			return fmt.Errorf("list %s repos: %w", *org, err)
		}
		for _, r := range all {
			if !r.Archived {
				targets = append(targets, r)
			}
		}
	}
	if len(repos) == 0 && *org == "" {
		out, err := exec.Command("gh", "repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner").Output()
		if err != nil {
			return errors.New("no --repo or --org given and the current directory is not a GitHub repo")
		}
		repos = append(repos, strings.TrimSpace(string(out)))
	}
	for _, name := range repos {
		r, err := client.Repo(ctx, name)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		targets = append(targets, r)
	}

	to := time.Now().UTC().Truncate(time.Minute)
	from := to.AddDate(0, 0, -*days)

	progress := func(done, total int) {}
	if !*asJSON && isTerminal(os.Stderr) {
		progress = func(done, total int) {
			fmt.Fprintf(os.Stderr, "\rreading jobs: %d/%d runs", done, total)
			if done == total {
				fmt.Fprint(os.Stderr, "\r\033[K")
			}
		}
	}
	// Fetching jobs takes one request per run; don't run the token's hourly
	// budget, which `spareci up` and `gh` share, dry.
	budget := func(runs int) error {
		left, reset, err := client.CoreRemaining(ctx)
		if err == nil && runs > left-100 {
			return fmt.Errorf("%d runs in the window but only %d API calls left until %s; try a smaller --days",
				runs, left, reset.Local().Format("15:04"))
		}
		return nil
	}
	jobs, runs, err := estimate.Collect(ctx, client, targets, from, to, budget, progress)
	if err != nil {
		return err
	}

	names := make([]string, len(targets))
	for i, r := range targets {
		names[i] = r.FullName
	}
	host := estimate.Host{
		OS:   map[string]string{"darwin": "macOS", "linux": "Linux", "windows": "Windows"}[runtime.GOOS],
		Arch: map[string]string{"arm64": "ARM64", "amd64": "X64"}[runtime.GOARCH],
	}
	rep := estimate.Build(jobs, host, names, runs, from, to)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	rep.Text(os.Stdout)
	return nil
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
