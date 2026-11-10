package gh

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// NewWith builds a client against any API base, for tests and GitHub
// Enterprise Server.
func NewWith(token, base string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{token: token, base: base, http: hc, etags: map[string]cached{}}
}

// Repo is the part of a repository spareci estimate and watchdog setup need.
type Repo struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
	Private  bool   `json:"private"`
	Archived bool   `json:"archived"`
	Owner    struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"` // "User" or "Organization"
	} `json:"owner"`
}

func (c *Client) Repo(ctx context.Context, repo string) (Repo, error) {
	var r Repo
	err := c.do(ctx, "GET", "/repos/"+repo, nil, &r)
	return r, err
}

// OrgRepos lists every repo in org that the token can see.
func (c *Client) OrgRepos(ctx context.Context, org string) ([]Repo, error) {
	var all []Repo
	for page := 1; ; page++ {
		var rs []Repo
		if err := c.do(ctx, "GET", fmt.Sprintf("/orgs/%s/repos?type=all&per_page=100&page=%d", url.PathEscape(org), page), nil, &rs); err != nil {
			return nil, err
		}
		all = append(all, rs...)
		if len(rs) < 100 {
			return all, nil
		}
	}
}

// runsCap is how many results GitHub returns for a filtered run listing,
// however many pages are asked for.
const runsCap = 1000

// Runs lists the workflow runs created in [from, to). A window holding more
// than GitHub's 1,000-result cap is split in half until each part fits.
func (c *Client) Runs(ctx context.Context, repo string, from, to time.Time) ([]Run, error) {
	created := url.QueryEscape(from.UTC().Format(time.RFC3339) + ".." + to.Add(-time.Second).UTC().Format(time.RFC3339))
	var all []Run
	for page := 1; ; page++ {
		var resp struct {
			TotalCount   int   `json:"total_count"`
			WorkflowRuns []Run `json:"workflow_runs"`
		}
		path := fmt.Sprintf("/repos/%s/actions/runs?created=%s&per_page=100&page=%d", repo, created, page)
		if err := c.do(ctx, "GET", path, nil, &resp); err != nil {
			return nil, err
		}
		if page == 1 && resp.TotalCount > runsCap && to.Sub(from) > 2*time.Second {
			mid := from.Add(to.Sub(from) / 2).Truncate(time.Second)
			a, err := c.Runs(ctx, repo, from, mid)
			if err != nil {
				return nil, err
			}
			b, err := c.Runs(ctx, repo, mid, to)
			if err != nil {
				return nil, err
			}
			return append(a, b...), nil
		}
		all = append(all, resp.WorkflowRuns...)
		if len(resp.WorkflowRuns) < 100 || len(all) >= resp.TotalCount || len(all) >= runsCap {
			return all, nil
		}
	}
}

// RunJob is a job as the run history reports it, every attempt included.
type RunJob struct {
	ID              int64      `json:"id"`
	RunID           int64      `json:"run_id"`
	RunAttempt      int        `json:"run_attempt"`
	Name            string     `json:"name"`
	Status          string     `json:"status"`
	Conclusion      string     `json:"conclusion"`
	StartedAt       time.Time  `json:"started_at"`
	CompletedAt     *time.Time `json:"completed_at"`
	Labels          []string   `json:"labels"`
	RunnerName      string     `json:"runner_name"`
	RunnerGroupID   *int64     `json:"runner_group_id"`
	RunnerGroupName string     `json:"runner_group_name"`
}

// AllRunJobs lists every job of every attempt of a run; RunJobs only sees
// the latest attempt.
func (c *Client) AllRunJobs(ctx context.Context, repo string, runID int64) ([]RunJob, error) {
	var all []RunJob
	for page := 1; ; page++ {
		var resp struct {
			TotalCount int      `json:"total_count"`
			Jobs       []RunJob `json:"jobs"`
		}
		path := fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=all&per_page=100&page=%d", repo, runID, page)
		if err := c.do(ctx, "GET", path, nil, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Jobs...)
		if len(resp.Jobs) < 100 || len(all) >= resp.TotalCount {
			return all, nil
		}
	}
}

// CoreRemaining is how many REST calls are left this hour. Reading it is free.
func (c *Client) CoreRemaining(ctx context.Context) (remaining int, reset time.Time, err error) {
	var resp struct {
		Resources struct {
			Core struct {
				Remaining int   `json:"remaining"`
				Reset     int64 `json:"reset"`
			} `json:"core"`
		} `json:"resources"`
	}
	if err := c.do(ctx, "GET", "/rate_limit", nil, &resp); err != nil {
		return 0, time.Time{}, err
	}
	return resp.Resources.Core.Remaining, time.Unix(resp.Resources.Core.Reset, 0), nil
}
