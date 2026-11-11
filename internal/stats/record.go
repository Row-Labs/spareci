// Package stats records every job spareci runs and adds up what those jobs
// would have cost on the GitHub-hosted runner they'd otherwise have run on.
package stats

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Record is one job this machine ran. It's a line of jobs.jsonl.
type Record struct {
	Repo   string `json:"repo"`
	Public bool   `json:"public,omitempty"` // standard runners are free in public repos
	Runner string `json:"runner"`           // the spareci runner that took it
	// Backend ran it: macos-arm64 natively, linux-arm64 in a container, ...
	// Empty in records from spareci 0.1, which ran every job natively.
	Backend string `json:"backend,omitempty"`

	RunID        int64  `json:"run_id,omitempty"`
	RunAttempt   int    `json:"run_attempt,omitempty"`
	JobID        int64  `json:"job_id,omitempty"`
	Workflow     string `json:"workflow,omitempty"`      // workflow name, e.g. CI
	WorkflowPath string `json:"workflow_path,omitempty"` // .github/workflows/ci.yml
	HeadSHA      string `json:"head_sha,omitempty"`
	Job          string `json:"job"` // as GitHub shows it: build, test (ubuntu, 22)
	URL          string `json:"url,omitempty"`

	// Started and Ended are GitHub's timestamps for the job, which it bills
	// by, or this machine's when GitHub's couldn't be read.
	Started    time.Time `json:"started"`
	Ended      time.Time `json:"ended"`
	Conclusion string    `json:"conclusion,omitempty"`

	// Fallback is the runs-on the job would have had without spareci, from
	// the workflow file at the run's commit. FallbackError says why it's
	// missing; such jobs aren't priced.
	Fallback      []string `json:"fallback,omitempty"`
	FallbackError string   `json:"fallback_error,omitempty"`
}

// File is where records go, under the spareci cache dir.
func File(root string) string { return filepath.Join(root, "stats", "jobs.jsonl") }

// Append adds r to the file at path. One write per line with O_APPEND, so
// several spareci processes (one per repo) can share the file.
func Append(path string, r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Load reads every record. A missing file is no records. Lines that don't
// parse (a write cut short by a crash) are skipped and counted.
func Load(path string) (recs []Record, bad int, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil || r.Ended.IsZero() {
			bad++
			continue
		}
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		return recs, bad, fmt.Errorf("read %s: %w", path, err)
	}
	return recs, bad, nil
}
