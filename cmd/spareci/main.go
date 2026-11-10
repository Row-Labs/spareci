// spareci turns the machine you're on into GitHub Actions runners for a repo:
// single-use, on demand, and out of the way when the machine is off.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Row-Labs/spareci/internal/cache"
	"github.com/Row-Labs/spareci/internal/gh"
	"github.com/Row-Labs/spareci/internal/pool"
	"github.com/Row-Labs/spareci/internal/runner"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

const usage = `spareci: run your GitHub Actions jobs on your own machine.

Usage:
  spareci up     [--repo owner/name] [--label spareci] [--max 1] [--dry-run]
                 [--cache=true] [--cache-size 10GB]
                 [--only-on-ac] [--idle 10m] [--hours 18:00-08:00]
  spareci status [--repo owner/name]
  spareci down   [--repo owner/name]   clear routing and stale runners after a crash
  spareci cache  ls|clear [--repo owner/name]
  spareci version

In your workflow, route jobs here when spareci is up, and to GitHub otherwise:

  runs-on: ${{ vars.SPARECI_RUNS_ON || 'macos-15' }}
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "up":
		err = up(os.Args[2:])
	case "status":
		err = status(os.Args[2:])
	case "down":
		err = down(os.Args[2:])
	case "cache":
		err = cacheCmd(os.Args[2:])
	case "version", "--version":
		fmt.Println("spareci", version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "spareci:", err)
		os.Exit(1)
	}
}

type common struct {
	repo  string
	label string
	vname string
}

func commonFlags(fs *flag.FlagSet) *common {
	c := &common{}
	fs.StringVar(&c.repo, "repo", "", "owner/name (default: the repo in the current directory)")
	fs.StringVar(&c.label, "label", "spareci", "label that routes a job to this machine")
	fs.StringVar(&c.vname, "fallback-var", "SPARECI_RUNS_ON", "repo variable set to --label while up (empty to disable)")
	return c
}

func (c *common) resolve() error {
	if c.repo != "" {
		return nil
	}
	out, err := exec.Command("gh", "repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner").Output()
	if err != nil {
		return fmt.Errorf("no --repo given and the current directory is not a GitHub repo")
	}
	c.repo = strings.TrimSpace(string(out))
	return nil
}

func up(args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	c := commonFlags(fs)
	maxN := fs.Int("max", 1, "concurrent jobs")
	interval := fs.Duration("interval", 5*time.Second, "poll interval")
	dry := fs.Bool("dry-run", false, "log the jobs we would take; register nothing")
	verbose := fs.Bool("v", false, "debug logging")
	version := fs.String("runner-version", "", "actions/runner version (default: latest)")
	useCache := fs.Bool("cache", true, "serve actions/cache from this machine's disk instead of GitHub")
	cacheSize := fs.String("cache-size", "10GB", "local cache size before least recently used entries are evicted")
	upstream := fs.String("cache-upstream", cache.DefaultUpstream, "GitHub results service, for calls the local cache passes through (artifacts)")
	laptop := laptopFlags(fs)
	_ = fs.Parse(args)
	if *verbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	if err := c.resolve(); err != nil {
		return err
	}
	g, err := laptop.gate(c.repo)
	if err != nil {
		return err
	}
	client, err := gh.New()
	if err != nil {
		return err
	}

	// First ^C: stop taking jobs, route back to GitHub, let running jobs end.
	// Second ^C: abort them.
	soft, softCancel := context.WithCancel(context.Background())
	hard, hardCancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		slog.Info("shutting down")
		softCancel()
		<-sigs
		slog.Info("aborting running jobs")
		hardCancel()
	}()

	host := &runner.Host{Root: runner.DefaultRoot(), Version: *version}
	if host.Version == "" {
		if host.Version, err = client.LatestRunnerVersion(soft); err != nil {
			return fmt.Errorf("find runner version: %w", err)
		}
	}
	if !*dry {
		if err := host.Install(soft); err != nil {
			return err
		}
		host.Prune()
	}

	var cacheSrv *cache.Server
	if *useCache && !*dry {
		if cacheSrv, err = startCache(host, *cacheSize, *upstream); err != nil {
			slog.Warn("local cache off; jobs will use GitHub's cache", "err", err)
		}
	}

	labels := append(runner.DefaultLabels(), c.label)
	slog.Info("spareci up", "repo", c.repo, "labels", labels, "max", *maxN, "runner", host.Version, "dir", host.Root, "local_cache", cacheSrv != nil, "dry_run", *dry)
	p := pool.New(pool.Config{
		Repo:        c.repo,
		Labels:      labels,
		Max:         *maxN,
		Interval:    *interval,
		FallbackVar: c.vname,
		RouteLabel:  c.label,
		DryRun:      *dry,
		Cache:       cacheSrv,
		Gate:        poolGate(g),
	}, client, host)
	if g != nil {
		slog.Info("laptop mode: taking jobs only when conditions are met", "when", g.Config().String())
		defer removeGateState(c.repo)
	}
	return p.Run(soft, hard)
}

func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	c := commonFlags(fs)
	_ = fs.Parse(args)
	if err := c.resolve(); err != nil {
		return err
	}
	client, err := gh.New()
	if err != nil {
		return err
	}
	ctx := context.Background()
	runners, err := client.ListRunners(ctx, c.repo)
	if err != nil {
		return err
	}
	fmt.Printf("repo: %s\n", c.repo)
	printGateState(c.repo)
	fmt.Printf("self-hosted runners: %d\n", len(runners))
	for _, r := range runners {
		fmt.Printf("  %-40s %-8s busy=%v\n", r.Name, r.Status, r.Busy)
	}
	jobs, err := client.QueuedJobs(ctx, c.repo)
	if err != nil {
		return err
	}
	fmt.Printf("queued jobs: %d\n", len(jobs))
	for _, j := range jobs {
		fmt.Printf("  %-40s runs-on=%v\n", j.Name, j.Labels)
	}
	return nil
}

func down(args []string) error {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	c := commonFlags(fs)
	_ = fs.Parse(args)
	if err := c.resolve(); err != nil {
		return err
	}
	client, err := gh.New()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if c.vname != "" {
		if err := client.DeleteVariable(ctx, c.repo, c.vname); err != nil {
			return err
		}
		fmt.Printf("cleared %s: jobs route to GitHub-hosted runners\n", c.vname)
	}
	runners, err := client.ListRunners(ctx, c.repo)
	if err != nil {
		return err
	}
	for _, r := range runners {
		if strings.HasPrefix(r.Name, "spareci-") && r.Status == "offline" {
			if err := client.DeleteRunner(ctx, c.repo, r.ID); err != nil {
				return err
			}
			fmt.Printf("removed stale runner %s\n", r.Name)
		}
	}
	return nil
}

func cacheDir() string { return filepath.Join(runner.DefaultRoot(), "actions-cache") }

func startCache(host *runner.Host, size, upstream string) (*cache.Server, error) {
	maxBytes, err := cache.ParseBytes(size)
	if err != nil {
		return nil, err
	}
	if err := host.PrepareCacheVariant(); err != nil {
		return nil, err
	}
	store, err := cache.Open(cacheDir(), maxBytes)
	if err != nil {
		return nil, err
	}
	return cache.NewServer(store, upstream)
}

func cacheCmd(args []string) error {
	if len(args) == 0 || (args[0] != "ls" && args[0] != "clear") {
		return fmt.Errorf("usage: spareci cache ls|clear [--repo owner/name]")
	}
	fs := flag.NewFlagSet("cache", flag.ExitOnError)
	repo := fs.String("repo", "", "only this repo (default: all)")
	_ = fs.Parse(args[1:])
	store, err := cache.Open(cacheDir(), 0)
	if err != nil {
		return err
	}
	if args[0] == "clear" {
		n, err := store.Delete(*repo)
		fmt.Printf("removed %d entries\n", n)
		return err
	}
	entries := store.List(*repo)
	var total int64
	for _, e := range entries {
		total += e.Size
		fmt.Printf("%-24s %-22s %9s  %s\n", e.Repo, e.Scope, cache.HumanBytes(e.Size), e.Key)
	}
	fmt.Printf("%d entries, %s in %s\n", len(entries), cache.HumanBytes(total), cacheDir())
	return nil
}
