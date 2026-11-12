// spareci turns the machine you're on into GitHub Actions runners for a repo:
// single-use, on demand, and out of the way when the machine is off.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Row-Labs/spareci/internal/cache"
	"github.com/Row-Labs/spareci/internal/gh"
	"github.com/Row-Labs/spareci/internal/gitmirror"
	"github.com/Row-Labs/spareci/internal/pool"
	"github.com/Row-Labs/spareci/internal/route"
	"github.com/Row-Labs/spareci/internal/runner"
	"github.com/Row-Labs/spareci/internal/service"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

const usage = `spareci: run your GitHub Actions jobs on your own machine.

Usage:
  spareci init      [--jobs workflow:job,...] [--all-compatible] [--dry-run]
                    opt this repo's workflow jobs in to spareci and open a PR
  spareci up        [--repo owner/name] [--label spareci] [--max 1] [--max-wait 10m]
                    [--os macos|linux]... [--name machine]
                    [--max-macos N] [--max-linux N] [--linux-x64-on-arm=false]
                    [--dry-run] [--cache=true] [--cache-size 10GB]
                    [--job-home spareci|user|PATH] [--git-mirror=true]
                    [--only-on-ac] [--idle 10m] [--hours 18:00-08:00]
                    [--drain-timeout 10m]
  spareci install   [--repo owner/name] [up flags]   run "up" in the background, at login
  spareci uninstall [--repo owner/name] [--drain-timeout 10m]
  spareci logs      [--repo owner/name] [-f] [-n 50]
  spareci watchdog  [--setup [--force]]   print the workflow that reroutes jobs if this
                    machine dies; --setup also creates the app it uses to write variables
  spareci status    [--repo owner/name]
  spareci down      [--repo owner/name]   route jobs back to GitHub after a crash
  spareci cache     ls|clear [--repo owner/name]
  spareci estimate  [--repo owner/name]... [--org name] [--days 30] [--json]
  spareci stats     [--json]   what the jobs this machine ran would have cost on GitHub
  spareci version

In your workflow (spareci init does this), route jobs here when spareci is up,
and to GitHub otherwise, with the variable for the job's OS:

  runs-on: ${{ vars.SPARECI_RUNS_ON_MACOS || 'macos-15' }}
  runs-on: ${{ vars.SPARECI_RUNS_ON_LINUX || 'ubuntu-24.04' }}
  runs-on: ${{ vars.SPARECI_RUNS_ON_LINUX_ARM || 'ubuntu-24.04-arm' }}

spareci up sets each one while this machine can run that kind of job.
Several machines can split a repo by OS: spareci up --os macos on a Mac,
spareci up --os linux on a Linux machine.
SPARECI_RUNS_ON, from spareci 0.1, still routes to this machine's own OS;
spareci init upgrades it.
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
	case "init":
		err = initCmd(os.Args[2:])
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
	case "estimate":
		err = estimateCmd(os.Args[2:])
	case "stats":
		err = statsCmd(os.Args[2:])
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
	name  string  // this machine, in heartbeats and runner names
	oses  osFlags // --os; empty: every OS the machine can run
}

// osFlags collects --os, repeated or comma-separated.
type osFlags []string

func (o *osFlags) String() string { return strings.Join(*o, ",") }

func (o *osFlags) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		s = strings.ToLower(strings.TrimSpace(s))
		if !slices.Contains(route.OSes, s) {
			return fmt.Errorf("unknown OS %q: want %s", s, strings.Join(route.OSes, " or "))
		}
		if !slices.Contains(*o, s) {
			*o = append(*o, s)
		}
	}
	return nil
}

// serves reports whether this machine was asked to serve an OS: every OS
// without --os.
func (c *common) serves(osName string) bool {
	return len(c.oses) == 0 || slices.Contains(c.oses, osName)
}

// split reports whether --os was given. Such a machine shares the repo with
// others, and doesn't own the old shared variable SPARECI_RUNS_ON.
func (c *common) split() bool { return len(c.oses) > 0 }

func defaultMachine() string {
	host, _ := os.Hostname()
	return route.CleanMachine(cmp.Or(strings.Split(host, ".")[0], "spareci"))
}

func commonFlags(fs *flag.FlagSet) *common {
	c := &common{}
	fs.StringVar(&c.repo, "repo", "", "owner/name (default: the repo in the current directory)")
	fs.StringVar(&c.label, "label", route.DefaultPrefix, "routing label prefix: jobs routed here carry <label>-macos, <label>-linux, ... (or <label> from SPARECI_RUNS_ON)")
	fs.StringVar(&c.vname, "fallback-var", route.DefaultBase, "base name of the routing variables: <name>_MACOS, <name>_LINUX, <name>_LINUX_ARM and <name> itself (empty to disable routing)")
	fs.StringVar(&c.hname, "heartbeat-var", "SPARECI_HEARTBEAT", "base name of the heartbeats refreshed while up, read by the watchdog workflow: <name>_MACOS, <name>_LINUX and <name> itself (empty to disable)")
	fs.StringVar(&c.name, "name", defaultMachine(), "this machine's name, written in its heartbeats and runner names")
	fs.Var(&c.oses, "os", "serve only this OS's jobs: macos or linux; repeat for both (default: every OS this machine can run). Lets several machines split a repo")
	return c
}

// router routes the given kinds of jobs here. A machine started without
// --os also owns the old shared variable.
func (c *common) router(client *gh.Client, targets []route.Target) *route.Router {
	r := &route.Router{API: client, Repo: c.repo, Machine: route.CleanMachine(c.name),
		Groups: route.Groups(c.vname, c.label, c.hname, targets, !c.split())}
	if c.split() {
		r.OldHeartbeat = c.hname
	}
	return r
}

// targets are the kinds of jobs of the OSes this machine serves.
func (c *common) targets() []route.Target {
	var out []route.Target
	for _, t := range route.Targets {
		if c.serves(t.OS) {
			out = append(out, t)
		}
	}
	return out
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
	maxMacOS  *int
	maxLinux  *int
	x64OnARM  *bool
	maxWait   *time.Duration
	interval  *time.Duration
	dry       *bool
	verbose   *bool
	version   *string
	useCache  *bool
	cacheSize *string
	upstream  *string
	jobHome   *string
	gitMirror *bool
	linux     linuxOpts
	laptop    *laptopOpts
	drain     *time.Duration
}

// upFlags defines up's flags; install checks its pass-through flags with it.
func upFlags(fs *flag.FlagSet) *upOpts {
	return &upOpts{
		common:    commonFlags(fs),
		maxN:      fs.Int("max", 1, "concurrent jobs, of every kind"),
		maxMacOS:  fs.Int("max-macos", 0, "concurrent macOS jobs (0: up to --max)"),
		maxLinux:  fs.Int("max-linux", 0, "concurrent Linux jobs (0: up to --max)"),
		x64OnARM:  fs.Bool("linux-x64-on-arm", true, "on an arm64 machine, run Linux x64 jobs (SPARECI_RUNS_ON_LINUX, ubuntu-24.04) as arm64; =false runs them as x64 (in containers under Rosetta on a Mac)"),
		maxWait:   fs.Duration("max-wait", 10*time.Minute, "send a job queued here longer than this to GitHub-hosted runners (0 to never)"),
		interval:  fs.Duration("interval", 5*time.Second, "poll interval"),
		dry:       fs.Bool("dry-run", false, "log the jobs we would take; register nothing"),
		verbose:   fs.Bool("v", false, "debug logging"),
		version:   fs.String("runner-version", "", "actions/runner version (default: latest)"),
		useCache:  fs.Bool("cache", true, "serve actions/cache from this machine's disk instead of GitHub"),
		cacheSize: fs.String("cache-size", "10GB", "local cache size before least recently used entries are evicted"),
		upstream:  fs.String("cache-upstream", cache.DefaultUpstream, "GitHub results service, for calls the local cache passes through (artifacts)"),
		jobHome:   fs.String("job-home", runner.HomeSpareci, `HOME for jobs: "spareci" (a separate home per repo), "user" (your own home and environment), or a path`),
		gitMirror: fs.Bool("git-mirror", true, "keep a local mirror of the repo so checkouts fetch only new objects (needs a separate --job-home)"),
		linux: linuxOpts{
			cpus:   fs.String("linux-cpus", "", `CPUs per Linux job container, as docker run --cpus takes them ("4"; default: no limit)`),
			memory: fs.String("linux-memory", "", `memory per Linux job container, as docker run --memory takes it ("16g"; default: no limit)`),
			docker: fs.String("linux-docker", runner.DockerSocket, `docker for Linux jobs: "socket" (the runtime's socket; jobs control it, no isolation) or "none"`),
		},
		laptop: laptopFlags(fs),
		drain:  fs.Duration("drain-timeout", defaultDrain, "on ^C or SIGTERM, how long running jobs get to finish before they're stopped and re-run on GitHub-hosted runners (0: no limit)"),
	}
}

const defaultDrain = 10 * time.Minute

// stopOn turns signals (^C, or SIGTERM from a service manager) into the
// stages of stopping. The first cancels soft: stop taking jobs, route back
// to GitHub, let running jobs finish. hard is cancelled drain later, or on a
// second signal: stop running jobs and re-run them on GitHub-hosted runners
// (pool.Pool.drain). A third signal calls quit.
func stopOn(sigs <-chan os.Signal, drain time.Duration, quit func()) (soft, hard context.Context) {
	soft, softCancel := context.WithCancel(context.Background())
	hard, hardCancel := context.WithCancel(context.Background())
	go func() {
		<-sigs
		if drain > 0 {
			slog.Info("shutting down: running jobs get until the drain timeout to finish", "drain_timeout", drain)
			t := time.AfterFunc(drain, func() {
				slog.Info("drain timeout reached", "drain_timeout", drain)
				hardCancel()
			})
			defer t.Stop()
		} else {
			slog.Info("shutting down")
		}
		softCancel()
		select {
		case <-sigs:
			slog.Info("stopping running jobs now")
			hardCancel()
		case <-hard.Done():
		}
		<-sigs
		slog.Info("quitting without cleaning up")
		quit()
	}()
	return soft, hard
}

func up(args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	o := upFlags(fs)
	_ = fs.Parse(args)
	c := o.common
	maxN, maxWait, interval, dry, verbose, version := o.maxN, o.maxWait, o.interval, o.dry, o.verbose, o.version
	useCache, cacheSize, upstream, jobHome := o.useCache, o.cacheSize, o.upstream, o.jobHome
	if *verbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	if err := c.resolve(); err != nil {
		return err
	}
	if c.serves(route.MacOS) && c.split() && runner.HostOS() != runner.OSMacOS {
		return fmt.Errorf("--os macos: this machine can't run macOS jobs; only a Mac can")
	}
	g, err := o.laptop.gate(c.repo)
	if err != nil {
		return err
	}
	client, err := gh.New()
	if err != nil {
		return err
	}
	if c.split() && !*dry && c.vname != "" {
		// Before the slow setup (the container image): refuse now if another
		// machine serves one of these OSes.
		if err := checkSplit(context.Background(), client, c); err != nil {
			return err
		}
	}

	sigs := make(chan os.Signal, 3)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	soft, hard := stopOn(sigs, *o.drain, func() { os.Exit(1) })

	host := &runner.Host{Root: runner.DefaultRoot(), Repo: c.repo, Version: *version}
	if !*dry {
		// Before any cleanup: another repo's spareci starting now must see
		// that this repo's jobs are live.
		release, err := runner.Claim(host.Root, c.repo)
		if err != nil {
			return err
		}
		defer release()
	}
	if host.JobHome, err = runner.ResolveJobHome(*jobHome, host.Root, c.repo); err != nil {
		return err
	}
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
		if err := host.PrepareDocker(); err != nil {
			slog.Warn("docker setup for jobs failed; docker steps may not find buildx or the local cache", "err", err)
		}
		if *o.gitMirror && host.JobHome != "" {
			host.Git = gitmirror.New(host.Root, c.repo, client.Token())
			go func() {
				ctx, cancel := context.WithTimeout(soft, time.Hour)
				defer cancel()
				if err := host.Git.Update(ctx); err != nil && ctx.Err() == nil {
					slog.Warn("no git mirror; checkouts fetch everything from GitHub", "err", err)
				}
			}()
		}
	}

	var cacheSrv *cache.Server
	if *useCache && !*dry {
		if cacheSrv, err = startCache(host, *cacheSize, *upstream); err != nil {
			slog.Warn("local cache off; jobs will use GitHub's cache", "err", err)
		}
	}

	var all []runner.Backend
	if c.serves(runner.HostOS()) {
		all = append(all, host)
	}
	if c.serves(route.Linux) {
		all = append(all, linuxBackends(soft, host, c.repo, *dry, *o.x64OnARM, o.linux)...)
	}
	backends, targets := pool.Assign(all, pool.Policy{
		Prefix: c.label, X64OnARM: *o.x64OnARM, Legacy: !c.split(),
		Max: map[string]int{runner.OSMacOS: *o.maxMacOS, runner.OSLinux: *o.maxLinux},
	})
	if len(backends) == 0 {
		if c.split() {
			return fmt.Errorf("this machine can't run %s jobs now (on a Mac, Linux jobs need a running container runtime: OrbStack, Docker Desktop or Colima)", strings.Join(c.oses, " or "))
		}
		return fmt.Errorf("no backend can run jobs on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	for _, osName := range c.oses {
		if !slices.ContainsFunc(backends, func(b *pool.Backend) bool { return b.OS() == osName }) {
			return fmt.Errorf("--os %s: this machine can't run %s jobs now (on a Mac, Linux jobs need a running container runtime: OrbStack, Docker Desktop or Colima)", osName, osName)
		}
	}
	for _, b := range backends {
		slog.Info("backend", "name", b.Name(), "labels", b.AllLabels(), "max", cmp.Or(b.Max, *maxN))
	}
	slog.Info("spareci up", "repo", c.repo, "machine", route.CleanMachine(c.name), "os", cmp.Or(c.oses.String(), "all"), "max", *maxN, "runner", host.Version, "dir", host.Root, "job_home", cmp.Or(host.JobHome, "user"), "local_cache", cacheSrv != nil, "git_mirror", host.Git != nil, "dry_run", *dry)
	p := pool.New(pool.Config{
		Repo:     c.repo,
		Backends: backends,
		Max:      *maxN,
		Interval: *interval,
		Router:   c.router(client, targets),
		Machine:  route.CleanMachine(c.name),
		MaxWait:  *maxWait,
		DryRun:   *dry,
		Cache:    cacheSrv,
		Gate:     poolGate(g),
		Stats:    recorder(*dry, client, c, host.Root),
	}, client)
	if g != nil {
		slog.Info("laptop mode: taking jobs only when conditions are met", "when", g.Config().String())
		defer removeGateState(c.repo)
	}
	return conflictHint(p.Run(soft, hard), c)
}

// conflictHint adds what to do to another machine serving the same OS.
func conflictHint(err error, c *common) error {
	var ce *route.ConflictError
	if !errors.As(err, &ce) {
		return err
	}
	if ce.Owner.Group.Legacy() || ce.Owner.Machine == "" {
		return fmt.Errorf("%w. Stop spareci there (or upgrade it to 0.3 and split the OSes with --os), then start this one again", err)
	}
	var rest []string
	for _, osName := range route.OSes {
		if osName != ce.Owner.Group.OS && (c.serves(osName) || !c.split()) {
			rest = append(rest, osName)
		}
	}
	hint := "stop spareci there"
	if len(rest) > 0 {
		hint += ", or start this machine with --os " + strings.Join(rest, " --os ")
	}
	return fmt.Errorf("%w. Two machines can't serve the same OS for one repo: %s. If %s is gone, its heartbeat goes stale in %s, or clear it with spareci down --name %s on any machine",
		err, hint, ce.Owner.Who(), route.StaleAfter-ce.Owner.Age.Round(time.Second), ce.Owner.Machine)
}

// checkSplit runs before a machine with --os starts: no other machine may
// serve its OSes, no spareci 0.2 (which takes every OS) may be up, and the
// repo's watchdog must know per-OS heartbeats, or it would clear this
// machine's routing every few minutes.
func checkSplit(ctx context.Context, client *gh.Client, c *common) error {
	r := c.router(client, c.targets())
	if err := r.Check(ctx); err != nil {
		return conflictHint(err, c)
	}
	if c.hname == "" {
		return nil
	}
	wf, err := client.FileAt(ctx, c.repo, watchdogPath, "")
	switch {
	case gh.IsStatus(err, 404):
	case err != nil:
		slog.Warn("could not read the watchdog workflow; if it's from spareci 0.2, regenerate it with spareci watchdog", "path", watchdogPath, "err", err)
	case !strings.Contains(string(wf), route.HeartbeatVar(c.hname, route.MacOS)):
		return fmt.Errorf("%s on %s's default branch is from spareci 0.2: it reads only %s, which machines started with --os don't write, so it would clear their routing every few minutes. Regenerate it (spareci watchdog > %s) and merge it, or start without --os",
			watchdogPath, c.repo, c.hname, watchdogPath)
	}
	return nil
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
	if err := printOwners(ctx, client, c); err != nil {
		return err
	}
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

// printOwners shows which machine serves each OS, by its heartbeat.
func printOwners(ctx context.Context, client *gh.Client, c *common) error {
	if c.vname == "" {
		return nil
	}
	now := time.Now()
	fmt.Println("routing:")
	old := route.Owner{}
	if c.hname != "" {
		var err error
		if old, err = route.ReadOwner(ctx, client, c.repo, route.Group{Heartbeat: c.hname}, now); err != nil {
			return err
		}
	}
	for _, g := range route.AllGroups(c.vname, c.label, c.hname) {
		o, err := route.ReadOwner(ctx, client, c.repo, g, now)
		if err != nil {
			return err
		}
		if !o.Beating && !g.Legacy() && old.Beating {
			// spareci 0.2 writes only the old heartbeat, as the watchdog reads it.
			o.Beating, o.Machine, o.Age = true, old.Machine, old.Age
		}
		var state string
		switch {
		case len(o.Set) == 0 && !o.Fresh():
			state = "off: jobs go to GitHub-hosted runners"
		case len(o.Set) == 0:
			state = fmt.Sprintf("off, but %s's heartbeat is %s old (laptop mode resuming?)", o.Who(), o.Age.Round(time.Second))
		case o.Fresh():
			state = fmt.Sprintf("%s, heartbeat %s ago", o.Who(), o.Age.Round(time.Second))
		case o.Beating:
			state = fmt.Sprintf("%s stopped beating %s ago: jobs queue for it until the watchdog or spareci down --name %s clears %s",
				o.Who(), o.Age.Round(time.Second), o.Machine, strings.Join(o.Set, ", "))
		default:
			state = "set with no heartbeat: jobs queue for a machine that may be gone (spareci down)"
		}
		if o.Fresh() && o.Machine == route.CleanMachine(c.name) {
			state += " (this machine)"
		}
		fmt.Printf("  %-16s %s\n", g.Name(), state)
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

// routeBack clears the routing variables of this machine's OSes (every OS
// without --os), sends jobs routed to them and still queued to
// GitHub-hosted runners, re-runs jobs left in progress on this machine's
// runners that are gone, and removes runner registrations left behind by a
// crash. An OS whose fresh heartbeat names another machine is left alone:
// that machine serves it.
func routeBack(ctx context.Context, client *gh.Client, c *common) error {
	runners, err := client.ListRunners(ctx, c.repo)
	if err != nil {
		return err
	}
	prefix := pool.RunnerPrefix(route.CleanMachine(c.name))
	r := c.router(client, c.targets())
	if r.Enabled() {
		if err := r.Stop(ctx); err != nil {
			return err
		}
		for _, h := range r.Held() {
			fmt.Printf("left %s routing alone: another machine is serving it (spareci status)\n", h)
		}
		if vars := r.Vars(); len(vars) > 0 {
			fmt.Printf("cleared %s: new jobs go to GitHub-hosted runners\n", strings.Join(vars, ", "))
			n, err := pool.RescueQueued(ctx, client, r, r.Routed)
			if n > 0 {
				fmt.Printf("re-ran %d run(s) whose jobs were queued for this machine\n", n)
			}
			if err != nil {
				return err
			}
		}
		runs, err := strandedRuns(ctx, client, c.repo, prefix, runners)
		if err != nil {
			return err
		}
		if len(runs) > 0 {
			fmt.Printf("cancelling %d run(s) with a job left in progress on a runner of this machine that's gone, and re-running them on GitHub-hosted runners\n", len(runs))
			if _, err := pool.RescueRuns(ctx, r, runs); err != nil {
				return err
			}
		}
	}
	for _, r := range runners {
		// Only this machine's: another machine's runner is offline for its
		// first seconds too, before it connects.
		if strings.HasPrefix(r.Name, prefix) && r.Status == "offline" {
			if err := client.DeleteRunner(ctx, c.repo, r.ID); err != nil {
				return err
			}
			fmt.Printf("removed stale runner %s\n", r.Name)
		}
	}
	return nil
}

// strandedRuns are runs with a job in progress on one of this machine's
// runners that is offline or no longer registered: spareci was killed
// before it could stop the runner, and GitHub would keep the job in
// progress for minutes.
func strandedRuns(ctx context.Context, client *gh.Client, repo, prefix string, runners []gh.Runner) ([]int64, error) {
	online := map[string]bool{}
	for _, r := range runners {
		online[r.Name] = r.Status != "offline"
	}
	runs, err := client.ListRuns(ctx, repo, "in_progress")
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, run := range runs {
		jobs, err := client.RunJobs(ctx, repo, run.ID)
		if err != nil {
			return nil, err
		}
		for _, j := range jobs {
			if j.Status == "in_progress" && strings.HasPrefix(j.RunnerName, prefix) && !online[j.RunnerName] {
				out = append(out, run.ID)
				break
			}
		}
	}
	return out, nil
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
		// The service manager lets spareci drain before killing it.
		Drain:     *checked.drain,
		StopAfter: pool.StopAfterDrain,
	}
	path, err := service.Install(spec, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Printf("installed %s\n  runs: %s %s\n  logs: %s (spareci logs -f)\n", path, exe, strings.Join(spec.Args, " "), spec.Log)
	fmt.Println("It starts at login and restarts if it crashes. To make a dead machine safe, set up the watchdog:")
	fmt.Println("  spareci watchdog --setup")
	if runtime.GOOS == "linux" {
		fmt.Println("To run while you're logged out, enable lingering: loginctl enable-linger $USER")
	}
	return nil
}

func uninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	c := commonFlags(fs)
	drain := fs.Duration("drain-timeout", defaultDrain, "how long running jobs get to finish before they're stopped and re-run on GitHub-hosted runners (0: no limit)")
	_ = fs.Parse(args)
	if err := c.resolve(); err != nil {
		return err
	}
	// Stop it as ^C does, so no job is left in progress on a runner that's
	// gone; the service manager would kill it after its stop timeout.
	if err := service.Stop(c.repo, *drain, pool.StopAfterDrain, logPath(c.repo), os.Stdout); err != nil {
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

func logPath(repo string) string {
	return filepath.Join(runner.DefaultRoot(), "service", service.Name(repo)+".log")
}
