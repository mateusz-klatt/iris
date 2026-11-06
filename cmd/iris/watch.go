package main

import (
	"context"
	"errors"
	"flag"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
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
	parent := os.Getppid()
	resolver := binding.New(&hubclient.Client{Config: hubclient.EnvConfig{}}, *client)
	go resolver.Maintain(ctx, "watch", 20*time.Second, func() bool { return proc.ParentUnchanged(parent) }, cancel)
	return (&watch.Runner{Resolver: resolver, Output: os.Stdout}).Run(ctx)
}
