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
	parent := os.Getppid()
	resolver := binding.New(&hubclient.Client{Config: hubclient.EnvConfig{}}, *client)
	go resolver.Maintain(ctx, "shim", 20*time.Second, func() bool { return proc.ParentUnchanged(parent) }, cancel)
	return mcpserver.New(resolver).Run(ctx, &mcp.StdioTransport{})
}
