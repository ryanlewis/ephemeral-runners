package github

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBadRepoEveryMethod(t *testing.T) {
	_, srv := newFake(t, time.Now())
	c := newClient(t, srv, nil)
	ctx := context.Background()
	const bad = "no-slash"
	calls := map[string]func() error{
		"GenerateJITConfig": func() error { _, err := c.GenerateJITConfig(ctx, bad, "n", nil); return err },
		"GetRunner":         func() error { _, err := c.GetRunner(ctx, bad, 1); return err },
		"ListRunners":       func() error { _, err := c.ListRunners(ctx, bad); return err },
		"DeleteRunner":      func() error { return c.DeleteRunner(ctx, bad, 1) },
	}
	for name, call := range calls {
		if err := call(); err == nil || !strings.Contains(err.Error(), "not in owner/name form") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if c.Stats().Requests != 0 {
		t.Errorf("a bad repo still made %d requests", c.Stats().Requests)
	}
}

func TestGenerateJITConfigIncompleteResponse(t *testing.T) {
	for name, body := range map[string]string{
		"no runner ID":   `{"runner":{"name":"a"},"encoded_jit_config":"abc"}`,
		"no config":      `{"runner":{"id":5,"name":"a"}}`,
		"empty object":   `{}`,
		"empty response": ``,
	} {
		t.Run(name, func(t *testing.T) {
			f, srv := newFake(t, time.Now())
			f.HandleFunc("POST /repos/example-org/example-repo/actions/runners/generate-jitconfig", func(w http.ResponseWriter, r *http.Request) {
				writeBody(t, w, body)
			})
			cfg, err := newClient(t, srv, nil).GenerateJITConfig(context.Background(), "example-org/example-repo", "a", nil)
			if err == nil || cfg != nil || !strings.Contains(err.Error(), "incomplete response") {
				t.Errorf("cfg = %v, err = %v", cfg, err)
			}
		})
	}
}

func TestRunnerErrorsPassThrough(t *testing.T) {
	f, srv := newFake(t, time.Now())
	f.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"message":"gone"}`, http.StatusNotFound) })
	c := newClient(t, srv, nil)
	ctx := context.Background()
	if _, err := c.GetRunner(ctx, "example-org/example-repo", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetRunner: err = %v", err)
	}
	if _, err := c.ListRunners(ctx, "example-org/example-repo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ListRunners: err = %v", err)
	}
	if err := c.DeleteRunner(ctx, "example-org/example-repo", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteRunner: err = %v", err)
	}
	if _, err := c.GenerateJITConfig(ctx, "example-org/example-repo", "a", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("GenerateJITConfig: err = %v", err)
	}
	if rl, err := c.GetRateLimit(ctx); !errors.Is(err, ErrNotFound) || rl != (RateLimit{}) {
		t.Errorf("GetRateLimit: %+v, %v", rl, err)
	}
}

func TestGetRateLimit(t *testing.T) {
	f, srv := newFake(t, time.Now())
	f.HandleFunc("GET /rate_limit", func(w http.ResponseWriter, r *http.Request) {
		writeBody(t, w, `{"resources":{"core":{"limit":5000,"remaining":4000,"used":1000,"reset":1800003600},"search":{"limit":30}}}`)
	})
	rl, err := newClient(t, srv, nil).GetRateLimit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rl.Limit != 5000 || rl.Remaining != 4000 || rl.Used != 1000 || rl.Reset.Unix() != 1800003600 || rl.Resource != "core" {
		t.Errorf("rate limit = %+v", rl)
	}
}

func TestQueuedJobsErrors(t *testing.T) {
	const runs = "GET /repos/example-org/example-repo/actions/runs"
	tests := []struct {
		name  string
		setup func(t *testing.T, f *fakeGitHub)
		want  error
	}{
		{"listing runs fails", func(t *testing.T, f *fakeGitHub) {
			f.HandleFunc(runs, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
		}, ErrServer},
		{"second status fails after the first succeeds", func(t *testing.T, f *fakeGitHub) {
			f.HandleFunc(runs, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("status") == "in_progress" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				writeBody(t, w, `{"workflow_runs":[]}`)
			})
		}, ErrForbidden},
		{"listing a run's jobs fails with something other than 404", func(t *testing.T, f *fakeGitHub) {
			f.HandleFunc(runs, func(w http.ResponseWriter, r *http.Request) { writeBody(t, w, `{"workflow_runs":[{"id":100}]}`) })
			f.HandleFunc(runs+"/100/jobs", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) })
		}, ErrServer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, srv := newFake(t, time.Now())
			tt.setup(t, f)
			jobs, err := newClient(t, srv, nil).QueuedJobs(context.Background(), "example-org/example-repo")
			if !errors.Is(err, tt.want) || jobs != nil {
				t.Errorf("jobs = %v, err = %v", jobs, err)
			}
		})
	}
}

// With nothing queued the result is nil and not an error.
func TestQueuedJobsNone(t *testing.T) {
	f, srv := newFake(t, time.Now())
	f.HandleFunc("GET /repos/example-org/example-repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		writeBody(t, w, `{"workflow_runs":[]}`)
	})
	jobs, err := newClient(t, srv, nil).QueuedJobs(context.Background(), "example-org/example-repo")
	if err != nil || jobs != nil {
		t.Errorf("jobs = %v, err = %v", jobs, err)
	}
}
