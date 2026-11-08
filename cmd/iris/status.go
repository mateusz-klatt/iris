package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func runStatus(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runStatusIO(ctx, args, hubclient.NewConfig(), os.Stdout, os.Stderr)
}

// runStatusIO prints hubclient.Status as text or, with --json, as one JSON object. Every failure
// exits 1, and with --json it is still one JSON object on stdout.
func runStatusIO(ctx context.Context, args []string, cfg hubclient.Config, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print one JSON object")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: iris status [--json]")
		return 2
	}
	report := hubclient.Status(ctx, cfg)
	switch {
	case *asJSON:
		raw, _ := json.Marshal(report) // strings, numbers and maps decoded from JSON always marshal
		_, _ = fmt.Fprintln(stdout, string(raw))
	case report.Error != nil:
		_, _ = fmt.Fprintf(stderr, "iris status: %s: %s\n", report.Error.Code, report.Error.Message)
	default:
		printStatus(stdout, report.Hub, report.Status)
	}
	if report.Error != nil {
		return 1
	}
	return 0
}

// printStatus writes the document as sorted key: value lines, counts on one line.
func printStatus(w io.Writer, hub string, doc map[string]any) {
	_, _ = fmt.Fprintf(w, "hub: %s\n", hub)
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		value := doc[k]
		if nested, ok := value.(map[string]any); ok {
			parts := make([]string, 0, len(nested))
			for name, n := range nested {
				parts = append(parts, fmt.Sprintf("%s=%v", name, n))
			}
			sort.Strings(parts)
			value = strings.Join(parts, " ")
		}
		_, _ = fmt.Fprintf(w, "%s: %v\n", k, value)
	}
}
