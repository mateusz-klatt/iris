package main

import (
	"context"
	"errors"
	"flag"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/watch"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	client := fs.String("client", "", "agent client")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *client == "" || fs.NArg() != 0 {
		return errors.New("usage: iris watch --client <client>")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	parent, err := watchParent()
	if err != nil {
		return err
	}
	maintained := make(chan struct{})
	defer func() { cancel(); <-maintained; parent.Close() }()
	resolver := binding.New(&hubclient.Client{Config: hubclient.NewConfig()}, *client)
	go func() { defer close(maintained); resolver.Maintain(ctx, "watch", 20*time.Second, parent.Alive, cancel) }()
	return (&watch.Runner{Resolver: resolver, Output: os.Stdout}).Run(ctx)
}
