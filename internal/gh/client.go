// Package gh is the small slice of the GitHub REST API spareci needs: finding
// queued jobs, minting just-in-time runners, and the fallback repo variable.
package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Client struct {
	token string
	base  string
	http  *http.Client

	mu    sync.Mutex
	etags map[string]cached // conditional GETs: a 304 does not count against the rate limit
}

type cached struct {
	etag string
	body []byte
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("github api %d: %s", e.Status, e.Body) }

func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// New uses GITHUB_TOKEN, falling back to the token `gh` is logged in with.
func New() (*Client, error) {
	tok := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	if tok == "" {
		out, err := exec.Command("gh", "auth", "token").Output()
		if err != nil {
			return nil, errors.New("no GITHUB_TOKEN and `gh auth token` failed; run `gh auth login`")
		}
		tok = strings.TrimSpace(string(out))
	}
	return &Client{
		token: tok,
		base:  "https://api.github.com",
		http:  &http.Client{Timeout: 30 * time.Second},
		etags: map[string]cached{},
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "spareci")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	var prev cached
	if method == http.MethodGet {
		c.mu.Lock()
		prev = c.etags[path]
		c.mu.Unlock()
		if prev.etag != "" {
			req.Header.Set("If-None-Match", prev.etag)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusNotModified && prev.body != nil {
		data = prev.body
	} else if resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	} else if method == http.MethodGet {
		if et := resp.Header.Get("ETag"); et != "" {
			c.mu.Lock()
			c.etags[path] = cached{etag: et, body: data}
			c.mu.Unlock()
		}
	}

	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type Job struct {
	ID          int64     `json:"id"`
	RunID       int64     `json:"run_id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Labels      []string  `json:"labels"`
	RunnerName  string    `json:"runner_name"`
	HTMLURL     string    `json:"html_url"`
	RunAttempt  int       `json:"run_attempt"`
	Conclusion  string    `json:"conclusion"`
	CreatedAt   time.Time `json:"created_at"`
	CompletedAt time.Time `json:"completed_at"`
}

// QueuedJobs returns every queued job in the repo. A run whose first job is
// already running reports in_progress, so both run states are scanned.
func (c *Client) QueuedJobs(ctx context.Context, repo string) ([]Job, error) {
	var jobs []Job
	seen := map[int64]bool{}
	for _, status := range []string{"queued", "in_progress"} {
		var runs struct {
			WorkflowRuns []struct {
				ID int64 `json:"id"`
			} `json:"workflow_runs"`
		}
		if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/actions/runs?status=%s&per_page=50", repo, status), nil, &runs); err != nil {
			return nil, err
		}
		for _, r := range runs.WorkflowRuns {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			page, err := c.RunJobs(ctx, repo, r.ID)
			if err != nil {
				return nil, err
			}
			for _, j := range page {
				if j.Status == "queued" {
					jobs = append(jobs, j)
				}
			}
		}
	}
	return jobs, nil
}

// RunJobs is every job in a run's latest attempt.
func (c *Client) RunJobs(ctx context.Context, repo string, runID int64) ([]Job, error) {
	var page struct {
		Jobs []Job `json:"jobs"`
	}
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", repo, runID), nil, &page)
	return page.Jobs, err
}

type Run struct {
	ID           int64     `json:"id"`
	Status       string    `json:"status"`
	RunAttempt   int       `json:"run_attempt"`
	RunStartedAt time.Time `json:"run_started_at"` // of the latest attempt
	HTMLURL      string    `json:"html_url"`
}

func (c *Client) GetRun(ctx context.Context, repo string, runID int64) (Run, error) {
	var r Run
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/actions/runs/%d", repo, runID), nil, &r)
	return r, err
}

// CancelRun cancels every job in a run. GitHub has no per-job cancel.
func (c *Client) CancelRun(ctx context.Context, repo string, runID int64) error {
	return c.do(ctx, "POST", fmt.Sprintf("/repos/%s/actions/runs/%d/cancel", repo, runID), nil, nil)
}

// ForceCancelRun cancels a run that ignores CancelRun, such as one whose job
// is held by a runner that has gone away.
func (c *Client) ForceCancelRun(ctx context.Context, repo string, runID int64) error {
	return c.do(ctx, "POST", fmt.Sprintf("/repos/%s/actions/runs/%d/force-cancel", repo, runID), nil, nil)
}

// RerunFailedJobs starts a new attempt of the run's failed and cancelled jobs
// and their dependents. The new attempt reads repo variables afresh.
func (c *Client) RerunFailedJobs(ctx context.Context, repo string, runID int64) error {
	return c.do(ctx, "POST", fmt.Sprintf("/repos/%s/actions/runs/%d/rerun-failed-jobs", repo, runID), nil, nil)
}

// RerunRun starts a new attempt of every job in the run.
func (c *Client) RerunRun(ctx context.Context, repo string, runID int64) error {
	return c.do(ctx, "POST", fmt.Sprintf("/repos/%s/actions/runs/%d/rerun", repo, runID), nil, nil)
}

type JITRunner struct {
	ID     int64
	Config string // pass to `run.sh --jitconfig`
}

// GenerateJIT registers a single-use runner. It takes exactly one job and is
// removed by GitHub when that job ends.
func (c *Client) GenerateJIT(ctx context.Context, repo, name string, labels []string) (JITRunner, error) {
	var resp struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	err := c.do(ctx, "POST", fmt.Sprintf("/repos/%s/actions/runners/generate-jitconfig", repo), map[string]any{
		"name":            name,
		"runner_group_id": 1,
		"labels":          labels,
		"work_folder":     "_work",
	}, &resp)
	return JITRunner{ID: resp.Runner.ID, Config: resp.EncodedJITConfig}, err
}

func (c *Client) DeleteRunner(ctx context.Context, repo string, id int64) error {
	err := c.do(ctx, "DELETE", fmt.Sprintf("/repos/%s/actions/runners/%d", repo, id), nil, nil)
	if IsStatus(err, 404) {
		return nil
	}
	return err
}

type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

func (c *Client) ListRunners(ctx context.Context, repo string) ([]Runner, error) {
	var resp struct {
		Runners []Runner `json:"runners"`
	}
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/actions/runners?per_page=100", repo), nil, &resp)
	return resp.Runners, err
}

// SetVariable creates or updates a repository Actions variable.
func (c *Client) SetVariable(ctx context.Context, repo, name, value string) error {
	body := map[string]string{"name": name, "value": value}
	err := c.do(ctx, "PATCH", fmt.Sprintf("/repos/%s/actions/variables/%s", repo, name), body, nil)
	if IsStatus(err, 404) {
		err = c.do(ctx, "POST", fmt.Sprintf("/repos/%s/actions/variables", repo), body, nil)
	}
	return err
}

func (c *Client) DeleteVariable(ctx context.Context, repo, name string) error {
	err := c.do(ctx, "DELETE", fmt.Sprintf("/repos/%s/actions/variables/%s", repo, name), nil, nil)
	if IsStatus(err, 404) {
		return nil
	}
	return err
}

// LatestRunnerVersion is the newest actions/runner release, without the "v".
func (c *Client) LatestRunnerVersion(ctx context.Context) (string, error) {
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := c.do(ctx, "GET", "/repos/actions/runner/releases/latest", nil, &rel); err != nil {
		return "", err
	}
	return strings.TrimPrefix(rel.TagName, "v"), nil
}
