package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

func runIntegrate(args []string) int {
	fs := flag.NewFlagSet("integrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	remove := fs.Bool("remove", false, "restore the files Iris wrote")
	all := fs.Bool("all", false, "every supported client")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var names []string
	if *all {
		if fs.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "usage: iris integrate [--remove] [--all] <client>")
			return 2
		}
		names = append(names, integrate.Names...)
	} else if fs.NArg() == 1 {
		name := integrate.Canonical(fs.Arg(0))
		if name == "" {
			fmt.Fprintf(os.Stderr, "unknown client %q\n", fs.Arg(0))
			return 2
		}
		names = []string{name}
	} else {
		fmt.Fprintln(os.Stderr, "usage: iris integrate [--remove] [--all] <client>")
		return 2
	}
	layout := integrate.Layout{}
	plugins := integrate.ExecPlugins{}
	code := 0
	for _, name := range names {
		// names are Canonical or integrate.Names, both of which ByName resolves.
		c, _ := integrate.ByName(name)
		var err error
		if *remove {
			err = c.Remove(layout, plugins)
		} else {
			err = c.Install(layout, plugins)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
			code = 1
			continue
		}
		if *remove {
			fmt.Printf("removed %s\n", name)
		} else {
			fmt.Printf("integrated %s\n", name)
		}
	}
	return code
}
