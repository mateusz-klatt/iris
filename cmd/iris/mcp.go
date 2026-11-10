package main

import (
	"context"
	"errors"
	"flag"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"os"
	"time"
)

func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	client := fs.String("client", "", "agent client")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *client == "" || fs.NArg() != 0 {
		return errors.New("usage: iris mcp --client <client>")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	parent, err := watchParent()
	if err != nil {
		return err
	}
	maintained := make(chan struct{})
	defer func() { cancel(); <-maintained; parent.Close() }()
	resolver := binding.New(&hubclient.Client{Config: hubclient.NewConfig()}, *client)
	// What a tool call's ids show goes to the log doctor reads, within a hook's small budget.
	resolver.Observe = hook.Observer(os.Getenv, *client)
	go func() { defer close(maintained); resolver.Maintain(ctx, "shim", 20*time.Second, parent.Alive, cancel) }()
	return mcpserver.New(resolver).Run(ctx, &mcp.StdioTransport{})
}
