package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/mateusz-klatt/iris/internal/doctor"
)

func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	live := fs.Bool("live", false, "run each client on its cheapest model (billable)")
	hub := fs.String("hub-url", "", "hub base URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: iris doctor [--live] [--hub-url url]")
		return 2
	}
	if *live {
		fmt.Fprintln(os.Stderr, "iris doctor --live calls a model and may incur a charge")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := doctor.Run(ctx, doctor.Options{HubURL: *hub, Live: *live})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Print(res.Format())
	return res.ExitCode()
}
