package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

const adminUsage = "usage: iris admin forget (--repository <id> | --agent <name>) [--dry-run]"

func runAdmin(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return adminCommand(ctx, &hubclient.Client{Config: hubclient.NewConfig()}, args, os.Stdout)
}

// adminCommand runs operator commands, which need the hub's own token. forget prints
// what it removed, or with --dry-run what it would remove, as JSON.
func adminCommand(ctx context.Context, client *hubclient.Client, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "forget" {
		return errors.New(adminUsage)
	}
	fs := flag.NewFlagSet("admin forget", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repository := fs.String("repository", "", "repository id")
	agent := fs.String("agent", "", "agent name")
	dryRun := fs.Bool("dry-run", false, "report what would be removed and change nothing")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || (*repository == "") == (*agent == "") {
		return errors.New(adminUsage)
	}
	var out map[string]any
	body := map[string]any{"repository_id": *repository, "agent": *agent, "dry_run": *dryRun}
	if err := client.Do(ctx, "POST", "/v1/admin/forget", body, &out); err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
