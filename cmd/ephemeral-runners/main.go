// Command ephemeral-runners runs self-hosted GitHub Actions runners as
// single-use VMs.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const usage = `usage: ephemeral-runners <command> [flags]

commands:
  check   check the GitHub App setup and show queued jobs
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "check":
		err = runCheck(ctx, os.Args[2:], os.Stdout)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ephemeral-runners:", err)
		os.Exit(1)
	}
}
