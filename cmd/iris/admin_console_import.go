package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mateusz-klatt/iris/internal/accountimport"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
)

// Only the source adapter reads SQLite here. The authenticated hub validates
// again and owns every target write. An omitted --apply is always a dry-run.
func adminConsoleImport(ctx context.Context, client *hubclient.Client, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("admin console import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	manifestPath := fs.String("manifest", "", "reviewed JSON manifest")
	sourcePath := fs.String("source", "", "read-only source SQLite file")
	apply := fs.Bool("apply", false, "commit the reviewed batch")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *manifestPath == "" || (args[0] == "import") != (*sourcePath != "") {
		return errors.New(adminConsoleUsage)
	}
	manifestFile, err := os.Open(*manifestPath)
	if err != nil {
		return errors.New("could not open import manifest")
	}
	defer func() { _ = manifestFile.Close() }()

	var request any
	route := "/v1/admin/console/import"
	if args[0] == "import" {
		manifest, err := accountimport.DecodeManifest(manifestFile)
		if err != nil {
			return err
		}
		batch, err := accountimport.Read(ctx, *sourcePath, manifest)
		if err != nil {
			return err
		}
		request = struct {
			Batch  accountimport.Batch `json:"batch"`
			DryRun bool                `json:"dry_run"`
		}{batch, !*apply}
	} else {
		var transfer store.ConsoleAssignmentTransfer
		if err := decodeAssignmentManifest(manifestFile, &transfer); err != nil {
			return err
		}
		route += "-grants"
		request = struct {
			Transfer store.ConsoleAssignmentTransfer `json:"transfer"`
			DryRun   bool                            `json:"dry_run"`
		}{transfer, !*apply}
	}
	var report store.ConsoleImportReport
	if err = client.Do(ctx, "POST", route, request, &report); err != nil {
		// Transport/server errors are not a trusted report: a proxy may echo a
		// credential-bearing request. Never forward their bodies to the terminal.
		return consoleImportFailure(err)
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func consoleImportFailure(err error) error {
	var refusal *hubclient.Error
	if errors.As(err, &refusal) {
		switch refusal.Code {
		case "bad_request", "conflict", "forbidden", "unauthorized", "too_large", "rate_limited":
			message := "console import refused: " + refusal.Code
			details, _ := refusal.Details.(map[string]any)
			row, _ := details["source_row_id"].(string)
			reason, _ := details["reason"].(string)
			if accountimport.ValidRowID(row) {
				switch reason {
				case "source_changed", "login_collision", "duplicate_assignment", "unsupported_assignment_role", "unmapped_project", "unimported_account", "assignment_requires_member", "unknown_repository", "assignment_changed", "grant_already_exists":
					message += fmt.Sprintf("; source row %s: %s", row, reason)
				}
			}
			return errors.New(message)
		}
	}
	return errors.New("console import request failed; no result confirmed; repeat dry-run before retrying")
}

func decodeAssignmentManifest(reader io.Reader, out *store.ConsoleAssignmentTransfer) error {
	if accountimport.DecodeJSON(reader, out) != nil {
		return errors.New("invalid assignment manifest")
	}
	return nil
}
