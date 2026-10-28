package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/ryanlewis/ephemeral-runners/internal/config"
	"github.com/ryanlewis/ephemeral-runners/internal/github"
	"github.com/ryanlewis/ephemeral-runners/internal/loop"
	"github.com/ryanlewis/ephemeral-runners/internal/vm"
)

// runService runs the runner loop until ctx ends.
func runService(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/ephemeral-runners/config.json", "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
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
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("starting", "repos", cfg.Repos, "prefix", cfg.NamePrefix, "budget_vcpus", cfg.Budget.VCPUs,
		"budget_memory_gib", cfg.Budget.MemoryGiB, "spares", cfg.Spares.Count, "spare_size", cfg.Spares.Size)
	l := loop.New(loop.Options{
		Config: cfg,
		GitHub: gh,
		VMs:    &vm.Launcher{URI: cfg.VM.LibvirtURI},
		Logger: log,
	})
	err = l.Run(ctx)
	log.Info("stopped")
	return err
}
