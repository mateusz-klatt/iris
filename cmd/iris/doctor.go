package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/doctor"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

func runDoctor(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runDoctorIO(ctx, args, os.Stdin, os.Stdout, os.Stderr, doctor.RunLive)
}

type liveRunner func(context.Context, doctor.LiveOptions, doctor.LivePlan) doctor.LiveResult

func runDoctorIO(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, live liveRunner) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	liveFlag := fs.Bool("live", false, "run each client headless on its cheapest model in a scratch repository (billable)")
	yes := fs.Bool("yes", false, "with --live, do not ask before spending")
	timeout := fs.Duration("timeout", 0, "with --live, the limit for each client (default 3m)")
	only := fs.String("client", "", "with --live, run only this client")
	hub := fs.String("hub-url", "", "hub base URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: iris doctor [--hub-url url] [--live [--client name] [--yes] [--timeout d]]")
		return 2
	}
	offline, cancel := context.WithTimeout(ctx, 3*time.Minute)
	res, err := doctor.Run(offline, doctor.Options{HubURL: *hub})
	cancel()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = fmt.Fprint(stdout, res.Format())
	code := res.ExitCode()
	if !*liveFlag {
		return code
	}
	plans, skipped := doctor.LivePlans(integrate.Layout{}, res.Report.Clients, *only)
	if liveStage(ctx, plans, skipped, *yes, *timeout, stdin, stdout, stderr, live) != 0 {
		code = 1
	}
	return code
}

// liveStage says which clients it skips and why, shows what will be spent, asks unless yes is set, and runs each
// client in turn. It returns 1 when nothing ran or any client failed.
func liveStage(ctx context.Context, plans []doctor.LivePlan, skipped []string, yes bool, timeout time.Duration, stdin io.Reader, stdout, stderr io.Writer, live liveRunner) int {
	for _, why := range skipped {
		_, _ = fmt.Fprintln(stderr, "iris doctor --live skips "+why)
	}
	if len(plans) == 0 {
		_, _ = fmt.Fprintln(stderr, "iris doctor --live: no client is ready for a live run")
		return 1
	}
	_, _ = fmt.Fprintln(stderr, "iris doctor --live runs these clients in a scratch repository, and each call may incur a charge:")
	for _, p := range plans {
		_, _ = fmt.Fprintf(stderr, "  %s (%s), model %s\n", p.Client, p.Binary, p.Model)
		if p.Requires != "" {
			_, _ = fmt.Fprintf(stderr, "    %s: its hook runs only if you have done this: %s\n", p.Client, p.Requires)
		}
	}
	if !yes {
		_, _ = fmt.Fprint(stderr, "Proceed? [y/N] ")
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
			_, _ = fmt.Fprintln(stderr, "live run declined; nothing was run")
			return 1
		}
	}
	code := 0
	for _, p := range plans {
		_, _ = fmt.Fprintf(stderr, "running %s...\n", p.Client)
		r := live(ctx, doctor.LiveOptions{Layout: integrate.Layout{}, Timeout: timeout}, p)
		_, _ = fmt.Fprint(stdout, doctor.FormatLive([]doctor.LiveResult{r}))
		if !r.OK() {
			code = 1
		}
	}
	return code
}
