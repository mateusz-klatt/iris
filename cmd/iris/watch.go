package main

import (
	"context"
	"errors"
	"flag"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/watch"
	"io"
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
	resolver := binding.New(&hubclient.Client{Config: hubclient.NewConfig()}, *client)
	// The monitor shell does not inherit GROK_SESSION_ID. The hook wrote it beside the grok
	// process. A variable that is already set is left alone.
	if hook.Canonical(*client) == hook.ClientGrok {
		resolver.Getenv = hook.GrokSessionEnv(resolver.Getenv, resolver.Ancestor)
	}
	parent, err := watchParent()
	if err != nil {
		return err
	}
	maintained := make(chan struct{})
	defer func() { cancel(); <-maintained; parent.Close() }()
	go func() { defer close(maintained); resolver.Maintain(ctx, "watch", 20*time.Second, parent.Alive, cancel) }()
	err = (&watch.Runner{Resolver: resolver, Output: os.Stdout}).Run(ctx)
	return stopLine(os.Stdout, err, parent.Alive())
}

// stopLine makes sure a monitor that ends says why on its output, which reaches the model. Run
// has already said it for a lost generation; a failure to print cannot be printed.
func stopLine(w io.Writer, err error, parentAlive bool) error {
	var reason string
	switch {
	case errors.Is(err, watch.ErrLost):
		return err
	case err != nil:
		reason = err.Error()
	case !parentAlive:
		reason = "the client process ended"
	default:
		reason = "the monitor was stopped"
	}
	if perr := watch.Stopped(w, reason); perr != nil && err == nil {
		return perr
	}
	return err
}
