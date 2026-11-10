package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/Row-Labs/spareci/internal/runner"
	"github.com/Row-Labs/spareci/internal/workflows"
)

const initUsage = `Usage: spareci init [--jobs workflow:job,...] [--all-compatible] [--dry-run]

Opts a repo's workflow jobs in to spareci: rewrites each chosen job's runs-on to
  runs-on: ${{ vars.SPARECI_RUNS_ON || '<original>' }}
and opens a pull request with the change. While spareci is up, those jobs run
on this machine; otherwise they run where they did before.

Run it in the repo. With a terminal it asks which jobs to convert; otherwise
pass --jobs or --all-compatible.

`

// initCmd is "spareci init".
func initCmd(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, initUsage); fs.PrintDefaults() }
	jobsFlag := fs.String("jobs", "", "jobs to convert, as workflow:job,... (workflow is the file name)")
	all := fs.Bool("all-compatible", false, "convert every job recommended for this machine")
	dry := fs.Bool("dry-run", false, "show the diff; change nothing")
	label := fs.String("label", "spareci", "label that routes a job to this machine (as for spareci up)")
	vname := fs.String("fallback-var", workflows.DefaultVar, "repo variable spareci sets while up")
	branch := fs.String("branch", "spareci-init", "branch to commit to")
	base := fs.String("base", "", "pull request base (default: the current branch)")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		fs.Usage()
		os.Exit(2)
	}
	if *jobsFlag != "" && *all {
		return fmt.Errorf("pass --jobs or --all-compatible, not both")
	}

	root, err := git("", "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("not in a git repo")
	}
	files, err := workflows.Scan(root, *vname)
	if os.IsNotExist(err) {
		return fmt.Errorf("no %s in %s", workflows.Dir, root)
	} else if err != nil {
		return err
	}
	m := workflows.Machine{OS: runtime.GOOS, Arch: runtime.GOARCH, Labels: append(runner.DefaultLabels(), *label)}
	m.Assess(files)

	choices := workflows.Convertible(files)
	printJobs(files, choices, m)
	if len(choices) == 0 {
		fmt.Println("\nNothing to convert.")
		return nil
	}

	interactive := isTerminal(os.Stdin)
	var chosen []*workflows.Job
	switch {
	case *jobsFlag != "":
		chosen, err = workflows.Select(files, *jobsFlag)
	case *all:
		if chosen = workflows.Recommended(files); len(chosen) == 0 {
			err = fmt.Errorf("no job is recommended for this machine; name jobs with --jobs")
		}
	case interactive:
		chosen, err = prompt(choices, workflows.Recommended(files))
	default:
		err = fmt.Errorf("choose jobs with --jobs workflow:job,... or --all-compatible")
	}
	if err != nil {
		return err
	}
	if len(chosen) == 0 {
		fmt.Println("Nothing chosen.")
		return nil
	}
	for _, j := range chosen {
		if j.Fit != workflows.FitGood {
			fmt.Printf("note: %s: %s\n", j.Name(), j.Why)
		}
	}

	changes, err := workflows.Apply(files, chosen)
	if err != nil {
		return err
	}
	fmt.Println()
	for _, c := range changes {
		fmt.Print(workflows.Diff(c.File.Path, c.File.Src, c.New))
	}
	if *dry {
		return nil
	}
	if interactive && !confirm("\nCommit this to a new branch "+*branch+", push it and open a pull request?") {
		return nil
	}
	return openPR(root, *branch, *base, *vname, chosen, changes)
}

func printJobs(files []*workflows.File, choices []*workflows.Job, m workflows.Machine) {
	num := map[*workflows.Job]int{}
	for i, j := range choices {
		num[j] = i + 1
	}
	fmt.Printf("Jobs in %s (this machine: %s %s):\n\n", workflows.Dir, map[string]string{"darwin": "macOS", "linux": "Linux"}[m.OS], m.Arch)
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	for _, f := range files {
		for _, j := range f.Jobs {
			n := ""
			if num[j] > 0 {
				n = strconv.Itoa(num[j])
			}
			var note string
			switch {
			case j.Skip != "":
				note = "skip: " + j.Skip
			case j.Fit == workflows.FitGood:
				note = "recommended"
			case j.Fit == workflows.FitNo:
				note = "can't run here: " + j.Why
			default:
				note = "maybe: " + j.Why
			}
			runsOn := j.RunsOn
			if runsOn == "" {
				runsOn = "-"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", n, j.Name(), runsOn, note)
		}
	}
	tw.Flush()
}

func prompt(choices, recommended []*workflows.Job) ([]*workflows.Job, error) {
	var def []string
	for _, r := range recommended {
		for i, c := range choices {
			if c == r {
				def = append(def, strconv.Itoa(i+1))
			}
		}
	}
	for {
		if len(def) > 0 {
			fmt.Printf("\nConvert which jobs? Numbers (e.g. 1,3), \"all\" or \"none\" [%s]: ", strings.Join(def, ","))
		} else {
			fmt.Print("\nConvert which jobs? Numbers (e.g. 1,3), \"all\" or \"none\": ")
		}
		line, err := initIn.ReadString('\n')
		if err != nil && line == "" {
			return nil, fmt.Errorf("no answer")
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "" && len(def) > 0:
			return recommended, nil
		case line == "none":
			return nil, nil
		case line == "all":
			return choices, nil
		}
		var out []*workflows.Job
		seen := map[int]bool{}
		bad := ""
		for _, s := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' }) {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > len(choices) {
				bad = s
				break
			}
			if !seen[n] {
				seen[n] = true
				out = append(out, choices[n-1])
			}
		}
		if bad == "" && len(out) > 0 {
			return out, nil
		}
		fmt.Printf("%q isn't one of 1-%d.\n", bad, len(choices))
	}
}

// initIn is shared by prompt and confirm so neither loses buffered input.
var initIn = bufio.NewReader(os.Stdin)

func confirm(q string) bool {
	fmt.Print(q + " [Y/n]: ")
	line, _ := initIn.ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "" || a == "y" || a == "yes"
}

// openPR commits the changes on a new branch off HEAD, pushes it and opens a
// pull request into base. HEAD must match origin's base, so the pull request
// holds only this commit.
func openPR(root, branch, base, vname string, chosen []*workflows.Job, changes []workflows.Change) error {
	var paths []string
	for _, c := range changes {
		paths = append(paths, c.File.Path)
	}
	if out, err := git(root, append([]string{"status", "--porcelain", "--"}, paths...)...); err != nil {
		return err
	} else if out != "" {
		return fmt.Errorf("these workflow files have uncommitted changes; commit or stash them first:\n%s", out)
	}
	cur, err := git(root, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("HEAD is detached; check out the branch the pull request should go into")
	}
	if base == "" {
		base = cur
	}
	head, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	remote, err := git(root, "ls-remote", "origin", "refs/heads/"+base)
	if err != nil {
		return err
	}
	if sha, _, _ := strings.Cut(remote, "\t"); sha != head {
		return fmt.Errorf("HEAD isn't origin/%s; check out %s and pull (or push), or pass --base, so the pull request holds only this change", base, base)
	}
	if _, err := git(root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return fmt.Errorf("branch %s already exists; delete it or pass --branch", branch)
	}
	if out, _ := git(root, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
		return fmt.Errorf("origin already has a branch %s; pass --branch", branch)
	}
	if vis, err := ghCLI(root, nil, "repo", "view", "--json", "visibility", "-q", ".visibility"); err == nil && vis == "PUBLIC" {
		fmt.Println("warning: this repo is public. spareci runs jobs on your machine as you, so a fork's pull request could run code here. See the README's Security section.")
	}

	if _, err := git(root, "switch", "-c", branch); err != nil {
		return err
	}
	committed := false
	defer func() {
		if _, err := git(root, "switch", cur); err != nil {
			fmt.Fprintf(os.Stderr, "spareci: switching back to %s: %v\n", cur, err)
		}
		if !committed {
			_, _ = git(root, append([]string{"checkout", "HEAD", "--"}, paths...)...)
			_, _ = git(root, "branch", "-D", branch)
		}
	}()
	for _, c := range changes {
		if err := os.WriteFile(filepath.Join(root, c.File.Path), c.New, 0o644); err != nil {
			return err
		}
	}
	names := jobNames(chosen)
	msg := "Run " + plural(len(chosen), "job") + " on spareci when it's up\n\n" +
		"runs-on falls back to the original runner when spareci isn't running.\n\n" +
		"Jobs: " + strings.Join(names, ", ") + "\n"
	if _, err := git(root, append([]string{"commit", "-m", msg, "--"}, paths...)...); err != nil {
		return err
	}
	committed = true
	if _, err := git(root, "push", "-u", "origin", branch); err != nil {
		return err
	}
	url, err := ghCLI(root, strings.NewReader(prBody(vname, chosen)), "pr", "create",
		"--base", base, "--head", branch,
		"--title", "Run "+plural(len(chosen), "job")+" on spareci when it's up",
		"--body-file", "-")
	if err != nil {
		return fmt.Errorf("pushed %s, but opening the pull request failed: %w", branch, err)
	}
	fmt.Println("\nOpened", url)
	fmt.Println("Merge it, then run `spareci up` here to take those jobs.")
	return nil
}

func prBody(vname string, chosen []*workflows.Job) string {
	example := "ubuntu-24.04"
	if l := chosen[0].Labels; len(l) == 1 {
		example = l[0]
	}
	var b strings.Builder
	b.WriteString(`This lets the jobs below run on a machine running [spareci](https://github.com/Row-Labs/spareci) instead of GitHub-hosted runners, and leaves them where they are the rest of the time.

## What spareci is

spareci runs GitHub Actions jobs on a machine you already own (a Mac mini, a spare laptop, a VPS). For each queued job it registers a single-use self-hosted runner with the official ` + "`actions/runner`" + `, runs the job, and deletes the runner. ` + "`actions/cache`" + ` is served from the machine's own disk.

## How the fallback works

Each job's ` + "`runs-on`" + ` becomes:

` + "```yaml\nruns-on: ${{ vars." + vname + " || '" + example + "' }}\n```" + `

While ` + "`spareci up`" + ` is running, it sets the repo variable ` + "`" + vname + "`" + ` to its runner label, so these jobs go to that machine. When it stops, it deletes the variable and the expression falls back to the original label: the jobs run on GitHub-hosted runners exactly as before. Nothing changes until someone runs spareci.

## Jobs

| Job | Falls back to |
|---|---|
`)
	for _, j := range chosen {
		fmt.Fprintf(&b, "| `%s` | `%s` |\n", j.Name(), strings.ReplaceAll(j.RunsOn, "|", `\|`))
	}
	b.WriteString("\nOpened by `spareci init`.\n")
	return b.String()
}

func jobNames(jobs []*workflows.Job) []string {
	var s []string
	for _, j := range jobs {
		s = append(s, j.Name())
	}
	sort.Strings(s)
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func git(dir string, args ...string) (string, error) {
	return run(dir, nil, "git", args...)
}

func ghCLI(dir string, stdin *strings.Reader, args ...string) (string, error) {
	return run(dir, stdin, "gh", args...)
}

func run(dir string, stdin *strings.Reader, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %v: %s", name, args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}
