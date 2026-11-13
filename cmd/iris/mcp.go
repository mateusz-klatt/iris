package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/mcpserver"
	"github.com/mateusz-klatt/iris/internal/wake"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpWakeWorker interface{ Run(context.Context) }

// These boundaries keep stdio transport and the optional worker's lifetime independently
// observable. The normal command uses the real MCP transport and wake runner.
var mcpStdioTransport = func() mcp.Transport { return &mcp.StdioTransport{} }
var mcpObserver = hook.Observer
var newMCPWakeWorker = func(resolver *binding.Resolver, sessions *hook.Sessions, version func() string, observe func(string)) mcpWakeWorker {
	if resolver.Client == "kimi" {
		return wake.NewKimi(resolver, sessions, version, observe)
	}
	return wake.New(resolver, sessions, version, observe)
}

func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	client := fs.String("client", "", "agent client")
	wakeFlag := fs.Bool("wake", false, "wake this native session when attention is pending (default for Codex, opt-in for Kimi; --wake=false disables)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *client == "" || fs.NArg() != 0 {
		return errors.New("usage: iris mcp --client <client> [--wake=true|false]")
	}
	wakeEnabled := *client == "codex"
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "wake" {
			wakeEnabled = *wakeFlag
		}
	})
	if wakeEnabled && *client != "codex" && *client != "kimi" {
		return errors.New("--wake requires --client codex or kimi")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	parent, err := watchParent()
	if err != nil {
		return err
	}
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait(); parent.Close() }()
	config := hubclient.NewConfig()
	resolver := binding.New(&hubclient.Client{Config: config}, *client)
	// What a tool call's ids show goes to the log doctor reads, within a hook's small budget.
	resolver.Observe = mcpObserver(os.Getenv, *client)
	workers.Add(1)
	go func() { defer workers.Done(); resolver.Maintain(ctx, "shim", 20*time.Second, parent.Alive, cancel) }()
	var options []mcpserver.Option
	if wakeEnabled {
		if tokenPath, err := config.TokenPath(); err != nil {
			resolver.Observe("wake_invalid_state")
		} else {
			var version atomic.Value
			version.Store("")
			options = append(options, mcpserver.WithClientVersion(func(v string) { version.Store(v) }))
			worker := newMCPWakeWorker(resolver, hook.NewSessions(tokenPath), func() string { return version.Load().(string) }, resolver.Observe)
			workers.Add(1)
			go func() { defer workers.Done(); worker.Run(ctx) }()
		}
	}
	return mcpserver.New(resolver, options...).Run(ctx, mcpStdioTransport())
}
