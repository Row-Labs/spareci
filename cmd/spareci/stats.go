package main

import (
	"encoding/json"
	"flag"
	"os"
	"time"

	"github.com/Row-Labs/spareci/internal/gh"
	"github.com/Row-Labs/spareci/internal/runner"
	"github.com/Row-Labs/spareci/internal/stats"
)

// recorder writes each job `up` runs to the stats file. Nothing is recorded
// in a dry run.
func recorder(dry bool, client *gh.Client, c *common, root string) *stats.Recorder {
	if dry {
		return nil
	}
	return &stats.Recorder{API: client, Repo: c.repo, Var: c.vname, Path: stats.File(root)}
}

// statsCmd adds up the jobs this machine has run, at the price of the
// GitHub-hosted runner each would otherwise have used. It reads only the
// local stats file.
func statsCmd(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)
	recs, skipped, err := stats.Load(stats.File(runner.DefaultRoot()))
	if err != nil {
		return err
	}
	rep := stats.Build(recs, skipped, time.Now())
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	rep.Text(os.Stdout)
	return nil
}
