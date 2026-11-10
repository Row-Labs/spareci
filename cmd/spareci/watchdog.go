package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Row-Labs/spareci/internal/gh"
	"github.com/Row-Labs/spareci/internal/ghapp"
	"github.com/Row-Labs/spareci/internal/route"
)

const watchdogUsage = `Usage: spareci watchdog [--setup [--force]] [--repo owner/name]

Without --setup, prints the watchdog workflow:
  spareci watchdog > .github/workflows/spareci-watchdog.yml

With --setup, sets the watchdog up for the repo in one step:
  1. Opens GitHub in your browser to create a private GitHub App with one
     permission (Variables: read and write), then to install it on the repo.
  2. Stores the app's client ID and private key as the repo secrets
     ` + route.AppClientIDSecret + ` and ` + route.AppKeySecret + `, encrypted to the repo's key.
  3. Writes .github/workflows/spareci-watchdog.yml when run inside the repo.
Each watchdog run mints an hour-long token from the app, so nothing expires.

Manual fallback, instead of --setup: a fine-grained token for the repo with
"Variables: read and write", stored with: gh secret set ` + route.WatchdogSecret + `

`

const watchdogPath = ".github/workflows/spareci-watchdog.yml"

func watchdog(args []string) error {
	fs := flag.NewFlagSet("watchdog", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, watchdogUsage); fs.PrintDefaults() }
	c := commonFlags(fs)
	setup := fs.Bool("setup", false, "create and install the watchdog's GitHub App and store its secrets")
	force := fs.Bool("force", false, "with --setup: create a new app even if the repo already has one's secrets")
	_ = fs.Parse(args)
	if c.vname == "" || c.hname == "" {
		return fmt.Errorf("the watchdog needs both --fallback-var and --heartbeat-var")
	}
	workflow := route.Watchdog(c.vname, c.hname)
	if !*setup {
		fmt.Print(workflow)
		return nil
	}

	inCheckout := c.repo == ""
	if err := c.resolve(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client, err := gh.New()
	if err != nil {
		return err
	}
	if !*force {
		has, err := client.SecretExists(ctx, c.repo, route.AppKeySecret)
		if err != nil {
			return fmt.Errorf("check %s's secrets (this needs admin rights on the repo): %w", c.repo, err)
		}
		if has {
			fmt.Printf("%s already has %s, so the watchdog app is set up. --force creates a new app.\n", c.repo, route.AppKeySecret)
			return writeWatchdog(inCheckout, workflow)
		}
	}
	repo, err := client.Repo(ctx, c.repo)
	if err != nil {
		return err
	}
	s := &ghapp.Setup{
		Target: ghapp.Target{
			Repo: repo.FullName, RepoID: repo.ID, RepoURL: repo.HTMLURL,
			Owner: repo.Owner.Login, OwnerID: repo.Owner.ID, OwnerType: repo.Owner.Type,
		},
		API:     ghapp.API{Base: "https://api.github.com"},
		Web:     "https://github.com",
		Secrets: client,
		Open:    openBrowser,
		Out:     os.Stdout,
	}
	if repo.Owner.Type == "Organization" {
		fmt.Printf("%s belongs to the %s organization, so the app will be owned by it (that needs an org owner or app manager).\n", repo.FullName, repo.Owner.Login)
	}
	if _, err := s.Run(ctx); err != nil {
		return err
	}
	if has, err := client.SecretExists(ctx, c.repo, route.WatchdogSecret); err == nil && has {
		fmt.Printf("%s is still set; the watchdog now uses the app first and the token only if the app fails. Remove it with: gh secret delete %s --repo %s\n",
			route.WatchdogSecret, route.WatchdogSecret, c.repo)
	}
	return writeWatchdog(inCheckout, workflow)
}

// writeWatchdog writes the workflow into the checkout, or says how to get it.
func writeWatchdog(inCheckout bool, workflow string) error {
	root := ""
	if inCheckout {
		if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
			root = strings.TrimSpace(string(out))
		}
	}
	if root == "" {
		fmt.Printf("Add the workflow to the repo's default branch:\n  spareci watchdog > %s\n", watchdogPath)
		return nil
	}
	path := filepath.Join(root, watchdogPath)
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, []byte(workflow)) {
		fmt.Printf("%s is up to date. It works once it's on the default branch.\n", watchdogPath)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(workflow), 0o644); err != nil {
		return err
	}
	fmt.Printf("Wrote %s. Commit it to the default branch; scheduled workflows only run from there.\n", watchdogPath)
	return nil
}

func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Start()
}
