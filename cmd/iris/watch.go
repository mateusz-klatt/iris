package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/wake"
	"github.com/mateusz-klatt/iris/internal/watch"
)

var (
	runAntigravityWake = wake.RunAntigravity
	watchProcessArgs   = func() []string { return os.Args[1:] }
)

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	client := fs.String("client", "", "agent client")
	_ = fs.Bool("wake", false, "start detached Antigravity idle-wake helper")
	if err := fs.Parse(args); err != nil {
		return err
	}
	hasWake := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "wake" {
			hasWake = true
		}
	})
	if hasWake {
		if !slices.Equal(args, []string{"--client", "antigravity", "--wake"}) || !slices.Equal(watchProcessArgs(), []string{"watch", "--client", "antigravity", "--wake"}) {
			return errors.New("usage: iris watch --client antigravity --wake")
		}
		return runAntigravityWake()
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
