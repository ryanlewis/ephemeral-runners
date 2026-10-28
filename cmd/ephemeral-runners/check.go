package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ryanlewis/ephemeral-runners/internal/config"
	"github.com/ryanlewis/ephemeral-runners/internal/github"
)

// runCheck authenticates as the App, shows the queued jobs this host would
// serve, polls a second time to show the ETag cache at work and, with
// -mint, registers a test runner and deletes it again. It never prints the
// token, the key or a JIT config.
func runCheck(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/ephemeral-runners/config.json", "config file")
	mint := fs.String("mint", "", "owner/repo to register and delete a test runner in")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if *mint != "" && !config.ValidRepo(*mint) {
		return fmt.Errorf("-mint %q is not in owner/name form", *mint)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	key, err := github.LoadPrivateKey(cfg.GitHub.PrivateKeyFile)
	if err != nil {
		return err
	}
	gh, err := github.New(github.Options{
		BaseURL:        cfg.GitHub.APIURL,
		AppID:          cfg.GitHub.AppID,
		InstallationID: cfg.GitHub.InstallationID,
		Key:            key,
	})
	if err != nil {
		return err
	}

	rl, err := gh.GetRateLimit(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "authenticated: installation token valid until %s\n", gh.TokenExpiry().UTC().Format(time.RFC3339))
	printRateLimit(out, rl)

	for i := 1; i <= 2; i++ {
		before := gh.Stats()
		fmt.Fprintf(out, "\npoll %d\n", i)
		for _, repo := range cfg.Repos {
			jobs, err := gh.QueuedJobs(ctx, repo)
			if err != nil {
				return fmt.Errorf("%s: %w", repo, err)
			}
			printJobs(out, cfg, repo, jobs)
		}
		after := gh.Stats()
		fmt.Fprintf(out, "poll %d: %d requests, %d not modified (304)\n", i,
			after.Requests-before.Requests, after.NotModified-before.NotModified)
	}
	printRateLimit(out, gh.RateLimit())

	if *mint != "" {
		return mintCheck(ctx, out, gh, cfg, *mint)
	}
	return nil
}

func printRateLimit(out io.Writer, rl github.RateLimit) {
	fmt.Fprintf(out, "rate limit: %d of %d remaining, resets %s\n",
		rl.Remaining, rl.Limit, rl.Reset.UTC().Format(time.RFC3339))
}

func printJobs(out io.Writer, cfg *config.Config, repo string, jobs []github.Job) {
	other := 0
	var lines []string
	for _, j := range jobs {
		size, ok := cfg.SizeFor(j.Labels)
		if !ok {
			other++
			continue
		}
		lines = append(lines, fmt.Sprintf("  %-8s job %d run %d %q labels [%s], queued %s ago",
			size, j.ID, j.RunID, j.Name, strings.Join(j.Labels, " "), time.Since(j.CreatedAt).Round(time.Second)))
	}
	fmt.Fprintf(out, "%s: %d queued for this host, %d for other runners\n", repo, len(lines), other)
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
}

// mintCheck registers a JIT runner in repo, shows that GitHub has it, then
// deletes it and shows that it is gone.
func mintCheck(ctx context.Context, out io.Writer, gh *github.Client, cfg *config.Config, repo string) (err error) {
	suffix := make([]byte, 2)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	name := cfg.NamePrefix + "-check-" + hex.EncodeToString(suffix)

	fmt.Fprintf(out, "\nmint in %s\n", repo)
	jit, err := gh.GenerateJITConfig(ctx, repo, name, cfg.Labels)
	if err != nil {
		return fmt.Errorf("generate JIT config: %w", err)
	}
	fmt.Fprintf(out, "minted runner %d %s with labels [%s] (JIT config %d bytes, not shown)\n",
		jit.RunnerID, jit.Name, strings.Join(cfg.Labels, " "), len(jit.EncodedConfig))

	deleted := false
	defer func() {
		if deleted {
			return
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if derr := gh.DeleteRunner(cctx, repo, jit.RunnerID); derr != nil && !errors.Is(derr, github.ErrNotFound) {
			err = errors.Join(err, fmt.Errorf("delete runner %d: %w", jit.RunnerID, derr))
		}
	}()

	r, err := gh.GetRunner(ctx, repo, jit.RunnerID)
	if err != nil {
		return fmt.Errorf("get runner: %w", err)
	}
	fmt.Fprintf(out, "get runner %d: name %s, status %s, busy %t, labels [%s]\n",
		r.ID, r.Name, r.Status, r.Busy, strings.Join(r.Labels, " "))

	runners, err := gh.ListRunners(ctx, repo)
	if err != nil {
		return fmt.Errorf("list runners: %w", err)
	}
	found := false
	for _, x := range runners {
		found = found || x.ID == jit.RunnerID
	}
	fmt.Fprintf(out, "list runners: %d registered, %s listed: %t\n", len(runners), name, found)
	if !found {
		return fmt.Errorf("runner %d missing from the list", jit.RunnerID)
	}

	if err := gh.DeleteRunner(ctx, repo, jit.RunnerID); err != nil {
		return fmt.Errorf("delete runner: %w", err)
	}
	deleted = true
	fmt.Fprintf(out, "deleted runner %d\n", jit.RunnerID)

	_, err = gh.GetRunner(ctx, repo, jit.RunnerID)
	switch {
	case errors.Is(err, github.ErrNotFound):
		fmt.Fprintf(out, "get runner %d: not found, it is gone\n", jit.RunnerID)
		return nil
	case err != nil:
		return fmt.Errorf("get deleted runner: %w", err)
	default:
		return fmt.Errorf("runner %d still exists after delete", jit.RunnerID)
	}
}
