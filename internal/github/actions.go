package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Job is a workflow job waiting for a runner.
type Job struct {
	ID        int64
	RunID     int64
	Name      string
	Labels    []string
	CreatedAt time.Time
}

// JITConfig is a single-use runner registration.
type JITConfig struct {
	RunnerID int64
	Name     string
	// EncodedConfig is the value for run.sh --jitconfig. It is a credential.
	EncodedConfig string
}

// Runner is a self-hosted runner registered to a repository.
type Runner struct {
	ID     int64
	Name   string
	OS     string
	Status string // "online" or "offline"
	Busy   bool
	Labels []string
}

// repoPath returns /repos/{owner}/{name} for a repo in owner/name form.
func repoPath(repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("repo %q is not in owner/name form", repo)
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

// QueuedJobs returns the jobs in repo that are waiting for a runner, oldest
// first. It looks at the runs that are queued or in progress, since an
// in-progress run can still have queued jobs.
func (c *Client) QueuedJobs(ctx context.Context, repo string) ([]Job, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	type runsPage struct {
		WorkflowRuns []struct {
			ID int64 `json:"id"`
		} `json:"workflow_runs"`
	}
	var runIDs []int64
	seen := map[int64]bool{}
	for _, status := range []string{"queued", "in_progress"} {
		err := getAll(ctx, c, rp+"/actions/runs?status="+status+"&per_page=100", func(p runsPage) {
			for _, r := range p.WorkflowRuns {
				if !seen[r.ID] {
					seen[r.ID] = true
					runIDs = append(runIDs, r.ID)
				}
			}
		})
		if err != nil {
			return nil, err
		}
	}

	type jobsPage struct {
		Jobs []struct {
			ID        int64     `json:"id"`
			RunID     int64     `json:"run_id"`
			Name      string    `json:"name"`
			Status    string    `json:"status"`
			Labels    []string  `json:"labels"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"jobs"`
	}
	var jobs []Job
	for _, id := range runIDs {
		err := getAll(ctx, c, fmt.Sprintf("%s/actions/runs/%d/jobs?per_page=100", rp, id), func(p jobsPage) {
			for _, j := range p.Jobs {
				if j.Status == "queued" {
					jobs = append(jobs, Job{ID: j.ID, RunID: j.RunID, Name: j.Name, Labels: j.Labels, CreatedAt: j.CreatedAt})
				}
			}
		})
		// A run deleted since it was listed has no jobs to serve.
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(jobs, func(a, b int) bool { return jobs[a].CreatedAt.Before(jobs[b].CreatedAt) })
	return jobs, nil
}

// GenerateJITConfig registers a single-use runner in repo, in the default
// runner group, and returns its config.
func (c *Client) GenerateJITConfig(ctx context.Context, repo, name string, labels []string) (*JITConfig, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"name":            name,
		"runner_group_id": 1,
		"labels":          labels,
		"work_folder":     "_work",
	}
	var out struct {
		Runner struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	if _, err := c.do(ctx, http.MethodPost, rp+"/actions/runners/generate-jitconfig", body, &out); err != nil {
		return nil, err
	}
	if out.Runner.ID == 0 || out.EncodedJITConfig == "" {
		return nil, fmt.Errorf("generate JIT config for %s: incomplete response", repo)
	}
	return &JITConfig{RunnerID: out.Runner.ID, Name: out.Runner.Name, EncodedConfig: out.EncodedJITConfig}, nil
}

type apiRunner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	OS     string `json:"os"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (r apiRunner) runner() Runner {
	out := Runner{ID: r.ID, Name: r.Name, OS: r.OS, Status: r.Status, Busy: r.Busy}
	for _, l := range r.Labels {
		out.Labels = append(out.Labels, l.Name)
	}
	return out
}

// GetRunner returns one runner. A runner that does not exist gives an error
// that matches ErrNotFound.
func (c *Client) GetRunner(ctx context.Context, repo string, id int64) (*Runner, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var r apiRunner
	if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/actions/runners/%d", rp, id), nil, &r); err != nil {
		return nil, err
	}
	out := r.runner()
	return &out, nil
}

// ListRunners returns every self-hosted runner registered to repo.
func (c *Client) ListRunners(ctx context.Context, repo string) ([]Runner, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	type page struct {
		Runners []apiRunner `json:"runners"`
	}
	var out []Runner
	err = getAll(ctx, c, rp+"/actions/runners?per_page=100", func(p page) {
		for _, r := range p.Runners {
			out = append(out, r.runner())
		}
	})
	return out, err
}

// DeleteRunner removes a runner from repo.
func (c *Client) DeleteRunner(ctx context.Context, repo string, id int64) error {
	rp, err := repoPath(repo)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/actions/runners/%d", rp, id), nil, nil)
	return err
}

// GetRateLimit asks GitHub for the installation's core rate limit. The call
// itself is not counted against the limit.
func (c *Client) GetRateLimit(ctx context.Context) (RateLimit, error) {
	var out struct {
		Resources struct {
			Core struct {
				Limit     int   `json:"limit"`
				Remaining int   `json:"remaining"`
				Used      int   `json:"used"`
				Reset     int64 `json:"reset"`
			} `json:"core"`
		} `json:"resources"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/rate_limit", nil, &out); err != nil {
		return RateLimit{}, err
	}
	core := out.Resources.Core
	return RateLimit{Limit: core.Limit, Remaining: core.Remaining, Used: core.Used, Reset: time.Unix(core.Reset, 0), Resource: "core"}, nil
}
