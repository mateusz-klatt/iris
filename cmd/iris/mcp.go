package main

import (
	"context"
	"errors"
	"flag"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/mcpserver"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	parent, err := proc.WatchParent()
	if err != nil {
		return err
	}
	maintained := make(chan struct{})
	defer func() { cancel(); <-maintained; parent.Close() }()
	resolver := binding.New(&hubclient.Client{Config: hubclient.EnvConfig{}}, *client)
	go func() { defer close(maintained); resolver.Maintain(ctx, "shim", 20*time.Second, parent.Alive, cancel) }()
	return mcpserver.New(resolver).Run(ctx, &mcp.StdioTransport{})
}
