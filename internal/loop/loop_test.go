package loop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ryanlewis/ephemeral-runners/internal/config"
	"github.com/ryanlewis/ephemeral-runners/internal/github"
	"github.com/ryanlewis/ephemeral-runners/internal/vm"
)

const repo = "example-org/example-repo"

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

type fakeGitHub struct {
	jobs       map[string][]github.Job // by repo
	runners    map[string][]github.Runner
	nextID     int64
	jobsErr    error
	jitErr     error
	deleteErr  error
	rate       github.RateLimit
	statuses   [][]string // run statuses asked for, per QueuedJobs call
	deleted    []int64
	registered []string
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{jobs: map[string][]github.Job{}, runners: map[string][]github.Runner{}, nextID: 100}
}

func (f *fakeGitHub) QueuedJobs(_ context.Context, repo string, statuses ...string) ([]github.Job, error) {
	f.statuses = append(f.statuses, statuses)
	if f.jobsErr != nil {
		return nil, f.jobsErr
	}
	return slices.Clone(f.jobs[repo]), nil
}

func (f *fakeGitHub) GenerateJITConfig(_ context.Context, repo, name string, labels []string) (*github.JITConfig, error) {
	if f.jitErr != nil {
		return nil, f.jitErr
	}
	f.nextID++
	f.runners[repo] = append(f.runners[repo], github.Runner{ID: f.nextID, Name: name, Status: "offline", Labels: labels})
	f.registered = append(f.registered, name)
	return &github.JITConfig{RunnerID: f.nextID, Name: name, EncodedConfig: "jit-" + name}, nil
}

func (f *fakeGitHub) ListRunners(_ context.Context, repo string) ([]github.Runner, error) {
	return slices.Clone(f.runners[repo]), nil
}

func (f *fakeGitHub) DeleteRunner(_ context.Context, repo string, id int64) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	i := slices.IndexFunc(f.runners[repo], func(r github.Runner) bool { return r.ID == id })
	if i < 0 {
		return &github.APIError{StatusCode: 404}
	}
	f.runners[repo] = slices.Delete(f.runners[repo], i, i+1)
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeGitHub) RateLimit() github.RateLimit { return f.rate }

// setRunner changes a registered runner's status.
func (f *fakeGitHub) setRunner(name, status string, busy bool) {
	for r, rs := range f.runners {
		for i := range rs {
			if rs[i].Name == name {
				f.runners[r][i].Status, f.runners[r][i].Busy = status, busy
				return
			}
		}
	}
	panic("no runner " + name)
}

type fakeVMs struct {
	running    map[string]vm.Spec
	created    []vm.Spec
	attached   map[string]string // VM name -> jit config
	destroyed  []string
	removed    []string
	pruned     int
	createErr  error
	destroyErr error
}

func newFakeVMs() *fakeVMs {
	return &fakeVMs{running: map[string]vm.Spec{}, attached: map[string]string{}}
}

func (f *fakeVMs) Create(_ context.Context, s vm.Spec) error {
	if f.createErr != nil {
		return f.createErr
	}
	if _, ok := f.running[s.Name]; ok {
		return fmt.Errorf("%s exists", s.Name)
	}
	f.running[s.Name] = s
	f.created = append(f.created, s)
	return nil
}

func (f *fakeVMs) Attach(_ context.Context, _, name, jit string) error {
	if _, ok := f.running[name]; !ok {
		return fmt.Errorf("%s not running", name)
	}
	f.attached[name] = jit
	return nil
}

func (f *fakeVMs) List(_ context.Context, prefix string) ([]string, error) {
	var out []string
	for n := range f.running {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (f *fakeVMs) Destroy(_ context.Context, name string) error {
	if f.destroyErr != nil {
		return f.destroyErr
	}
	delete(f.running, name)
	f.destroyed = append(f.destroyed, name)
	return nil
}

func (f *fakeVMs) Remove(_, name string) error {
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeVMs) Prune(context.Context, string, string) ([]string, error) {
	f.pruned++
	return nil, nil
}

// powerOff is a VM shutting itself down after its job.
func (f *fakeVMs) powerOff(name string) { delete(f.running, name) }

func (f *fakeVMs) specsOfSize(vcpus int) []vm.Spec {
	var out []vm.Spec
	for _, s := range f.running {
		if s.VCPUs == vcpus {
			out = append(out, s)
		}
	}
	return out
}

type rig struct {
	t     *testing.T
	cfg   *config.Config
	gh    *fakeGitHub
	vms   *fakeVMs
	clock *fakeClock
	image string
	l     *Loop
}

// newRig makes a loop with fixed sizes and budget, which the tests' budget
// arithmetic relies on, and the given number of spares.
func newRig(t *testing.T, spares int, extra string) *rig {
	t.Helper()
	cfg, err := config.Parse(fmt.Appendf(nil, `{
		"github": {"app_id": 1, "installation_id": 2, "private_key_file": "/dev/null"},
		"repos": [%q],
		"labels": ["self-hosted", "linux", "x64", "example"],
		"name_prefix": "ex",
		"sizes": {
			"small": {"vcpus": 2, "memory_gib": 4},
			"medium": {"vcpus": 4, "memory_gib": 12},
			"large": {"vcpus": 8, "memory_gib": 20}
		},
		"budget": {"vcpus": 24, "memory_gib": 32},
		"spares": {"size": "medium", "count": %d, "idle_after": "2m"}
		%s
	}`, repo, spares, extra))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{
		t: t, cfg: cfg, gh: newFakeGitHub(), vms: newFakeVMs(),
		clock: &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)},
		image: "/images/runner-1.qcow2",
	}
	r.l = New(Options{
		Config: cfg, GitHub: r.gh, VMs: r.vms, Clock: r.clock,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		ResolveImage: func(string) (string, error) { return r.image, nil },
	})
	return r
}

var jobID int64

// queue adds a queued job with these labels, created age ago.
func (r *rig) queue(age time.Duration, labels ...string) github.Job {
	jobID++
	j := github.Job{ID: jobID, RunID: 1, Name: fmt.Sprint("job", jobID), Labels: labels, CreatedAt: r.clock.t.Add(-age)}
	r.gh.jobs[repo] = append(r.gh.jobs[repo], j)
	return j
}

// take marks a queued job as taken: it leaves the queue.
func (r *rig) take(j github.Job) {
	r.gh.jobs[repo] = slices.DeleteFunc(r.gh.jobs[repo], func(x github.Job) bool { return x.ID == j.ID })
}

func (r *rig) tick() time.Duration { return r.l.Tick(context.Background()) }

func TestDemandAndLabels(t *testing.T) {
	r := newRig(t, 0, "")
	r.queue(time.Minute, "self-hosted", "example", "small")
	r.queue(30*time.Second, "self-hosted", "example") // default size: medium
	r.queue(time.Minute, "ubuntu-latest")
	r.queue(time.Minute, "self-hosted", "example", "gpu")
	r.queue(time.Minute, "self-hosted", "example", "small", "large")

	r.tick()
	if len(r.vms.created) != 2 {
		t.Fatalf("created %d VMs, want 2: %+v", len(r.vms.created), r.vms.created)
	}
	small, medium := r.vms.created[0], r.vms.created[1]
	if small.VCPUs != 2 || small.MemoryMiB != 4096 || medium.VCPUs != 4 || medium.MemoryMiB != 12*1024 {
		t.Errorf("sizes: %+v %+v", small, medium)
	}
	if !strings.HasPrefix(small.Name, "ex-example-repo-small-") || len(small.Name) != len("ex-example-repo-small-")+4 {
		t.Errorf("name %q", small.Name)
	}
	if small.JITConfig != "jit-"+small.Name || small.Image != r.image {
		t.Errorf("spec %+v", small)
	}
	runner := r.gh.runners[repo][0]
	if want := []string{"self-hosted", "linux", "x64", "example", "small"}; !slices.Equal(runner.Labels, want) {
		t.Errorf("runner labels %q, want %q", runner.Labels, want)
	}

	// The runners are starting, so the same jobs need nothing more.
	r.clock.advance(5 * time.Second)
	r.tick()
	if len(r.vms.created) != 2 {
		t.Errorf("created %d VMs after a second tick, want 2", len(r.vms.created))
	}

	// One more small job needs one more VM.
	r.queue(0, "self-hosted", "small")
	r.tick()
	if len(r.vms.created) != 3 || r.vms.created[2].VCPUs != 2 {
		t.Errorf("created %+v", r.vms.created)
	}
}

func TestBudgetMixedSizes(t *testing.T) {
	r := newRig(t, 0, "")
	// Budget 24 vCPU, 32 GiB. large is 8/20, small 2/4, medium 4/12.
	r.queue(4*time.Minute, "self-hosted", "large")
	r.queue(3*time.Minute, "self-hosted", "large")
	r.queue(2*time.Minute, "self-hosted", "small")
	r.queue(time.Minute, "self-hosted", "medium")

	r.tick()
	// The second large job doesn't fit beside the first, and younger jobs
	// wait behind it.
	if len(r.vms.created) != 1 || r.vms.created[0].VCPUs != 8 {
		t.Fatalf("created %+v", r.vms.created)
	}

	// The first large VM finishes its job and powers off.
	first := r.vms.created[0].Name
	r.take(r.gh.jobs[repo][0])
	r.gh.setRunner(first, "online", true)
	r.clock.advance(5 * time.Second)
	r.tick()
	r.vms.powerOff(first)
	r.clock.advance(5 * time.Second)
	r.tick()
	// large (20 GiB) and small (4 GiB) fit; medium (12 GiB) would make 36.
	if got := len(r.vms.running); got != 2 {
		t.Fatalf("%d running, want 2", got)
	}
	if len(r.vms.specsOfSize(8)) != 1 || len(r.vms.specsOfSize(2)) != 1 {
		t.Errorf("running %+v", r.vms.running)
	}
}

func TestSpareHandover(t *testing.T) {
	r := newRig(t, 2, "")
	r.tick()
	if len(r.vms.created) != 0 {
		t.Fatalf("spares started before any job: %+v", r.vms.created)
	}

	j := r.queue(0, "self-hosted", "small")
	r.tick()
	// One small cold start and two spares: 2+8 vCPU, 4+24 GiB fits.
	if len(r.vms.specsOfSize(2)) != 1 || len(r.l.spares()) != 2 {
		t.Fatalf("running %+v", r.vms.running)
	}
	for _, s := range r.l.spares() {
		if !strings.HasPrefix(s.name, "ex-spare-") || r.vms.running[s.name].JITConfig != "" {
			t.Errorf("spare %s spec %+v", s.name, r.vms.running[s.name])
		}
	}
	r.take(j)

	// A medium job goes to the oldest spare.
	spare := r.l.spares()[0].name
	r.clock.advance(5 * time.Second)
	r.queue(0, "self-hosted", "medium")
	created := len(r.vms.created)
	r.tick()
	jit, ok := r.vms.attached[spare]
	if !ok {
		t.Fatalf("nothing attached to %s; attached %v", spare, r.vms.attached)
	}
	m := r.l.machines[spare]
	if m.state != starting || !strings.HasPrefix(m.runner, "ex-example-repo-medium-") || jit != "jit-"+m.runner {
		t.Errorf("spare after handover: %+v, jit %q", m, jit)
	}
	// The used spare is replaced if the budget allows: small 2/4 + medium
	// 4/12 + two spares 8/24 is 40 GiB, so only one spare is left.
	if len(r.vms.created) != created {
		t.Errorf("started %d VMs, want none", len(r.vms.created)-created)
	}
	if n := len(r.l.spares()); n != 1 {
		t.Errorf("%d spares, want 1", n)
	}
}

func TestSparesDestroyedToMakeRoom(t *testing.T) {
	r := newRig(t, 2, "")
	j := r.queue(0, "self-hosted", "small")
	r.tick()
	r.take(j)
	r.vms.powerOff(r.vms.specsOfSize(2)[0].Name)
	r.clock.advance(5 * time.Second)
	r.tick()
	spares := r.l.spares()
	if len(spares) != 2 || len(r.l.machines) != 2 {
		t.Fatalf("spares %d, machines %d", len(spares), len(r.l.machines))
	}
	// large needs 20 GiB: two spares (24) and 20 is too much, one (12) and
	// 20 fits, so one spare goes.
	r.clock.advance(5 * time.Second)
	r.queue(0, "self-hosted", "large")
	r.tick()
	if len(r.vms.destroyed) != 1 || !slices.Contains([]string{spares[0].name, spares[1].name}, r.vms.destroyed[0]) {
		t.Errorf("destroyed %q, want one spare", r.vms.destroyed)
	}
	if len(r.vms.specsOfSize(8)) != 1 {
		t.Errorf("no large VM: %+v", r.vms.running)
	}
}

func TestSparesGoAfterIdle(t *testing.T) {
	r := newRig(t, 2, "")
	j := r.queue(0, "self-hosted", "small")
	r.tick()
	r.take(j)
	if len(r.l.spares()) != 2 {
		t.Fatal("no spares")
	}
	r.clock.advance(119 * time.Second)
	r.tick()
	if len(r.l.spares()) != 2 {
		t.Fatal("spares went before idle_after")
	}
	r.clock.advance(2 * time.Second)
	r.tick()
	if n := len(r.l.spares()); n != 0 {
		t.Errorf("%d spares after idle_after", n)
	}
	r.tick()
	for name := range r.vms.running {
		if strings.HasPrefix(name, "ex-spare-") {
			t.Errorf("spare %s still running", name)
		}
	}
	if len(r.l.machines) != 1 {
		t.Errorf("machines %v, want only the small VM", r.l.sorted())
	}
}

func TestSparesReplacedAfterImageRebuild(t *testing.T) {
	r := newRig(t, 1, "")
	r.queue(0, "self-hosted", "small")
	r.tick()
	old := r.l.spares()[0].name
	r.image = "/images/runner-2.qcow2"
	r.clock.advance(5 * time.Second)
	r.tick()
	if !slices.Contains(r.vms.destroyed, old) {
		t.Errorf("old spare %s not destroyed", old)
	}
	s := r.l.spares()
	if len(s) != 1 || s[0].name == old || r.vms.running[s[0].name].Image != r.image {
		t.Errorf("spares %+v", s)
	}
	// The small VM, which has a job, is left alone.
	if len(r.vms.specsOfSize(2)) != 1 {
		t.Error("small VM destroyed")
	}
}

func TestTimeouts(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(r *rig, vm string)
		wait    time.Duration
	}{
		{"not online", func(r *rig, vm string) {}, 3*time.Minute + time.Second},
		{"online without a job", func(r *rig, vm string) {
			r.gh.setRunner(vm, "online", false)
			r.tick()
		}, 10*time.Minute + time.Second},
		{"past the job limit", func(r *rig, vm string) {
			r.gh.setRunner(vm, "online", true)
			r.tick()
		}, 6*time.Hour + time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, 0, "")
			j := r.queue(0, "self-hosted", "small")
			r.tick()
			r.take(j)
			name := r.vms.created[0].Name
			id := r.l.machines[name].runnerID
			tt.prepare(r, name)

			r.clock.advance(tt.wait - time.Second - time.Nanosecond)
			r.tick()
			if len(r.vms.destroyed) != 0 {
				t.Fatalf("destroyed early: %q", r.vms.destroyed)
			}
			r.clock.advance(2 * time.Second)
			r.tick()
			if !slices.Equal(r.vms.destroyed, []string{name}) {
				t.Errorf("destroyed %q, want %s", r.vms.destroyed, name)
			}
			if !slices.Contains(r.gh.deleted, id) {
				t.Errorf("runner %d not deleted", id)
			}
			r.tick()
			if _, ok := r.l.machines[name]; ok || !slices.Contains(r.vms.removed, name) {
				t.Errorf("not reaped: removed %q", r.vms.removed)
			}
		})
	}
}

func TestVMVanishes(t *testing.T) {
	r := newRig(t, 0, "")
	r.queue(0, "self-hosted", "small")
	r.tick()
	name := r.vms.created[0].Name
	id := r.l.machines[name].runnerID
	r.vms.powerOff(name)
	r.clock.advance(5 * time.Second)
	r.tick()
	if !slices.Equal(r.vms.removed, []string{name}) {
		t.Errorf("removed %q", r.vms.removed)
	}
	if !slices.Contains(r.gh.deleted, id) {
		t.Errorf("runner %d not deleted", id)
	}
	if _, ok := r.l.machines[name]; ok {
		t.Error("machine still tracked")
	}
}

// A runner whose delete fails is retried on later ticks.
func TestRunnerDeleteRetried(t *testing.T) {
	r := newRig(t, 0, "")
	j := r.queue(0, "self-hosted", "small")
	r.tick()
	r.take(j)
	name := r.vms.created[0].Name
	r.vms.powerOff(name)
	r.gh.deleteErr = errors.New("connection reset")
	r.tick()
	if len(r.gh.deleted) != 0 || len(r.l.pendingDeletes) != 1 {
		t.Fatalf("deleted %v, pending %v", r.gh.deleted, r.l.pendingDeletes)
	}
	r.gh.deleteErr = nil
	r.tick()
	if len(r.gh.deleted) != 1 || len(r.l.pendingDeletes) != 0 {
		t.Errorf("deleted %v, pending %v", r.gh.deleted, r.l.pendingDeletes)
	}
}

func TestStartupCleansUp(t *testing.T) {
	r := newRig(t, 0, "")
	r.vms.running["ex-spare-0001"] = vm.Spec{}
	r.vms.running["ex-example-repo-small-0002"] = vm.Spec{}
	r.vms.running["other-vm"] = vm.Spec{}
	r.gh.runners[repo] = []github.Runner{
		{ID: 1, Name: "ex-example-repo-small-0002", Status: "offline"},
		{ID: 2, Name: "ex-example-repo-large-0003", Status: "online", Busy: true},
		{ID: 3, Name: "ex-example-repo-medium-0004", Status: "online"},
		{ID: 4, Name: "someone-else", Status: "offline"},
	}
	if err := r.l.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ex-example-repo-small-0002", "ex-spare-0001"}; !slices.Equal(r.vms.destroyed, want) {
		t.Errorf("destroyed %q, want %q", r.vms.destroyed, want)
	}
	if r.vms.pruned != 1 {
		t.Errorf("pruned %d times", r.vms.pruned)
	}
	if want := []int64{1, 3}; !slices.Equal(r.gh.deleted, want) {
		t.Errorf("deleted runners %v, want %v", r.gh.deleted, want)
	}
}

func TestShutdown(t *testing.T) {
	r := newRig(t, 1, "")
	r.queue(0, "self-hosted", "small")
	r.tick()
	small := r.vms.specsOfSize(2)[0].Name
	id := r.l.machines[small].runnerID
	r.l.Shutdown()
	if len(r.vms.running) != 0 {
		t.Errorf("still running: %v", r.vms.running)
	}
	if !slices.Equal(r.gh.deleted, []int64{id}) {
		t.Errorf("deleted %v, want %d", r.gh.deleted, id)
	}
	if r.vms.pruned != 1 || len(r.l.machines) != 0 {
		t.Errorf("pruned %d, machines %v", r.vms.pruned, r.l.machines)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	r := newRig(t, 0, "")
	ctx, cancel := context.WithCancel(context.Background())
	r.vms.running["ex-spare-0001"] = vm.Spec{}
	cancel()
	if err := r.l.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.vms.running) != 0 || r.vms.pruned != 2 {
		t.Errorf("running %v, pruned %d", r.vms.running, r.vms.pruned)
	}
}

func TestRateLimitWait(t *testing.T) {
	r := newRig(t, 0, "")
	r.gh.jobsErr = fmt.Errorf("list runs: %w", &github.APIError{StatusCode: 403, RateLimited: true})
	r.gh.rate = github.RateLimit{Remaining: 0, Reset: r.clock.t.Add(90 * time.Second)}
	if got := r.tick(); got != 90*time.Second {
		t.Errorf("wait %s, want 90s", got)
	}
	// No reset time known: wait a minute.
	r.gh.rate = github.RateLimit{}
	if got := r.tick(); got != time.Minute {
		t.Errorf("wait %s, want 1m", got)
	}
}

func TestBackoff(t *testing.T) {
	r := newRig(t, 0, "")
	r.gh.jobsErr = errors.New("connection refused")
	var got []time.Duration
	for range 6 {
		got = append(got, r.tick())
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	if !slices.Equal(got, want) {
		t.Errorf("waits %v, want %v", got, want)
	}
	r.gh.jobsErr = nil
	if d := r.tick(); d != 5*time.Second {
		t.Errorf("after success: %s, want the poll interval", d)
	}
	r.gh.jobsErr = errors.New("connection refused")
	if d := r.tick(); d != 5*time.Second {
		t.Errorf("backoff did not reset: %s", d)
	}
}

func TestInProgressPolledLessOften(t *testing.T) {
	r := newRig(t, 0, "")
	for range 7 {
		r.tick()
		r.clock.advance(5 * time.Second)
	}
	var withInProgress int
	for _, s := range r.gh.statuses {
		if slices.Contains(s, "in_progress") {
			withInProgress++
		}
		if !slices.Contains(s, "queued") {
			t.Errorf("tick without queued runs: %v", s)
		}
	}
	// Ticks at 0, 5, ..., 30 s: in-progress runs at 0 and 30.
	if withInProgress != 2 {
		t.Errorf("in-progress runs polled %d times in 7 ticks, want 2", withInProgress)
	}
}

func TestLaunchFailure(t *testing.T) {
	r := newRig(t, 0, "")
	r.queue(0, "self-hosted", "small")
	r.vms.createErr = errors.New("no space")
	r.tick()
	if r.l.launchFailures != 1 || len(r.l.machines) != 0 {
		t.Errorf("failures %d, machines %v", r.l.launchFailures, r.l.machines)
	}
	if len(r.gh.runners[repo]) != 0 {
		t.Errorf("runner left registered: %+v", r.gh.runners[repo])
	}
	r.vms.createErr = nil
	r.tick()
	if len(r.l.machines) != 1 {
		t.Error("no retry on the next tick")
	}
}

func TestMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runners.prom")
	r := newRig(t, 1, fmt.Sprintf(`, "metrics_file": %q`, path))
	r.queue(0, "self-hosted", "small")
	r.tick()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`ephemeral_runners_vms{state="starting",size="small"} 1`,
		`ephemeral_runners_vms{state="spare",size="medium"} 1`,
		`ephemeral_runners_vms{state="busy",size="large"} 0`,
		"ephemeral_runners_spares 1\n",
		"ephemeral_runners_jobs_started_total 1\n",
		"ephemeral_runners_launch_failures_total 0\n",
		fmt.Sprintf("ephemeral_runners_last_poll_success_seconds %d\n", r.clock.t.Unix()),
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("metrics missing %q:\n%s", want, b)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temporary files left: %v", entries)
	}
}

// With spares waiting, a medium job queued behind a large one still gets a
// spare, though the large job's cold start needs room.
func TestSpareKeptForMediumBehindLarge(t *testing.T) {
	r := newRig(t, 2, "")
	j := r.queue(time.Minute, "self-hosted", "small")
	r.tick()
	r.take(j)
	r.vms.powerOff(r.vms.specsOfSize(2)[0].Name)
	r.clock.advance(5 * time.Second)
	r.tick()
	if len(r.l.spares()) != 2 {
		t.Fatalf("%d spares", len(r.l.spares()))
	}
	r.clock.advance(5 * time.Second)
	r.queue(3*time.Second, "self-hosted", "large")
	r.queue(2*time.Second, "self-hosted", "small")
	r.queue(time.Second, "self-hosted", "medium")
	r.tick()
	if len(r.vms.attached) != 1 {
		t.Errorf("medium job not handed to a spare: attached %v", r.vms.attached)
	}
	// medium (12 GiB) on a spare, large (20) cold, which leaves no room for
	// the other spare or the small job yet.
	if len(r.vms.specsOfSize(8)) != 1 || len(r.vms.specsOfSize(2)) != 0 || len(r.l.spares()) != 0 {
		t.Errorf("running %+v, spares %d", r.vms.running, len(r.l.spares()))
	}
}

// GitHub can show a runner busy before its job leaves the queued list. The
// job must not get a second VM in that window.
func TestNoSecondVMWhileQueueLags(t *testing.T) {
	r := newRig(t, 1, "")
	r.queue(0, "self-hosted", "small")
	r.tick()
	small := r.vms.specsOfSize(2)[0].Name
	r.gh.setRunner(small, "online", true)
	r.clock.advance(5 * time.Second)
	r.tick() // the job is still listed as queued
	if n := len(r.vms.specsOfSize(2)); n != 1 {
		t.Errorf("%d small VMs, want 1", n)
	}

	// The same for a job handed to a spare.
	r.queue(0, "self-hosted", "medium")
	r.clock.advance(5 * time.Second)
	r.tick()
	if len(r.vms.attached) != 1 {
		t.Fatalf("attached %v", r.vms.attached)
	}
	var runner string
	for _, m := range r.l.machines {
		if m.state == starting && m.size == "medium" {
			runner = m.runner
		}
	}
	r.gh.setRunner(runner, "online", true)
	r.clock.advance(5 * time.Second)
	r.tick()
	r.clock.advance(5 * time.Second)
	r.tick()
	if len(r.vms.attached) != 1 || len(r.vms.specsOfSize(4)) != 1+len(r.l.spares()) {
		t.Errorf("second launch for the medium job: attached %v, running %v", r.vms.attached, r.vms.running)
	}
	for _, s := range r.vms.created {
		if s.VCPUs == 4 && s.JITConfig != "" {
			t.Errorf("cold medium VM %s for a job a spare has", s.Name)
		}
	}
}

// A job of an in-progress run is listed only every inProgressEvery. In
// between, its runner must not count against another job: if it took that
// job, its own would wait for it to finish.
func TestRunnerForInProgressJobNotCountedAgainstAnother(t *testing.T) {
	r := newRig(t, 0, "")
	x := r.queue(time.Minute, "self-hosted", "small")
	r.tick() // in-progress runs polled: a VM for x
	a := r.vms.created[0].Name
	// Queued runs only: x isn't listed, y in a new run is.
	r.take(x)
	y := r.queue(0, "self-hosted", "small")
	r.clock.advance(5 * time.Second)
	r.tick()
	if len(r.vms.created) != 2 {
		t.Errorf("created %d VMs for x and y, want 2", len(r.vms.created))
	}
	// Even if the runner for x takes y, x has a runner when it is listed.
	r.gh.setRunner(a, "online", true)
	r.take(y)
	r.clock.advance(5 * time.Second)
	r.tick()
	r.gh.jobs[repo] = append(r.gh.jobs[repo], x)
	r.clock.advance(20 * time.Second)
	r.tick()
	if len(r.vms.created) != 2 {
		t.Errorf("created %d VMs, want 2", len(r.vms.created))
	}
}

// Runners take any queued job of their labels. A job whose runner took
// another one gets a VM once GitHub would have stopped listing it.
func TestJobGetsVMAfterItsRunnerTookAnother(t *testing.T) {
	r := newRig(t, 0, "")
	x := r.queue(2*time.Minute, "self-hosted", "small")
	r.queue(time.Minute, "self-hosted", "small") // y
	r.tick()
	a, b := r.l.machines[r.vms.created[0].Name], r.l.machines[r.vms.created[1].Name]
	if a.jobID != x.ID {
		a, b = b, a
	}
	r.gh.setRunner(b.runner, "online", true) // the runner for y takes x
	r.take(x)
	r.clock.advance(5 * time.Second)
	r.tick()
	// At the next poll of in-progress runs, z comes and the runner for x
	// takes it, which leaves y.
	r.clock.advance(25 * time.Second)
	z := r.queue(0, "self-hosted", "small")
	r.tick()
	r.gh.setRunner(a.runner, "online", true)
	r.take(z)
	for range queueLag / (5 * time.Second) {
		r.clock.advance(5 * time.Second)
		r.tick()
	}
	if len(r.vms.created) != 3 {
		t.Errorf("created %d VMs, want a third for y", len(r.vms.created))
	}
}

// A secondary rate limit leaves requests remaining and says how long to
// wait in Retry-After.
func TestSecondaryRateLimitWait(t *testing.T) {
	r := newRig(t, 0, "")
	r.gh.jobsErr = fmt.Errorf("list runs: %w", &github.APIError{StatusCode: 403, RateLimited: true, RetryAfter: 30 * time.Second})
	r.gh.rate = github.RateLimit{Remaining: 4000, Reset: r.clock.t.Add(50 * time.Minute)}
	if got := r.tick(); got != 30*time.Second {
		t.Errorf("wait %s, want 30s", got)
	}
	// Without Retry-After: a minute.
	r.gh.jobsErr = fmt.Errorf("list runs: %w", &github.APIError{StatusCode: 429, RateLimited: true})
	if got := r.tick(); got != time.Minute {
		t.Errorf("wait %s, want 1m", got)
	}
}

// No runner is registered while the image can't be found, such as during
// a rebuild.
func TestNoRunnerWithoutImage(t *testing.T) {
	r := newRig(t, 0, "")
	r.l.resolve = func(string) (string, error) { return "", errors.New("no such file") }
	r.queue(0, "self-hosted", "small")
	r.tick()
	if len(r.gh.registered) != 0 || r.l.launchFailures != 1 {
		t.Errorf("registered %q, launch failures %d", r.gh.registered, r.l.launchFailures)
	}
}

// A spare that won't die keeps its room: the job waits.
func TestNoRoomWhileSpareWontDie(t *testing.T) {
	r := newRig(t, 2, "")
	j := r.queue(0, "self-hosted", "small")
	r.tick()
	r.take(j)
	r.vms.powerOff(r.vms.specsOfSize(2)[0].Name)
	r.clock.advance(5 * time.Second)
	r.tick()
	r.vms.destroyErr = errors.New("libvirt busy")
	r.clock.advance(5 * time.Second)
	r.queue(0, "self-hosted", "large")
	r.tick()
	if len(r.vms.specsOfSize(8)) != 0 {
		t.Errorf("large VM started beside two spares: %+v", r.vms.running)
	}
	r.vms.destroyErr = nil
	r.clock.advance(5 * time.Second)
	r.tick()
	if len(r.vms.specsOfSize(8)) != 1 {
		t.Errorf("no large VM once the spare could go: %+v", r.vms.running)
	}
}
