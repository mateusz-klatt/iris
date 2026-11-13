package main

import (
	"flag"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// hookShellDeadline is the shell reconcile budget a hook process uses. Zero, as in every real
// hook, leaves it to the hub's address (ReconcileDeadline for a hub on this machine). A test's
// child hook raises it when the test checks what a reduced reconcile does, not how fast a loaded
// machine runs it.
var hookShellDeadline time.Duration

func runHook(args []string) int {
	return runHookIO(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv, hook.DefaultDeadline)
}

func runHookIO(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string, deadline time.Duration) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	client := fs.String("client", "", "agent client")
	event := fs.String("event", "", "hook event name")
	_ = fs.Parse(args)
	// A parse error must not brick the client. Prefer the client named in argv
	// so the allow bytes still match that contract.
	name := *client
	if name == "" {
		name = clientFromArgs(args)
	}
	out := hook.Runner{
		ClientName: name,
		EventName:  *event,
		Stdin:      stdin,
		Getenv:     getenv,
		// Load client.json inside Run. A stalled or panicking read must stay
		// inside the deadline and the panic recovery, or the hook breaks D3.
		Hub:           &hubclient.Client{Config: hubclient.EnvConfig{Getenv: getenv}, HTTP: hubclient.HedgedClient()},
		Deadline:      deadline,
		ShellDeadline: hookShellDeadline,
	}.Run()
	_, _ = stdout.Write(out.Stdout)
	_, _ = stderr.Write(out.Stderr)
	return out.Exit
}

func clientFromArgs(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--client" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, "--client=") {
			return strings.TrimPrefix(a, "--client=")
		}
	}
	return ""
}
