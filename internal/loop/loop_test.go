package loop

import (
	"bytes"
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
	queuedErr  map[string]error // QueuedJobs error, by repo
	jitErr     error
	deleteErr  error
	runnersErr map[string]error // ListRunners error, by repo
	jobStatus  map[int64]string // JobStatus answer, by job; by default whether the job is listed
	statusErr  error
	statusAsks []int64 // jobs asked about, per JobStatus call
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
	if err := f.queuedErr[repo]; err != nil {
		return nil, err
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
	if err := f.runnersErr[repo]; err != nil {
		return nil, err
	}
	return slices.Clone(f.runners[repo]), nil
}

func (f *fakeGitHub) JobStatus(_ context.Context, repo string, id int64) (string, error) {
	f.statusAsks = append(f.statusAsks, id)
	if f.statusErr != nil {
		return "", f.statusErr
	}
	if s, ok := f.jobStatus[id]; ok {
		return s, nil
	}
	if slices.ContainsFunc(f.jobs[repo], func(j github.Job) bool { return j.ID == id }) {
		return "queued", nil
	}
	return "completed", nil
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
	leftover   []string // files Prune finds to remove
	createErr  error
	destroyErr error
	stuck      map[string]bool // VMs Destroy fails on
	listErr    error
	pruneErr   error
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
	if f.listErr != nil {
		return nil, f.listErr
	}
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
	if f.stuck[name] {
		return fmt.Errorf("%s will not die", name)
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
	if f.pruneErr != nil {
		return nil, f.pruneErr
	}
	return f.leftover, nil
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

// captureLogs sends the loop's log to the returned buffer.
func (r *rig) captureLogs() *bytes.Buffer {
	var b bytes.Buffer
	r.l.log = slog.New(slog.NewTextHandler(&b, nil))
	return &b
}

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

// Startup stops at an error listing VMs or removing their files, before it
// touches any runner.
func TestStartupStopsOnVMError(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name      string
		fail      func(*fakeVMs)
		want      string
		destroyed []string
		pruned    int
	}{
		{"list", func(f *fakeVMs) { f.listErr = boom }, "list VMs: boom", nil, 0},
		{"prune", func(f *fakeVMs) { f.pruneErr = boom }, "remove leftover VM files: boom",
			[]string{"ex-spare-0001", "ex-spare-0002", "ex-spare-0003"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, 0, "")
			for _, n := range []string{"ex-spare-0001", "ex-spare-0002", "ex-spare-0003"} {
				r.vms.running[n] = vm.Spec{}
			}
			r.gh.runners[repo] = []github.Runner{{ID: 1, Name: "ex-spare-0001", Status: "offline"}}
			tc.fail(r.vms)
			err := r.l.Startup(context.Background())
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			if !errors.Is(err, boom) {
				t.Errorf("%v does not wrap the cause", err)
			}
			if !slices.Equal(r.vms.destroyed, tc.destroyed) {
				t.Errorf("destroyed %q, want %q", r.vms.destroyed, tc.destroyed)
			}
			if r.vms.pruned != tc.pruned {
				t.Errorf("pruned %d times, want %d", r.vms.pruned, tc.pruned)
			}
			if len(r.gh.deleted) != 0 {
				t.Errorf("deleted runners %v", r.gh.deleted)
			}
		})
	}
}

// A leftover VM that won't die doesn't stop Startup. It is kept like a VM
// whose start failed: it holds its room in the budget, is destroyed again
// each tick, and is reaped once gone.
func TestStartupKeepsWedgedVM(t *testing.T) {
	const wedged = "ex-example-repo-large-0002"
	r := newRig(t, 0, "")
	logs := r.captureLogs()
	for _, n := range []string{"ex-spare-0001", wedged, "ex-spare-0003"} {
		r.vms.running[n] = vm.Spec{Name: n}
	}
	r.vms.stuck = map[string]bool{wedged: true}
	r.gh.runners[repo] = []github.Runner{{ID: 1, Name: "ex-spare-0001", Status: "offline"}}
	if err := r.l.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ex-spare-0001", "ex-spare-0003"}; !slices.Equal(r.vms.destroyed, want) {
		t.Errorf("destroyed %q, want %q", r.vms.destroyed, want)
	}
	if r.vms.pruned != 1 || !slices.Equal(r.gh.deleted, []int64{1}) {
		t.Errorf("pruned %d, deleted runners %v", r.vms.pruned, r.gh.deleted)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 || !strings.Contains(logs.String(), "vm="+wedged) {
		t.Errorf("want one warning naming %s:\n%s", wedged, logs)
	}

	// A large job would fit only without it.
	r.queue(0, "self-hosted", "large")
	r.tick()
	if len(r.vms.created) != 0 {
		t.Fatalf("started %+v beside a VM that won't die", r.vms.created)
	}
	r.vms.stuck = nil
	r.clock.advance(5 * time.Second)
	r.tick()
	if !slices.Contains(r.vms.destroyed, wedged) {
		t.Errorf("not destroyed again: destroyed %q", r.vms.destroyed)
	}
	r.clock.advance(5 * time.Second)
	r.tick()
	if _, ok := r.l.machines[wedged]; ok || !slices.Contains(r.vms.removed, wedged) {
		t.Errorf("not reaped: removed %q", r.vms.removed)
	}
	if len(r.vms.created) != 1 {
		t.Errorf("no VM for the job once the room was free: %+v", r.vms.created)
	}
}

// A wedged leftover VM counts in the budget at the size in its name, even
// one with a hyphen. With no size in its name, as a spare's, it counts as
// the most of each that any size has.
func TestWedgedVMRoom(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sizes map[string]config.Size
		want  config.Size
	}{
		{"ex-example-repo-small-0001", nil, config.Size{VCPUs: 2, MemoryGiB: 4}},
		{"ex-example-repo-x-large-0001", map[string]config.Size{"large": {VCPUs: 8, MemoryGiB: 8}, "x-large": {VCPUs: 16, MemoryGiB: 32}},
			config.Size{VCPUs: 16, MemoryGiB: 32}},
		{"ex-spare-0001", map[string]config.Size{"cpu": {VCPUs: 16, MemoryGiB: 8}, "mem": {VCPUs: 2, MemoryGiB: 32}},
			config.Size{VCPUs: 16, MemoryGiB: 32}},
		{"ex-odd", nil, config.Size{VCPUs: 8, MemoryGiB: 20}},
	} {
		r := newRig(t, 0, "")
		if tc.sizes != nil {
			r.l.cfg.Sizes = tc.sizes
		}
		r.vms.running[tc.name] = vm.Spec{}
		r.vms.stuck = map[string]bool{tc.name: true}
		if err := r.l.Startup(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := r.l.used(); got != tc.want {
			t.Errorf("%s: counts as %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// Startup logs the leftover files it removed, and says nothing when there
// were none.
func TestStartupLogsRemovedFiles(t *testing.T) {
	for _, tc := range []struct {
		leftover []string
		want     int
	}{{nil, 0}, {[]string{"ex-spare-0001"}, 1}} {
		r := newRig(t, 0, "")
		logs := r.captureLogs()
		r.vms.leftover = tc.leftover
		if err := r.l.Startup(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(logs.String(), "removed leftover VM files"); got != tc.want {
			t.Errorf("leftover %q: logged %d times, want %d:\n%s", tc.leftover, got, tc.want, logs)
		}
	}
}

// GitHub errors don't stop Startup: a repo it can't list is skipped, and a
// runner it can't delete is left registered. A runner already gone counts
// as deleted.
func TestStartupCarriesOnPastGitHubErrors(t *testing.T) {
	const other = "example-org/other-repo"
	runners := []github.Runner{
		{ID: 1, Name: "ex-example-repo-small-0001", Status: "offline"},
		{ID: 2, Name: "ex-example-repo-small-0002", Status: "online"},
	}
	for _, tc := range []struct {
		name               string
		err                error
		deleted, notDelete int
	}{
		{"failed", errors.New("connection reset"), 0, 2},
		{"gone", &github.APIError{StatusCode: 404}, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, 0, "")
			logs := r.captureLogs()
			// The repo that fails comes first, so Startup must go past it.
			r.l.cfg.Repos = []string{other, repo}
			r.gh.runnersErr = map[string]error{other: errors.New("bad gateway")}
			r.gh.runners[repo] = slices.Clone(runners)
			r.gh.deleteErr = tc.err
			if err := r.l.Startup(context.Background()); err != nil {
				t.Fatal(err)
			}
			out := logs.String()
			if !strings.Contains(out, `msg="list runners for clean-up" repo=`+other+` err="bad gateway"`) {
				t.Errorf("no warning for %s:\n%s", other, out)
			}
			if got := strings.Count(out, `msg="deleted leftover runner"`); got != tc.deleted {
				t.Errorf("logged %d deletes, want %d:\n%s", got, tc.deleted, out)
			}
			if got := strings.Count(out, `msg="delete leftover runner"`); got != tc.notDelete {
				t.Errorf("logged %d failed deletes, want %d:\n%s", got, tc.notDelete, out)
			}
		})
	}
}

// Shutdown destroys the VMs it tracks even when it can't list VMs, in name
// order, and deletes their runners.
func TestShutdownWithoutVMList(t *testing.T) {
	r := newRig(t, 0, "")
	logs := r.captureLogs()
	r.queue(0, "self-hosted", "large")
	r.queue(0, "self-hosted", "small")
	r.tick()
	r.vms.running["ex-untracked-0009"] = vm.Spec{}
	var names []string
	var ids []int64
	for n, m := range r.l.machines {
		names = append(names, n)
		ids = append(ids, m.runnerID)
	}
	if len(names) != 2 {
		t.Fatalf("tick launched %q, want two VMs", names)
	}
	slices.Sort(names)
	r.vms.listErr = errors.New("libvirt down")
	r.l.Shutdown()
	if !slices.Equal(r.vms.destroyed, names) {
		t.Errorf("destroyed %q, want %q", r.vms.destroyed, names)
	}
	slices.Sort(ids)
	slices.Sort(r.gh.deleted)
	if !slices.Equal(r.gh.deleted, ids) {
		t.Errorf("deleted runners %v, want %v", r.gh.deleted, ids)
	}
	if !strings.Contains(logs.String(), `msg="list VMs on shutdown" err="libvirt down"`) {
		t.Errorf("no warning in log:\n%s", logs)
	}
	if _, ok := r.vms.running["ex-untracked-0009"]; !ok {
		t.Error("destroyed a VM it could not have known about")
	}
	if r.vms.pruned != 1 || len(r.l.machines) != 0 {
		t.Errorf("pruned %d, machines %v", r.vms.pruned, r.l.machines)
	}
}

// Shutdown goes past a VM that won't die and still deletes its runner, so
// the runner can't take a job, retries earlier failed deletes, and forgets
// its machines even if pruning fails.
func TestShutdownCarriesOnPastErrors(t *testing.T) {
	r := newRig(t, 0, "")
	logs := r.captureLogs()
	r.queue(0, "self-hosted", "large")
	r.queue(0, "self-hosted", "small")
	r.tick()
	var stuck, gone *machine
	for _, m := range r.l.machines {
		if m.size == "large" {
			stuck = m
		} else {
			gone = m
		}
	}
	if stuck == nil || gone == nil {
		t.Fatalf("tick launched %v, want one large and one small VM", r.l.machines)
	}
	r.vms.stuck = map[string]bool{stuck.name: true}
	r.gh.runners[repo] = append(r.gh.runners[repo], github.Runner{ID: 50, Name: "ex-old"})
	r.l.pendingDeletes = []runnerRef{{repo, 50}}
	r.vms.pruneErr = errors.New("permission denied")
	r.l.Shutdown()
	if !slices.Equal(r.vms.destroyed, []string{gone.name}) {
		t.Errorf("destroyed %q, want %q", r.vms.destroyed, gone.name)
	}
	slices.Sort(r.gh.deleted)
	want := []int64{50, stuck.runnerID, gone.runnerID} // runner IDs start above 100
	slices.Sort(want)
	if !slices.Equal(r.gh.deleted, want) {
		t.Errorf("deleted runners %v, want %v", r.gh.deleted, want)
	}
	out := logs.String()
	for _, want := range []string{
		`msg="destroy VM on shutdown" vm=` + stuck.name,
		`msg="remove VM files on shutdown" err="permission denied"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %s in log:\n%s", want, out)
		}
	}
	if r.vms.pruned != 1 || len(r.l.machines) != 0 {
		t.Errorf("pruned %d, machines %v", r.vms.pruned, r.l.machines)
	}
}

// hungDestroy is a Destroy that hangs on stuck VMs until its context ends,
// as a virsh destroy cut off by its deadline does.
type hungDestroy struct{ *fakeVMs }

func (h hungDestroy) Destroy(ctx context.Context, name string) error {
	if h.stuck[name] {
		<-ctx.Done()
		return ctx.Err()
	}
	return h.fakeVMs.Destroy(ctx, name)
}

// ctxGitHub fails calls whose context has ended, as the real client does.
type ctxGitHub struct{ *fakeGitHub }

func (g ctxGitHub) DeleteRunner(ctx context.Context, repo string, id int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return g.fakeGitHub.DeleteRunner(ctx, repo, id)
}

// A destroy that hangs for the whole shutdown time limit still leaves time
// to delete every runner.
func TestShutdownDeletesRunnersPastHungDestroy(t *testing.T) {
	defer func(d time.Duration) { shutdownTimeout = d }(shutdownTimeout)
	shutdownTimeout = 200 * time.Millisecond
	r := newRig(t, 0, "")
	r.queue(0, "self-hosted", "large")
	r.queue(0, "self-hosted", "small")
	r.tick()
	var ids []int64
	for _, m := range r.l.machines {
		ids = append(ids, m.runnerID)
		r.vms.stuck = map[string]bool{m.name: true} // one of the two
	}
	r.l.vms = hungDestroy{r.vms}
	r.l.gh = ctxGitHub{r.gh}
	r.l.Shutdown()
	slices.Sort(ids)
	slices.Sort(r.gh.deleted)
	if !slices.Equal(r.gh.deleted, ids) {
		t.Errorf("deleted runners %v, want %v", r.gh.deleted, ids)
	}
}

// A repo whose poll keeps failing doesn't stop the loop serving the
// others, enforcing time limits or letting spares go.
func TestOneRepoFailing(t *testing.T) {
	const other = "example-org/other-repo"
	r := newRig(t, 1, "")
	logs := r.captureLogs()
	r.l.cfg.Repos = []string{other, repo}
	r.gh.queuedErr = map[string]error{other: &github.APIError{StatusCode: 404}}
	j := r.queue(0, "self-hosted", "large")
	if d := r.tick(); d != minBackoff {
		t.Errorf("wait %s, want the backoff", d)
	}
	large := r.vms.specsOfSize(8)
	if len(large) != 1 || len(r.vms.specsOfSize(4)) != 1 {
		t.Fatalf("running %v, want a large VM for the job and a spare", r.vms.running)
	}
	if !strings.Contains(logs.String(), other+": queued jobs") {
		t.Errorf("failing repo not logged:\n%s", logs)
	}
	r.gh.setRunner(large[0].Name, "online", true)
	r.take(j)
	r.tick()
	r.clock.advance(r.cfg.Timeouts.Job.D() + time.Minute)
	r.tick()
	if len(r.vms.running) != 0 {
		t.Errorf("still running %v, destroyed %v", r.vms.running, r.vms.destroyed)
	}
}

// A rate limit stops the tick's GitHub calls, but not its time limits.
func TestRateLimitedPollStillEnforcesLimits(t *testing.T) {
	const other = "example-org/other-repo"
	r := newRig(t, 0, "")
	r.l.cfg.Repos = []string{other, repo}
	j := r.queue(0, "self-hosted", "large")
	r.tick()
	name := r.vms.created[0].Name
	r.gh.setRunner(name, "online", true)
	r.take(j)
	r.tick()
	r.queue(0, "self-hosted", "small")
	r.gh.queuedErr = map[string]error{other: &github.APIError{StatusCode: 403, RateLimited: true}}
	r.gh.statuses = nil
	r.clock.advance(r.cfg.Timeouts.Job.D() + time.Minute)
	if d := r.tick(); d != rateLimitWait {
		t.Errorf("wait %s, want %s", d, rateLimitWait)
	}
	if len(r.gh.statuses) != 1 {
		t.Errorf("polled %d repos after the rate limit, want 1", len(r.gh.statuses))
	}
	if len(r.vms.created) != 1 {
		t.Errorf("launched %v while rate limited", r.vms.created[1:])
	}
	if !slices.Equal(r.vms.destroyed, []string{name}) {
		t.Errorf("destroyed %q, want %q", r.vms.destroyed, name)
	}
}

// A runner the loop could not check on is not held to the online or idle
// limits that tick: its VM may well have taken a job.
func TestUncheckedRunnerNotTimedOut(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(*fakeGitHub)
	}{
		{"list runners fails", func(f *fakeGitHub) {
			f.runnersErr = map[string]error{repo: &github.APIError{StatusCode: 502}}
		}},
		{"rate limited", func(f *fakeGitHub) {
			f.jobsErr = &github.APIError{StatusCode: 403, RateLimited: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, 0, "")
			j := r.queue(0, "self-hosted", "small")
			r.tick()
			name := r.vms.created[0].Name
			r.take(j)
			r.gh.setRunner(name, "online", true)
			tc.fail(r.gh)
			r.clock.advance(r.cfg.Timeouts.Online.D() + time.Minute)
			r.tick()
			if len(r.vms.destroyed) != 0 {
				t.Errorf("destroyed %q without checking its runner", r.vms.destroyed)
			}
		})
	}
}

// One failing repo doesn't make the loop poll in-progress runs every tick.
func TestOneRepoFailingKeepsInProgressPace(t *testing.T) {
	const other = "example-org/other-repo"
	r := newRig(t, 0, "")
	r.l.cfg.Repos = []string{other, repo}
	r.gh.queuedErr = map[string]error{other: &github.APIError{StatusCode: 404}}
	for range 3 {
		r.tick()
		r.clock.advance(5 * time.Second)
	}
	n := 0
	for _, s := range r.gh.statuses {
		if slices.Contains(s, "in_progress") {
			n++
		}
	}
	if n != 2 { // both repos, on the first tick only
		t.Errorf("polled in-progress runs %d times, want 2: %q", n, r.gh.statuses)
	}
}

// A rate limit's Retry-After is honoured when another repo failed first.
func TestRetryAfterBehindOtherRepoError(t *testing.T) {
	const other = "example-org/other-repo"
	r := newRig(t, 0, "")
	r.l.cfg.Repos = []string{other, repo}
	r.gh.rate = github.RateLimit{Remaining: 100}
	r.gh.queuedErr = map[string]error{
		other: &github.APIError{StatusCode: 404},
		repo:  &github.APIError{StatusCode: 403, RateLimited: true, RetryAfter: 5 * time.Minute},
	}
	if d := r.tick(); d != 5*time.Minute {
		t.Errorf("wait %s, want 5m", d)
	}
}

// A leftover runner that Startup fails to delete is retried on later
// ticks; one already gone is not.
func TestStartupDeleteRetried(t *testing.T) {
	r := newRig(t, 0, "")
	r.gh.runners[repo] = []github.Runner{{ID: 1, Name: "ex-example-repo-small-0001", Status: "offline"}}
	r.gh.deleteErr = errors.New("connection reset")
	if err := r.l.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []runnerRef{{repo, 1}}; !slices.Equal(r.l.pendingDeletes, want) {
		t.Fatalf("pending %v, want %v", r.l.pendingDeletes, want)
	}
	r.gh.deleteErr = nil
	r.tick()
	if !slices.Equal(r.gh.deleted, []int64{1}) || len(r.gh.runners[repo]) != 0 || len(r.l.pendingDeletes) != 0 {
		t.Errorf("deleted %v, runners %v, pending %v", r.gh.deleted, r.gh.runners[repo], r.l.pendingDeletes)
	}

	r = newRig(t, 0, "")
	r.gh.runners[repo] = []github.Runner{{ID: 1, Name: "ex-example-repo-small-0001", Status: "offline"}}
	r.gh.deleteErr = &github.APIError{StatusCode: 404}
	if err := r.l.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.l.pendingDeletes) != 0 {
		t.Errorf("queued a runner already gone: %v", r.l.pendingDeletes)
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

// errMayRun is the error of a Create that left a domain that may be running.
var errMayRun = fmt.Errorf("vm x: virsh create: signal: killed\n%w", vm.ErrMayBeRunning)

// failedStart makes the next Create fail with errMayRun and returns the name
// of the VM the loop keeps for it.
func (r *rig) failedStart() string {
	r.t.Helper()
	r.vms.createErr = errMayRun
	r.tick()
	r.vms.createErr = nil
	if len(r.l.machines) != 1 {
		r.t.Fatalf("machines %v, want the failed VM kept", r.l.machines)
	}
	for n := range r.l.machines {
		return n
	}
	return ""
}

// A VM whose start failed but may be running holds its room in the budget
// until it is destroyed and gone, then is reaped.
func TestFailedStartTracked(t *testing.T) {
	r := newRig(t, 0, "")
	logs := r.captureLogs()
	r.queue(0, "self-hosted", "large")
	name := r.failedStart()
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "vm="+name) {
		t.Errorf("no warning naming %s:\n%s", name, logs)
	}
	if len(r.gh.runners[repo]) != 0 {
		t.Errorf("runner left registered: %+v", r.gh.runners[repo])
	}

	// The domain turns up, and won't die at first. A second large VM
	// would fit only without it.
	r.vms.running[name] = vm.Spec{Name: name, VCPUs: 8}
	r.vms.stuck = map[string]bool{name: true}
	r.clock.advance(5 * time.Second)
	r.tick()
	if len(r.vms.created) != 0 {
		t.Fatalf("started %+v beside a VM that may be running", r.vms.created)
	}
	r.vms.stuck = nil
	r.clock.advance(5 * time.Second)
	r.tick()
	if !slices.Equal(r.vms.destroyed, []string{name}) {
		t.Errorf("destroyed %q, want %s", r.vms.destroyed, name)
	}
	if len(r.vms.created) != 0 {
		t.Fatalf("started %+v before the VM was seen gone", r.vms.created)
	}
	r.clock.advance(5 * time.Second)
	r.tick()
	if _, ok := r.l.machines[name]; ok || !slices.Contains(r.vms.removed, name) {
		t.Errorf("not reaped: removed %q", r.vms.removed)
	}
	if len(r.vms.created) != 1 {
		t.Errorf("no VM for the job once the room was free: %+v", r.vms.created)
	}
	if !strings.Contains(logs.String(), `level=INFO msg="VM gone" vm=`+name) {
		t.Errorf("no log of %s gone:\n%s", name, logs)
	}
}

// A VM whose start failed and never shows up is forgotten after the online
// time limit, and its files removed.
func TestFailedStartNeverAppears(t *testing.T) {
	r := newRig(t, 0, "")
	logs := r.captureLogs()
	r.queue(0, "self-hosted", "large")
	name := r.failedStart()
	r.clock.advance(3 * time.Minute)
	r.tick()
	if _, ok := r.l.machines[name]; !ok || len(r.vms.removed) != 0 {
		t.Fatalf("forgotten early: removed %q", r.vms.removed)
	}
	r.clock.advance(time.Second)
	r.tick()
	if _, ok := r.l.machines[name]; ok || !slices.Equal(r.vms.removed, []string{name}) {
		t.Errorf("not forgotten: removed %q", r.vms.removed)
	}
	if !strings.Contains(logs.String(), "never appeared") {
		t.Errorf("no log of forgetting %s:\n%s", name, logs)
	}
}

// A spare whose start failed but may be running is kept too.
func TestFailedSpareTracked(t *testing.T) {
	r := newRig(t, 1, "")
	r.queue(0, "self-hosted", "small")
	r.tick()
	n := len(r.l.machines)
	r.vms.createErr = errMayRun
	r.vms.powerOff(r.vms.specsOfSize(4)[0].Name)
	r.clock.advance(5 * time.Second)
	r.tick()
	r.vms.createErr = nil
	var kept []string
	for name, m := range r.l.machines {
		if m.state == killed && strings.Contains(name, "spare") {
			kept = append(kept, name)
		}
	}
	if len(kept) != 1 || len(r.l.machines) != n {
		t.Errorf("machines %v, want the failed spare kept", r.l.machines)
	}
}

// Shutdown destroys a VM whose start failed, even when it can't list VMs.
func TestShutdownDestroysFailedStart(t *testing.T) {
	r := newRig(t, 0, "")
	r.queue(0, "self-hosted", "small")
	name := r.failedStart()
	r.vms.running[name] = vm.Spec{Name: name}
	r.vms.listErr = errors.New("libvirt down")
	r.l.Shutdown()
	if !slices.Equal(r.vms.destroyed, []string{name}) {
		t.Errorf("destroyed %q, want %s", r.vms.destroyed, name)
	}
}

// slowFailedCreate is a Create that takes d, as virsh create cut off by its
// deadline does, then fails leaving a VM that may be running.
type slowFailedCreate struct {
	*fakeVMs
	clock *fakeClock
	d     time.Duration
}

func (s slowFailedCreate) Create(context.Context, vm.Spec) error {
	s.clock.advance(s.d)
	return errMayRun
}

// The wait for a VM whose start failed counts from when Create returned, not
// from the start of the tick that called it.
func TestFailedStartWaitsFromFailure(t *testing.T) {
	r := newRig(t, 0, "")
	r.queue(0, "self-hosted", "large")
	r.l.vms = slowFailedCreate{r.vms, r.clock, 4 * time.Minute}
	r.tick()
	r.l.vms = r.vms
	if len(r.l.machines) != 1 {
		t.Fatalf("machines %v, want the failed VM kept", r.l.machines)
	}
	r.clock.advance(5 * time.Second)
	r.tick()
	if len(r.l.machines) != 1 || len(r.vms.removed) != 0 {
		t.Fatalf("forgotten 5s after its start failed: removed %q", r.vms.removed)
	}
	r.clock.advance(3 * time.Minute)
	r.tick()
	if len(r.vms.removed) != 1 {
		t.Errorf("not forgotten after the online time limit: removed %q", r.vms.removed)
	}
}

// staleQueue launches a VM for a small job, has its runner take a job, and
// ticks past queueLag while GitHub still lists the job as queued. It
// returns the job.
func (r *rig) staleQueue() github.Job {
	r.t.Helper()
	j := r.queue(0, "self-hosted", "small")
	r.tick()
	r.gh.setRunner(r.vms.created[0].Name, "online", true)
	for range queueLag/(5*time.Second) + 1 {
		r.clock.advance(5 * time.Second)
		r.tick()
	}
	return j
}

// While GitHub is struggling, a run's list of jobs can show a job as queued
// long after it started. The loop asks about the job itself before
// launching for it again, and asks only once.
func TestNoRelaunchForJobAlreadyStarted(t *testing.T) {
	r := newRig(t, 0, "")
	r.gh.jobStatus = map[int64]string{jobID + 1: "in_progress"}
	j := r.staleQueue()
	if len(r.vms.created) != 1 {
		t.Errorf("created %d VMs, want 1 for a job that has started", len(r.vms.created))
	}
	if !slices.Equal(r.gh.statusAsks, []int64{j.ID}) {
		t.Errorf("asked about jobs %v, want %d once", r.gh.statusAsks, j.ID)
	}
	// Once GitHub stops listing it, including in in-progress runs, the
	// loop forgets it.
	r.take(j)
	r.clock.advance(inProgressEvery)
	r.tick()
	if len(r.l.launchedJobs) != 0 {
		t.Errorf("still tracking %v", r.l.launchedJobs)
	}
}

// A job that really is still queued gets another VM, as before.
func TestRelaunchForJobStillQueued(t *testing.T) {
	r := newRig(t, 0, "")
	j := r.staleQueue()
	if len(r.vms.created) != 2 {
		t.Errorf("created %d VMs, want a second for the queued job", len(r.vms.created))
	}
	if !slices.Equal(r.gh.statusAsks, []int64{j.ID}) {
		t.Errorf("asked about jobs %v, want %d once", r.gh.statusAsks, j.ID)
	}
}

// A queued job whose VM keeps failing to start is asked about once per
// jobRecheck, not every tick.
func TestJobRecheckPaced(t *testing.T) {
	r := newRig(t, 0, "")
	r.staleQueue()
	r.gh.statusAsks = nil
	r.vms.createErr = errors.New("no space")
	r.vms.powerOff(r.vms.created[1].Name)
	for range jobRecheck / (5 * time.Second) {
		r.clock.advance(5 * time.Second)
		r.tick()
	}
	if len(r.gh.statusAsks) != 1 {
		t.Errorf("asked %d times in %s, want once", len(r.gh.statusAsks), jobRecheck)
	}
}

// If GitHub can't say, the job gets a VM as it would without the check; a
// rate limit stops the launch.
func TestJobStatusErrors(t *testing.T) {
	r := newRig(t, 0, "")
	r.gh.statusErr = &github.APIError{StatusCode: 502}
	r.staleQueue()
	if len(r.vms.created) != 2 {
		t.Errorf("created %d VMs after a failed check, want 2", len(r.vms.created))
	}

	r = newRig(t, 0, "")
	r.gh.statusErr = &github.APIError{StatusCode: 403, RateLimited: true}
	r.staleQueue()
	if len(r.vms.created) != 1 {
		t.Errorf("created %d VMs while rate limited, want 1", len(r.vms.created))
	}
}

// A repo with runs GitHub couldn't list is still served from the others,
// without a backoff.
func TestSkippedRunsStillServed(t *testing.T) {
	r := newRig(t, 0, "")
	logs := r.captureLogs()
	r.queue(0, "self-hosted", "small")
	jobs := slices.Clone(r.gh.jobs[repo])
	r.gh.queuedErr = nil
	sk := &github.SkippedRunsError{RunIDs: []int64{7}, Err: &github.APIError{StatusCode: 502}}
	r.l.gh = skippingGitHub{r.gh, jobs, sk}
	if d := r.tick(); d != r.cfg.PollInterval.D() {
		t.Errorf("wait %s, want the poll interval", d)
	}
	if len(r.vms.created) != 1 {
		t.Errorf("created %d VMs, want 1", len(r.vms.created))
	}
	if !strings.Contains(logs.String(), "skipped runs") {
		t.Errorf("skipped runs not logged:\n%s", logs)
	}
}

// A skipped run may still have the jobs it had at the last poll of
// in-progress runs, so their runners stay covered.
func TestSkippedRunKeepsInProgressJobs(t *testing.T) {
	r := newRig(t, 0, "")
	r.l.inProgressJobs[repo] = []int64{42}
	sk := &github.SkippedRunsError{RunIDs: []int64{7}, Err: &github.APIError{StatusCode: 502}}
	r.l.gh = skippingGitHub{r.gh, nil, sk}
	r.tick()
	if !slices.Contains(r.l.inProgressJobs[repo], 42) {
		t.Errorf("in-progress jobs %v, want 42 kept", r.l.inProgressJobs[repo])
	}
}

// skippingGitHub returns its jobs with a *SkippedRunsError.
type skippingGitHub struct {
	*fakeGitHub
	jobs []github.Job
	err  *github.SkippedRunsError
}

func (g skippingGitHub) QueuedJobs(context.Context, string, ...string) ([]github.Job, error) {
	return slices.Clone(g.jobs), g.err
}
