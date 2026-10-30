// Package loop turns queued GitHub Actions jobs into single-use runner VMs.
//
// Each tick it reaps VMs that have powered off, asks GitHub for queued jobs,
// checks on the runners it has started, and launches a VM for each job that
// no runner is already coming for, oldest first while the budget allows. A
// job of the spare size goes to a warm spare if one is free. While jobs keep
// coming, it keeps a few spares booted.
//
// The loop runs in one goroutine and keeps no state on disk: on start it
// destroys every VM with its prefix and begins again.
package loop

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ryanlewis/ephemeral-runners/internal/config"
	"github.com/ryanlewis/ephemeral-runners/internal/github"
	"github.com/ryanlewis/ephemeral-runners/internal/vm"
)

// GitHub is the part of the GitHub client the loop uses.
type GitHub interface {
	QueuedJobs(ctx context.Context, repo string, runStatuses ...string) ([]github.Job, error)
	GenerateJITConfig(ctx context.Context, repo, name string, labels []string) (*github.JITConfig, error)
	ListRunners(ctx context.Context, repo string) ([]github.Runner, error)
	DeleteRunner(ctx context.Context, repo string, id int64) error
	RateLimit() github.RateLimit
}

// VMs is the part of the VM launcher the loop uses.
type VMs interface {
	Create(ctx context.Context, s vm.Spec) error
	Attach(ctx context.Context, stateDir, name, jitConfig string) error
	List(ctx context.Context, prefix string) ([]string, error)
	Destroy(ctx context.Context, name string) error
	Remove(stateDir, name string) error
	Prune(ctx context.Context, stateDir, prefix string) ([]string, error)
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

const (
	// inProgressEvery is how often the loop looks for queued jobs in runs
	// that are already in progress. Their job lists change as steps run,
	// so each look costs requests even with ETags.
	inProgressEvery = 30 * time.Second
	minBackoff      = 5 * time.Second
	maxBackoff      = time.Minute
	// rateLimitWait is the wait after a rate limit with no reset time.
	rateLimitWait = time.Minute
	// queueLag is how long GitHub can go on listing a job as queued after
	// a runner has taken it.
	queueLag = time.Minute
	// shutdownTimeout bounds the clean-up on the way out.
	shutdownTimeout = 2 * time.Minute
)

type state int

const (
	spare    state = iota // booted, waiting for a job config
	starting              // has a config, its runner is not online yet
	idle                  // runner online, waiting for its job
	busy                  // runner running its job, or done with it
	killed                // destroyed, waiting to be reaped
)

var stateNames = []string{"spare", "starting", "idle", "busy", "killed"}

func (s state) String() string { return stateNames[s] }

// machine is one runner VM the loop started.
type machine struct {
	name    string
	size    string
	image   string // the resolved image it booted from
	created time.Time
	state   state

	// Set once the VM has a job config.
	repo     string
	jobID    int64 // the queued job it was launched for
	runnerID int64
	runner   string
	assigned time.Time
	online   time.Time
	busy     time.Time // when the loop saw its runner take a job

	// mayRun is set when the VM's start failed but its domain may be
	// running. It counts in the budget and is destroyed each tick until
	// it has been seen running and then gone, or never shows up.
	mayRun bool
	seen   bool // listed running since its start failed
}

type runnerRef struct {
	repo string
	id   int64
}

type job struct {
	repo string
	size string
	github.Job
}

// Options are the loop's collaborators. Clock, Logger and ResolveImage
// have defaults.
type Options struct {
	Config *config.Config
	GitHub GitHub
	VMs    VMs
	Clock  Clock
	Logger *slog.Logger
	// ResolveImage turns the configured image path into the file it points
	// to. It defaults to filepath.EvalSymlinks.
	ResolveImage func(string) (string, error)
}

// Loop is the runner loop. Its methods are not safe for concurrent use.
type Loop struct {
	cfg     *config.Config
	gh      GitHub
	vms     VMs
	clock   Clock
	log     *slog.Logger
	resolve func(string) (string, error)

	machines       map[string]*machine
	pendingDeletes []runnerRef
	lastJob        time.Time // when a job this host serves was last seen
	lastInProgress time.Time // when in-progress runs were last polled
	// inProgressJobs are the jobs seen, by repo, when in-progress runs
	// were last polled. Ticks in between don't see jobs of in-progress
	// runs.
	inProgressJobs map[string][]int64
	backoff        time.Duration

	jobsStarted    int64
	launchFailures int64
	lastPoll       time.Time
}

// New returns a Loop.
func New(o Options) *Loop {
	l := &Loop{
		cfg:      o.Config,
		gh:       o.GitHub,
		vms:      o.VMs,
		clock:    o.Clock,
		log:      o.Logger,
		resolve:  o.ResolveImage,
		machines: map[string]*machine{},

		inProgressJobs: map[string][]int64{},
	}
	if l.clock == nil {
		l.clock = realClock{}
	}
	if l.log == nil {
		l.log = slog.Default()
	}
	if l.resolve == nil {
		l.resolve = filepath.EvalSymlinks
	}
	return l
}

// Run cleans up after any earlier run, then ticks until ctx ends, then
// destroys its VMs and deletes their runners.
func (l *Loop) Run(ctx context.Context) error {
	if err := l.Startup(ctx); err != nil {
		return err
	}
	defer l.Shutdown()
	for {
		next := l.Tick(ctx)
		t := time.NewTimer(next)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

func (l *Loop) prefix() string { return l.cfg.NamePrefix + "-" }

// Startup destroys every VM with the prefix, removes their files, and
// deletes the prefix's runners in each repo: with their VMs gone, they are
// dead even if GitHub still shows them online. GitHub refuses to delete a
// runner it thinks is running a job; it removes that one itself once the
// job fails for want of its runner.
func (l *Loop) Startup(ctx context.Context) error {
	names, err := l.vms.List(ctx, l.prefix())
	if err != nil {
		return fmt.Errorf("list VMs: %w", err)
	}
	for _, n := range names {
		if err := l.vms.Destroy(ctx, n); err != nil {
			return fmt.Errorf("destroy leftover VM %s: %w", n, err)
		}
		l.log.Info("destroyed leftover VM", "vm", n)
	}
	removed, err := l.vms.Prune(ctx, l.cfg.VM.StateDir, l.prefix())
	if err != nil {
		return fmt.Errorf("remove leftover VM files: %w", err)
	}
	if len(removed) > 0 {
		l.log.Info("removed leftover VM files", "vms", removed)
	}
	for _, repo := range l.cfg.Repos {
		runners, err := l.gh.ListRunners(ctx, repo)
		if err != nil {
			l.log.Warn("list runners for clean-up", "repo", repo, "err", err)
			continue
		}
		for _, r := range runners {
			if !strings.HasPrefix(r.Name, l.prefix()) {
				continue
			}
			if r.Busy {
				l.log.Info("leaving leftover runner to GitHub: it still shows a job", "repo", repo, "runner", r.Name)
				continue
			}
			if err := l.gh.DeleteRunner(ctx, repo, r.ID); err != nil && !errors.Is(err, github.ErrNotFound) {
				l.log.Warn("delete leftover runner", "repo", repo, "runner", r.Name, "err", err)
				l.pendingDeletes = append(l.pendingDeletes, runnerRef{repo, r.ID})
				continue
			}
			l.log.Info("deleted leftover runner", "repo", repo, "runner", r.Name, "status", r.Status)
		}
	}
	return nil
}

// Shutdown destroys the loop's VMs, deletes their runners and removes
// their files. It has its own time limit, since the caller's context has
// usually ended.
func (l *Loop) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	names, err := l.vms.List(ctx, l.prefix())
	if err != nil {
		l.log.Warn("list VMs on shutdown", "err", err)
	}
	for _, m := range l.machines {
		if !slices.Contains(names, m.name) {
			names = append(names, m.name)
		}
	}
	slices.Sort(names)
	for _, n := range names {
		if err := l.vms.Destroy(ctx, n); err != nil {
			l.log.Warn("destroy VM on shutdown", "vm", n, "err", err)
			continue
		}
		l.log.Info("destroyed VM on shutdown", "vm", n)
		if m := l.machines[n]; m != nil && m.runnerID != 0 {
			l.deleteRunner(ctx, m.repo, m.runnerID)
		}
	}
	l.retryDeletes(ctx)
	if _, err := l.vms.Prune(ctx, l.cfg.VM.StateDir, l.prefix()); err != nil {
		l.log.Warn("remove VM files on shutdown", "err", err)
	}
	l.machines = map[string]*machine{}
}

// Tick does one round of work and returns how long to wait before the next.
func (l *Loop) Tick(ctx context.Context) time.Duration {
	defer l.writeMetrics()
	now := l.clock.Now()
	l.reap(ctx)
	l.retryDeletes(ctx)

	// A repo that fails is skipped, so the others are still served and
	// the time limits still apply. A rate limit covers the whole
	// installation, so it stops the tick's GitHub calls.
	jobs, err := l.poll(ctx, now)
	var checked map[string]bool
	if !errors.Is(err, github.ErrRateLimited) {
		var cerr error
		checked, cerr = l.checkRunners(ctx, now)
		err = errors.Join(err, cerr)
	}
	if err == nil {
		l.lastPoll = now
	}
	l.timeouts(ctx, now, checked)
	if !errors.Is(err, github.ErrRateLimited) {
		err = errors.Join(err, l.launch(ctx, now, jobs))
	}
	l.keepSpares(ctx, now)
	if err != nil {
		return l.failed(err, now)
	}
	l.backoff = 0
	return l.cfg.PollInterval.D()
}

// failed logs a GitHub error and returns how long to wait: what a rate
// limit asks for, or a backoff that doubles from 5 s up to a minute.
func (l *Loop) failed(err error, now time.Time) time.Duration {
	if errors.Is(err, github.ErrRateLimited) {
		// A secondary limit says how long to wait in Retry-After, and
		// leaves the primary limit's requests remaining.
		var wait time.Duration
		if rl, apiErr := l.gh.RateLimit(), rateLimitError(err); apiErr != nil && apiErr.RetryAfter > 0 {
			wait = apiErr.RetryAfter
		} else if rl.Remaining == 0 {
			wait = rl.Reset.Sub(now)
		}
		if wait <= 0 {
			wait = rateLimitWait
		}
		l.log.Warn("rate limited by GitHub, waiting for the reset", "wait", wait.Round(time.Second), "err", err)
		return wait
	}
	l.backoff = min(max(l.backoff*2, minBackoff), maxBackoff)
	l.log.Warn("GitHub request failed, backing off", "wait", l.backoff, "err", err)
	return l.backoff
}

// rateLimitError returns the rate-limited API error in err, which may hold
// other repos' errors too.
func rateLimitError(err error) *github.APIError {
	if apiErr, ok := err.(*github.APIError); ok && errors.Is(apiErr, github.ErrRateLimited) {
		return apiErr
	}
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for _, e := range e.Unwrap() {
			if apiErr := rateLimitError(e); apiErr != nil {
				return apiErr
			}
		}
	case interface{ Unwrap() error }:
		return rateLimitError(e.Unwrap())
	}
	return nil
}

// reap forgets VMs that are no longer running: it removes their files and
// deletes their runners. A VM whose start failed may not be listed yet, so
// it waits until it has been seen, or for the online time limit.
func (l *Loop) reap(ctx context.Context) {
	names, err := l.vms.List(ctx, l.prefix())
	if err != nil {
		l.log.Warn("list VMs", "err", err)
		return
	}
	for _, m := range l.sorted() {
		if slices.Contains(names, m.name) {
			if m.mayRun {
				m.seen = true
			}
			continue
		}
		msg := "VM gone"
		if m.mayRun && !m.seen {
			if l.clock.Now().Sub(m.created) <= l.cfg.Timeouts.Online.D() {
				continue
			}
			msg = "VM never appeared after its start failed, forgetting it"
		}
		if err := l.vms.Remove(l.cfg.VM.StateDir, m.name); err != nil {
			l.log.Warn("remove VM files", "vm", m.name, "err", err)
			continue
		}
		if m.runnerID != 0 {
			l.deleteRunner(ctx, m.repo, m.runnerID)
		}
		delete(l.machines, m.name)
		l.log.Info(msg, "vm", m.name, "size", m.size, "was", m.state.String(), "runner", m.runner,
			"age", l.clock.Now().Sub(m.created).Round(time.Second))
	}
}

// deleteRunner deletes a runner on GitHub, or queues it to try again.
func (l *Loop) deleteRunner(ctx context.Context, repo string, id int64) {
	err := l.gh.DeleteRunner(ctx, repo, id)
	if err != nil && !errors.Is(err, github.ErrNotFound) {
		l.log.Warn("delete runner, will retry", "repo", repo, "runner_id", id, "err", err)
		l.pendingDeletes = append(l.pendingDeletes, runnerRef{repo, id})
	}
}

func (l *Loop) retryDeletes(ctx context.Context) {
	pending := l.pendingDeletes
	l.pendingDeletes = nil
	for _, r := range pending {
		l.deleteRunner(ctx, r.repo, r.id)
	}
}

// poll returns the queued jobs this host serves, oldest first. It looks at
// queued runs every tick and in-progress runs every inProgressEvery. It
// skips a repo that fails and returns the errors with the other repos'
// jobs; a rate limit stops it.
func (l *Loop) poll(ctx context.Context, now time.Time) ([]job, error) {
	statuses := []string{"queued"}
	inProgress := now.Sub(l.lastInProgress) >= inProgressEvery
	if inProgress {
		statuses = append(statuses, "in_progress")
	}
	var jobs []job
	var errs error
	for _, repo := range l.cfg.Repos {
		js, err := l.gh.QueuedJobs(ctx, repo, statuses...)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("%s: queued jobs: %w", repo, err))
			if errors.Is(err, github.ErrRateLimited) {
				break
			}
			continue
		}
		var ids []int64
		for _, j := range js {
			if size, ok := l.cfg.SizeFor(j.Labels); ok {
				jobs = append(jobs, job{repo: repo, size: size, Job: j})
				ids = append(ids, j.ID)
			}
		}
		// A repo that failed keeps the jobs from its last poll.
		if inProgress {
			l.inProgressJobs[repo] = ids
		}
	}
	if inProgress {
		l.lastInProgress = now
	}
	slices.SortStableFunc(jobs, func(a, b job) int { return a.CreatedAt.Compare(b.CreatedAt) })
	if len(jobs) > 0 {
		l.lastJob = now
	}
	return jobs, errs
}

// checkRunners asks GitHub about the runners that have not taken a job
// yet, one request per repo that has any. Like poll, it skips a repo that
// fails and stops at a rate limit. It returns the repos it checked.
func (l *Loop) checkRunners(ctx context.Context, now time.Time) (map[string]bool, error) {
	checked := map[string]bool{}
	var errs error
	byRepo := map[string][]*machine{}
	for _, m := range l.sorted() {
		if m.state == starting || m.state == idle {
			byRepo[m.repo] = append(byRepo[m.repo], m)
		}
	}
	for _, repo := range l.cfg.Repos {
		ms := byRepo[repo]
		if len(ms) == 0 {
			checked[repo] = true
			continue
		}
		runners, err := l.gh.ListRunners(ctx, repo)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("%s: list runners: %w", repo, err))
			if errors.Is(err, github.ErrRateLimited) {
				break
			}
			continue
		}
		checked[repo] = true
		for _, m := range ms {
			i := slices.IndexFunc(runners, func(r github.Runner) bool { return r.ID == m.runnerID })
			switch {
			case i < 0:
				// GitHub removes a JIT runner once its job is done.
				l.setBusy(m, now, "runner gone from GitHub")
			case runners[i].Busy:
				l.setBusy(m, now, "runner took a job")
			case runners[i].Status == "online" && m.state == starting:
				m.online = now
				l.setState(m, idle, "runner online")
			}
		}
	}
	return checked, errs
}

func (l *Loop) setBusy(m *machine, now time.Time, why string) {
	if m.state != busy {
		m.busy = now
	}
	l.setState(m, busy, why)
}

func (l *Loop) setState(m *machine, s state, why string) {
	if m.state == s {
		return
	}
	l.log.Info(why, "vm", m.name, "runner", m.runner, "size", m.size, "from", m.state.String(), "to", s.String())
	m.state = s
}

// timeouts destroys VMs whose runner did not come online, did not take a
// job, or has run past the job limit. The first two need a runner state
// from this tick, so they apply only to the repos checked.
func (l *Loop) timeouts(ctx context.Context, now time.Time, checked map[string]bool) {
	t := l.cfg.Timeouts
	for _, m := range l.sorted() {
		var why string
		switch {
		case m.mayRun:
			why = "its start failed and it may be running"
		case m.state == spare || m.state == killed:
			continue
		case now.Sub(m.assigned) > t.Job.D():
			why = "past the job time limit"
		case !checked[m.repo]:
			continue
		case m.state == starting && now.Sub(m.assigned) > t.Online.D():
			why = "runner did not come online in time"
		case m.state == idle && now.Sub(m.online) > t.Idle.D():
			why = "runner did not get a job in time"
		default:
			continue
		}
		l.kill(ctx, m, why)
	}
}

// kill destroys a VM and deletes its runner, and reports whether the VM is
// gone. reap removes its files once it has gone from the list.
func (l *Loop) kill(ctx context.Context, m *machine, why string) bool {
	if err := l.vms.Destroy(ctx, m.name); err != nil {
		l.log.Warn("destroy VM", "vm", m.name, "err", err)
		return false
	}
	if m.runnerID != 0 {
		l.deleteRunner(ctx, m.repo, m.runnerID)
		m.runnerID = 0
	}
	l.setState(m, killed, "destroyed: "+why)
	return true
}

// launch starts a runner for each job that no runner is coming for. Jobs of
// the spare size go to free spares first, since that is quick and would
// otherwise be undone by making room for a cold start. The rest start
// oldest first while the budget allows; a job that doesn't fit waits, and
// so do the younger ones behind it. GitHub errors are returned, so the tick
// backs off; VM errors only count as failed launches.
func (l *Loop) launch(ctx context.Context, now time.Time, jobs []job) error {
	// A job that a VM was launched for is covered while that VM is
	// starting or idle, and for queueLag after its runner takes a job:
	// GitHub can show the runner busy before the job leaves the queued
	// list. After that, the runner took another job of the same labels,
	// and this one needs a VM of its own. A job seen at the last poll of
	// in-progress runs may still be queued, so it covers its VM between
	// those polls too. Other runners that are starting or idle will take
	// a queued job of their repo and size, so count them against the
	// oldest jobs left.
	launchedFor := map[int64]*machine{}
	for _, m := range l.machines {
		if m.state == starting || m.state == idle || (m.state == busy && now.Sub(m.busy) < queueLag) {
			launchedFor[m.jobID] = m
		}
	}
	covered := map[*machine]bool{}
	for _, j := range jobs {
		if m := launchedFor[j.ID]; m != nil {
			covered[m] = true
		}
	}
	for _, ids := range l.inProgressJobs {
		for _, id := range ids {
			if m := launchedFor[id]; m != nil {
				covered[m] = true
			}
		}
	}
	supply := map[[2]string]int{}
	for _, m := range l.machines {
		if (m.state == starting || m.state == idle) && !covered[m] {
			supply[[2]string{m.repo, m.size}]++
		}
	}
	var wanted []job
	for _, j := range jobs {
		k := [2]string{j.repo, j.size}
		if launchedFor[j.ID] != nil {
			continue
		}
		if supply[k] > 0 {
			supply[k]--
			continue
		}
		wanted = append(wanted, j)
	}

	var cold []job
	for _, j := range wanted {
		s := l.freeSpare(j.size)
		if s == nil || !l.fits(l.cfg.Sizes[j.size]) {
			cold = append(cold, j)
			continue
		}
		if err := l.handOver(ctx, now, s, j); err != nil {
			return err
		}
	}
	for _, j := range cold {
		size := l.cfg.Sizes[j.size]
		if !l.fits(size) {
			l.log.Debug("job waits for room in the budget", "repo", j.repo, "job", j.ID, "size", j.size)
			return nil
		}
		if !l.makeRoom(ctx, size) {
			return nil
		}
		if err := l.coldStart(ctx, now, j); err != nil {
			return err
		}
	}
	return nil
}

// fits reports whether a VM of size fits in the budget beside the VMs that
// have jobs.
func (l *Loop) fits(size config.Size) bool {
	u := l.used()
	return u.VCPUs+size.VCPUs <= l.cfg.Budget.VCPUs && u.MemoryGiB+size.MemoryGiB <= l.cfg.Budget.MemoryGiB
}

// used is the total size of VMs with a job, and of VMs whose start failed
// that may be running; spares don't count.
func (l *Loop) used() config.Size {
	var u config.Size
	for _, m := range l.machines {
		if m.state == starting || m.state == idle || m.state == busy || m.mayRun {
			s := l.cfg.Sizes[m.size]
			u.VCPUs += s.VCPUs
			u.MemoryGiB += s.MemoryGiB
		}
	}
	return u
}

// spares returns the spares, oldest first.
func (l *Loop) spares() []*machine {
	var out []*machine
	for _, m := range l.sorted() {
		if m.state == spare {
			out = append(out, m)
		}
	}
	slices.SortStableFunc(out, func(a, b *machine) int { return a.created.Compare(b.created) })
	return out
}

func (l *Loop) freeSpare(size string) *machine {
	if size != l.cfg.Spares.Size {
		return nil
	}
	if s := l.spares(); len(s) > 0 {
		return s[0]
	}
	return nil
}

// makeRoom destroys spares, newest first, until a VM of size fits in the
// budget alongside the VMs with jobs and the spares that are left. It
// reports false if a spare it had to destroy is still running.
func (l *Loop) makeRoom(ctx context.Context, size config.Size) bool {
	spareSize := l.cfg.Sizes[l.cfg.Spares.Size]
	spares := l.spares()
	for len(spares) > 0 {
		u := l.used()
		n := len(spares)
		if u.VCPUs+n*spareSize.VCPUs+size.VCPUs <= l.cfg.Budget.VCPUs &&
			u.MemoryGiB+n*spareSize.MemoryGiB+size.MemoryGiB <= l.cfg.Budget.MemoryGiB {
			return true
		}
		if !l.kill(ctx, spares[n-1], "making room for a job") {
			return false
		}
		spares = spares[:n-1]
	}
	return true
}

func (l *Loop) labels(size string) []string {
	return append(slices.Clone(l.cfg.Labels), size)
}

// handOver registers a runner for j and hot-plugs its config into a spare.
func (l *Loop) handOver(ctx context.Context, now time.Time, s *machine, j job) error {
	name := l.newName(repoName(j.repo) + "-" + j.size)
	jit, err := l.gh.GenerateJITConfig(ctx, j.repo, name, l.labels(j.size))
	if err != nil {
		l.launchFailures++
		return fmt.Errorf("%s: generate JIT config: %w", j.repo, err)
	}
	if err := l.vms.Attach(ctx, l.cfg.VM.StateDir, s.name, jit.EncodedConfig); err != nil {
		l.launchFailures++
		l.log.Warn("hand job to spare", "vm", s.name, "err", err)
		l.deleteRunner(ctx, j.repo, jit.RunnerID)
		l.kill(ctx, s, "failed to take a job config")
		return nil
	}
	s.repo, s.jobID, s.runnerID, s.runner, s.assigned = j.repo, j.ID, jit.RunnerID, name, now
	l.jobsStarted++
	l.setState(s, starting, "handed job to spare")
	l.log.Info("launched", "vm", s.name, "runner", name, "repo", j.repo, "size", j.size,
		"job", j.ID, "queued_for", now.Sub(j.CreatedAt).Round(time.Second), "spare", true)
	return nil
}

// coldStart registers a runner for j and boots a VM with its config.
func (l *Loop) coldStart(ctx context.Context, now time.Time, j job) error {
	image, err := l.resolve(l.cfg.VM.Image)
	if err != nil {
		l.launchFailures++
		l.log.Warn("resolve image", "image", l.cfg.VM.Image, "err", err)
		return nil
	}
	name := l.newName(repoName(j.repo) + "-" + j.size)
	jit, err := l.gh.GenerateJITConfig(ctx, j.repo, name, l.labels(j.size))
	if err != nil {
		l.launchFailures++
		return fmt.Errorf("%s: generate JIT config: %w", j.repo, err)
	}
	if err := l.vms.Create(ctx, l.spec(name, j.size, image, jit.EncodedConfig)); err != nil {
		l.startFailed("start VM", name, j.size, image, err)
		l.deleteRunner(ctx, j.repo, jit.RunnerID)
		return nil
	}
	l.machines[name] = &machine{
		name: name, size: j.size, image: image, created: now, state: starting,
		repo: j.repo, jobID: j.ID, runnerID: jit.RunnerID, runner: name, assigned: now,
	}
	l.jobsStarted++
	l.log.Info("launched", "vm", name, "runner", name, "repo", j.repo, "size", j.size,
		"job", j.ID, "queued_for", now.Sub(j.CreatedAt).Round(time.Second), "spare", false)
	return nil
}

// keepSpares keeps spares.count spares booted while jobs have been seen
// within spares.idle_after, replaces spares on an old image, and destroys
// them all once jobs stop.
func (l *Loop) keepSpares(ctx context.Context, now time.Time) {
	sp := l.cfg.Spares
	active := sp.Count > 0 && !l.lastJob.IsZero() && now.Sub(l.lastJob) < sp.IdleAfter.D()
	spares := l.spares()
	if !active {
		for _, s := range spares {
			l.kill(ctx, s, "no jobs for "+sp.IdleAfter.String())
		}
		return
	}
	image, err := l.resolve(l.cfg.VM.Image)
	if err != nil {
		l.log.Warn("resolve image", "image", l.cfg.VM.Image, "err", err)
		return
	}
	n := 0
	for _, s := range spares {
		if s.image != image {
			l.kill(ctx, s, "image rebuilt")
			continue
		}
		n++
	}
	size := l.cfg.Sizes[sp.Size]
	for ; n < sp.Count; n++ {
		u := l.used()
		if u.VCPUs+(n+1)*size.VCPUs > l.cfg.Budget.VCPUs || u.MemoryGiB+(n+1)*size.MemoryGiB > l.cfg.Budget.MemoryGiB {
			return
		}
		name := l.newName("spare")
		if err := l.vms.Create(ctx, l.spec(name, sp.Size, image, "")); err != nil {
			l.startFailed("start spare", name, sp.Size, image, err)
			return
		}
		l.machines[name] = &machine{name: name, size: sp.Size, image: image, created: now, state: spare}
		l.log.Info("started spare", "vm", name, "size", sp.Size)
	}
}

// startFailed counts and logs a failed Create. If the VM may be running, it
// keeps the VM as killed, so later ticks destroy it and reap it. Create can
// take minutes to fail, so the wait for the VM to appear starts now.
func (l *Loop) startFailed(msg, name, size, image string, err error) {
	l.launchFailures++
	if !errors.Is(err, vm.ErrMayBeRunning) {
		l.log.Warn(msg, "vm", name, "err", err)
		return
	}
	l.log.Warn(msg+" failed and it may be running, will destroy it", "vm", name, "size", size, "err", err)
	l.machines[name] = &machine{name: name, size: size, image: image, created: l.clock.Now(), state: killed, mayRun: true}
}

func (l *Loop) spec(name, size, image, jit string) vm.Spec {
	s := l.cfg.Sizes[size]
	return vm.Spec{
		Name:      name,
		VCPUs:     s.VCPUs,
		MemoryMiB: s.MemoryGiB * 1024,
		Image:     image,
		Network:   l.cfg.VM.Network,
		StateDir:  l.cfg.VM.StateDir,
		JITConfig: jit,
	}
}

// newName returns <prefix>-<middle>-<4 hex> that no VM of the loop has.
func (l *Loop) newName(middle string) string {
	for {
		b := make([]byte, 2)
		rand.Read(b)
		name := l.prefix() + middle + "-" + hex.EncodeToString(b)
		if _, taken := l.machines[name]; !taken {
			return name
		}
	}
}

// repoName returns the name part of owner/name, in lower case.
func repoName(repo string) string {
	_, name, _ := strings.Cut(repo, "/")
	return strings.ToLower(name)
}

// sorted returns the machines in name order, so each tick acts on them in
// the same order.
func (l *Loop) sorted() []*machine {
	out := make([]*machine, 0, len(l.machines))
	for _, m := range l.machines {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b *machine) int { return cmp.Compare(a.name, b.name) })
	return out
}
