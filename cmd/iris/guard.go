package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mateusz-klatt/iris/internal/guard"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func runGuard(args []string) error {
	const help = "usage: iris guard install|remove|check\nCooperative, per-repository pre-commit checkpoint: --no-verify bypasses it. Checks staged paths against claims in other worktrees. Hub failures warn and allow. It detects conflicts and cannot undo edits."
	if len(args) == 0 || (len(args) == 1 && (args[0] == "--help" || args[0] == "-h")) {
		fmt.Println(help)
		return nil
	}
	if len(args) != 1 {
		return errors.New(help) //nolint:staticcheck // usage text for a person, shown as written
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	switch args[0] {
	case "install":
		return guard.Install(ctx, dir)
	case "remove":
		return guard.Remove(ctx, dir)
	case "check":
		message, err := guard.Check(ctx, dir, &hubclient.Client{Config: hubclient.NewConfig()})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Iris: guard could not check claims; allowing commit: %v\n", err)
			return nil
		}
		if message != "" {
			return errors.New(message)
		}
		return nil
	default:
		return errors.New(help) //nolint:staticcheck // usage text for a person, shown as written
	}
}
