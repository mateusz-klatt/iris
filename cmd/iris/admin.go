package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/mateusz-klatt/iris/internal/backup"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/restorecheck"
)

const adminUsage = "usage: iris admin forget (--repository <id> | --agent <name>) [--dry-run]\n       iris admin merge --from <id> --into <id> [--dry-run]\n       iris admin contact --repository <id> (<agent> | --clear)\n       iris admin backup <hub database> <new backup file>\n       iris admin restore-check <backup file> <empty scratch directory>\n       iris admin console (create | reset) <username> < password-file\n       iris admin console import --source <SQLite file> --manifest <JSON file> [--apply]\n       iris admin console import-grants --manifest <JSON file> [--apply]"

func runAdmin(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return adminCommand(ctx, &hubclient.Client{Config: hubclient.NewConfig()}, args, os.Stdout)
}

// adminCommand runs operator commands, which need the hub's own token, and prints
// the hub's answer as JSON. forget prints what it removed, or with --dry-run what it
// would remove. contact sets or clears who a repository's repo: mail reaches (ADR 004).
func adminCommand(ctx context.Context, client *hubclient.Client, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(adminUsage)
	}
	var route string
	var body map[string]any
	switch args[0] {
	case "console":
		return adminConsoleCommand(ctx, client, args[1:], os.Stdin, stdout)
	case "merge":
		fs := flag.NewFlagSet("admin merge", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		from := fs.String("from", "", "source repository id")
		into := fs.String("into", "", "destination repository id")
		dryRun := fs.Bool("dry-run", false, "report what would move and change nothing")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *from == "" || *into == "" || *from == *into {
			return errors.New(adminUsage)
		}
		route, body = "/v1/admin/merge", map[string]any{"from_repository_id": *from, "into_repository_id": *into, "dry_run": *dryRun}
	case "backup":
		// Local too: it reads the database file the hub serves, on the hub's machine, while
		// the hub runs.
		if len(args) != 3 {
			return errors.New(adminUsage)
		}
		return backup.Run(ctx, args[1], args[2], stdout)
	case "restore-check":
		// Local: it runs a throwaway hub on a copy of a backup and never calls the live hub
		// (ADR 006).
		if len(args) != 3 {
			return errors.New(adminUsage)
		}
		return restorecheck.Run(ctx, args[1], args[2], stdout)
	case "forget":
		fs := flag.NewFlagSet("admin forget", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		repository := fs.String("repository", "", "repository id")
		agent := fs.String("agent", "", "agent name")
		dryRun := fs.Bool("dry-run", false, "report what would be removed and change nothing")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || (*repository == "") == (*agent == "") {
			return errors.New(adminUsage)
		}
		route, body = "/v1/admin/forget", map[string]any{"repository_id": *repository, "agent": *agent, "dry_run": *dryRun}
	case "contact":
		fs := flag.NewFlagSet("admin contact", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		repository := fs.String("repository", "", "repository id")
		clearContact := fs.Bool("clear", false, "remove the contact, so repo: mail reaches the human inbox")
		if err := fs.Parse(args[1:]); err != nil || *repository == "" || (fs.NArg() == 1) == *clearContact || fs.NArg() > 1 {
			return errors.New(adminUsage)
		}
		route, body = "/v1/admin/contact", map[string]any{"repository_id": *repository, "agent": fs.Arg(0)}
	default:
		return errors.New(adminUsage)
	}
	var out map[string]any
	if err := client.Do(ctx, "POST", route, body, &out); err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
