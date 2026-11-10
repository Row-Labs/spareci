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
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Row-Labs/spareci/internal/cache"
	"github.com/Row-Labs/spareci/internal/gh"
	"github.com/Row-Labs/spareci/internal/pool"
	"github.com/Row-Labs/spareci/internal/route"
	"github.com/Row-Labs/spareci/internal/runner"
	"github.com/Row-Labs/spareci/internal/service"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

const usage = `spareci: run your GitHub Actions jobs on your own machine.

Usage:
  spareci up        [--repo owner/name] [--label spareci] [--max 1] [--max-wait 10m]
                    [--dry-run] [--cache=true] [--cache-size 10GB]
                    [--only-on-ac] [--idle 10m] [--hours 18:00-08:00]
  spareci install   [--repo owner/name] [up flags]   run "up" in the background, at login
  spareci uninstall [--repo owner/name]
  spareci logs      [--repo owner/name] [-f] [-n 50]
  spareci watchdog  print the workflow that reroutes jobs if this machine dies
  spareci status    [--repo owner/name]
  spareci down      [--repo owner/name]   route jobs back to GitHub after a crash
  spareci cache     ls|clear [--repo owner/name]
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
	case "install":
		err = install(os.Args[2:])
	case "uninstall":
		err = uninstall(os.Args[2:])
	case "logs":
		err = logs(os.Args[2:])
	case "watchdog":
		err = watchdog(os.Args[2:])
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
	hname string
}

func commonFlags(fs *flag.FlagSet) *common {
	c := &common{}
	fs.StringVar(&c.repo, "repo", "", "owner/name (default: the repo in the current directory)")
	fs.StringVar(&c.label, "label", "spareci", "label that routes a job to this machine")
	fs.StringVar(&c.vname, "fallback-var", "SPARECI_RUNS_ON", "repo variable set to --label while up (empty to disable)")
	fs.StringVar(&c.hname, "heartbeat-var", "SPARECI_HEARTBEAT", "repo variable refreshed while up, read by the watchdog workflow (empty to disable)")
	return c
}

func (c *common) router(client *gh.Client) *route.Router {
	return &route.Router{API: client, Repo: c.repo, Var: c.vname, Value: c.label, HeartbeatVar: c.hname}
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

type upOpts struct {
	*common
	maxN      *int
	maxWait   *time.Duration
	interval  *time.Duration
	dry       *bool
	verbose   *bool
	version   *string
	useCache  *bool
	cacheSize *string
	upstream  *string
	laptop    *laptopOpts
}

// upFlags defines up's flags; install checks its pass-through flags with it.
func upFlags(fs *flag.FlagSet) *upOpts {
	return &upOpts{
		common:    commonFlags(fs),
		maxN:      fs.Int("max", 1, "concurrent jobs"),
		maxWait:   fs.Duration("max-wait", 10*time.Minute, "send a job queued here longer than this to GitHub-hosted runners (0 to never)"),
		interval:  fs.Duration("interval", 5*time.Second, "poll interval"),
		dry:       fs.Bool("dry-run", false, "log the jobs we would take; register nothing"),
		verbose:   fs.Bool("v", false, "debug logging"),
		version:   fs.String("runner-version", "", "actions/runner version (default: latest)"),
		useCache:  fs.Bool("cache", true, "serve actions/cache from this machine's disk instead of GitHub"),
		cacheSize: fs.String("cache-size", "10GB", "local cache size before least recently used entries are evicted"),
		upstream:  fs.String("cache-upstream", cache.DefaultUpstream, "GitHub results service, for calls the local cache passes through (artifacts)"),
		laptop:    laptopFlags(fs),
	}
}

func up(args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	o := upFlags(fs)
	_ = fs.Parse(args)
	c := o.common
	maxN, maxWait, interval, dry, verbose, version := o.maxN, o.maxWait, o.interval, o.dry, o.verbose, o.version
	useCache, cacheSize, upstream := o.useCache, o.cacheSize, o.upstream
	if *verbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	if err := c.resolve(); err != nil {
		return err
	}
	g, err := o.laptop.gate(c.repo)
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
		Repo:     c.repo,
		Labels:   labels,
		Max:      *maxN,
		Interval: *interval,
		Router:   c.router(client),
		MaxWait:  *maxWait,
		DryRun:   *dry,
		Cache:    cacheSrv,
		Gate:     poolGate(g),
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
	return routeBack(context.Background(), client, c)
}

// routeBack clears routing, sends jobs already queued here to GitHub-hosted
// runners, and removes runner registrations left behind by a crash.
func routeBack(ctx context.Context, client *gh.Client, c *common) error {
	r := c.router(client)
	if c.vname != "" {
		if err := r.Stop(ctx); err != nil {
			return err
		}
		fmt.Printf("cleared %s: new jobs go to GitHub-hosted runners\n", c.vname)
		labels := append(runner.DefaultLabels(), c.label)
		match := func(j gh.Job) bool { return pool.Matches(labels, j) }
		n, err := pool.RescueQueued(ctx, client, r, match)
		if n > 0 {
			fmt.Printf("re-ran %d run(s) whose jobs were queued for this machine\n", n)
		}
		if err != nil {
			return err
		}
	}
	runners, err := client.ListRunners(ctx, c.repo)
	if err != nil {
		return err
	}
	for _, r := range runners {
		// Only this machine's: another machine's runner is offline for its
		// first seconds too, before it connects.
		if strings.HasPrefix(r.Name, pool.RunnerPrefix()) && r.Status == "offline" {
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

// install runs `spareci up` for a repo as a background service that starts at
// login and restarts after a crash. Flags other than --repo are passed to up.
func install(args []string) error {
	c := &common{}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--repo" || a == "-repo":
			if i+1 == len(args) {
				return fmt.Errorf("--repo needs a value")
			}
			c.repo = args[i+1]
			i++
		case strings.HasPrefix(a, "--repo=") || strings.HasPrefix(a, "-repo="):
			c.repo = a[strings.Index(a, "=")+1:]
		default:
			rest = append(rest, a)
		}
	}
	// Check the flags now rather than in a restart loop.
	check := flag.NewFlagSet("up", flag.ContinueOnError)
	checked := upFlags(check)
	if err := check.Parse(rest); err != nil {
		return err
	}
	if _, err := checked.laptop.config(); err != nil {
		return err
	}
	if err := c.resolve(); err != nil {
		return err
	}
	if _, err := gh.New(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if strings.Contains(exe, "go-build") {
		return fmt.Errorf("install from a built binary (go install), not go run")
	}
	spec := service.Spec{
		Repo: c.repo,
		Exec: exe,
		Args: append([]string{"up", "--repo", c.repo}, rest...),
		Env:  map[string]string{"PATH": os.Getenv("PATH")}, // so the service finds gh
		Log:  logPath(c.repo),
	}
	path, err := service.Install(spec)
	if err != nil {
		return err
	}
	fmt.Printf("installed %s\n  runs: %s %s\n  logs: %s (spareci logs -f)\n", path, exe, strings.Join(spec.Args, " "), spec.Log)
	fmt.Println("It starts at login and restarts if it crashes. To make a dead machine safe, add the watchdog workflow:")
	fmt.Println("  spareci watchdog > .github/workflows/spareci-watchdog.yml")
	if runtime.GOOS == "linux" {
		fmt.Println("To run while you're logged out, enable lingering: loginctl enable-linger $USER")
	}
	return nil
}

func uninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	c := commonFlags(fs)
	_ = fs.Parse(args)
	if err := c.resolve(); err != nil {
		return err
	}
	found, err := service.Uninstall(c.repo)
	if err != nil {
		return err
	}
	if !found {
		fmt.Printf("no service installed for %s\n", c.repo)
	} else {
		fmt.Printf("stopped and removed the service for %s\n", c.repo)
	}
	// The service clears routing as it stops; do it again in case it was
	// already dead or was killed before it finished.
	client, err := gh.New()
	if err != nil {
		return err
	}
	return routeBack(context.Background(), client, c)
}

func logs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	repo := fs.String("repo", "", "owner/name (default: the repo in the current directory, or the only installed one)")
	follow := fs.Bool("f", false, "keep printing new lines")
	lines := fs.Int("n", 50, "lines to show")
	_ = fs.Parse(args)
	c := &common{repo: *repo}
	if c.repo == "" {
		if installed, _ := service.Installed(); len(installed) == 1 {
			c.repo = installed[0]
		}
	}
	if err := c.resolve(); err != nil {
		return err
	}
	path := logPath(c.repo)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no log for %s at %s; is it installed? (spareci install)", c.repo, path)
	}
	targs := []string{"-n", fmt.Sprint(*lines)}
	if *follow {
		targs = append(targs, "-f")
	}
	cmd := exec.Command("tail", append(targs, path)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func watchdog(args []string) error {
	fs := flag.NewFlagSet("watchdog", flag.ExitOnError)
	c := commonFlags(fs)
	_ = fs.Parse(args)
	if c.vname == "" || c.hname == "" {
		return fmt.Errorf("the watchdog needs both --fallback-var and --heartbeat-var")
	}
	fmt.Print(route.Watchdog(c.vname, c.hname))
	return nil
}

func logPath(repo string) string {
	return filepath.Join(runner.DefaultRoot(), "service", service.Name(repo)+".log")
}
